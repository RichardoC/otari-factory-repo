package main

// The contract tests run the harness binary the way the workflows do
// (issue #135): TestMain builds it into a temp dir with the config and
// instruction files copied beside it (exactly what codeaf.yml's run job
// does with cp -r .github/codeaf and go build -trimpath), and every
// subcommand below runs with only the environment the workflow sets, in a
// scratch working directory, against fixtures under testdata/contract/.
// Each test asserts the exact stdout or GITHUB_OUTPUT lines the workflows
// parse with sed and grep -x, so a subcommand that reads the wrong variable
// or prints the wrong line fails here, not in Actions.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// needContractBin skips a subtest when TestMain did not build the binary
// (-short): the contract tests are the only ones that need it.
func needContractBin(t *testing.T) {
	t.Helper()
	if contractBin == "" {
		t.Skip("no contract binary: tests run with -short")
	}
}

// contractFixture names a file under testdata/contract, as an absolute
// path: the child's working directory is a scratch dir, so a relative path
// would not resolve.
func contractFixture(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(wd, "testdata", "contract", name)
}

// runHarness runs contractBin with ONLY the given environment plus PATH: a
// subcommand that reads a variable the workflow does not set fails here, not
// pass through the test process's own environment. The child's working
// directory is a fresh empty temp dir, mimicking a workflow step at the
// repository root: relative fixture paths must be absolute, and a
// harnessDir() that answered os.Getwd() would find no config files.
func runHarness(t *testing.T, env map[string]string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	scratch := t.TempDir()
	cmd := exec.Command(contractBin, args...)
	cmd.Dir = scratch
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		cmd.Env = append(cmd.Env, name+"="+env[name])
	}
	var out, err strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &err
	code = 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatalf("run %s: %v", strings.Join(args, " "), err)
		}
	}
	return out.String(), err.String(), code
}

