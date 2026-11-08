// merge.go brings a pull request's base branch into its head branch (issue
// #57). A PR that conflicts with its base stays "dirty" forever: the run job
// hands publish a patch against the PR head, publish applies it and pushes a
// plain commit, and nothing ever produces a merge commit, so the branch never
// contains its base (#53 stayed stuck against #52 until a person merged by
// hand in #56).
//
// The flow, in pr mode:
//
//	run      (no write token; nothing it makes is pushed) checks out the PR
//	         head, then merges the base tip in BEFORE CodeAF works: a clean
//	         merge is committed as an ordinary merge commit, a conflicted one
//	         as a merge commit whose files carry conflict markers, and the
//	         brief names those files for CodeAF to resolve. The run's base
//	         commit becomes that merge commit, and a merge-info.json artifact
//	         records what was merged.
//	publish  (fresh runner, harness built from the default branch) recomputes
//	         the merge from the same two parents -- the PR head it fetched and
//	         the base tip named in merge-info.json -- and refuses unless the
//	         tree hash it computed itself matches the recorded one. The run's
//	         merge commit exists only in the run job's checkout, so it cannot
//	         be fetched; like every run artifact it is data, and it is
//	         verified by recomputation (AGENTS.md, "Security rules"). The
//	         patch is then applied -- it removes the markers -- and publish
//	         refuses while any conflict marker remains. When the run left no
//	         merge info (it died before merging, or a run the gate kept from
//	         merging), publish still lands the base as a merge commit with
//	         both parents first -- never a fast-forward, and never inside the
//	         work commit -- and refuses a patch that would re-publish the
//	         base's own changes, the squash run 39094290325 published for
//	         #163 (issue #171).
//
// A merge that carries gated .github/ changes -- any path under .github/
// outside .github/codeaf/ (issue #20; before it, .github/workflows/ only,
// issue #16) -- is not skipped: the run commits and records it
// (merge-info.json "workflows": true), the workflow reads that field through
// "harness merge-default workflows" and routes publish into the
// approval-gated codeaf-harness environment, and publish recreates it only
// once a maintainer approved. "Carries" is measured relative to the PR head:
// the gated paths the merge commit changes against the head it started from,
// each of which must equal the merged-in base tip's content and mode. The
// head's own gated edits are kept; a gated file both the branch and the base
// changed is not merged at all -- the merged content would equal neither
// side's, so publish would refuse it even approved -- and the patch is
// pushed alone instead. Nor is a merge made that would carry gated content
// the base tip does not have -- a rename the merge follows into .github/,
// say (issue #20): publish would refuse it even approved, on every run, so
// the run skips it with a reason in the brief, and the publish fallbacks
// push the patch alone instead of failing.
//
// The invariants hold: `git apply` is still the only thing applied from run;
// the .github/ approval check still reads the staged patch diff; and every
// pushed commit descends from the PR head, so there is no force-push.
//
// The merge itself uses `git merge-tree --write-tree --merge-base` (git 2.40,
// shipped on current runners), which computes the merged tree without touching
// the index or working tree, and `git commit-tree`. No history is walked, so
// the workflow's shallow checkouts work: the merge base comes from the compare
// API, and the three commits involved are fetched one level deep.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The two kinds a merge can have, as recorded in merge-info.json.
const (
	mergeKindClean      = "clean"
	mergeKindConflicted = "conflicted"
)

// mergeInfo is what the run job records about the base-branch merge. Every
// field is bounded and checked before use: it comes from the run job, whose
// outputs are untrusted data (AGENTS.md, "Security rules").
type mergeInfo struct {
	Kind   string   `json:"kind"`   // mergeKindClean or mergeKindConflicted
	Commit string   `json:"commit"` // the merge commit the run made (local to the run job)
	Base   string   `json:"base"`   // the PR head the merge started from
	Onto   string   `json:"onto"`   // the base branch tip merged in
	Tree   string   `json:"tree"`   // the merged tree (carrying markers when conflicted)
	Files  []string `json:"files"`  // the paths the merge conflicted in
	// Workflows records that the merge changes a gated .github/ path
	// relative to the PR head (issue #16; widened to every path under
	// .github/ outside .github/codeaf/ by issue #20) -- the base branch's
	// gated changes since the branch point, plus any path a rename the
	// merge follows lands in .github/. The field name predates issue #20.
	// Such a merge can only be pushed with the GitHub App token, so the
	// workflow reads this field (through "harness merge-default workflows",
	// never parsed in shell) to route publish into the codeaf-harness
	// environment; recreateMerge still checks the approval itself, against
	// its own recomputation, never this field.
	Workflows bool `json:"workflows"`
}

// mergeOutcome is what a merge phase did, for the workflow steps to read.
// State "none" means no merge was made and the patch alone is pushed.
type mergeOutcome struct {
	State  string   // "clean", "conflicted" or "none"
	Commit string   // the merge commit made here, when one was
	Tree   string   // its tree
	Files  []string // the paths a conflicted merge conflicts in
}

// mergeCommitIdent names the author of the merge commits, matching the
// identity publish configures for its own commits.
var mergeCommitIdent = []string{
	"GIT_AUTHOR_NAME=codeaf[bot]",
	"GIT_AUTHOR_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
	"GIT_COMMITTER_NAME=codeaf[bot]",
	"GIT_COMMITTER_EMAIL=41898282+github-actions[bot]@users.noreply.github.com",
}

