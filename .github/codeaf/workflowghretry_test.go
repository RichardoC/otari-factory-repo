package main

// TestWorkflowGhRetryWrapsFailSoftCalls checks a whole class of bug that no
// test caught before (issue #429): a fail-soft GitHub call in the publish or
// queue path that runs as raw `gh`, so one transient GitHub 5xx silences the
// review run, the continuation, the fix run or CI for that PR with nothing
// but a ::warning in the log. The repository already owns the retrier for
// this class of failure -- `harness gh-retry` (issue #146), which retries a
// 5xx or a network error and never a 4xx -- and the pull-request-create call
// is wrapped in it; every other fail-soft label edit and every CI/queue/run
// dispatch in codeaf.yml and codeaf-queue.yml must be wrapped the same way,
// with the existing ::warning kept as the after-retries fallback.
//
// The workflow YAML is read as text, not through a YAML library, in the same
// style as TestWorkflowPermissionsCoverAPIUse: the file is the artifact under
// test, and the shapes that matter (a gh call inside a `run:` block, the
// `gh-retry --` wrapper in front of it) are line-oriented. Backslash
// continuations are joined first (shellLines) and comments stripped
// (stripComment), both reused from the permissions test, so a wrapper on the
// first physical line of a continued command is seen and a comment that
// mentions a call is not scanned as one.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ghRetryCovered matches the gh invocations that must go through gh-retry in
// the two queue-setting workflows: every pr-edit label call (all of them
// label a PR with a codeaf-* label or strip one), and the dispatches that
// start CI or another CodeAF run (`ci.yml`, `codeaf-queue.yml`,
// `codeaf.yml`). Issue #444 adds the load-bearing lookups around them: the
// existing-PR lookup and the issue-title read that feed `gh pr create`, the
// two maintainer-approval api calls, and the open-issues listing that
// plan-report and plan-check dedupe against. The `codeaf-state.yml`
// dispatch in the handoff job is deliberately not in the set: it starts the
// state merge, not a run, and its job holds no harness. Also deliberately
// out: the best-effort calls that already carry their own fallback (`gh pr
// ready`, `gh label create`, the compare-status probe). Issues #462 and
// #463 add the comment posts: the review findings and continue comments
// (fail-soft, so a 5xx on them silently skipped the label edits after them)
// and the run report with its cross-reference (a report that never posts is
// one countReports cannot see, so the next run undercounts continuations).
var ghRetryCovered = regexp.MustCompile(`\bgh\s+(?:pr edit|pr comment|issue comment|pr list --head|issue view|workflow run (codeaf-queue|codeaf|ci).yml|api --paginate "repos/\$GITHUB_REPOSITORY/issues\?state=open|api "repos/\$GITHUB_REPOSITORY/(?:actions/runs/\$GITHUB_RUN_ID/approvals|collaborators/[^"]+/permission))`)

// unwrappedGhRetryCalls reports every covered gh invocation in a workflow's
// text that does not run through `gh-retry --`: the text before the
// invocation, with trailing spaces dropped, must end with the wrapper's
// `gh-retry --`.
func unwrappedGhRetryCalls(name, text string) []string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		lines = append(lines, stripComment(line))
	}
	var bad []string
	for _, line := range shellLines(strings.Join(lines, "\n")) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		for _, loc := range ghRetryCovered.FindAllStringIndex(line, -1) {
			before := strings.TrimRight(line[:loc[0]], " \t")
			wrapped := strings.HasSuffix(before, "--") &&
				strings.HasSuffix(strings.TrimRight(strings.TrimSuffix(before, "--"), " \t"), "gh-retry")
			if !wrapped {
				bad = append(bad, name+": "+line)
			}
		}
	}
	return bad
}