// sortStrings sorts in place; a local helper keeps this file free of a sort
// import clash with main.go's use of slices.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// writeContractFile writes data to a fresh file and returns its path.
func writeContractFile(t *testing.T, dir, name, data string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// readContractFile reads a file whose content a subtest asserts on.
func readContractFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestContract exercises every subcommand the workflows call, through the
// binary and the environment, never through the Go functions.
func TestContract(t *testing.T) {
	t.Run("gate", func(t *testing.T) {
		needContractBin(t)
		// The gate job's API: /repos/o/r/issues/N and /pulls/N, as
		// GITHUB_API_URL serves them.
		issue3 := item{Number: 3, Title: "Harness: contract tests", State: "open",
			Labels: []label{{Name: "codeaf"}}, CreatedAt: "2026-10-16T10:00:00Z"}
		issue4 := item{Number: 4, Title: "Model experiment: glm53", State: "open",
			Labels: []label{{Name: "codeaf"}, {Name: experimentLabel}}, CreatedAt: "2026-10-16T11:00:00Z"}
		pr7 := item{Number: 7, Title: "Harness: contract tests", State: "open",
			Labels: []label{{Name: "codeaf"}}, CreatedAt: "2026-10-16T12:00:00Z"}
		pr7.Head.Ref = "codeaf/issue-3-1"
		pr7.Head.Repo = &struct {
			FullName string `json:"full_name"`
		}{FullName: "o/r"}
		pr7.Base.Ref = "main"
		// The issues are served as literal JSON, like the real API: an
		// issue carries no pull_request key at all, and a "pull_request":
		// null would read back as a non-empty RawMessage and make the gate
		// see a PR.
		issueJSON := func(it item) string {
			labels := make([]string, 0, len(it.Labels))
			for _, l := range it.Labels {
				labels = append(labels, `{"name":"`+l.Name+`"}`)
			}
			return fmt.Sprintf(`{"number":%d,"title":%q,"state":%q,"labels":[%s],"created_at":%q}`,
				it.Number, it.Title, it.State, strings.Join(labels, ","), it.CreatedAt)
		}
		prJSON, _ := json.Marshal(pr7)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body string
			switch r.URL.Path {
			case "/repos/o/r/issues/3":
				body = issueJSON(issue3)
			case "/repos/o/r/issues/4":
				body = issueJSON(issue4)
			case "/repos/o/r/issues/7", "/repos/o/r/pulls/7":
				body = string(prJSON)
			default:
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		defer server.Close()

		// The module dir, as `go run . gate` with HARNESS_DIR="$PWD" sees
		// it in the gate job: models.json and experiments.json beside the
		// source. Without HARNESS_DIR the binary finds them beside itself,
		// as the run job's copied dir does.
		moduleDir, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}

		// want is the exact GITHUB_OUTPUT content the gate job's outputs
		// carry (and the gate's stdout): GITHUB_RUN_ID=1, so the issue
		// branch is codeaf/issue-<n>-1.
		runGate := func(t *testing.T, event string, want string, extra map[string]string) {
			t.Helper()
			env := map[string]string{
				"GITHUB_API_URL":    server.URL,
				"GITHUB_REPOSITORY": "o/r",
				"GITHUB_RUN_ID":     "1",
				"GITHUB_EVENT_NAME": "workflow_dispatch",
				"GITHUB_EVENT_PATH": contractFixture(t, event),
				"GITHUB_OUTPUT":     filepath.Join(t.TempDir(), "output.txt"),
			}
			for k, v := range extra {
				env[k] = v
			}
			stdout, stderr, code := runHarness(t, env, "gate")
			if code != 0 {
				t.Fatalf("gate: exit %d, stderr: %s", code, stderr)
			}
			if stdout != want {
				t.Errorf("gate stdout =\n%q\nwant\n%q", stdout, want)
			}
			if got := readContractFile(t, env["GITHUB_OUTPUT"]); got != want {
				t.Errorf("GITHUB_OUTPUT =\n%q\nwant\n%q", got, want)
			}
		}
		runGateBoth := func(t *testing.T, event, want string) {
			t.Helper()
			t.Run("without_HARNESS_DIR", func(t *testing.T) {
				runGate(t, event, want, nil)
			})
			t.Run("with_HARNESS_DIR", func(t *testing.T) {
				runGate(t, event, want, map[string]string{"HARNESS_DIR": moduleDir})
			})
		}

		t.Run("issue", func(t *testing.T) {
			runGateBoth(t, "dispatch-issue.json",
				"run=true\nreason=workflow dispatch\nlane=2\nmode=issue\nnumber=3\nbranch=codeaf/issue-3-1\nbase=main\nprofile=\n")
		})
		t.Run("pr", func(t *testing.T) {
			runGateBoth(t, "dispatch-pr.json",
				"run=true\nreason=workflow dispatch\nlane=0\nmode=pr\nnumber=7\nbranch=codeaf/issue-3-1\nbase=main\nprofile=\n")
		})
		t.Run("review", func(t *testing.T) {
			runGateBoth(t, "dispatch-review.json",
				"run=true\nreason=review dispatch\nlane=0\nmode=review\nnumber=7\nbranch=codeaf/issue-3-1\nbase=main\nprofile=\n")
		})
		t.Run("plan", func(t *testing.T) {
			runGateBoth(t, "dispatch-plan.json",
				"run=true\nreason=plan dispatch\nlane=1\nmode=plan\nnumber=0\nbranch=\nbase=main\nprofile=\n")
		})
		// A routine dispatch (issue #117) validates its name against
		// routines.json beside the binary or beside the source, like the
		// profile against experiments.json: both layouts must find it.
		t.Run("routine", func(t *testing.T) {
			runGateBoth(t, "dispatch-routine.json",
				"run=true\nreason=routine dispatch\nlane=0\nmode=routine\nnumber=0\nbranch=\nbase=main\nprofile=\n")
		})
		// The profile dispatch validates the named profile against
		// experiments.json beside the binary or beside the source (the
		// regression PR #85 fixed: the go run layout found no
		// experiments.json). Both layouts must find it.
		t.Run("profile_without_HARNESS_DIR", func(t *testing.T) {
			runGate(t, "dispatch-profile.json",
				"run=true\nreason=workflow dispatch\nlane=0\nmode=issue\nnumber=4\nbranch=codeaf/issue-4-1\nbase=main\nprofile=glm53\n", nil)
		})
		t.Run("profile_with_HARNESS_DIR", func(t *testing.T) {
			runGate(t, "dispatch-profile.json",
				"run=true\nreason=workflow dispatch\nlane=0\nmode=issue\nnumber=4\nbranch=codeaf/issue-4-1\nbase=main\nprofile=glm53\n",
				map[string]string{"HARNESS_DIR": moduleDir})
		})
	})

	t.Run("seat-config", func(t *testing.T) {
		needContractBin(t)
		cfg, err := loadConfig("models.json")
		if err != nil {
			t.Fatal(err)
		}
		wantParallel := float64(minPerRunSlots(cfg.Models, cfg.Queue.MaxParallelRuns))
		allowed := map[string]bool{}
		for _, m := range cfg.Models {
			allowed[m.ID] = true
		}
		lanes := laneModels(cfg.Models)
		for lane := 0; lane < cfg.Queue.MaxParallelRuns; lane++ {
			stdout, stderr, code := runHarness(t, nil, "seat-config", fmt.Sprint(lane), "")
			if code != 0 {
				t.Fatalf("seat-config %d: exit %d, stderr: %s", lane, code, stderr)
			}
			var config map[string]any
			if err := json.Unmarshal([]byte(stdout), &config); err != nil {
				t.Fatalf("seat-config %d: stdout is not JSON: %v: %s", lane, err, stdout)
			}
			worker, _ := config["models.tiers.worker"].(string)
			if !allowed[worker] {
				t.Errorf("seat-config %d: models.tiers.worker %q is not a models.json id", lane, worker)
			}
			mastermind, _ := config["models.tiers.mastermind"].(string)
			high, _ := config["models.tiers.high"].(string)
			// Issue #181: the three seats are allowed models, and the
			// worker differs from the planner and the checker on every
			// lane, so a run never has all three seats on one model. The
			// planner and the checker may share a model (issue #222).
			for key, id := range map[string]string{"models.tiers.worker": worker, "models.tiers.mastermind": mastermind, "models.tiers.high": high} {
				if !allowed[id] {
					t.Errorf("seat-config %d: %s %q is not a models.json id", lane, key, id)
				}
			}
			if mastermind == worker {
				t.Errorf("seat-config %d: mastermind %q is the worker model", lane, mastermind)
			}
			if high == worker {
				t.Errorf("seat-config %d: high %q is the worker model", lane, high)
			}
			if worker == mastermind && worker == high && mastermind == high {
				t.Errorf("seat-config %d: all three seats are %q", lane, worker)
			}
			// The lane whose worker is the model the config pins the two
			// seats to rotates both onto the cheapest other lane model:
			// with the committed files, lane 1 (worker
			// deepseek-ai/deepseek-v4.1-flash-1017) seats the planner and the
			// checker on zai-org/glm-5.3-flash.
			if want := lanes[lane%len(lanes)].ID; worker != want {
				t.Errorf("seat-config %d: worker %q, want lane model %q", lane, worker, want)
			}
			if worker == mastermind || worker == high {
				var rotated string
				var cost float64
				for _, m := range lanes {
					if m.ID == worker {
						continue
					}
					if rotated == "" || m.APIInput+m.APIOutput < cost {
						rotated, cost = m.ID, m.APIInput+m.APIOutput
					}
				}
				if rotated == "" {
					t.Fatalf("seat-config %d: no lane model differs from the worker", lane)
				}
				if worker == mastermind && mastermind != rotated {
					t.Errorf("seat-config %d: mastermind %q, want the cheapest other lane model %q", lane, mastermind, rotated)
				}
				if worker == high && high != rotated {
					t.Errorf("seat-config %d: high %q, want the cheapest other lane model %q", lane, high, rotated)
				}
			}
			if got := config["task.parallel"]; got != wantParallel {
				t.Errorf("seat-config %d: task.parallel = %v, want minPerRunSlots %v", lane, got, wantParallel)
			}
		}
		// An unknown profile fails the run rather than quietly running the
		// default seats.
		if _, _, code := runHarness(t, nil, "seat-config", "0", "nosuch"); code == 0 {
			t.Error("seat-config 0 nosuch: exit 0, want non-zero")
		}
	})

	t.Run("next", func(t *testing.T) {
		needContractBin(t)
		issues, runs := contractFixture(t, "issues.json"), contractFixture(t, "runs.json")
		stdout, stderr, code := runHarness(t, map[string]string{"CODEAF_WEEK_SPENT_USD": "0"}, "next", issues, runs)
		if code != 0 {
			t.Fatalf("next: exit %d, stderr: %s", code, stderr)
		}
		// Issue #137: next names the kind of run it picked.
		if want := "kind=issue\nnumber=3\nlane=0\n"; stdout != want {
			t.Errorf("next stdout =\n%q\nwant\n%q", stdout, want)
		}
		// A missing week-spend total must fail the pick: fail closed, no
		// label on unknown spend.
		if _, _, code := runHarness(t, nil, "next", issues, runs); code == 0 {
			t.Error("next without CODEAF_WEEK_SPENT_USD: exit 0, want non-zero")
		}
	})

	t.Run("plan-check", func(t *testing.T) {
		needContractBin(t)
		issues, runs := contractFixture(t, "issues.json"), contractFixture(t, "runs.json")
		stdout, stderr, code := runHarness(t, map[string]string{"CODEAF_WEEK_SPENT_USD": "0"}, "plan-check", issues, runs)
		if code != 0 {
			t.Fatalf("plan-check: exit %d, stderr: %s", code, stderr)
		}
		if want := "plan=1\nlane=0\n"; stdout != want {
			t.Errorf("plan-check stdout =\n%q\nwant\n%q (the one queued issue is under min_backlog)", stdout, want)
		}
	})

	t.Run("workflow-policy", func(t *testing.T) {
		needContractBin(t)
		if _, err := exec.LookPath("yq"); err != nil {
			t.Skip("yq is not on PATH")
		}
		workflows, err := filepath.Abs(filepath.Join("..", "workflows"))
		if err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(workflows)
		if err != nil {
			t.Fatal(err)
		}
		// Both dirs hold the current workflows, converted with yq exactly
		// as the publish job converts the default branch's and the
		// patched ones (codeaf.yml).
		convert := func(dir string) {
			t.Helper()
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
					continue
				}
				out, err := exec.Command("yq", "-o=json", ".", filepath.Join(workflows, e.Name())).Output()
				if err != nil {
					t.Fatalf("yq %s: %v", e.Name(), err)
				}
				writeContractFile(t, dir, strings.TrimSuffix(e.Name(), ".yml")+".json", string(out))
			}
		}
		// The publish job relies on mikefarah's yq, preinstalled on GitHub's
		// runners; another tool named yq takes different arguments, so skip
		// rather than fail where that one is installed.
		if v, err := exec.Command("yq", "--version").CombinedOutput(); err != nil || !strings.Contains(string(v), "mikefarah") {
			t.Skipf("mikefarah yq not available (yq --version: %q, %v)", strings.TrimSpace(string(v)), err)
		}
		before, after := t.TempDir(), t.TempDir()
		convert(before)
		convert(after)
		stdout, stderr, code := runHarness(t, nil, "workflow-policy", before, after)
		if code != 0 {
			t.Fatalf("workflow-policy: exit %d, stderr: %s", code, stderr)
		}
		if want := "workflow policy: ok\n"; stdout != want {
			t.Errorf("workflow-policy stdout = %q, want %q", stdout, want)
		}
	})

	t.Run("pr-mode", func(t *testing.T) {
		needContractBin(t)
		stale := writeContractFile(t, t.TempDir(), "pr-body-extra.md", "an early-stop note from a previous run")
		stdout, stderr, code := runHarness(t, map[string]string{
			"RESULT_PATH":        contractFixture(t, "codeaf-result.json"),
			"PR_BODY_EXTRA_PATH": stale,
		}, "pr-mode")
		if code != 0 {
			t.Fatalf("pr-mode: exit %d, stderr: %s", code, stderr)
		}
		// codeaf.yml greps these lines exactly: early=false, ready=true,
		// stop=done.
		if want := "early=false\nready=true\nstop=done\n"; stdout != want {
			t.Errorf("pr-mode stdout = %q, want %q", stdout, want)
		}
		// A finished run drops a stale early-stop note.
		if _, err := os.Stat(stale); !os.IsNotExist(err) {
			t.Errorf("pr-mode left the stale PR_BODY_EXTRA_PATH file behind: %v", err)
		}
	})

	t.Run("continue-plan", func(t *testing.T) {
		needContractBin(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/o/r/issues/5/comments":
				w.Write([]byte("[]")) // no comments: no report, so 0 continuations used
			case "/repos/o/r/pulls/5":
				// Issue #137: after a done stop continue-plan reads the PR's
				// merge state; a clean PR needs no continuation.
				w.Write([]byte(`{"number":5,"mergeable_state":"clean"}`))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		body := filepath.Join(t.TempDir(), "continue.md")
		env := func(resultPath string) map[string]string {
			return map[string]string{
				"GITHUB_API_URL":        server.URL,
				"GITHUB_REPOSITORY":     "o/r",
				"RESULT_PATH":           resultPath,
				"CONTINUE_COMMENT_PATH": body,
			}
		}
		// A done run needs no continuation.
		stdout, stderr, code := runHarness(t, env(contractFixture(t, "codeaf-result.json")), "continue-plan", "5")
		if code != 0 {
			t.Fatalf("continue-plan (done): exit %d, stderr: %s", code, stderr)
		}
		if want := "action=none\n"; stdout != want {
			t.Errorf("continue-plan (done) stdout = %q, want %q", stdout, want)
		}
		// An early stop that continues writes the comment body for the
		// workflow to post.
		result := writeContractFile(t, t.TempDir(), "result.json", `{"stop":"deadline"}`)
		stdout, stderr, code = runHarness(t, env(result), "continue-plan", "5")
		if code != 0 {
			t.Fatalf("continue-plan (deadline): exit %d, stderr: %s", code, stderr)
		}
		if want := "action=continue\n"; stdout != want {
			t.Errorf("continue-plan (deadline) stdout = %q, want %q", stdout, want)
		}
		if got := readContractFile(t, body); !strings.Contains(got, "/codeaf continue: deadline") ||
			!strings.Contains(got, "Continuation 1 of 2") {
			t.Errorf("continue-plan comment body = %q, want the /codeaf continue command and the continuation count", got)
		}
	})

	t.Run("review-report", func(t *testing.T) {
		needContractBin(t)
		// Issue #237: the review plan reads the pull request before it
		// labels, so a contract review runs against a served pull request
		// that is ready for review. The diff fetch stays best-effort: a
		// server without it just means the findings are parsed without one.
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/o/r/pulls/7" {
				_, _ = fmt.Fprint(w, `{"number":7,"title":"t","state":"open","draft":false}`)
				return
			}
			http.NotFound(w, r)
		}))
		defer server.Close()
		comment := filepath.Join(t.TempDir(), "review-comment.md")
		stdout, stderr, code := runHarness(t, map[string]string{
			"RESULT_PATH":         contractFixture(t, "codeaf-result.json"),
			"LEDGER_PATH":         contractFixture(t, "codeaf-ledger.jsonl"),
			"REVIEW_COMMENT_PATH": comment,
			"GITHUB_API_URL":      server.URL,
			"GITHUB_REPOSITORY":   "o/r",
			"GITHUB_TOKEN":        "tok",
		}, "review-report", "7")
		if code != 0 {
			t.Fatalf("review-report: exit %d, stderr: %s", code, stderr)
		}
		// A done review with no blocking findings labels the PR: three
		// output lines, parsed by codeaf.yml with sed.
		if want := "action=label\nused=0\nblocking=0\n"; stdout != want {
			t.Errorf("review-report stdout = %q, want %q", stdout, want)
		}
		if got := readContractFile(t, comment); !strings.Contains(got, reviewPassedLabel) {
			t.Errorf("review comment body = %q, want it to name the %s label", got, reviewPassedLabel)
		}
	})

	t.Run("plan-report", func(t *testing.T) {
		needContractBin(t)
		// A plan run's proposals live in its answer (the plan-brief tells
		// CodeAF the format plan-report parses back).
		result := writeContractFile(t, t.TempDir(), "plan-result.json",
			`{"stop":"done","answer":"## Proposals\n\n### proposal: Add contract tests for the harness\n- Problem: The harness subcommands are exercised only through Go functions.\n- What to do: Add contract tests.\n- Done when: go test -run TestContract passes.\n- Area: harness\n- Depends on: #135\n"}`)
		proposals := filepath.Join(t.TempDir(), "proposals.json")
		stdout, stderr, code := runHarness(t, map[string]string{
			"RESULT_PATH":    result,
			"PROPOSALS_PATH": proposals,
		}, "plan-report")
		if code != 0 {
			t.Fatalf("plan-report: exit %d, stderr: %s", code, stderr)
		}
		if want := "proposals=1 held=0\n"; stdout != want {
			t.Errorf("plan-report stdout = %q, want %q", stdout, want)
		}
		var out []planProposalOut
		if err := json.Unmarshal([]byte(readContractFile(t, proposals)), &out); err != nil {
			t.Fatalf("plan-report PROPOSALS_PATH is not a JSON array: %v", err)
		}
		if len(out) != 1 || out[0].Title != "Add contract tests for the harness" {
			t.Fatalf("plan-report proposals = %+v, want one titled proposal", out)
		}
		if !strings.Contains(out[0].Body, "### Proposal: Add contract tests for the harness") {
			t.Errorf("plan-report proposal body = %q, want the ProposalBody template", out[0].Body)
		}
	})

	t.Run("report", func(t *testing.T) {
		needContractBin(t)
		stdout, stderr, code := runHarness(t, map[string]string{
			"RESULT_PATH":         contractFixture(t, "codeaf-result.json"),
			"LEDGER_PATH":         contractFixture(t, "codeaf-ledger.jsonl"),
			"PUBLISH_ERRORS_PATH": contractFixture(t, "publish-errors.log"),
			"PATCH_PATH":          contractFixture(t, "changes.patch"),
			"GITHUB_RUN_ID":       "1",
			"GITHUB_REPOSITORY":   "o/r",
		}, "report")
		if code != 0 {
			t.Fatalf("report: exit %d, stderr: %s", code, stderr)
		}
		if n := strings.Count(stdout, reportMarker); n != 1 {
			t.Errorf("report carries the marker %d times, want exactly once", n)
		}
		modelHeader := "| model | $/M in · cached · out | calls | input tok | cached tok | output tok | reasoning tok | reasoning share | cost |"
		if !strings.Contains(stdout, modelHeader) {
			t.Errorf("report has no per-model table header:\n%s", stdout)
		}
		if !strings.Contains(stdout, "| `zai-org/glm-5.3-flash` |") {
			t.Errorf("report has no per-model row for the ledger's worker model")
		}
		// The fixtures name only models models.json still lists, so the
		// seats render by id (not "(other)") and the ledger row is priced:
		// issue #518 retired deepseek-ai/deepseek-v4.1-flash, and a fixture
		// still naming it would render an unpriced "(other)" row here.
		if !strings.Contains(stdout, "planner `deepseek-ai/deepseek-v4.1-flash-1017`, checker `deepseek-ai/deepseek-v4.1-flash-1017`") ||
			strings.Contains(stdout, "(other)") {
			t.Errorf("report does not render the planner and the checker by id:\n%s", stdout)
		}
		if !strings.Contains(stdout, "| `deepseek-ai/deepseek-v4.1-flash-1017` | 0.6 · 0.03 · 1.2 |") {
			t.Errorf("report has no priced per-model row for the ledger's planner model:\n%s", stdout)
		}
		// The publish job's ::error:: lines reach the report verbatim (the
		// two most recent; the fixture's oldest line is dropped).
		if !strings.Contains(stdout, "| publish error | push rejected: non-fast-forward |") ||
			!strings.Contains(stdout, "| publish error | the workflow change breaks 1 rule(s) |") {
			t.Errorf("report lost the publish-errors lines:\n%s", stdout)
		}
		if strings.Contains(stdout, "oldest failure") {
			t.Errorf("report kept a publish-errors line past the last two:\n%s", stdout)
		}
	})

	t.Run("summary", func(t *testing.T) {
		needContractBin(t)
		stdout, stderr, code := runHarness(t, map[string]string{
			"RESULT_PATH": contractFixture(t, "codeaf-result.json"),
		}, "summary")
		if code != 0 {
			t.Fatalf("summary: exit %d, stderr: %s", code, stderr)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(stdout), &fields); err != nil {
			t.Fatalf("summary stdout is not JSON: %v: %s", err, stdout)
		}
		if _, ok := fields["answer"]; ok {
			t.Error("summary printed the answer; the workflow post must stay short")
		}
		if got := string(fields["stop"]); got != `"done"` {
			t.Errorf("summary stop = %s, want \"done\"", got)
		}
	})

	t.Run("state", func(t *testing.T) {
		needContractBin(t)
		// A bundle as the codeaf-state workflow keeps it: manifest.json and
		// the router ledger, here with three generations and $1.25 of
		// spend today.
		bundle := t.TempDir()
		// A bundle the workflow merged moments ago: the spend days are
		// checked against the manifest's own updated timestamp, so the
		// fixture must be a recent write, not a date from last month.
		written := time.Now().UTC().Format(time.RFC3339)
		today := time.Now().UTC().Format(time.DateOnly)
		writeContractFile(t, bundle, "manifest.json", fmt.Sprintf(
			`{"version":1,"generation":3,"merged":["75-1"],"spend":{%q:1.25},"updated":%q}`, today, written))
		writeContractFile(t, bundle, "router-ledger.json",
			`{"entries":[{"model":"zai-org/glm-5.3-flash","class":"good","rating":4.5,"count":7,"updated":"2026-10-17T00:00:00Z"}]}`)

		home := t.TempDir()
		output := filepath.Join(t.TempDir(), "output.txt")
		_, stderr, code := runHarness(t, map[string]string{"GITHUB_OUTPUT": output},
			"state", "load", bundle, home)
		if code != 0 {
			t.Fatalf("state load: exit %d, stderr: %s", code, stderr)
		}
		want := "generation=3\nday_usd=1.25\nweek_usd=1.25\n"
		if got := readContractFile(t, output); got != want {
			t.Errorf("state load output = %q, want %q", got, want)
		}
		if _, err := os.Stat(filepath.Join(home, "router-ledger.json")); err != nil {
			t.Errorf("state load did not install the router ledger into CODEAF_HOME: %v", err)
		}

		// Pack what a run left: the spend comes from the running proxy's
		// /healthz (the in-memory total), as in the run job.
		health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"run_spent_usd":0.5}`))
		}))
		defer health.Close()
		port := ""
		if u, err := url.Parse(health.URL); err == nil {
			port = u.Port()
		}
		packed := t.TempDir()
		_, stderr, code = runHarness(t, map[string]string{
			"GITHUB_RUN_ID":   "77",
			"PROXY_PORT":      port,
			"BASE_GENERATION": "3",
		}, "state", "pack", home, packed)
		if code != 0 {
			t.Fatalf("state pack: exit %d, stderr: %s", code, stderr)
		}
		var meta RunMeta
		if err := json.Unmarshal([]byte(readContractFile(t, filepath.Join(packed, "meta.json"))), &meta); err != nil {
			t.Fatalf("state pack meta.json: %v", err)
		}
		if meta.RunID != "77" || meta.RunUSD != 0.5 {
			t.Errorf("state pack meta = %+v, want run 77 at $0.5", meta)
		}

		// Merge the packed run into the bundle: spend is added, the
		// generation moves, the run is marked merged.
		merged := t.TempDir()
		output = filepath.Join(t.TempDir(), "output.txt")
		_, stderr, code = runHarness(t, map[string]string{"GITHUB_OUTPUT": output},
			"state", "merge", bundle, merged, packed)
		if code != 0 {
			t.Fatalf("state merge: exit %d, stderr: %s", code, stderr)
		}
		want = "changed=true\ngeneration=4\n"
		if got := readContractFile(t, output); got != want {
			t.Errorf("state merge output = %q, want %q", got, want)
		}
		var m Manifest
		if err := json.Unmarshal([]byte(readContractFile(t, filepath.Join(merged, "manifest.json"))), &m); err != nil {
			t.Fatalf("merged manifest: %v", err)
		}
		if m.Generation != 4 || len(m.Merged) != 2 || m.Merged[1] != "77-1" {
			t.Errorf("merged manifest = %+v, want generation 4 with 77-1 merged", m)
		}
		if got := m.Spend[today]; got != 1.75 {
			t.Errorf("merged spend = %v, want 1.25 + 0.5", got)
		}
	})

	t.Run("merge-default", func(t *testing.T) {
		needContractBin(t)
		// Not a pr run: the command is a no-op.
		stdout, _, code := runHarness(t, nil, "merge-default", "run")
		if code != 0 || stdout != "" {
			t.Errorf("merge-default without MODE=pr: exit %d stdout %q, want a silent 0", code, stdout)
		}
		// In pr mode, wrong usage exits non-zero with the usage line.
		_, stderr, code := runHarness(t, map[string]string{"MODE": "pr"}, "merge-default", "run")
		if code == 0 {
			t.Error("merge-default run without its files: exit 0, want non-zero")
		}
		if want := "usage: harness merge-default run BASE_COMMIT_FILE MERGE_INFO_FILE"; !strings.Contains(stderr, want) {
			t.Errorf("merge-default stderr = %q, want the usage line %q", stderr, want)
		}
	})
}