// isSHA reports whether s is a full lowercase hex commit id.
func isSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// gitOutput runs git in dir and returns its stdout. A nonzero exit is an
// error carrying the first of stderr, with the exit status preserved for
// callers that expect one (git merge-tree exits 1 on a conflict).
func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if len(stderr) > 300 {
			stderr = stderr[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// gitOutputAs runs git with the merge committer's identity, for commands that
// author commits.
func gitOutputAs(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), mergeCommitIdent...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if len(stderr) > 300 {
			stderr = stderr[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// gitRun runs git in dir, discarding stdout.
func gitRun(dir string, args ...string) error {
	_, err := gitOutput(dir, args...)
	return err
}

// revParse resolves one ref to a full commit id.
func revParse(dir, ref string) (string, error) {
	out, err := gitOutput(dir, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !isSHA(sha) {
		return "", fmt.Errorf("git rev-parse %s printed %q, not a commit id", ref, sha)
	}
	return sha, nil
}

// fetchRef fetches one branch tip from origin, one level deep.
func fetchRef(dir, ref string) error {
	_, err := gitOutput(dir, "fetch", "-q", "--depth=1", "origin", "refs/heads/"+ref)
	return err
}

// haveCommit reports whether the commit object is already in the repository.
func haveCommit(dir, sha string) bool {
	_, err := gitOutput(dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// fetchCommit fetches one commit by id, one level deep (its tree and blobs
// come with it). Both ids merge-default fetches this way -- a merge base and
// the base tip a recorded merge named -- are reachable from a branch, which
// is what GitHub allows fetching by id.
func fetchCommit(dir, sha string) error {
	if haveCommit(dir, sha) {
		return nil
	}
	_, err := gitOutput(dir, "fetch", "-q", "--depth=1", "origin", sha)
	return err
}

// compareInfo is what the compare API says about two commits: their status
// (ahead, behind, diverged or identical, describing head against base) and
// their merge base.
type compareInfo struct {
	Status    string
	MergeBase string // empty when the API named none
}

// compareFunc asks the repository host about two commits. The production
// implementation is ghCompare; tests stub it, the way Decide takes lookups.
type compareFunc func(base, head string) (compareInfo, error)

// ghCompare reads the compare API for two commit ids.
func ghCompare(base, head string) (compareInfo, error) {
	if !isSHA(base) || !isSHA(head) {
		return compareInfo{}, errors.New("the compare API needs two 40-hex commit ids")
	}
	var out struct {
		Status          string `json:"status"`
		MergeBaseCommit *struct {
			Sha string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := newGitHub().get(fmt.Sprintf("/compare/%s...%s", base, head), &out); err != nil {
		return compareInfo{}, err
	}
	info := compareInfo{Status: out.Status}
	if out.MergeBaseCommit != nil {
		info.MergeBase = out.MergeBaseCommit.Sha
	}
	return info, nil
}

// mergeTree computes the merge of ours and theirs over the explicit merge
// base, writing the merged tree into the object database without touching the
// index or the working tree. git exits 1 when the merge conflicts; the merged
// files then carry conflict markers, and the conflicted paths are listed
// after the tree id, up to the blank line before the informational messages.
func mergeTree(dir, base, ours, theirs string) (tree string, files []string, conflicted bool, err error) {
	out, err := gitOutput(dir, "merge-tree", "--write-tree", "--name-only", "--merge-base="+base, ours, theirs)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code != 1 {
			return "", nil, false, err
		}
		conflicted = true
	} else if err != nil {
		return "", nil, false, err
	}
	lines := strings.Split(out, "\n")
	if len(lines) == 0 || !isSHA(strings.TrimSpace(lines[0])) {
		return "", nil, false, fmt.Errorf("git merge-tree printed no tree id: %.200s", out)
	}
	tree = strings.TrimSpace(lines[0])
	if conflicted {
		for _, line := range lines[1:] {
			if line == "" {
				break // a blank line ends the conflicted paths; messages follow
			}
			files = append(files, unquoteGitPath(line))
		}
	}
	return tree, files, conflicted, nil
}

// commitMerge makes the merge commit for a computed tree with the two parents.
func commitMerge(dir, tree, ours, theirs, message string) (string, error) {
	out, err := gitOutputAs(dir, "commit-tree", tree, "-p", ours, "-p", theirs, "-m", message)
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if !isSHA(sha) {
		return "", fmt.Errorf("git commit-tree printed %q, not a commit id", sha)
	}
	return sha, nil
}

// usableMergePath bounds and checks one conflicted path. The paths end up as
// file reads in publish and as brief text, never inside a shell command.
func usableMergePath(path string) bool {
	if path == "" || len(path) > 512 {
		return false
	}
	// Not one control byte of any kind (issue #34 security review): unquoting
	// can bare a raw CR or TAB in a conflicted path, and a lone CR breaks a
	// log line -- the runner treats it as a line break, so the rest of the
	// name would run as a workflow command. \n and \x00 are covered too; the
	// #22 rule is that no raw control character reaches a log line.
	for i := 0; i < len(path); i++ {
		if path[i] < 0x20 || path[i] == 0x7f {
			return false
		}
	}
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "-") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// MaxMergeConflictPaths is the most conflicted paths a merge info may
// name. The writer (runMerge) and the reader (sanitizeMergeFiles) share
// it, so a merge that conflicts more widely degrades to no merge on the
// run side instead of producing a merge-info.json the reader would
// refuse on every rerun (issue #21).
const MaxMergeConflictPaths = 200

// sanitizeMergeFiles bounds the conflicted path list itself.
func sanitizeMergeFiles(files []string) ([]string, error) {
	if len(files) > MaxMergeConflictPaths {
		return nil, fmt.Errorf("merge info names %d conflicted paths, over the %d allowed", len(files), MaxMergeConflictPaths)
	}
	out := make([]string, 0, len(files))
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if !usableMergePath(f) {
			return nil, fmt.Errorf("merge info names an unusable conflicted path %q", f)
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out, nil
}

// readMergeInfo loads and validates the run job's merge-info.json. Every
// field is bounded: this file is run-job output, and untrusted (AGENTS.md).
func readMergeInfo(path string) (*mergeInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<16 {
		return nil, errors.New("merge-info.json is over 64 KB")
	}
	var info mergeInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("merge-info.json: %w", err)
	}
	switch info.Kind {
	case mergeKindClean, mergeKindConflicted:
	default:
		return nil, fmt.Errorf("merge-info.json has unknown kind %q", info.Kind)
	}
	for name, sha := range map[string]string{
		"commit": info.Commit, "base": info.Base, "onto": info.Onto, "tree": info.Tree,
	} {
		if !isSHA(sha) {
			return nil, fmt.Errorf("merge-info.json %s is %q, not a 40-hex commit id", name, sha)
		}
	}
	if info.Kind == mergeKindConflicted {
		files, err := sanitizeMergeFiles(info.Files)
		if err != nil {
			return nil, err
		}
		info.Files = files
	} else {
		info.Files = nil
	}
	return &info, nil
}

// diffNameOnly lists the paths one git diff names, one per line, under the
// given pathspec. A name may contain spaces (#343), so splitting on
// whitespace would turn one file into two paths that do not exist; git
// C-quotes a non-ASCII name when core.quotePath is on (the default), so
// those lines are unquoted.
func diffNameOnly(dir, from, to string, pathspec ...string) ([]string, error) {
	args := append([]string{"diff", "--no-renames", "--name-only", from, to, "--"}, pathspec...)
	out, err := gitOutput(dir, args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		files = append(files, unquoteGitPath(line))
	}
	return files, nil
}

// gatedGitHubChanges lists every path under .github/ outside
// .github/codeaf/ that differs between from and to (issue #20): the paths a
// base merge may not carry without a maintainer's approval -- workflows,
// CODEOWNERS, .github/actions/, dependabot.yml and the rest. The patch-level
// gate already holds a PR's own .github/ edits to this rule (AGENTS.md); a
// base merge carried the base branch's copies of them unchecked, which was
// fine while the base was the default branch and a gap once a PR could
// target a non-default one, so the merge side now applies the same per-path
// rule. .github/codeaf/ stays outside the gate, like the patch-level one:
// the harness is built only from the default branch, so its copy on a PR
// branch is inert, it publishes like app code, and a person reviews every
// PR before it merges.
func gatedGitHubChanges(dir, from, to string) ([]string, error) {
	return diffNameOnly(dir, from, to, ".github/", ":(exclude).github/codeaf")
}

// unquoteGitPath unquotes one path from git's --name-only output, which names
// a non-ASCII or otherwise special path as a C string when core.quotePath is
// on. A line git did not quote is returned unchanged; one whose quoting
// strconv cannot parse has its quote marks trimmed rather than trusted.
func unquoteGitPath(line string) string {
	if !strings.HasPrefix(line, `"`) || !strings.HasSuffix(line, `"`) {
		return line
	}
	if u, err := strconv.Unquote(line); err == nil {
		return u
	}
	return strings.Trim(line, `"`)
}

// runStateFile is the sibling of merge-info.json where runMerge records why
// it merged nothing, for the brief (issue #16): without merge info the brief
// would otherwise say nothing, and the agent might merge or copy the base
// branch by hand, which publish refuses as an unverifiable replay.
func runStateFile(mergeInfoFile string) string {
	return mergeInfoFile + ".run"
}

// maxNoMergeReasonBytes caps a recorded or rendered no-merge reason, and the
// skip-warning message built from the same kind of error text. A reason can
// carry non-ASCII paths from git or API error text (issue #22).
const maxNoMergeReasonBytes = 300

// capUTF8 caps s at max bytes without splitting a UTF-8 rune: it walks back
// from the cut until text[cut] starts a rune. It is the rune-safe form of a
// plain s[:max] (issue #22), like capAppFile (apprules.go).
func capUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// stripControl removes control characters, including newlines, from untrusted
// text -- bytes below 32 and 127 -- so it cannot break a log line or forge a
// workflow command (issue #22).
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
}

// mergeNotMade records that the run job merged nothing, and why. The reason
// is error text, so it is bounded and stripped of control characters: this
// file is run-job output, untrusted like the rest (AGENTS.md, "Security
// rules"), and the brief puts it in front of the model. An empty reason (the
// base tip was already contained in the branch) writes nothing: there is
// nothing to tell the agent.
func mergeNotMade(mergeInfoFile, format string, args ...any) {
	if mergeInfoFile == "" {
		return
	}
	reason := stripControl(strings.TrimSpace(fmt.Sprintf(format, args...)))
	if reason == "" {
		return
	}
	reason = capUTF8(reason, maxNoMergeReasonBytes)
	if err := os.WriteFile(runStateFile(mergeInfoFile), []byte("state=none\nreason="+reason+"\n"), 0o644); err != nil {
		log.Printf("merge-default: could not record why no merge was made: %v", err)
	}
}

// runMerge is the run-job half of issue #57: it merges the base branch tip
// into the PR head branch in the run job's checkout, before CodeAF works. A
// clean merge is committed as an ordinary merge commit; a conflicted one as a
// merge commit whose files carry conflict markers, which the brief then names
// for CodeAF to resolve. The base-commit file is rewritten to the merge
// commit -- that is the commit the patch is diffed against -- and
// merge-info.json records the merge for publish to recreate and verify.
// Nothing here is ever pushed: the run job holds no write token.
//
// Every condition it cannot establish (a fetch or API failure, an unusable
// merge base) degrades to "no merge": the run proceeds on the bare PR head
// and publish falls back to merging the base tip itself. Only a failure
// recording a merge that was already made is an error, because the checkout
// and the artifacts would then disagree.
func runMerge(dir, branch, baseRef, baseCommitFile, mergeInfoFile string, compare compareFunc) (mergeOutcome, error) {
	out := mergeOutcome{State: "none"}
	if branch == "" || baseRef == "" {
		mergeNotMade(mergeInfoFile, "no branch or base branch was named")
		log.Printf("merge-default: no branch or base branch named; nothing to merge")
		return out, nil
	}
	head, err := revParse(dir, "HEAD")
	if err != nil {
		return out, err
	}
	// Only the branch the gate named is merged into: the checkout step put
	// this checkout on it, and anything else is a state worth stopping for.
	ref, err := gitOutput(dir, "symbolic-ref", "--short", "HEAD")
	if err != nil || strings.TrimSpace(ref) != branch {
		mergeNotMade(mergeInfoFile, "the checkout is not on the branch %q (it is on %q), so the base was not merged in", branch, strings.TrimSpace(ref))
		log.Printf("merge-default: HEAD is on %s, not %s; nothing to merge", strings.TrimSpace(ref), branch)
		return out, nil
	}
	if err := fetchRef(dir, baseRef); err != nil {
		mergeNotMade(mergeInfoFile, "the base branch %s could not be fetched: %v", baseRef, err)
		log.Printf("merge-default: could not fetch %s (%v); continuing without bringing it in", baseRef, err)
		return out, nil
	}
	onto, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if head == onto {
		return out, nil // already the base tip: nothing to bring in
	}
	cmp, err := compare(head, onto)
	if err != nil {
		mergeNotMade(mergeInfoFile, "the branch could not be compared with %s: %v", baseRef, err)
		log.Printf("merge-default: could not compare the branch with %s (%v); continuing without a merge", baseRef, err)
		return out, nil
	}
	// The branch point the merge is computed over: for a diverged branch the
	// compare API's merge base; for a strictly-behind branch (issue #171) the
	// head itself, which is its own branch point.
	base := ""
	switch {
	case cmp.Status == "behind" || cmp.Status == "identical":
		return out, nil // the base tip is already contained in the branch
	case cmp.Status == "ahead":
		// The branch is strictly behind its base: still a merge, never a
		// fast-forward -- a single-parent commit would leave the base's
		// changes indistinguishable from the branch's own work (issue #171).
		base = head
	case cmp.Status == "diverged":
		if cmp.MergeBase == "" || !isSHA(cmp.MergeBase) {
			mergeNotMade(mergeInfoFile, "the compare API named no merge base for %s and %s", shortSHA(head), shortSHA(onto))
			log.Printf("merge-default: the compare API named no merge base; continuing without a merge")
			return out, nil
		}
		base = cmp.MergeBase
	default:
		mergeNotMade(mergeInfoFile, "the compare API returned an unexpected status %q", cmp.Status)
		log.Printf("merge-default: unexpected compare status %q; continuing without a merge", cmp.Status)
		return out, nil
	}
	if err := fetchCommit(dir, base); err != nil {
		mergeNotMade(mergeInfoFile, "the merge base %s could not be fetched: %v", shortSHA(base), err)
		log.Printf("merge-default: could not fetch the merge base %s (%v); continuing without a merge", shortSHA(base), err)
		return out, nil
	}
	// Issue #16, widened to every gated .github/ path by issue #20: a
	// gated file the base changed and this branch also edits is not
	// merged -- unless the branch already made the identical change
	// (overlappingGatedPaths leaves that out). The merged content would
	// equal neither the head's nor the base tip's, so publish would refuse
	// the merge even with a maintainer's approval, and every later run on
	// this PR would lose its work. The run proceeds on the bare head
	// instead, the brief names the files, and publish pushes the patch
	// alone until a person merges the branch by hand.
	if overlap, err := overlappingGatedPaths(dir, base, onto, head); err != nil {
		log.Printf("merge-default: could not tell whether the branch and %s edited the same gated .github/ files (%v); the overlap check is skipped", baseRef, err)
	} else if len(overlap) > 0 {
		mergeNotMade(mergeInfoFile, "the base branch changed .github/ file(s) %s that this branch also edits; the merged content would equal neither side's, so a person must merge it by hand", strings.Join(overlap, ", "))
		log.Printf("merge-default: %s changed .github/ file(s) %s that %s also edits; continuing without a merge (a person must merge it by hand)", baseRef, strings.Join(overlap, ", "), branch)
		return out, nil
	}
	// Issue #16: the base having changed .github/workflows/ no longer skips
	// the merge. Such a merge can only be pushed with the GitHub App token,
	// so it is recorded as merge-info.json "workflows": true and the
	// workflow routes publish into the approval-gated codeaf-harness
	// environment; publish re-checks the approval itself and verifies that
	// the merge brings in only what the base branch already has. An error
	// telling the cases apart counts as "changed": the merge still happens,
	// and publish may wait for an approval it did not need, rather than a
	// push GitHub would refuse.
	carryGated := false
	if wf, err := gatedGitHubChanges(dir, base, onto); err != nil {
		log.Printf("merge-default: could not tell whether %s changed gated .github/ paths since the branch point (%v); recording that it may have", baseRef, err)
		carryGated = true
	} else {
		carryGated = len(wf) > 0
	}
	if carryGated {
		log.Printf("merge-default: %s changed gated .github/ paths since the branch point; the merge goes through, and publishing it waits for a maintainer approval", baseRef)
	}
	tree, files, conflicted, err := mergeTree(dir, base, head, onto)
	if err != nil {
		mergeNotMade(mergeInfoFile, "the merge could not be computed: %v", err)
		log.Printf("merge-default: could not compute the merge (%v); continuing without one", err)
		return out, nil
	}
	kind := mergeKindClean
	message := fmt.Sprintf("Merge %s into %s", baseRef, branch)
	if conflicted {
		kind = mergeKindConflicted
		message = fmt.Sprintf("Merge %s into %s (conflicts, to be resolved by CodeAF)", baseRef, branch)
	}
	// Issue #21: publish reads merge-info.json under bounds it refuses the
	// file on -- at most MaxMergeConflictPaths conflicted paths, and at
	// most 64 KB. A merge conflicting in more files than that (a wide
	// refactor, import or line-ending sweep) would produce a merge info
	// that can never validate, and publish treats a merge info that exists
	// but does not validate as fatal, so every rerun would fail the same
	// way until a person merges the branch by hand. Like everything else
	// the run cannot establish, that degrades to no merge: the brief names
	// the count, and publish pushes the patch alone.
	if conflicted && len(files) > MaxMergeConflictPaths {
		// The reason leads with the instruction: mergeNotMade caps it at
		// 300 bytes, and a long base ref could cut the tail off.
		mergeNotMade(mergeInfoFile, "a person must merge the branch by hand: the merge of %s conflicts in %d files, over the %d a merge info can name", baseRef, len(files), MaxMergeConflictPaths)
		log.Printf("merge-default: the merge of %s conflicts in %d files, over the %d allowed; continuing without a merge (a person must merge it by hand)", baseRef, len(files), MaxMergeConflictPaths)
		return out, nil
	}
	if conflicted {
		// The reader (readMergeInfo, via sanitizeMergeFiles) refuses a
		// conflicted path usableMergePath rejects -- one over 512 bytes,
		// starting with "-", or the like -- and publish treats a merge
		// info that exists but does not validate as fatal, so recording
		// such a merge would fail every rerun the same way (issue #21,
		// widened here to the per-path rule). Degrade to no merge instead.
		for _, f := range files {
			if !usableMergePath(f) {
				mergeNotMade(mergeInfoFile, "a person must merge the branch by hand: the merge of %s conflicts in a path the merge info cannot name (%q)", baseRef, f)
				log.Printf("merge-default: the merge of %s conflicts in a path the merge info cannot name (%q); continuing without a merge (a person must merge it by hand)", baseRef, f)
				return out, nil
			}
		}
	}
	if conflicted {
		// The merge info the run would record must fit what the reader
		// accepts. The commit id is a 40-hex sha of fixed length, so a
		// placeholder of that length sizes the record exactly; the same
		// MarshalIndent form is what gets written below. The recorded
		// Workflows value is settled only after checkMergeGated, so the
		// probe sizes the longer one ("false" is a byte longer than
		// "true"): a probe at exactly 64 KB must not pass and then write
		// a byte over.
		probe := mergeInfo{Kind: kind, Commit: strings.Repeat("0", 40), Base: head, Onto: onto, Tree: tree, Files: files, Workflows: false}
		if data, err := json.MarshalIndent(probe, "", "  "); err == nil && len(data)+1 > 1<<16 {
			// The reason leads with the instruction: mergeNotMade caps it
			// at 300 bytes, and a long base ref could cut the tail off.
			mergeNotMade(mergeInfoFile, "a person must merge the branch by hand: the merge info for %d conflicted files would be over the 64 KB the reader accepts", len(files))
			log.Printf("merge-default: the merge info for %s would be %d bytes, over the 64 KB allowed; continuing without a merge (a person must merge it by hand)", baseRef, len(data)+1)
			return out, nil
		}
	}
	commit, err := commitMerge(dir, tree, head, onto, message)
	if err != nil {
		mergeNotMade(mergeInfoFile, "the merge commit could not be committed: %v", err)
		log.Printf("merge-default: could not commit the merge (%v); continuing without one", err)
		return out, nil
	}
	// Issue #20: the merge may follow a rename into .github/ and land
	// gated content the base tip does not have -- publish would refuse it
	// even with a maintainer's approval, on every run (the followed-rename
	// shape of issue #16). The check runs here, before the merge is
	// recorded: when it fails the merge is undone, the brief records why,
	// and the patch goes alone until a person merges the branch by hand.
	if err := checkMergeGated(dir, head, commit, onto); err != nil {
		// The reason leads with the instruction: mergeNotMade caps it at
		// 300 bytes, and the tail of the error text is the part that may go.
		mergeNotMade(mergeInfoFile, "a person must merge it by hand: the merge of %s would carry gated .github/ changes the base branch does not have (%v)", baseRef, err)
		if err := gitRun(dir, "reset", "--hard", head); err != nil {
			return out, fmt.Errorf("refused the merge but could not undo it: %w", err)
		}
		log.Printf("merge-default: the merge of %s would carry gated .github/ changes the base tip does not have (%v); continuing without a merge (a person must merge it by hand)", baseRef, err)
		return out, nil
	}
	// The recorded flag matches what publish checks: whether the merge
	// commit changes a gated .github/ path relative to the head it started
	// from -- exactly that, since issue #20, no longer "the base may have
	// changed one": a path the branch already changed identically to the
	// base (a cherry-picked fix) changes nothing relative to the head, so
	// the merge carries nothing and publishes without an approval.
	// merge-ort follows renames, so this can differ from merge-base..onto:
	// a branch that moved a file into .github/ while the base edited it at
	// the old path carries a change the range above does not see. An error
	// counts as carrying, as above.
	if relWf, err := gatedGitHubChanges(dir, head, commit); err != nil {
		log.Printf("merge-default: could not tell which gated .github/ paths the merge changes relative to the head (%v); recording that it may carry them", err)
		carryGated = true
	} else {
		carryGated = len(relWf) > 0
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		mergeNotMade(mergeInfoFile, "the merge commit could not be checked out: %v", err)
		log.Printf("merge-default: could not check the merge out (%v); continuing without one", err)
		return out, nil
	}
	out = mergeOutcome{State: kind, Commit: commit, Tree: tree, Files: files}
	// The patch is diffed against this commit, and publish recreates the
	// merge it names: both artifacts must carry it.
	if err := os.WriteFile(baseCommitFile, []byte(commit+"\n"), 0o644); err != nil {
		return out, fmt.Errorf("wrote the merge commit but could not record it as the base commit: %w", err)
	}
	info := mergeInfo{Kind: kind, Commit: commit, Base: head, Onto: onto, Tree: tree, Files: files, Workflows: carryGated}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return out, err
	}
	if err := os.WriteFile(mergeInfoFile, append(data, '\n'), 0o644); err != nil {
		return out, fmt.Errorf("wrote the merge commit but could not record the merge info: %w", err)
	}
	// A merge info from an earlier attempt in this checkout would leave the
	// brief reporting a no-merge that no longer is one.
	if err := os.Remove(runStateFile(mergeInfoFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("wrote the merge commit but could not clear the no-merge record: %w", err)
	}
	if conflicted {
		printConflictNotice(baseRef, onto, branch, files)
	} else {
		fmt.Printf("::notice::merged %s (%s) into %s cleanly\n", baseRef, shortSHA(onto), branch)
	}
	if carryGated {
		fmt.Printf("::notice::the merge carries %s .github/ changes; publishing it waits for a maintainer approval (the codeaf-harness environment)\n", baseRef)
	}
	return out, nil
}

// printConflictNotice prints the run job's ::notice:: about a conflicted
// merge. Unquoted paths can hold non-ASCII bytes and control characters
// (issue #33): the controls are stripped -- a notice line must not carry
// them, and a raw CR would forge a workflow command (issue #34) -- and the
// list is capped on a rune boundary so the cut cannot split a character
// (issue #22).
func printConflictNotice(baseRef, onto, branch string, files []string) {
	list := stripControl(strings.Join(files, ", "))
	if len(list) > 400 {
		list = capUTF8(list, 400) + "…"
	}
	fmt.Printf("::notice::merged %s (%s) into %s with conflicts; CodeAF must resolve: %s\n", baseRef, shortSHA(onto), branch, list)
}

// publishMerge is the publish half of issues #57 and #171. With the run's
// merge info it recreates the merge from the same two parents, refusing
// unless the tree it computes itself matches the recorded one. Without it --
// a run that merged nothing, or died before merging -- it still lands the
// base as a merge commit with both parents (never a fast-forward, and never
// inside the work commit), and refuses a patch that would re-publish the
// base's own changes, the squash run 39094290325 published for #163, rather
// than push a single commit whose parents cannot show what came from the
// base. The stale-patch refusal stays for a branch that moved. It writes the
// merge state for the workflow steps and the marker check; the caller pushes
// only what this produced.
func publishMerge(dir, branch, baseRef, baseCommitFile, mergeInfoFile, stateFile, patchPath string, compare compareFunc) (out mergeOutcome, err error) {
	// On success the state file records what happened, for the workflow
	// steps (the push condition, the report) and the marker check.
	defer func() {
		if err == nil {
			err = writeMergeState(stateFile, out)
		}
	}()
	head, err := revParse(dir, "HEAD")
	if err != nil {
		return mergeOutcome{State: "none"}, err
	}
	baseCommitBytes, err := os.ReadFile(baseCommitFile)
	if err != nil {
		return mergeOutcome{State: "none"}, fmt.Errorf("the run recorded no base commit: %w", err)
	}
	baseCommit := strings.TrimSpace(string(baseCommitBytes))
	info, err := readMergeInfo(mergeInfoFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A merge info that exists but does not validate is refused, not
		// ignored: the run's checkout was built on it.
		return mergeOutcome{State: "none"}, err
	}
	if info != nil {
		return recreateMerge(dir, branch, baseRef, info, baseCommit, head, compare)
	}
	// No merge info: the run merged nothing. The patch must be against the
	// branch head, as it always was.
	if baseCommit != head {
		return mergeOutcome{State: "none"}, fmt.Errorf("the run's base commit %.12s is not the PR head publish fetched (%.12s): the branch moved while CodeAF ran, or the run made a base merge it left no verifiable merge info for; refusing to apply a stale patch", baseCommit, head)
	}
	out = mergeOutcome{State: "none"}
	if baseRef == "" {
		mergeSkipped("no base branch named; pushing the patch alone")
		return out, nil
	}
	if err := fetchRef(dir, baseRef); err != nil {
		mergeSkipped("could not fetch %s (%v); pushing the patch without bringing it in", baseRef, err)
		return out, nil
	}
	onto, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if onto == head {
		return out, nil // already the base tip
	}
	cmp, err := compare(head, onto)
	if err != nil {
		mergeSkipped("could not compare the branch with %s (%v); pushing the patch without a merge", baseRef, err)
		return out, nil
	}
	switch {
	case cmp.Status == "behind" || cmp.Status == "identical":
		return out, nil // the base tip is already contained in the branch
	case cmp.Status == "ahead":
		// The branch is strictly behind its base (issue #171): the base comes
		// in as a merge commit with both parents, never a fast-forward -- a
		// single-parent commit leaves the base's changes indistinguishable
		// from the branch's own work, and the PR's diff replays them.
		if err := refuseReplayedBaseMerge(dir, patchPath, head, onto); err != nil {
			return out, err
		}
		// Issue #16, widened by issue #20: with a maintainer's approval the
		// merge goes through -- the push step then uses the GitHub App
		// token, which can carry the gated .github/ changes. Without one
		// the patch is pushed alone, as before. (No overlap check here: the
		// merge base is the head itself, so the branch made no change since
		// the branch point and the overlap is always empty.)
		if wf, err := gatedGitHubChanges(dir, head, onto); (err != nil || len(wf) > 0) && !mergeApproved() {
			mergeSkipped("%s changed gated .github/ paths (%v); without a maintainer approval the push token cannot carry them, so pushing the patch without the merge", baseRef, wf)
			return out, nil
		}
		tree, _, conflicted, err := mergeTree(dir, head, head, onto)
		if err != nil || conflicted {
			// ours is the merge base here, so the merge cannot conflict;
			// anything else is a degraded host state, and the patch goes
			// alone as before #57.
			mergeSkipped("could not compute the behind-branch merge (%v); pushing the patch without one", err)
			return out, nil
		}
		commit, err := commitMerge(dir, tree, head, onto, fmt.Sprintf("Merge %s into %s", baseRef, branch))
		if err != nil {
			return out, err
		}
		// The same approval rule as recreateMerge: a merge that changes
		// any gated .github/ path relative to the PR head needs a
		// maintainer's approval, even when the head..base-tip comparison
		// above found none -- merge-ort follows renames, so the merge can
		// land gated content that comparison does not see. An error counts
		// as changed, as in recreateMerge.
		if carried, cerr := gatedGitHubChanges(dir, head, commit); (cerr != nil || len(carried) > 0) && !mergeApproved() {
			mergeSkipped("%s's merge changes gated .github/ paths relative to the PR head (paths %v, error %v); without a maintainer approval the push token cannot carry them, so pushing the patch without the merge", baseRef, carried, cerr)
			return out, nil
		}
		// Issue #16, widened by issue #20: every gated .github/ path the
		// merge changes relative to the PR head must be byte-for-byte what
		// the commit it merged in already has -- the same check
		// recreateMerge runs, so a merge that would publish gated content
		// the base does not have is not pushed, while the head's own edits
		// are kept. A refusal here used to fail every publish (issue #20):
		// the merge is skipped instead and the patch goes alone, the way
		// the run side now skips it. Checked before the reset, so the
		// branch stays untouched.
		if err := checkMergeGated(dir, head, commit, onto); err != nil {
			mergeSkipped("the merge of %s would carry gated .github/ changes the base tip does not have (%v); pushing the patch without the merge", baseRef, err)
			return out, nil
		}
		if err := gitRun(dir, "reset", "--hard", commit); err != nil {
			return out, err
		}
		out = mergeOutcome{State: mergeKindClean, Commit: commit, Tree: tree}
		fmt.Printf("::notice::landed a merge commit bringing %s (%s) into %s before the patch\n", baseRef, shortSHA(onto), branch)
		return out, nil
	case cmp.Status == "diverged":
		// handled below, over the compare API's merge base
	default:
		mergeSkipped("unexpected compare status %q; pushing the patch without a merge", cmp.Status)
		return out, nil
	}
	if cmp.MergeBase == "" || !isSHA(cmp.MergeBase) {
		mergeSkipped("the compare API named no merge base; pushing the patch without a merge")
		return out, nil
	}
	base := cmp.MergeBase
	if err := fetchCommit(dir, base); err != nil {
		mergeSkipped("could not fetch the merge base %s (%v); pushing the patch without a merge", shortSHA(base), err)
		return out, nil
	}
	// Issue #171: a patch that would re-publish the base's own changes is a
	// base merge the run left unverifiable, and is refused before anything is
	// applied -- checked here and on the behind branch above, always before
	// the workflow gate, whose "patch alone" outcome is where the squash of
	// run 39094290325 happened.
	if err := refuseReplayedBaseMerge(dir, patchPath, base, onto); err != nil {
		return out, err
	}
	// Issue #16, widened to every gated .github/ path by issue #20: a
	// gated file the base changed and the branch also edits is not merged
	// here either -- unless the branch already made the identical change:
	// the merged content would equal neither the head's nor the base tip's,
	// and publish would refuse it even with a maintainer's approval. The
	// patch goes alone, whatever the approval said, and a person merges the
	// branch by hand.
	if overlap, err := overlappingGatedPaths(dir, base, onto, head); err != nil {
		log.Printf("merge-default: could not tell whether the branch and %s edited the same gated .github/ files (%v); the overlap check is skipped", baseRef, err)
	} else if len(overlap) > 0 {
		mergeSkipped("%s changed .github/ file(s) %s that the branch also edits; pushing the patch without the merge (a person must merge it by hand)", baseRef, strings.Join(overlap, ", "))
		return out, nil
	}
	// Issue #16: with a maintainer's approval the merge goes through -- the
	// push step then uses the GitHub App token, which can carry the gated
	// .github/ changes. Without one the patch is pushed alone, as before.
	if wf, err := gatedGitHubChanges(dir, base, onto); (err != nil || len(wf) > 0) && !mergeApproved() {
		mergeSkipped("%s changed gated .github/ paths since the branch point (%v); without a maintainer approval the push token cannot carry them, so pushing the patch without the merge", baseRef, wf)
		return out, nil
	}
	tree, _, conflicted, err := mergeTree(dir, base, head, onto)
	if err != nil {
		mergeSkipped("could not compute the merge (%v); pushing the patch without one", err)
		return out, nil
	}
	if conflicted {
		// A conflict CodeAF never saw. Publishing a merge whose files carry
		// markers would push an unresolved merge, so the patch goes alone:
		// the PR stays dirty, and the next run does the conflicted flow.
		mergeSkipped("merging %s would conflict; pushing the patch without the merge", baseRef)
		return out, nil
	}
	commit, err := commitMerge(dir, tree, head, onto, fmt.Sprintf("Merge %s into %s", baseRef, branch))
	if err != nil {
		return out, err
	}
	// The same approval rule as on the behind branch above and in
	// recreateMerge: gated .github/ paths the merge changes relative to the
	// PR head need a maintainer's approval, with an error counting as
	// changed.
	if carried, cerr := gatedGitHubChanges(dir, head, commit); (cerr != nil || len(carried) > 0) && !mergeApproved() {
		mergeSkipped("%s's merge changes gated .github/ paths relative to the PR head (paths %v, error %v); without a maintainer approval the push token cannot carry them, so pushing the patch without the merge", baseRef, carried, cerr)
		return out, nil
	}
	// The same per-path check as on the behind branch above -- and the same
	// patch-alone outcome on a refusal (issue #20) -- before the reset so
	// the branch stays untouched.
	if err := checkMergeGated(dir, head, commit, onto); err != nil {
		mergeSkipped("the merge of %s would carry gated .github/ changes the base tip does not have (%v); pushing the patch without the merge", baseRef, err)
		return out, nil
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		return out, err
	}
	out.State = mergeKindClean
	out.Commit = commit
	out.Tree = tree
	fmt.Printf("::notice::merged %s (%s) into %s cleanly before the patch\n", baseRef, shortSHA(onto), branch)
	return out, nil
}

// mergeWorkflowsRouting reads, for the run job's packaging step, whether the
// recorded merge carries the base branch's gated .github/ changes -- every
// path under .github/ outside .github/codeaf/ since issue #20, before it the
// workflow paths only (issue #16):
// merge info through the validating reader, so the workflow never parses the
// file in shell. A missing file reads false; one that exists but does not
// validate is an error, not a silent false.
func mergeWorkflowsRouting(path string) (bool, error) {
	info, err := readMergeInfo(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Workflows, nil
}

// intersectPaths returns the paths that appear in both lists, sorted.
func intersectPaths(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, p := range b {
		inB[p] = true
	}
	var both []string
	for _, p := range a {
		if inB[p] {
			both = append(both, p)
		}
	}
	sort.Strings(both)
	return both
}

// overlappingGatedPaths returns the gated .github/ paths (issue #20; before
// it, the workflow paths only, issue #16) that both the base branch and the
// PR branch changed since the branch point: paths a merge would set to
// content that is neither the head's nor the base tip's, which publish
// refuses even with a maintainer's approval, so such a merge is not made at
// all and the patch is pushed alone. A path the branch already changed
// identically to the base tip -- the same blob and mode, a cherry-picked fix
// say (issue #20) -- is left out: the merge keeps the shared change, carries
// nothing relative to the head, and publishes without an approval. An error
// means the question could not be answered; callers skip the overlap check
// then, and the approval gate and the byte check decide instead.
func overlappingGatedPaths(dir, base, onto, head string) ([]string, error) {
	baseWf, err := gatedGitHubChanges(dir, base, onto)
	if err != nil {
		return nil, err
	}
	headWf, err := gatedGitHubChanges(dir, base, head)
	if err != nil {
		return nil, err
	}
	var both []string
	for _, path := range intersectPaths(baseWf, headWf) {
		inHead, err := blobEntry(dir, head, path)
		if err != nil {
			return nil, err
		}
		inOnto, err := blobEntry(dir, onto, path)
		if err != nil {
			return nil, err
		}
		if inHead != inOnto {
			both = append(both, path)
		}
	}
	return both, nil
}

// checkMergeGated is the per-path byte check shared by runMerge,
// recreateMerge and the fallbacks (issue #16; widened by issue #20): for
// each gated .github/ path -- every path under .github/ outside
// .github/codeaf/ -- that the merge commit changes relative to the PR head,
// the merge's content and file mode must equal the commit merged in's
// (onto) -- additions, edits and deletions alike -- so a merge brings in
// only what the base already has, never gated content of its own. Paths the
// merge leaves as they are on head are the branch's own edits, already on
// the branch and gated by the patch check that carries them, so they are
// not checked.
func checkMergeGated(dir, head, commit, onto string) error {
	changed, err := gatedGitHubChanges(dir, head, commit)
	if err != nil {
		return fmt.Errorf("could not list the gated .github/ paths the merge changes relative to the PR head: %w", err)
	}
	for _, path := range changed {
		inMerge, err := blobEntry(dir, commit, path)
		if err != nil {
			return fmt.Errorf("could not read %s in the merge commit %s: %w", path, shortSHA(commit), err)
		}
		inOnto, err := blobEntry(dir, onto, path)
		if err != nil {
			return fmt.Errorf("could not read %s in the commit merged in (%s): %w", path, shortSHA(onto), err)
		}
		if inMerge != inOnto {
			return fmt.Errorf("the merge changes the .github/ file %s, but it is not what the commit it merged in (%s) already has (content or mode differ); a base merge brings in only what the base branch already has, so refusing the merge", path, shortSHA(onto))
		}
	}
	return nil
}

// blobEntry returns git's ls-tree entry for path in rev -- its mode and
// blob hash, so the per-path check compares the file mode as well as the
// content -- or the empty string when the path does not exist there (a
// deletion), so a workflow file the merge deletes compares equal only to one
// the merged-in commit lacks too. Reading entries keeps the comparison off
// the work tree.
func blobEntry(dir, rev, path string) (string, error) {
	out, err := gitOutput(dir, "ls-tree", rev, "--", path)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil // the path is not in rev: deleted there
	}
	// The line is "<mode> <type> <hash>\t<path>"; the hash ends at the tab,
	// which a path with spaces cannot move.
	meta, _, ok := strings.Cut(line, "\t")
	if !ok {
		return "", fmt.Errorf("could not parse git ls-tree output for %s in %s", path, shortSHA(rev))
	}
	fields := strings.Fields(meta)
	if len(fields) != 3 {
		return "", fmt.Errorf("could not parse git ls-tree output for %s in %s", path, shortSHA(rev))
	}
	return fields[0] + " " + fields[2], nil
}

// mergeApproved reports whether the publish job recorded a maintainer's
// approval of this run: "Check the maintainer's approval" verified through
// the API that someone with admin or maintain access approved the
// codeaf-harness environment for this run, and set HARNESS_APPROVED=true.
// Only that checked value counts; the run job's routing output never does.
func mergeApproved() bool {
	return envOr("HARNESS_APPROVED", "") == "true"
}

// mergeSkipped reports a fallback merge publish declined to make: the patch
// goes alone and the base is not merged in. The log line is for the run's
// records; when the run's maintainer approved the merge, the ::warning::
// also reaches the job log, so the approver sees the base was not merged
// after all instead of reading a green run as if it had been.
func mergeSkipped(format string, args ...any) {
	// The message carries baseRef, git error text and lists of gated paths,
	// none of it trusted: diffNameOnly unquotes git's C-quoting, so a path can
	// contain a raw newline and start a ::command:: line in the workflow log.
	// Build the message once, strip control characters and cap it before both
	// the log line and the ::warning:: line (issue #22).
	msg := capUTF8(stripControl(fmt.Sprintf(format, args...)), maxNoMergeReasonBytes)
	log.Printf("merge-default: %s", msg)
	if mergeApproved() {
		fmt.Printf("::warning::the base merge was skipped: %s\n", msg)
	}
}

// recreateMerge rebuilds the merge the run job committed, from the same two
// parents, and refuses unless everything checks out: the recorded base is the
// branch head publish fetched, the recorded commit is the run's base commit,
// the base tip it merged is a commit the base branch contains (anything else
// could smuggle content into the merge under the cover of "bringing the base
// in"), and the tree of the merge publish computes itself matches the
// recorded tree. A merge carrying gated .github/ changes relative to the
// PR head -- every path under .github/ outside .github/codeaf/ (issue #20;
// before it, .github/workflows/ only, issue #16) -- is recreated only when
// a maintainer approved this run: the push then uses the GitHub App token,
// which GitHub lets carry workflow files. Publish recomputes "carries"
// itself, relative to the head -- the gated paths the merge commit changes
// against the head it started from, never the run's "workflows" field --
// and every path it changes must equal the recorded base tip's content and
// mode: the merge brings in only what the base already has, never a gated
// edit of the branch's own (that travels in the patch, where the approval
// check and the workflow policy see it).
func recreateMerge(dir, branch, baseRef string, info *mergeInfo, baseCommit, head string, compare compareFunc) (mergeOutcome, error) {
	out := mergeOutcome{State: "none"}
	if info.Workflows && !mergeApproved() {
		return out, errors.New("the recorded merge carries the base branch's gated .github/ changes, and publishing one needs a maintainer's approval of the codeaf-harness environment (HARNESS_APPROVED, checked through the API); no maintainer approved this run, so refusing the merge")
	}
	if info.Base != head {
		return out, fmt.Errorf("the merge info names base %s but the branch head is %s; refusing a merge made against another commit", shortSHA(info.Base), shortSHA(head))
	}
	if baseCommit != info.Commit {
		return out, fmt.Errorf("the run's base commit %s is not the merge commit its merge info names (%s); refusing", shortSHA(baseCommit), shortSHA(info.Commit))
	}
	if baseRef == "" {
		return out, errors.New("the merge info cannot be checked without the base branch name; refusing")
	}
	if err := fetchRef(dir, baseRef); err != nil {
		return out, fmt.Errorf("could not fetch %s to check the recorded merge: %w", baseRef, err)
	}
	tip, err := revParse(dir, "FETCH_HEAD")
	if err != nil {
		return out, err
	}
	if err := fetchCommit(dir, info.Onto); err != nil {
		return out, fmt.Errorf("could not fetch the recorded merge's base tip %s: %w", shortSHA(info.Onto), err)
	}
	rel, err := compare(info.Onto, tip)
	if err != nil {
		return out, fmt.Errorf("could not compare the recorded merge's base tip with %s: %w", baseRef, err)
	}
	// compare(base, head) reports head relative to base: "ahead" means the
	// base branch has moved on past the recorded tip, which it therefore
	// contains; "behind" or "diverged" mean the tip is not in the branch.
	if rel.Status != "ahead" && rel.Status != "identical" {
		return out, fmt.Errorf("the recorded merge's base tip %s is not contained in %s (compare says %q); refusing the merge", shortSHA(info.Onto), baseRef, rel.Status)
	}
	// The merge base is the one the run used: the base of the two parents of
	// the merge, which the API reports for the same fixed commits. It is not
	// the merge base of the containment comparison above, which is between
	// the recorded tip and the base branch. Fixed inputs give a fixed merge
	// base, so this recomputes what the run merged over; a disagreement ends
	// in the tree check below.
	rel, err = compare(head, info.Onto)
	if err != nil {
		return out, fmt.Errorf("could not compare the branch with the recorded base tip: %w", err)
	}
	// A diverged branch merges over the compare API's merge base; a strictly
	// behind one (issue #171: a run that brought a behind branch up to its
	// base) merges over the head itself, which is its own branch point.
	if rel.Status != "diverged" && rel.Status != "ahead" {
		return out, fmt.Errorf("the parents of the recorded merge are not diverged (compare says %q); refusing the merge", rel.Status)
	}
	base := rel.MergeBase
	if rel.Status == "ahead" {
		base = head
	}
	if base == "" || !isSHA(base) {
		return out, errors.New("the compare API named no merge base for the recorded merge; refusing")
	}
	if err := fetchCommit(dir, base); err != nil {
		return out, fmt.Errorf("could not fetch the merge base %s: %w", shortSHA(base), err)
	}
	tree, files, conflicted, err := mergeTree(dir, base, head, info.Onto)
	if err != nil {
		return out, fmt.Errorf("could not recompute the recorded merge: %w", err)
	}
	kind := mergeKindClean
	if conflicted {
		kind = mergeKindConflicted
	}
	if kind != info.Kind {
		return out, fmt.Errorf("the recomputed merge is %s but the run recorded %s; refusing", kind, info.Kind)
	}
	if tree != info.Tree {
		return out, fmt.Errorf("the recomputed merge tree %s does not match the tree the run recorded (%s); refusing the merge", shortSHA(tree), shortSHA(info.Tree))
	}
	commit, err := commitMerge(dir, tree, head, info.Onto, fmt.Sprintf("Merge %s into %s", baseRef, branch))
	if err != nil {
		return out, err
	}
	// Issue #16: "carries" is measured relative to the PR head -- the
	// workflow paths the merge commit changes against the head it started
	// from. The head's own workflow edits are already on the branch and
	// publish nothing new, and merge-ort follows renames, so this can
	// differ from what merge-base..onto changes: a merge over a base that
	// changed no workflow path can still land one in .github/workflows/
	// when the branch moved a file there and the base edited it at the old
	// path. An error counts as carrying, as on the run side. Everything
	// below is checked before the reset, so a refusal leaves the branch
	// untouched.
	headCarries := false
	if changed, err := gatedGitHubChanges(dir, head, commit); err != nil {
		log.Printf("merge-default: could not tell which gated .github/ paths the merge changes relative to the head (%v); treating it as carrying them", err)
		headCarries = true
	} else {
		headCarries = len(changed) > 0
	}
	// The run's flag is checked against the recomputed value, never
	// trusted: merge-info.json comes from the run job. A run that recorded
	// "workflows": false for a merge that changes workflow paths relative
	// to the head lied about its merge -- refused even if a maintainer
	// approved, because nothing else it recorded about the merge can be
	// trusted either.
	if headCarries && !info.Workflows {
		return out, errors.New("the run recorded that its merge carries no gated .github/ changes, but the merge changes gated paths under .github/ relative to the branch head; the run lied about its merge, so refusing it even if a maintainer approved")
	}
	// Carrying gated .github/ changes relative to the head (recomputed or
	// recorded) needs the maintainer's approval, whatever the run recorded:
	// the push then uses the GitHub App token, which GitHub lets carry
	// workflow files.
	if (headCarries || info.Workflows) && !mergeApproved() {
		return out, errors.New("the merge carries gated .github/ changes relative to the branch head, and publishing one needs a maintainer's approval of the codeaf-harness environment (HARNESS_APPROVED, checked through the API); no maintainer approved this run, so refusing the merge")
	}
	// The per-path byte check runs whatever the flags say: for each gated
	// .github/ path the merge changes relative to the PR head, its content
	// and mode must be what the recorded base tip (info.Onto) already has
	// -- not the base branch's current tip, which may have moved on since
	// the run. The tree hash above proves the merge is the one the run
	// recorded; this proves that what it records brings in only the base's
	// gated content. The head's own edits are kept: paths the merge leaves
	// as they are on head are not checked, and a merge that carries nothing
	// changes no gated path relative to the head, so the check costs
	// nothing there.
	if err := checkMergeGated(dir, head, commit, info.Onto); err != nil {
		return out, err
	}
	if err := gitRun(dir, "reset", "--hard", commit); err != nil {
		return out, err
	}
	out = mergeOutcome{State: kind, Commit: commit, Tree: tree, Files: files}
	fmt.Printf("::notice::recreated the run's merge of %s (%s) into %s as %s; tree %s verified\n", baseRef, shortSHA(info.Onto), branch, shortSHA(commit), shortSHA(tree))
	return out, nil
}

// writeMergeState records what publishMerge did, for the workflow steps (the
// push condition and the report) and for the marker check. Only fixed words
// and numbers go in, so the file can be read with grep like pr-mode's output.
func writeMergeState(path string, out mergeOutcome) error {
	merged := out.State != "none"
	if err := os.WriteFile(path, []byte(fmt.Sprintf("state=%s\nmerged=%t\n", out.State, merged)), 0o644); err != nil {
		return err
	}
	if out.State == mergeKindConflicted {
		return os.WriteFile(path+".files", []byte(strings.Join(out.Files, "\n")+"\n"), 0o644)
	}
	return nil
}

// findConflictMarker reports the first line that opens or closes a conflict
// region. The bare "=======" separator is deliberately not a marker on its
// own: it is legal text, and an unresolved region always carries its two
// outer markers.
func findConflictMarker(content []byte) (line int, ok bool) {
	for i, text := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(text, "<<<<<<<") || strings.HasPrefix(text, ">>>>>>>") {
			return i + 1, true
		}
	}
	return 0, false
}

// verifyMerge refuses the publish while a conflict marker remains in any file
// the recreated merge conflicted in (issue #57): the patch was supposed to
// resolve them, whatever else it did. A conflicted file the patch deleted
// counts as resolved.
func verifyMerge(dir, stateFile string) error {
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return fmt.Errorf("could not read the merge state: %w", err)
	}
	state := ""
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "state="); ok {
			state = strings.TrimSpace(v)
		}
	}
	if state != mergeKindConflicted {
		return nil // nothing was conflicted; nothing to resolve
	}
	listData, err := os.ReadFile(stateFile + ".files")
	if err != nil {
		return fmt.Errorf("the merge was conflicted but its file list is missing: %w", err)
	}
	var marked []string
	for _, f := range strings.Split(strings.TrimRight(string(listData), "\n"), "\n") {
		if f == "" {
			continue
		}
		if !usableMergePath(f) {
			return fmt.Errorf("the merge state names an unusable conflicted path %q", f)
		}
		content, err := os.ReadFile(filepath.Join(dir, f))
		if errors.Is(err, os.ErrNotExist) {
			continue // resolved by deleting the file
		}
		if err != nil {
			return fmt.Errorf("could not read the conflicted file %s: %w", f, err)
		}
		if line, ok := findConflictMarker(content); ok {
			where := f
			if line > 0 {
				where = fmt.Sprintf("%s line %d", f, line)
			}
			marked = append(marked, where)
		}
	}
	if len(marked) > 0 {
		for _, m := range marked {
			// Backstop (issue #34 security review): no raw path reaches a
			// ::error:: line even if the path check above ever changes --
			// a raw CR would forge a workflow command.
			fmt.Printf("::error::conflict marker left in %s\n", stripControl(m))
		}
		return fmt.Errorf("%d conflicted file(s) still carry conflict markers; every conflict must be resolved before publishing", len(marked))
	}
	return nil
}

// The bounds of the issue #171 replay check. A patch or a base changed-file
// list over either cap is not checked: publish then relies on the strict (or
// 3-way) apply, as before #171. The check never reads past these caps.
const maxReplayNames = 2000
const maxReplayPatchSize = 1 << 26 // 64 MB; the workflow refuses over 50 MB

// refuseReplayedBaseMerge refuses a patch that would re-publish the base
// branch's own changes (issue #171): when the run made a base merge it left
// no verifiable record of -- merge-info.json missing or unusable -- its
// content reaches publish only inside the patch, and applying it lands the
// base's changes in the work commit as a squash, the way run 39094290325
// published #163: one commit carrying #161/#164's snyk-linear-sync code,
// #166's queue gate and #162's copied workflow edit, with no merge commit
// anywhere.
//
// Only an exact replay is refused: a post-image the base tip also has means
// the patch is carrying the base's content, and without merge info publish
// cannot tell a base merge the run made from work the agent did on top of it.
// A file the agent edited beyond the base's content is the agent's own work;
// the strict (or 3-way) apply then decides whether it fits. Every read is
// bounded, and an over-cap or unparsable patch skips the check: the apply
// decides, as it did before #171. The patch is run-job output, so it is
// untrusted data (AGENTS.md): parsed, never sourced, and applied only in a
// private index that starts at the branch head.
func refuseReplayedBaseMerge(dir, patchPath, branchPoint, onto string) error {
	if patchPath == "" || !isSHA(branchPoint) || !isSHA(onto) {
		return nil
	}
	// What the base branch changed since the branch point, bounded.
	changed, err := gitOutput(dir, "diff", "--no-renames", "--name-only", branchPoint, onto)
	if err != nil {
		return nil // cannot tell what the base changed; the apply decides
	}
	baseFiles, bounded := boundedNames(changed, maxReplayNames)
	if !bounded {
		log.Printf("merge-default: over %d files changed in the base since the branch point; the replay check is skipped", maxReplayNames)
		return nil
	}
	patchFiles, bounded := patchTouchedFiles(patchPath)
	if !bounded {
		log.Printf("merge-default: over %d files touched by the patch; the replay check is skipped", maxReplayNames)
		return nil
	}
	var common []string
	for f := range baseFiles {
		if patchFiles[f] {
			common = append(common, f)
		}
	}
	sort.Strings(common)
	if len(common) == 0 {
		return nil // the patch touches nothing the base changed: no replay
	}
	post, ok := patchPostImages(dir, patchPath, common)
	if !ok {
		return nil // the patch does not apply on the branch head; the apply decides
	}
	for _, f := range common {
		if post[f] == baseBlob(dir, onto, f) {
			return fmt.Errorf("the patch replays the base branch's changes to %s: the run's base merge is present but unverifiable (no merge info), and applying it would land the base's changes in the work commit; refusing to publish them as a single commit", f)
		}
	}
	return nil
}

// boundedNames splits a newline list into a set, reporting whether it fit.
func boundedNames(list string, max int) (map[string]bool, bool) {
	out := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimRight(list, "\n"), "\n") {
		if line == "" {
			continue
		}
		if len(out) >= max {
			return nil, false
		}
		out[line] = true
	}
	return out, true
}

// patchTouchedFiles names the files a patch touches, from its "diff --git"
// lines. It reports false when the patch is over its size cap or names more
// files than the cap, or when a "diff --git" line cannot be parsed with
// bounds (a quoted path): the caller then skips the replay check and lets the
// apply decide. A missing patch file touches nothing.
func patchTouchedFiles(patchPath string) (map[string]bool, bool) {
	data, err := os.ReadFile(patchPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]bool{}, true
		}
		return nil, false
	}
	if len(data) > maxReplayPatchSize {
		return nil, false
	}
	out := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "diff --git ") || strings.Contains(line, `"`) {
			continue // a quoted path: parsed with bounds, the apply decides
		}
		rest := strings.TrimPrefix(line, "diff --git a/")
		i := strings.Index(rest, " b/")
		if i <= 0 {
			continue
		}
		a, b := rest[:i], rest[i+3:]
		for _, name := range []string{a, b} {
			if name == "" {
				continue
			}
			if len(out) >= maxReplayNames {
				return nil, false
			}
			out[name] = true
		}
	}
	return out, true
}

