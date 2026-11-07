// check_routines.go implements the check routines of issue #117 whose facts
// are computable without a model: quota-calibration, pr-sweep, flaky-tests
// and factory-report. Each has a pure core that takes parsed, validated
// inputs and returns harness-rendered markdown, plus a thin live wrapper
// that reads the paths and commands the routine's workflow passes through
// the environment and degrades to an error, never a panic, when they are
// missing. Strings from GitHub or from test logs are untrusted data: they
// are escaped with escapeMD and bounded before they reach a report.
//
// branch-cleanup is deliberately not here: the maintainer dropped it with
// the contents:write permission it needed (issue #117 discussion).
//
// Every check registers itself in routineChecks under the command name its
// routines.json entry carries, so TestRoutineCatalog pins the committed
// file to these implementations.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func init() {
	registerCheck("quota-calibration", checkQuotaCalibration)
	registerCheck("pr-sweep", checkPRSweep)
	registerCheck("flaky-tests", checkFlakyTests)
	registerCheck("factory-report", checkFactoryReport)
}

// ---------------------------------------------------------------- quota-calibration

// quotaSnapshot is one reading of synthetic.new's GET /v2/quotas, the call
// models.json's limits.plan_note documents having used to calibrate
// weekly_credit_usd. ts is when the reading was taken.
type quotaSnapshot struct {
	WeeklyTokenLimit struct {
		PercentRemaining float64 `json:"percentRemaining"`
	} `json:"weeklyTokenLimit"`
	TS time.Time `json:"ts"`
}

// quotasJSON is the quota file the quota-calibration routine reads: the
// snapshots, and the weekly_credit_usd figure they are compared against.
// The live wrapper takes the snapshots from CODEAF_QUOTAS_PATH and fills
// WeeklyCreditUSD from models.json; a test sets both directly.
type quotasJSON struct {
	WeeklyCreditUSD float64         `json:"weekly_credit_usd"`
	Snapshots       []quotaSnapshot `json:"snapshots"`
}

// ledgerJSON is the harness proxy's ledger (main.go's LedgerLine: model,
// usd, ts), the spend (at API list prices, like every ledger figure) the
// calibration reads. It is the
// shape state.go and merge.go already carry, not a new one.
type ledgerJSON = []LedgerLine

// minIntervalDropPct is the smallest percent-point drop between two
// snapshots from which an estimate is still derived. Below it, a reading's
// rounding error dominates the ratio and the interval says nothing.
const minIntervalDropPct = 0.1

// quotaCalibration re-estimates the account's weekly credit from the drop
// in weeklyTokenLimit.percentRemaining against the ledger spend (API list
// prices) recorded in between, so the estimate is in the same overage
// dollars as weekly_credit_usd. Each consecutive pair of snapshots (sorted by ts)
// yields spend * 100 / drop percent points; the estimate is the mean of
// those interval figures. changed is true when the estimate drifts more
// than 20% from the configured weekly_credit_usd — the threshold the issue
// sets — and the report states both numbers and the recommendation, in
// harness-rendered markdown built from validated data.
func quotaCalibration(entries ledgerJSON, quotas quotasJSON) (changed bool, report string, estimate float64) {
	// Sort copies: the inputs are data, and their order is not promised.
	snaps := make([]quotaSnapshot, len(quotas.Snapshots))
	copy(snaps, quotas.Snapshots)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].TS.Before(snaps[j].TS) })
	spend := make(ledgerJSON, len(entries))
	copy(spend, entries)
	sort.Slice(spend, func(i, j int) bool { return spend[i].Time.Before(spend[j].Time) })

	var estimates []float64
	for i := 1; i < len(snaps); i++ {
		a, b := snaps[i-1], snaps[i]
		if a.TS.IsZero() || b.TS.IsZero() || !b.TS.After(a.TS) {
			continue
		}
		drop := a.WeeklyTokenLimit.PercentRemaining - b.WeeklyTokenLimit.PercentRemaining
		if math.IsNaN(drop) || drop < minIntervalDropPct {
			continue // no usable drop: nothing was spent, or the readings are junk
		}
		between := 0.0
		for _, e := range spend {
			if e.Time.After(a.TS) && !e.Time.After(b.TS) {
				between += e.CostUSD
			}
		}
		if between <= 0 {
			continue
		}
		estimates = append(estimates, between*100/drop)
	}
	if len(estimates) == 0 {
		return false, "## Quota calibration\n\nNot enough data to estimate the weekly credit: fewer than two usable quota snapshots, or no recorded spend between them. No change proposed.\n", 0
	}
	sum := 0.0
	for _, e := range estimates {
		sum += e
	}
	estimate = sum / float64(len(estimates))

	var b strings.Builder
	b.WriteString("## Quota calibration\n\n")
	fmt.Fprintf(&b, "Configured `limits.weekly_credit_usd`: %s. ", money(quotas.WeeklyCreditUSD))
	fmt.Fprintf(&b, "Re-estimated from %d usable quota snapshot interval(s): **%s**.\n", len(estimates), money(estimate))
	if quotas.WeeklyCreditUSD <= 0 {
		b.WriteString("\n`weekly_credit_usd` is not set in models.json, so no drift can be computed; set it from the estimate above.\n")
		return false, b.String(), estimate
	}
	drift := math.Abs(estimate-quotas.WeeklyCreditUSD) / quotas.WeeklyCreditUSD
	fmt.Fprintf(&b, "\nDrift from the configured figure: **%.1f%%** (threshold 20%%).\n", drift*100)
	if drift > 0.20 {
		changed = true
		fmt.Fprintf(&b, "\n**Recommendation: update `limits.weekly_credit_usd` from %s to %s** in models.json, and note the recalibration in `limits.plan_note`.\n",
			money(quotas.WeeklyCreditUSD), money(estimate))
	} else {
		b.WriteString("\n**Recommendation: no change** — the estimate is within tolerance of the configured figure.\n")
	}
	return changed, b.String(), estimate
}