func TestWorkflowGhRetryWrapsFailSoftCalls(t *testing.T) {
	dir := filepath.Join("..", "workflows")
	for _, name := range []string{"codeaf.yml", "codeaf-queue.yml"} {
		name := name
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			for _, v := range unwrappedGhRetryCalls(name, string(data)) {
				t.Errorf("fail-soft gh call not wrapped in gh-retry: %s", v)
			}
		})
	}

	// The test must actually catch the class of bug, not parse its way to a
	// pass: unwrap one call of each shape in a copy of the committed text and
	// the check must fail naming it.
	t.Run("unwrapping a dispatch fails", func(t *testing.T) {
		bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"),
			`"$HARNESS" gh-retry -- gh workflow run ci.yml`, `gh workflow run ci.yml`)
		if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the CI dispatch produced no violation")
		}
		bare = unwrapOne(t, filepath.Join(dir, "codeaf-queue.yml"),
			`"$HARNESS" gh-retry -- gh workflow run codeaf.yml --ref "$DEFAULT_BRANCH" -f "review=$N"`,
			`gh workflow run codeaf.yml --ref "$DEFAULT_BRANCH" -f "review=$N"`)
		if v := unwrappedGhRetryCalls("codeaf-queue.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the queue's review dispatch produced no violation")
		}
	})
	t.Run("unwrapping a load-bearing lookup fails", func(t *testing.T) {
		for _, tc := range []struct{ name, wrapped, bare string }{
			{
				"the existing-PR lookup",
				`pr=$("$HARNESS" gh-retry -- gh pr list --head "$BRANCH" --state open --json number --jq '.[0].number')`,
				`pr=$(gh pr list --head "$BRANCH" --state open --json number --jq '.[0].number')`,
			},
			{
				"the issue-title read",
				`title=$("$HARNESS" gh-retry -- gh issue view "$NUMBER" --json title --jq .title)`,
				`title=$(gh issue view "$NUMBER" --json title --jq .title)`,
			},
			{
				"the approval check",
				`approvers=$("$HARNESS" gh-retry -- gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/approvals" \`,
				`approvers=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/approvals" \`,
			},
			{
				"the plan-report issue listing",
				`"$HARNESS" gh-retry -- gh api --paginate "repos/$GITHUB_REPOSITORY/issues?state=open&per_page=100" > "$OPEN_ISSUES_PATH"`,
				`gh api --paginate "repos/$GITHUB_REPOSITORY/issues?state=open&per_page=100" > "$OPEN_ISSUES_PATH"`,
			},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"), tc.wrapped, tc.bare)
				if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
					t.Fatalf("unwrapping %s produced no violation", tc.name)
				}
			})
		}
	})
	t.Run("unwrapping a comment post fails", func(t *testing.T) {
		for _, tc := range []struct{ name, wrapped, bare string }{
			{
				"the review findings comment",
				`"$HARNESS" gh-retry -- gh pr comment "$NUMBER" --body-file "$REVIEW_COMMENT_PATH" || true`,
				`gh pr comment "$NUMBER" --body-file "$REVIEW_COMMENT_PATH"`,
			},
			{
				"the continue comment",
				`"$HARNESS" gh-retry -- gh pr comment "$PR_NUMBER" --body-file "$CONTINUE_COMMENT_PATH" || true`,
				`gh pr comment "$PR_NUMBER" --body-file "$CONTINUE_COMMENT_PATH" || true`,
			},
			{
				"the run report",
				`"$HARNESS" gh-retry -- gh issue comment "$target" --body-file "$RUNNER_TEMP/report.md"`,
				`gh issue comment "$target" --body-file "$RUNNER_TEMP/report.md"`,
			},
			{
				"the opened-PR cross-reference",
				`"$HARNESS" gh-retry -- gh issue comment "$NUMBER" --body "CodeAF opened #$target for this issue."`,
				`gh issue comment "$NUMBER" --body "CodeAF opened #$target for this issue."`,
			},
		} {
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"), tc.wrapped, tc.bare)
				if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
					t.Fatalf("unwrapping %s produced no violation", tc.name)
				}
			})
		}
	})
	t.Run("unwrapping a label edit fails", func(t *testing.T) {
		bare := unwrapOne(t, filepath.Join(dir, "codeaf.yml"),
			`"$HARNESS" gh-retry -- gh pr edit "$PR_NUMBER" --add-label codeaf-review-needed`,
			`gh pr edit "$PR_NUMBER" --add-label codeaf-review-needed`)
		if v := unwrappedGhRetryCalls("codeaf.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the review-needed label edit produced no violation")
		}
		bare = unwrapOne(t, filepath.Join(dir, "codeaf-queue.yml"),
			`"$HARNESS" gh-retry -- gh pr edit "$N" --remove-label "$CODEAF_REVIEW_NEEDED_LABEL" || true`,
			`gh pr edit "$N" --remove-label "$CODEAF_REVIEW_NEEDED_LABEL" || true`)
		if v := unwrappedGhRetryCalls("codeaf-queue.yml", bare); len(v) == 0 {
			t.Fatal("unwrapping the queue's label removal produced no violation")
		}
	})
}