// patchPostImages reads the post-image (mode and blob) the patch produces for
// each named file, in a private index that starts at the branch head: the
// checkout's index and working tree are never touched. It reports false when
// the patch does not apply on the head; the caller then lets the apply
// decide.
func patchPostImages(dir, patchPath string, files []string) (map[string]string, bool) {
	index, err := os.CreateTemp("", "codeaf-replay-index-")
	if err != nil {
		return nil, false
	}
	name := index.Name()
	index.Close()
	defer os.Remove(name)
	if err := gitIndexRun(dir, name, "read-tree", "HEAD"); err != nil {
		return nil, false
	}
	if err := gitIndexRun(dir, name, "apply", "--cached", "--binary", "--whitespace=nowarn", patchPath); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		line, err := gitIndexOutput(dir, name, "ls-files", "-s", "--", f)
		if err != nil {
			return nil, false
		}
		out[f] = indexPostImage(line)
	}
	return out, true
}

// indexPostImage turns one "git ls-files -s" line into its "mode blob" pair,
// empty when the index carries no entry (the patch deleted the file).
func indexPostImage(line string) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < 2 {
		return ""
	}
	return fields[0] + " " + fields[1]
}

// baseBlob reads the "mode blob" pair the named commit carries for a path,
// empty when the commit has no entry there.
func baseBlob(dir, commit, path string) string {
	out, err := gitOutput(dir, "ls-tree", commit, "--", path)
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) < 3 || fields[1] != "blob" {
		return ""
	}
	return fields[0] + " " + fields[2]
}