// checkQuotaCalibration is the live wrapper: it reads the proxy ledger from
// CODEAF_LEDGER_PATH and the quota snapshots from CODEAF_QUOTAS_PATH, and
// the configured weekly credit from models.json. The ledger is required; a
// missing or unreadable quota-snapshot file is not an error any more -- the
// proxy stopped fetching /v2/quotas when chat moved to hosted Otari (issue
// #1), so snapshots may simply not exist -- and the routine then reports
// that it has nothing to calibrate and exits clean.
func checkQuotaCalibration() (string, error) {
	ledgerPath := os.Getenv("CODEAF_LEDGER_PATH")
	quotasPath := os.Getenv("CODEAF_QUOTAS_PATH")
	if ledgerPath == "" {
		return "", errors.New("quota-calibration: CODEAF_LEDGER_PATH is required")
	}
	if quotasPath == "" {
		return "quota-calibration: no CODEAF_QUOTAS_PATH; the proxy no longer fetches /v2/quotas, so there are no snapshots to calibrate against", nil
	}
	if _, err := os.Stat(ledgerPath); err != nil {
		return "", fmt.Errorf("quota-calibration: %s: %w", ledgerPath, err)
	}
	if _, err := os.Stat(quotasPath); err != nil {
		return "quota-calibration: no quota snapshots (" + quotasPath + " unreadable); the proxy no longer fetches /v2/quotas, so there is nothing to calibrate against", nil
	}
	entries, err := readLedger(ledgerPath)
	if err != nil {
		return "", fmt.Errorf("quota-calibration: %w", err)
	}
	data, err := os.ReadFile(quotasPath)
	if err != nil {
		return "", fmt.Errorf("quota-calibration: %w", err)
	}
	var quotas quotasJSON
	if err := json.Unmarshal(data, &quotas.Snapshots); err != nil {
		return "", fmt.Errorf("quota-calibration: %s: %w", quotasPath, err)
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return "", fmt.Errorf("quota-calibration: %w", err)
	}
	quotas.WeeklyCreditUSD = cfg.Limits.WeeklyCreditUSD
	_, report, _ := quotaCalibration(entries, quotas)
	return report, nil
}

// ---------------------------------------------------------------- pr-sweep