// unwrapOne returns a copy of a workflow's text with exactly one `wrapped`
// spelling replaced by its `bare` one, failing the test when the committed
// workflow no longer spells the call that way (the mutation must land, or
// the negative proof proves nothing).
func unwrapOne(t *testing.T, path, wrapped, bare string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := string(data)
	if !strings.Contains(text, wrapped) {
		t.Fatalf("%s no longer spells the call as %q; update the negative proof", path, wrapped)
	}
	return strings.Replace(text, wrapped, bare, 1)
}

// reviewPassedWithdrawalProblems reports every way a workflow's text fails
// the label contract of issue #5: `label` is the only review outcome that
// may leave codeaf-review-passed on the pull request, so the review-findings
// step must withdraw it outside the label branch (the fix branch and a
// catch-all covering every report-only outcome), and the publish step must
// withdraw it when its push moves an existing pull request's head — an
// update that never reaches a review must not leave an old pass standing.
func reviewPassedWithdrawalProblems(text string) []string {
	const withdraw = `"$HARNESS" gh-retry -- gh pr edit "$NUMBER" --remove-label "$label" || true`
	const publishWithdraw = `"$HARNESS" gh-retry -- gh pr edit "$pr" --remove-label codeaf-review-passed || true`
	var bad []string
	findings, ok := stepBetween(text, "Independent review findings", "Open the proposed issues")
	if !ok {
		return []string{"the review-findings step is missing"}
	}
	if n := strings.Count(findings, withdraw); n != 2 {
		bad = append(bad, fmt.Sprintf("the review-findings step withdraws the pass label %d times, want twice: the fix branch and a catch-all for every non-passing outcome", n))
	}
	_, arms, _ := strings.Cut(findings, "label)")
	_, catchAll, found := strings.Cut(arms, "*)")
	if !found || !strings.Contains(catchAll, withdraw) {
		bad = append(bad, "the review-findings catch-all arm does not withdraw the pass label; a report-only outcome leaves an old pass standing")
	}
	publish, ok := stepBetween(text, "Push and open/update PR", "Continue or hand over")
	if !ok {
		return append(bad, "the publish step is missing")
	}
	flips := strings.Index(publish, "gh pr ready --undo")
	got := strings.Index(publish, publishWithdraw)
	if got < 0 {
		bad = append(bad, "the publish step never withdraws the pass label when it pushes an update to a pull request")
	} else if flips >= 0 && got < flips {
		bad = append(bad, "the publish withdrawal is not in the existing-PR branch after the draft flips")
	}
	return bad
}

// stepBetween returns the workflow text between two step names, false when
// either name is missing.
func stepBetween(text, from, to string) (string, bool) {
	i := strings.Index(text, from)
	if i < 0 {
		return "", false
	}
	j := strings.Index(text[i+len(from):], to)
	if j < 0 {
		return "", false
	}
	return text[i : i+len(from)+j], true
}

// TestWorkflowWithdrawsReviewPassedLabel pins the workflow half of issue #5
// against the committed YAML, read as text like the other workflow tests,
// and proves the checks catch the class: dropping either withdrawal must
// fail them.
func TestWorkflowWithdrawsReviewPassedLabel(t *testing.T) {
	path := filepath.Join("..", "workflows", "codeaf.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := string(data)
	if bad := reviewPassedWithdrawalProblems(text); len(bad) > 0 {
		t.Fatalf("codeaf.yml does not withdraw codeaf-review-passed as issue #5 asks: %s", strings.Join(bad, "; "))
	}
	// Drop the catch-all withdrawal (the last of the two identical lines)
	// and the structural check must name it.
	catchAllLine := `"$HARNESS" gh-retry -- gh pr edit "$NUMBER" --remove-label "$label" || true` + "\n"
	i := strings.LastIndex(text, catchAllLine)
	if i < 0 {
		t.Fatal("the catch-all withdrawal line is missing verbatim; update the negative proof")
	}
	if bad := reviewPassedWithdrawalProblems(text[:i] + text[i+len(catchAllLine):]); len(bad) == 0 {
		t.Error("dropping the catch-all withdrawal produced no violation")
	}
	// Drop the publish withdrawal and the check must name it too.
	publishLine := `"$HARNESS" gh-retry -- gh pr edit "$pr" --remove-label codeaf-review-passed || true` + "\n"
	if bad := reviewPassedWithdrawalProblems(strings.Replace(text, publishLine, "", 1)); len(bad) == 0 {
		t.Error("dropping the publish withdrawal produced no violation")
	}
}