// gitIndexOutput runs git with a private index (GIT_INDEX_FILE) and returns
// its stdout, for reads that must not touch the checkout's index.
func gitIndexOutput(dir, indexFile string, args ...string) (string, error) {
	return gitEnvOutput(dir, []string{"GIT_INDEX_FILE=" + indexFile}, args...)
}

// gitIndexRun runs git with a private index, discarding stdout.
func gitIndexRun(dir, indexFile string, args ...string) error {
	_, err := gitEnvOutput(dir, []string{"GIT_INDEX_FILE=" + indexFile}, args...)
	return err
}

// gitEnvOutput runs git with extra environment and returns its stdout. A
// nonzero exit is an error carrying the first of stderr.
func gitEnvOutput(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		stderr := strings.TrimSpace(errBuf.String())
		if len(stderr) > 300 {
			stderr = stderr[:300]
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// runMergeDefault implements `harness merge-default` (issue #57):
//
//	harness merge-default run BASE_COMMIT_FILE MERGE_INFO_FILE
//	    run job, pr mode: merge the base branch tip into the branch before
//	    CodeAF works, commit it (with markers on a conflict), make it the base
//	    commit, and record it in merge-info.json for publish to recreate.
//	harness merge-default publish BASE_COMMIT_FILE MERGE_INFO_FILE STATE_FILE
//	    publish, pr mode: recreate the run's merge from the same two parents,
//	    checked by tree hash, or merge the base tip first when the run left no
//	    merge info; write the merge state for the workflow and the marker check.
//	harness merge-default verify STATE_FILE
//	    publish, after the patch: refuse while a conflict marker remains in a
//	    conflicted file.
//	harness merge-default workflows MERGE_INFO_FILE
//	    run job, packaging: print whether the recorded merge carries gated
//	    .github/ changes (issue #16; widened by issue #20), so the step routes
//	    publish into the codeaf-harness environment. The file is read through the
//	    validating reader, never parsed in shell; a missing file reads false.
//
// Every other phase is a no-op outside pr mode, so review and issue runs are
// untouched; the workflow steps gate on the mode as well.
func runMergeDefault() error {
	if len(os.Args) < 3 {
		return errors.New("usage: harness merge-default run|publish|verify|workflows ...")
	}
	phase, args := os.Args[2], os.Args[3:]
	// Routing reads merge info in modes that merge nothing (a patch-only
	// issue run has no merge info and reads false), so this phase does not
	// gate on pr mode.
	if phase == "workflows" {
		if len(args) != 1 {
			return errors.New("usage: harness merge-default workflows MERGE_INFO_FILE")
		}
		routed, err := mergeWorkflowsRouting(args[0])
		if err != nil {
			return err
		}
		fmt.Printf("%t\n", routed)
		return nil
	}
	if envOr("MODE", "") != "pr" {
		return nil
	}
	branch, baseRef := os.Getenv("BRANCH"), os.Getenv("BASE")
	switch phase {
	case "run":
		if len(args) != 2 {
			return errors.New("usage: harness merge-default run BASE_COMMIT_FILE MERGE_INFO_FILE")
		}
		_, err := runMerge(".", branch, baseRef, args[0], args[1], ghCompare)
		return err
	case "publish":
		if len(args) != 3 && len(args) != 4 {
			return errors.New("usage: harness merge-default publish BASE_COMMIT_FILE MERGE_INFO_FILE STATE_FILE [PATCH_FILE]")
		}
		// The patch file is optional so an unmerged workflow line (applied by
		// a maintainer, per AGENTS.md) keeps calling this with three
		// arguments; without it the issue #171 replay check is skipped.
		patchPath := ""
		if len(args) == 4 {
			patchPath = args[3]
		}
		_, err := publishMerge(".", branch, baseRef, args[0], args[1], args[2], patchPath, ghCompare)
		return err
	case "verify":
		if len(args) != 1 {
			return errors.New("usage: harness merge-default verify STATE_FILE")
		}
		return verifyMerge(".", args[0])
	}
	return fmt.Errorf("unknown merge-default command %q", phase)
}

// mergeBriefSection renders the brief's paragraph about the base-branch merge
// (issue #57). A missing file is the normal case: the run merged nothing, and
// the brief then says why, when the run recorded a reason beside it (issue
// #16) -- so CodeAF does not merge or copy the base branch by hand, which
// publish refuses as an unverifiable replay. A conflicted merge that cannot
// be read is an error, because CodeAF would otherwise work on files carrying
// markers it was never told about.
func mergeBriefSection(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := readMergeInfo(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return noMergeBriefSection(runStateFile(path))
		}
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## The base branch was merged into this branch")
	if info.Kind == mergeKindConflicted {
		fmt.Fprintf(&b, " — resolve the conflicts\n\n")
		fmt.Fprintf(&b, "HEAD is now a merge commit that brings the base branch tip %s into this branch. "+
			"The merge conflicts in these files, which currently carry conflict markers:\n\n", shortSHA(info.Onto))
		for _, f := range info.Files {
			// A backtick in a name would end the inline code span early and
			// let the rest of the name render as brief text (issue #34
			// security review). A backslash escape does not work inside a
			// code span, so such a name is listed without one instead, the
			// backticks escaped; the merge info itself keeps the real name.
			if strings.Contains(f, "`") {
				fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(f, "`", "\\`"))
			} else {
				fmt.Fprintf(&b, "- `%s`\n", f)
			}
		}
		b.WriteString("\nResolve every conflict in place: keep the intended code and delete every conflict marker line " +
			"(the lines starting with `<<<<<<<`, `=======` and `>>>>>>>`). The publish step refuses to push anything " +
			"while a conflict marker remains in any of these files. Your diff is measured against that merge commit, " +
			"so a file you leave untouched is not part of your patch — the markers must be resolved in the files themselves.\n")
	} else {
		fmt.Fprintf(&b, "\n\n")
		fmt.Fprintf(&b, "HEAD is now a merge commit that brings the base branch tip %s into this branch; the merge was clean, "+
			"so nothing conflicts. Your diff is measured against that merge commit: files the merge changed are part of the "+
			"branch now, not of your patch, unless you edit them.\n", shortSHA(info.Onto))
	}
	if info.Workflows {
		fmt.Fprintf(&b, "\nThe merge carries gated .github/ changes, so publishing this branch waits for a "+
			"maintainer's approval of the codeaf-harness environment; that is expected, and not something you act on.\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// noMergeBriefSection renders the brief's paragraph for a base merge the run
// job could not make (issue #16). The record is run-job output and untrusted:
// the reason is re-bounded here, and a missing or empty record (the base tip
// was already contained in the branch) says nothing.
func noMergeBriefSection(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	reason := ""
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "reason="); ok {
			reason = capUTF8(stripControl(strings.TrimSpace(v)), maxNoMergeReasonBytes)
		}
	}
	if reason == "" {
		return "", nil
	}
	return "## The base branch was not merged into this branch\n\n" +
		fmt.Sprintf("The run job could not merge the base branch into this branch (%s).\n\n", reason) +
		"Do not merge the base branch and do not copy its changes by hand: publish refuses a patch that replays " +
		"base-branch changes it cannot verify as a merge. Work on the branch as it is, make only this task's changes, " +
		"and say in your result that the pull request still needs to catch up with its base branch.\n", nil
}