// prCheck is one entry of a PR's statusCheckRollup, the union of gh's
// CheckRun (conclusion) and StatusContext (state) shapes.
type prCheck struct {
	State      string `json:"state,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
}

// prReviewAuthor is a review's author; gh sends {login: "..."}.
type prReviewAuthor struct {
	Login string `json:"login"`
}

// prReview is one review of a PR, enough to tell an unaddressed
// CHANGES_REQUESTED finding from one that was later approved or dismissed.
type prReview struct {
	Author      *prReviewAuthor `json:"author,omitempty"`
	State       string          `json:"state,omitempty"`
	SubmittedAt time.Time       `json:"submittedAt,omitempty"`
}

// prStatus is one PR as `gh pr list --json` shapes it, with the fields the
// sweep needs. Unknown fields are ignored: the input is untrusted data.
type prStatus struct {
	Number            int        `json:"number"`
	HeadRefName       string     `json:"headRefName,omitempty"`
	State             string     `json:"state,omitempty"`
	Mergeable         string     `json:"mergeable,omitempty"`
	StatusCheckRollup []prCheck  `json:"statusCheckRollup,omitempty"`
	Reviews           []prReview `json:"reviews,omitempty"`
}

// prFailingStates are the rollup values that mean a check is red, across
// both of gh's check shapes.
var prFailingStates = map[string]bool{
	"FAILURE": true, "ERROR": true, "TIMED_OUT": true,
	"ACTION_REQUIRED": true, "STARTUP_FAILURE": true,
}

// prSweep reports the open codeaf/* PRs that need attention: red (a failing
// check), conflicted with main, or carrying an unaddressed review finding —
// a CHANGES_REQUESTED review with no later approving or dismissed review.
// One line per finding, harness-rendered. conflictedFree carries the
// conflicted PR numbers for the caller to dispatch: whether the queue has a
// free slot is a workflow decision, not this routine's.
func prSweep(prs []prStatus) (report string, conflictedFree []int) {
	var red, conflicted, unaddressed []prStatus
	examined := 0
	for _, pr := range prs {
		if !strings.HasPrefix(pr.HeadRefName, "codeaf/") {
			continue // only the factory's own branches count
		}
		if pr.State != "" && pr.State != "OPEN" {
			continue // merged and closed PRs need no sweep
		}
		examined++ // healthy PRs are still part of the checked queue
		fails := 0
		for _, c := range pr.StatusCheckRollup {
			if prFailingStates[c.State] || prFailingStates[c.Conclusion] {
				fails++
			}
		}
		isConflicted := pr.Mergeable == "CONFLICTING"
		unaddr := hasUnaddressedReview(pr.Reviews)
		switch {
		case fails > 0:
			red = append(red, pr)
		case isConflicted:
			conflicted = append(conflicted, pr)
		case unaddr:
			unaddressed = append(unaddressed, pr)
		}
		if isConflicted {
			conflictedFree = append(conflictedFree, pr.Number)
		}
	}
	sort.Ints(conflictedFree)

	var b strings.Builder
	fmt.Fprintf(&b, "## PR sweep: %d open `codeaf/*` PR(s) checked\n\n", examined)
	line := func(label string, pr prStatus, detail string) {
		fmt.Fprintf(&b, "- #%d `%s` — %s", pr.Number, escapeMD(pr.HeadRefName, 80, true), label)
		if detail != "" {
			fmt.Fprintf(&b, ": %s", detail)
		}
		b.WriteString("\n")
	}
	if len(red) == 0 && len(conflicted) == 0 && len(unaddressed) == 0 {
		fmt.Fprintf(&b, "No red, conflicted or unaddressed-review PR(s) among the %d checked.\n", examined)
		return b.String(), conflictedFree
	}
	if len(red) > 0 {
		b.WriteString("\n### Red (failing checks)\n\n")
		for _, pr := range red {
			n := 0
			for _, c := range pr.StatusCheckRollup {
				if prFailingStates[c.State] || prFailingStates[c.Conclusion] {
					n++
				}
			}
			line("failing checks", pr, fmt.Sprintf("%d failing check(s)", n))
		}
	}
	if len(conflicted) > 0 {
		b.WriteString("\n### Conflicted with main\n\n")
		for _, pr := range conflicted {
			line("merge conflict", pr, "dispatch `codeaf.yml -f pr=N` when the queue has a free slot")
		}
	}
	if len(unaddressed) > 0 {
		b.WriteString("\n### Unaddressed review findings\n\n")
		for _, pr := range unaddressed {
			who, _ := unaddressedReviewer(pr.Reviews)
			if who == "" {
				// A standing finding whose author is null (a deleted
				// account): name nobody rather than drop the finding.
				line("unaddressed finding", pr, "@nobody (reviewer unknown)")
			} else {
				line("unaddressed finding", pr, "@"+escapeMD(strings.TrimPrefix(who, "@"), 80, true))
			}
		}
	}
	return b.String(), conflictedFree
}

// hasUnaddressedReview reports whether the PR carries a CHANGES_REQUESTED
// review with no later approving or dismissed review: reviews are read in
// submittedAt order when any stamp is set, otherwise in list order.
func hasUnaddressedReview(reviews []prReview) bool {
	_, standing := unaddressedReviewer(reviews)
	return standing
}

// unaddressedReviewer names the reviewer whose CHANGES_REQUESTED finding is
// still standing — the raw login, escaped only where it is rendered — or ""
// when the reviewer is unknown. The bool says whether a finding stands at
// all: a CHANGES_REQUESTED review whose author is null — what `gh pr list
// --json` sends for a deleted account — is a standing finding with an unknown
// reviewer, not the absence of one. The later-review rule is
// hasUnaddressedReview's.
func unaddressedReviewer(reviews []prReview) (who string, standing bool) {
	ordered := make([]prReview, len(reviews))
	copy(ordered, reviews)
	stamped := false
	for _, r := range ordered {
		if !r.SubmittedAt.IsZero() {
			stamped = true
			break
		}
	}
	if stamped {
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].SubmittedAt.Before(ordered[j].SubmittedAt) })
	}
	for i, r := range ordered {
		switch r.State {
		case "CHANGES_REQUESTED":
			who = ""
			standing = true
			if r.Author != nil && r.Author.Login != "" {
				who = r.Author.Login
			}
		case "APPROVED", "DISMISSED":
			// An approving or dismissed review after the finding clears it;
			// anything before it is history.
			if i > 0 {
				who = ""
				standing = false
			}
		}
	}
	return who, standing
}

// checkPRSweep is the live wrapper: it reads the PR list from PRS_PATH
// (required), a JSON array in `gh pr list --json` shape.
func checkPRSweep() (string, error) {
	path := os.Getenv("PRS_PATH")
	if path == "" {
		return "", errors.New("pr-sweep: PRS_PATH is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("pr-sweep: %w", err)
	}
	var prs []prStatus
	if err := json.Unmarshal(data, &prs); err != nil {
		return "", fmt.Errorf("pr-sweep: %s: %w", path, err)
	}
	report, _ := prSweep(prs)
	return report, nil
}

// ---------------------------------------------------------------- flaky-tests

// flakyMaxLines and flakyMaxChars bound the failure output kept per test:
// a proposal issue carries the shape of the failure, not its whole log.
const (
	flakyMaxLines = 40
	flakyMaxChars = 2000
)

// flakyTests parses `go test -race -count=10` output into per-test
// intermittent failures: a test with at least one FAIL run. failed carries
// the names, sorted; report is one proposal-ready issue body section per
// flaky test, with that test's first failure output capped. Untrusted log
// text is bounded and stripped of control characters and backticks before
// it goes into a fenced block, so no output can break the fence or inject
// markdown.
//
// The parser also reads the module notes the live wrapper writes around
// each module's output (`# module <dir>` headers, budget-skipped lines,
// run-errored and budget-killed notes). The not-tested judgement is per
// package, not per module (issue #492): a package-level `FAIL\t<pkg>` line
// with no `--- FAIL:` marker attributed to it — a build failure, a
// test-timeout panic, a TestMain exit — is reported even when a sibling
// package in the same module flaked, and a module whose run was killed by
// the budget after a marker is reported as partially tested. A module that
// was skipped, or whose run ended in an error with no marker at all, is
// reported under a "not tested this run" section, so an incomplete run can
// never read as a clean bill of health (issue #477).
func flakyTests(output []byte) (report string, failed []string) {
	lines := strings.Split(string(output), "\n")
	type flaky struct {
		fails  int
		output []string
	}
	// pkgState is what the parser saw of one package within a module.
	type pkgState struct {
		fail    bool // a package-level `FAIL\t<pkg>` line
		markers int  // `--- FAIL:` markers attributed to this package
	}
	// modState accumulates what the parser saw of one module's slice of
	// the combined output, to judge whether the module was really tested.
	// The judgement is per package (issue #492): a completed run
	// summarizes every package with `ok` or `FAIL\t<pkg>`, so a package
	// whose FAIL line carries no attributed `--- FAIL:` marker was not
	// actually tested, even when a sibling package flaked.
	type modState struct {
		markers int  // all `--- FAIL: <test>` markers in the module's slice
		pending int  // markers since the last package summary line, unattributed
		errored bool // the wrapper noted that this run ended in an error
		killed  bool // the wrapper noted the budget killed the run mid-module
		pkgs    map[string]*pkgState
	}
	found := map[string]*flaky{}
	mods := map[string]*modState{}
	var order []string
	type untested struct{ dir, reason string }
	var notRun []untested
	var partial []untested // modules killed by the budget after a marker
	cur := ""              // module dir whose output slice is open, "" when none
	capturing := ""
	// judge closes the current module's slice. A package counts as not
	// tested when its run ended in a package-level FAIL and no
	// `--- FAIL:` marker was attributed to it; a module whose run the
	// budget killed after a marker is partially tested, because the
	// packages after the kill never printed a summary line; and a module
	// whose run errored or was killed with no marker at all is not
	// tested. A module with markers and a summary for every package is
	// covered by the per-test sections whatever its exit status.
	judge := func() {
		if cur == "" {
			return
		}
		s := mods[cur]
		dir := cur
		cur = ""
		if s == nil {
			return
		}
		var untestedPkgs []string
		for pkg, p := range s.pkgs {
			if p.fail && p.markers == 0 {
				untestedPkgs = append(untestedPkgs, pkg)
			}
		}
		sort.Strings(untestedPkgs)
		for _, pkg := range untestedPkgs {
			notRun = append(notRun, untested{dir,
				"package `" + pkg + "` ended in a package-level FAIL with no `--- FAIL:` marker (a build failure, a test-timeout panic, or a TestMain exit)"})
		}
		switch {
		case s.killed && s.markers > 0:
			partial = append(partial, untested{dir, fmt.Sprintf(
				"run was killed by the %s budget mid-module after a `--- FAIL:` marker, so packages later in the module were not tested", flakyTestBudget)})
		case s.killed && len(untestedPkgs) > 0:
			partial = append(partial, untested{dir, fmt.Sprintf(
				"run was killed by the %s budget mid-module after a failed package, so packages later in the module were not tested", flakyTestBudget)})
		case len(untestedPkgs) == 0 && s.markers == 0 && s.killed:
			notRun = append(notRun, untested{dir, fmt.Sprintf(
				"run was killed by the %s budget mid-module with no per-test failure marker (the output may be truncated)", flakyTestBudget)})
		case len(untestedPkgs) == 0 && s.markers == 0 && s.errored:
			notRun = append(notRun, untested{dir,
				"run ended in an error with no per-test failure marker (the output may be truncated)"})
		}
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "--- FAIL: "):
			fields := strings.Fields(line[len("--- FAIL: "):])
			if len(fields) == 0 {
				// A bare "--- FAIL: " marker carries no test name: log
				// noise, not a failure. Skip it rather than panic or
				// invent a name (untrusted log text, fail-closed).
				continue
			}
			name := fields[0]
			f, ok := found[name]
			if !ok {
				f = &flaky{}
				found[name] = f
				order = append(order, name)
			}
			f.fails++
			if s := mods[cur]; s != nil {
				s.markers++
				s.pending++
			}
			// Keep the first failure's output only; later runs say the same.
			if len(f.output) == 0 {
				capturing = name
			} else {
				capturing = ""
			}
		case strings.HasPrefix(line, "# module "):
			// The wrapper's module notes frame each module's output.
			capturing = ""
			rest := line[len("# module "):]
			if i := strings.Index(rest, ": skipped, the "); i > 0 {
				judge()
				notRun = append(notRun, untested{rest[:i],
					"skipped: the " + flakyTestBudget.String() + " budget ran out"})
				continue
			}
			if strings.HasSuffix(rest, "budget ran out mid-module") &&
				strings.Contains(rest, ": run ended in an error: the ") {
				// The budget killed this module's run mid-module: its
				// slice is still open, and the packages after the kill
				// never printed a summary line (issue #492).
				if s := mods[cur]; s != nil {
					s.errored, s.killed = true, true
				}
				continue
			}
			if strings.HasSuffix(rest, ": run ended in an error") {
				// The errored run's module slice is still open: flag it.
				if s := mods[cur]; s != nil {
					s.errored = true
				}
				continue
			}
			judge()
			cur = rest
			mods[cur] = &modState{pkgs: map[string]*pkgState{}}
		case strings.HasPrefix(line, "FAIL\t"):
			// A package-level FAIL line: a failing package, or one that
			// never built or crashed before any test reported. The
			// `--- FAIL:` markers seen since the last package summary
			// line belong to this package; a FAIL line with none of them
			// is judged untested (issue #492).
			if s := mods[cur]; s != nil {
				fields := strings.Fields(line[len("FAIL\t"):])
				if len(fields) > 0 {
					p := s.pkgs[fields[0]]
					if p == nil {
						p = &pkgState{}
						s.pkgs[fields[0]] = p
					}
					p.fail = true
					p.markers += s.pending
				}
				s.pending = 0
			}
			capturing = ""
		case strings.HasPrefix(line, "ok\t") || strings.HasPrefix(line, "ok  \t"):
			// A passing package's summary line: it closes the marker
			// attribution window. A package that prints ok has no
			// unattributed failure, so only the window is reset.
			if s := mods[cur]; s != nil {
				s.pending = 0
			}
			capturing = ""
		case capturing == "":
			// Not inside a failure's output; nothing to do.
		case strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "=== ") ||
			strings.HasPrefix(line, "ok ") || strings.HasPrefix(line, "FAIL") ||
			strings.HasPrefix(line, "PASS") || strings.HasPrefix(line, "# "):
			capturing = "" // the failure's output ends at the next marker
		default:
			f := found[capturing]
			if len(f.output) < flakyMaxLines {
				f.output = append(f.output, line)
			}
		}
	}
	judge()
	sort.Strings(order)
	var b strings.Builder
	if len(order) == 0 {
		if len(notRun) == 0 && len(partial) == 0 {
			return "## Flaky tests\n\nNo intermittent test failures.\n", nil
		}
		// Incomplete run: never let it read as an all-clear (issues
		// #477, #492).
		b.WriteString("## Flaky tests\n\nNo test carried a `--- FAIL:` marker, but the run is incomplete — see the not-tested and partially-tested sections below.\n")
	} else {
		fmt.Fprintf(&b, "## Flaky tests: %d test(s) failed intermittently under `go test -race -count=10`\n", len(order))
		for _, name := range order {
			f := found[name]
			fmt.Fprintf(&b, "\n### %s\n\nFailed %d of its runs. First failure output:\n\n```\n",
				escapeMD(name, 120, true), f.fails)
			b.WriteString(fenceText(strings.Join(f.output, "\n")))
			b.WriteString("\n```\n\nPropose a fix or a quarantine for this test; rerun `go test -race -count=10` on its module before closing.\n")
		}
	}
	if len(notRun) > 0 {
		fmt.Fprintf(&b, "\n## Not tested this run: %d module(s) or package(s)\n\nThese modules or packages were skipped or ended without a per-test failure marker, so their tests were not checked and may hide intermittent failures:\n\n", len(notRun))
		for _, m := range notRun {
			fmt.Fprintf(&b, "- `%s` — %s\n", escapeMD(m.dir, 200, true), m.reason)
		}
		b.WriteString("\nRerun `go test -race -count=10 ./...` on these modules before trusting this report as a clean bill of health.\n")
	}
	if len(partial) > 0 {
		fmt.Fprintf(&b, "\n## Partially tested this run: %d module(s)\n\nThese module runs were killed by the budget after a per-test failure marker, so packages later in the module were not checked and may hide intermittent failures:\n\n", len(partial))
		for _, m := range partial {
			fmt.Fprintf(&b, "- `%s` — %s\n", escapeMD(m.dir, 200, true), m.reason)
		}
		b.WriteString("\nRerun `go test -race -count=10 ./...` on these modules before trusting this report as a clean bill of health.\n")
	}
	return b.String(), order
}

// fenceText makes a string safe inside a fenced code block: control
// characters other than newline are dropped and backticks are removed, so
// no line of test output can close the fence or inject markdown.
func fenceText(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= flakyMaxChars {
			b.WriteString("\n… (output capped)\n")
			break
		}
		switch {
		case r == '\n':
			b.WriteByte('\n')
			n++
		case r == '`':
			// dropped: a run of three would close the fence
		case r < 0x20 || r == 0x7f:
			// dropped
		default:
			b.WriteRune(r)
			n++
		}
	}
	return b.String()
}

// flakyTestBudget bounds the whole routine: 20 minutes across every module,
// after which the remaining modules are skipped and reported.
const flakyTestBudget = 20 * time.Minute

// checkFlakyTests is the live wrapper: it runs `go test -race -count=10
// ./...` for every module — each directory with a go.mod under app/, plus
// the harness module itself — within a 20-minute budget, and feeds the
// combined output to the core. A module whose tests all pass contributes
// nothing. The repo root comes from CODEAF_REPO_ROOT (default: the working
// directory); with no modules found the routine errors rather than reports
// a false all-clear.
func checkFlakyTests() (string, error) {
	root := envOr("CODEAF_REPO_ROOT", ".")
	var modules []string
	for _, pattern := range []string{filepath.Join(root, "app", "*", "go.mod"), filepath.Join(root, ".github", "codeaf", "go.mod")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return "", fmt.Errorf("flaky-tests: %w", err)
		}
		sort.Strings(matches)
		modules = append(modules, matches...)
	}
	if len(modules) == 0 {
		return "", fmt.Errorf("flaky-tests: no Go modules under %s (set CODEAF_REPO_ROOT to the checkout)", root)
	}
	deadline := time.Now().Add(flakyTestBudget)
	var out strings.Builder
	for _, mod := range modules {
		dir := filepath.Dir(mod)
		remaining := time.Until(deadline)
		if remaining <= 0 {
			fmt.Fprintf(&out, "# module %s: skipped, the %s budget ran out\n", dir, flakyTestBudget)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		cmd := exec.CommandContext(ctx, "go", "test", "-race", "-count=10", "./...")
		cmd.Dir = dir
		b, err := cmd.CombinedOutput()
		// Read the budget verdict before cancel(): after cancel ctx.Err()
		// is always non-nil, which would mark every module as killed.
		killed := err != nil && ctx.Err() != nil
		cancel()
		fmt.Fprintf(&out, "# module %s\n", dir)
		out.Write(b)
		out.WriteString("\n")
		if killed {
			// The budget killed this module's run mid-module: the
			// packages after the kill never print a summary line, so
			// flakyTests cannot name them — note the kill and let it
			// report the module as partially (or not) tested
			// (issue #492).
			fmt.Fprintf(&out, "# module %s: run ended in an error: the %s budget ran out mid-module\n", dir, flakyTestBudget)
		} else if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				fmt.Fprintf(&out, "# (%s: %v)\n", dir, err)
			}
			// flakyTests judges packages, so a run that died another way —
			// a build failure, a test-timeout panic, a TestMain exit — is
			// named by its package-level FAIL line. The module note keeps
			// the fail-closed case: an error with no marker at all
			// (issue #477).
			fmt.Fprintf(&out, "# module %s: run ended in an error\n", dir)
		}
	}
	report, _ := flakyTests([]byte(out.String()))
	return report, nil
}

// ---------------------------------------------------------------- factory-report

// runRecordSummary is factory-report's projection of a RunRecord
// (runrecord.go), carrying the fields the weekly report needs. model,
// queued_at and merged_at are optional extras: the records file carries
// none today, so spend by model falls back to an unattributed row and the
// queued-to-merged time to "n/a" until a record carries the stamps.
type runRecordSummary struct {
	Mode          string  `json:"mode"`
	Stop          string  `json:"stop"`
	StoreFinished bool    `json:"store_finished"`
	Number        int     `json:"number"`
	CostUSD       float64 `json:"cost_usd"`
	WallSeconds   float64 `json:"wall_seconds"`
	Date          string  `json:"date"`
	Model         string  `json:"model,omitempty"`
	QueuedAt      string  `json:"queued_at,omitempty"` // RFC3339; when the run was queued
	MergedAt      string  `json:"merged_at,omitempty"` // RFC3339; when its PR merged
}

// factoryReport renders the weekly report: runs started and finished, done
// vs incomplete stops, PRs merged, spend by model, and the mean time from
// queued to merged. PRs merged counts the distinct pr-mode numbers whose
// run stopped done — the run record knows the run finished, not the merge
// itself, and the report says so. Everything is harness-rendered from the
// validated records; no model text.
func factoryReport(records []runRecordSummary) string {
	done, incomplete, finished := 0, 0, 0
	prMerged := map[int]bool{}
	spendByModel := map[string]float64{}
	var queuedToMerged []float64
	for _, r := range records {
		if r.StoreFinished {
			finished++
		}
		switch r.Stop {
		case stopDone:
			done++
			if r.Mode == "pr" && r.Number > 0 {
				prMerged[r.Number] = true
			}
		case stopIncomplete:
			incomplete++
		}
		model := r.Model
		if model == "" {
			model = "(unattributed)"
		}
		spendByModel[model] += r.CostUSD
		if r.QueuedAt != "" && r.MergedAt != "" {
			q, err1 := time.Parse(time.RFC3339, r.QueuedAt)
			m, err2 := time.Parse(time.RFC3339, r.MergedAt)
			if err1 == nil && err2 == nil && m.After(q) {
				queuedToMerged = append(queuedToMerged, m.Sub(q).Seconds())
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Factory report: %d runs started, %d finished in the store\n\n", len(records), finished)
	b.WriteString("| done | incomplete |\n|--:|--:|\n")
	fmt.Fprintf(&b, "| %d | %d |\n", done, incomplete)
	fmt.Fprintf(&b, "\nPRs merged: %d (distinct `pr`-mode runs with a done stop; the record knows the run finished, not the merge itself)\n", len(prMerged))

	b.WriteString("\nSpend by model:\n\n| model | spend |\n|--:|--:|\n")
	models := make([]string, 0, len(spendByModel))
	for m := range spendByModel {
		models = append(models, m)
	}
	sort.Strings(models)
	total := 0.0
	for _, m := range models {
		fmt.Fprintf(&b, "| %s | %s |\n", escapeMD(m, 80, true), money(spendByModel[m]))
		total += spendByModel[m]
	}
	fmt.Fprintf(&b, "| total | %s |\n", money(total))

	if len(queuedToMerged) == 0 {
		b.WriteString("\nMean time queued→merged: n/a (no records carry both stamps)\n")
	} else {
		sum := 0.0
		for _, s := range queuedToMerged {
			sum += s
		}
		fmt.Fprintf(&b, "\nMean time queued→merged: %s over %d run(s)\n", fmtDuration(secondsDuration(sum/float64(len(queuedToMerged)))), len(queuedToMerged))
	}
	return b.String()
}

// checkFactoryReport is the live wrapper: it reads the run records from
// RECORDS_PATH (required), a JSONL file of RunRecord-shaped lines as the
// state bundle's records.jsonl carries them. A line that does not validate
// is dropped and counted, never fatal.
func checkFactoryReport() (string, error) {
	path := os.Getenv("RECORDS_PATH")
	if path == "" {
		return "", errors.New("factory-report: RECORDS_PATH is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("factory-report: %w", err)
	}
	var records []runRecordSummary
	dropped := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r RunRecord
		var extra struct {
			Model    string `json:"model"`
			QueuedAt string `json:"queued_at"`
			MergedAt string `json:"merged_at"`
		}
		if json.Unmarshal([]byte(line), &r) != nil || r.validate() != nil ||
			json.Unmarshal([]byte(line), &extra) != nil {
			dropped++
			continue
		}
		records = append(records, runRecordSummary{
			Mode: r.Mode, Stop: r.Stop, StoreFinished: r.StoreFinished,
			Number: r.Number, CostUSD: r.CostUSD, WallSeconds: r.WallSeconds,
			Date: r.Date, Model: extra.Model, QueuedAt: extra.QueuedAt, MergedAt: extra.MergedAt,
		})
	}
	if dropped > 0 {
		// Reported, not fatal: the file is data from run jobs, and one
		// hostile line must not sink the whole report.
		fmt.Printf("# factory-report: dropped %d invalid record line(s) from %s\n", dropped, path)
	}
	return factoryReport(records), nil
}
