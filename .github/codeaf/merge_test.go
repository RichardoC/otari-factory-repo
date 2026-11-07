package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tests build real git repositories with plumbing (hash-object, mktree,
// commit-tree), so they never need a working tree to stage from, and they
// stand in for the compare API with git's own merge-base. Everything
// merge.go does in production — merge-tree with an explicit base, fetching a
// commit by id, commit-tree — is exercised against the real git binary.

// gitTest runs git in dir and fails the test when it exits nonzero.
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitTestIn(t, dir, "", args...)
}

// gitTestIn runs git with stdin, for plumbing that reads objects.
func gitTestIn(t *testing.T, dir, input string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(input)
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errBuf.String()))
	}
	return out.String()
}

// testRepo is a scratch repository whose commits are made with plumbing. The
// parent edges are recorded as commits are made, so the compare stub never
// needs git history (see dagCompare).
type testRepo struct {
	t     *testing.T
	dir   string
	edges map[string][]string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", "-b", "main", ".")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "test")
	return &testRepo{t: t, dir: dir, edges: map[string][]string{}}
}

func (r *testRepo) tree(files map[string]string) string {
	r.t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	for _, name := range names {
		blob := strings.TrimSpace(gitTestIn(r.t, r.dir, files[name], "hash-object", "-w", "--stdin"))
		buf.WriteString("100644 blob " + blob + "\t" + name + "\n")
	}
	return strings.TrimSpace(gitTestIn(r.t, r.dir, buf.String(), "mktree"))
}

func (r *testRepo) commitTree(tree string, parents []string, message string) string {
	r.t.Helper()
	args := []string{"commit-tree", tree}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	args = append(args, "-m", message)
	sha := strings.TrimSpace(gitTest(r.t, r.dir, args...))
	r.edges[sha] = append([]string(nil), parents...)
	return sha
}

// commit writes every file given (a full snapshot) and commits the tree.
func (r *testRepo) commit(message string, files map[string]string, parents ...string) string {
	r.t.Helper()
	return r.commitTree(r.tree(files), parents, message)
}

func (r *testRepo) setBranch(name, sha string) {
	r.t.Helper()
	gitTest(r.t, r.dir, "update-ref", "refs/heads/"+name, sha)
}

func (r *testRepo) onBranch(name string) {
	r.t.Helper()
	gitTest(r.t, r.dir, "symbolic-ref", "HEAD", "refs/heads/"+name)
}

func (r *testRepo) head() string {
	r.t.Helper()
	return strings.TrimSpace(gitTest(r.t, r.dir, "rev-parse", "HEAD"))
}

func (r *testRepo) parents(sha string) []string {
	r.t.Helper()
	line := strings.TrimSpace(gitTest(r.t, r.dir, "log", "--format=%P", "-1", sha))
	if line == "" {
		return nil
	}
	return strings.Fields(line)
}

func (r *testRepo) file(sha, path string) string {
	r.t.Helper()
	return gitTest(r.t, r.dir, "cat-file", "blob", sha+":"+path)
}

// withOrigin publishes the named branches to a bare origin, configures it to
// allow fetching commits by id (GitHub allows reachable ids; the tests fetch
// merge bases the same way), and points the work repo at it.
func (r *testRepo) withOrigin(branches ...string) string {
	r.t.Helper()
	origin := filepath.Join(r.t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		r.t.Fatal(err)
	}
	gitTest(r.t, origin, "init", "-q", "--bare", "-b", "main", ".")
	gitTest(r.t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	for _, b := range branches {
		gitTest(r.t, r.dir, "push", "-q", origin, "refs/heads/"+b+":refs/heads/"+b)
	}
	gitTest(r.t, r.dir, "remote", "add", "origin", origin)
	return origin
}

// publishCheckout mimics the publish job's checkout of the PR branch: a clone
// of the base branch with the PR branch fetched and checked out.
func publishCheckout(t *testing.T, origin, branch string) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "clone", "-q", origin, ".")
	gitTest(t, dir, "config", "user.email", "test@example.com")
	gitTest(t, dir, "config", "user.name", "test")
	gitTest(t, dir, "fetch", "-q", "origin", "refs/heads/"+branch)
	gitTest(t, dir, "checkout", "-q", "-B", branch, "FETCH_HEAD")
	return dir
}

// dagCompare stands in for the compare API: the same status (head relative to
// base) and merge base, computed from the DAG the test built. It must not use
// git for this: the production code fetches with --depth=1, which grafts the
// fetched branch shallow, and git merge-base would then find no common
// history — which is exactly why the production code takes the merge base
// from the API instead of walking local history.
func dagCompare(r *testRepo) compareFunc {
	return func(base, head string) (compareInfo, error) {
		if base == head {
			return compareInfo{Status: "identical", MergeBase: base}, nil
		}
		baseAnc := r.ancestors(base)
		if baseAnc[head] {
			return compareInfo{Status: "behind", MergeBase: head}, nil
		}
		headAnc := r.ancestors(head)
		if headAnc[base] {
			return compareInfo{Status: "ahead", MergeBase: base}, nil
		}
		var commons []string
		for sha := range baseAnc {
			if headAnc[sha] {
				commons = append(commons, sha)
			}
		}
		sort.Strings(commons)
		if len(commons) == 0 {
			return compareInfo{Status: "diverged"}, nil
		}
		return compareInfo{Status: "diverged", MergeBase: commons[0]}, nil
	}
}

// ancestors walks the DAG the test recorded, not git history.
func (r *testRepo) ancestors(sha string) map[string]bool {
	out := map[string]bool{}
	var walk func(string)
	walk = func(s string) {
		if out[s] {
			return
		}
		out[s] = true
		for _, p := range r.edges[s] {
			walk(p)
		}
	}
	walk(sha)
	return out
}

func failCompare() compareFunc {
	return func(base, head string) (compareInfo, error) {
		return compareInfo{}, errors.New("compare unavailable")
	}
}

// divergedConflict builds the issue #53 shape: two branches off one base that
// changed the same region of f.txt, with pr also changing g.txt and main also
// adding h.txt. divergedClean changes disjoint files instead.
func divergedConflict(t *testing.T) (*testRepo, string, string, string) {
	t.Helper()
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\n"})
	pr := r.commit("pr change", map[string]string{"f.txt": "a\nB\nc\n", "g.txt": "x\npr\n"}, base)
	mainTip := r.commit("main change", map[string]string{"f.txt": "a\nb\nC\n", "g.txt": "x\n", "h.txt": "new\n"}, base)
	r.setBranch("pr", pr)
	r.setBranch("main", mainTip)
	r.onBranch("pr")
	return r, base, pr, mainTip
}

func divergedClean(t *testing.T) (*testRepo, string, string, string) {
	t.Helper()
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\n"})
	pr := r.commit("pr change", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\npr\n"}, base)
	mainTip := r.commit("main change", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\n", "h.txt": "new\n"}, base)
	r.setBranch("pr", pr)
	r.setBranch("main", mainTip)
	r.onBranch("pr")
	return r, base, pr, mainTip
}

func artifactDir(t *testing.T) (baseFile, infoFile, stateFile string) {
	t.Helper()
	out := t.TempDir()
	return filepath.Join(out, "base-commit"), filepath.Join(out, "merge-info.json"), filepath.Join(out, "state.env")
}

// ---------------------------------------------------------------- merge-tree

func TestMergeTreeParsesConflicts(t *testing.T) {
	r, _, pr, mainTip := divergedConflict(t)
	mergeBase := strings.TrimSpace(gitTest(t, r.dir, "merge-base", pr, mainTip))
	tree, files, conflicted, err := mergeTree(r.dir, mergeBase, pr, mainTip)
	if err != nil || !conflicted {
		t.Fatalf("mergeTree: conflicted=%t err=%v", conflicted, err)
	}
	if !reflect.DeepEqual(files, []string{"f.txt"}) {
		t.Fatalf("conflicted files = %v, want [f.txt]", files)
	}
	if marked := r.file(tree, "f.txt"); !strings.Contains(marked, "<<<<<<<") || !strings.Contains(marked, ">>>>>>>") {
		t.Fatalf("merged file carries no markers: %q", marked)
	}
	// The same inputs must give the same tree: publish checks the run's
	// recorded tree against its own recomputation.
	tree2, _, _, err := mergeTree(r.dir, mergeBase, pr, mainTip)
	if err != nil || tree2 != tree {
		t.Fatalf("mergeTree is not deterministic: %s vs %s (%v)", tree, tree2, err)
	}
}

func TestMergeTreeCleanHasNoFiles(t *testing.T) {
	r, _, pr, mainTip := divergedClean(t)
	mergeBase := strings.TrimSpace(gitTest(t, r.dir, "merge-base", pr, mainTip))
	tree, files, conflicted, err := mergeTree(r.dir, mergeBase, pr, mainTip)
	if err != nil || conflicted || files != nil {
		t.Fatalf("mergeTree: conflicted=%t files=%v err=%v", conflicted, files, err)
	}
	if got := r.file(tree, "g.txt"); got != "x\npr\n" {
		t.Fatalf("merged g.txt = %q", got)
	}
	if got := r.file(tree, "h.txt"); got != "new\n" {
		t.Fatalf("merged h.txt = %q", got)
	}
}

// ---------------------------------------------------------------- the run job

// The run job commits the conflicted merge, makes it the base commit and
// records it, so the brief can name the files and publish can recreate the
// merge from the same two parents.
func TestRunMergeCommitsConflictedMergeAndRecordsIt(t *testing.T) {
	r, _, pr, mainTip := divergedConflict(t)
	r.withOrigin("pr", "main")
	baseFile, infoFile, _ := artifactDir(t)

	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	if got.State != mergeKindConflicted {
		t.Fatalf("state = %q, want conflicted", got.State)
	}
	head := r.head()
	if head == pr {
		t.Fatal("the branch did not move to a merge commit")
	}
	if parents := r.parents(head); !reflect.DeepEqual(parents, []string{pr, mainTip}) {
		t.Fatalf("merge parents = %v, want [%s %s]", parents, pr, mainTip)
	}
	// The checkout now shows the conflicted files, markers and all.
	marked, err := os.ReadFile(filepath.Join(r.dir, "f.txt"))
	if err != nil || !strings.Contains(string(marked), "<<<<<<<") {
		t.Fatalf("checked-out f.txt carries no markers: %q (%v)", marked, err)
	}
	// The base commit is the merge commit, and the merge info records the
	// merge for publish.
	if base, err := os.ReadFile(baseFile); err != nil || strings.TrimSpace(string(base)) != head {
		t.Fatalf("base-commit file = %q, want %s (%v)", base, head, err)
	}
	info, err := readMergeInfo(infoFile)
	if err != nil {
		t.Fatalf("merge info: %v", err)
	}
	if info.Kind != mergeKindConflicted || info.Commit != head || info.Base != pr || info.Onto != mainTip || info.Tree != got.Tree {
		t.Fatalf("merge info = %+v", info)
	}
	if !reflect.DeepEqual(info.Files, []string{"f.txt"}) {
		t.Fatalf("merge info files = %v, want [f.txt]", info.Files)
	}
}

// A diverged branch whose trees merge cleanly gets an ordinary merge commit:
// the branch then contains its base tip, and GitHub stops calling it dirty.
func TestRunMergeCommitsCleanMerge(t *testing.T) {
	r, _, pr, mainTip := divergedClean(t)
	r.withOrigin("pr", "main")
	baseFile, infoFile, _ := artifactDir(t)

	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	if got.State != mergeKindClean {
		t.Fatalf("state = %q, want clean", got.State)
	}
	head := r.head()
	if parents := r.parents(head); !reflect.DeepEqual(parents, []string{pr, mainTip}) {
		t.Fatalf("merge parents = %v, want [%s %s]", parents, pr, mainTip)
	}
	info, err := readMergeInfo(infoFile)
	if err != nil {
		t.Fatalf("merge info: %v", err)
	}
	if info.Kind != mergeKindClean || info.Files != nil {
		t.Fatalf("merge info = %+v", info)
	}
	if base, err := os.ReadFile(baseFile); err != nil || strings.TrimSpace(string(base)) != head {
		t.Fatalf("base-commit file = %q, want %s (%v)", base, head, err)
	}
}

// No merge is made when the branch already contains the base tip, when HEAD
// is not the branch the gate named, or when the compare API is unavailable. A
// branch strictly behind its base is not a skip any more (issue #171): the
// run commits the merge the same way it does for a diverged branch.
func TestRunMergeSkipsWhenNoMergeNeeded(t *testing.T) {
	baseFiles := map[string]string{"f.txt": "a\n"}
	r := newTestRepo(t)
	base := r.commit("base", baseFiles)
	ahead := r.commit("pr change", map[string]string{"f.txt": "a\npr\n"}, base)
	r.setBranch("main", base) // the base tip is older: the branch contains it
	r.setBranch("pr", ahead)
	r.onBranch("pr")
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, _ := artifactDir(t)

	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != "none" {
		t.Fatalf("contained base: state=%q err=%v", got.State, err)
	}
	if r.head() != ahead {
		t.Fatal("the branch moved without a merge")
	}
	if _, err := os.Stat(infoFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("merge info written for a skipped merge: %v", err)
	}

	// Strictly behind (issue #171): the run still lands the base as an
	// ordinary merge commit with both parents, and records it.
	r.setBranch("main", ahead)
	r.setBranch("pr", base)
	r.onBranch("pr")
	gitTest(t, r.dir, "push", "-qf", origin, "refs/heads/main:refs/heads/main")
	gitTest(t, r.dir, "push", "-qf", origin, "refs/heads/pr:refs/heads/pr")
	got, err = runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != mergeKindClean {
		t.Fatalf("behind branch: state=%q err=%v; want a clean merge", got.State, err)
	}
	head := strings.TrimSpace(gitTest(t, r.dir, "rev-parse", "HEAD"))
	if parents := strings.Fields(gitTest(t, r.dir, "log", "--format=%P", "-1", head)); len(parents) != 2 || parents[0] != base || parents[1] != ahead {
		t.Fatalf("behind merge parents = %v; want [%s %s]", parents, base, ahead)
	}
	if _, err := os.Stat(infoFile); err != nil {
		t.Fatalf("behind merge left no merge info: %v", err)
	}

	// A compare API failure degrades to no merge, never an error.
	got, err = runMerge(r.dir, "pr", "main", baseFile, infoFile, failCompare())
	if err != nil || got.State != "none" {
		t.Fatalf("compare failure: state=%q err=%v", got.State, err)
	}

	// HEAD not on the named branch: nothing is merged.
	r.onBranch("main")
	got, err = runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != "none" || r.head() != ahead {
		t.Fatalf("wrong branch: state=%q head=%s err=%v", got.State, r.head(), err)
	}
}

// ---------------------------------------------------------------- the brief

func TestMergeBriefSectionNamesConflictedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "merge-info.json")
	info := mergeInfo{
		Kind: mergeKindConflicted, Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40),
		Onto: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40),
		Files: []string{"app/main_test.go", "app/other.go"},
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	section, err := mergeBriefSection(path)
	if err != nil {
		t.Fatalf("mergeBriefSection: %v", err)
	}
	for _, want := range []string{"app/main_test.go", "app/other.go", "<<<<<<<", "resolve every conflict", "publish step refuses"} {
		if !strings.Contains(strings.ToLower(section), strings.ToLower(want)) {
			t.Fatalf("brief section lacks %q:\n%s", want, section)
		}
	}

	// A clean merge tells the run the base came in without conflicts.
	info.Kind = mergeKindClean
	info.Files = nil
	data, _ = json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	section, err = mergeBriefSection(path)
	if err != nil {
		t.Fatalf("mergeBriefSection: %v", err)
	}
	if strings.Contains(section, "<<<<<<<") || !strings.Contains(section, "merge commit") {
		t.Fatalf("clean section = %q", section)
	}

	// A merge carrying the base branch's workflow changes (issue #16) says
	// that publishing waits for a maintainer's approval.
	info.Workflows = true
	info.Kind = mergeKindClean
	data, _ = json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	section, err = mergeBriefSection(path)
	if err != nil {
		t.Fatalf("mergeBriefSection: %v", err)
	}
	if !strings.Contains(section, "maintainer") {
		t.Fatalf("workflows-carrying merge section lacks the approval note:\n%s", section)
	}
	info.Workflows = false
	data, _ = json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if section, err = mergeBriefSection(path); err != nil || strings.Contains(section, "maintainer") {
		t.Fatalf("plain merge section mentions approval: %q (%v)", section, err)
	}

	// No merge info is the normal case: an empty section, no error.
	if section, err := mergeBriefSection(filepath.Join(dir, "absent.json")); err != nil || section != "" {
		t.Fatalf("absent merge info: section=%q err=%v", section, err)
	}

	// A conflicted merge that cannot be read fails the brief: the run would
	// otherwise work on marked files it was never told about.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeBriefSection(path); err == nil {
		t.Fatal("an unreadable conflicted merge must fail the brief")
	}
}

// Issue #16: when the run job could not merge the base branch, the brief says
// so plainly and tells the agent not to merge or copy the base by hand --
// publish refuses an unverifiable replay of the base's changes (issue #171).
func TestMergeBriefSectionNoMergeReason(t *testing.T) {
	dir := t.TempDir()
	infoPath := filepath.Join(dir, "merge-info.json")
	runPath := runStateFile(infoPath)

	// No merge info and no run record: the brief says nothing, as before.
	if section, err := mergeBriefSection(infoPath); err != nil || section != "" {
		t.Fatalf("absent both: section=%q err=%v", section, err)
	}

	// A recorded no-merge reason: the brief names it, forbids a hand merge
	// and asks the run to report the PR still needs to catch up.
	if err := os.WriteFile(runPath, []byte("state=none\nreason=the base branch main could not be fetched: exit 128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	section, err := mergeBriefSection(infoPath)
	if err != nil {
		t.Fatalf("mergeBriefSection: %v", err)
	}
	for _, want := range []string{"not merged", "could not be fetched", "Do not merge", "catch up with its base branch"} {
		if !strings.Contains(section, want) {
			t.Fatalf("no-merge section lacks %q:\n%s", want, section)
		}
	}

	// An empty reason (the base was already contained) says nothing.
	if err := os.WriteFile(runPath, []byte("state=none\nreason=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if section, err := mergeBriefSection(infoPath); err != nil || section != "" {
		t.Fatalf("empty reason: section=%q err=%v", section, err)
	}

	// The record is run-job output, so untrusted: a reason is re-bounded and
	// stripped of control characters, whatever the file says.
	big := strings.Repeat("x", 500) + "\nnewline\n"
	if err := os.WriteFile(runPath, []byte("state=none\nreason="+big+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	section, err = mergeBriefSection(infoPath)
	if err != nil {
		t.Fatalf("mergeBriefSection: %v", err)
	}
	if i := strings.Index(section, "("); i >= 0 {
		if j := strings.Index(section[i:], ")"); j >= 0 {
			reason := section[i+1 : i+j]
			if len(reason) > 300 {
				t.Fatalf("reason not bounded: %d characters", len(reason))
			}
			if strings.ContainsFunc(reason, func(r rune) bool { return r < 32 || r == 127 }) {
				t.Fatalf("reason carries raw control characters: %q", reason)
			}
		} else {
			t.Fatalf("no closing paren for the reason: %q", section)
		}
	} else {
		t.Fatal("no reason in the section:\n" + section)
	}
}

// Issue #16: the run job's packaging step reads the workflows field through
// the harness binary. A missing file reads false; an invalid one is an error,
// never a silent false; a recorded one routes publish into the
// approval-gated environment.
func TestMergeWorkflowsRouting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "merge-info.json")

	if routed, err := mergeWorkflowsRouting(filepath.Join(dir, "absent.json")); err != nil || routed {
		t.Fatalf("absent merge info: routed=%t err=%v", routed, err)
	}

	info := mergeInfo{Kind: mergeKindClean, Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40),
		Onto: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40)}
	data, _ := json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if routed, err := mergeWorkflowsRouting(path); err != nil || routed {
		t.Fatalf("plain merge info: routed=%t err=%v", routed, err)
	}

	info.Workflows = true
	data, _ = json.Marshal(info)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if routed, err := mergeWorkflowsRouting(path); err != nil || !routed {
		t.Fatalf("workflows merge info: routed=%t err=%v", routed, err)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeWorkflowsRouting(path); err == nil {
		t.Fatal("an invalid merge info must error, not read false")
	}
}

// ---------------------------------------------------------------- publish

// Publish recreates the run's conflicted merge from the same two parents: the
// same tree (checked by hash), a merge commit whose parents are the PR head
// it fetched and the recorded base tip.
func TestPublishRecreatesConflictedMerge(t *testing.T) {
	r, _, pr, mainTip := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	// Publish starts from a fresh checkout of the PR branch at the pushed
	// head; the run's merge commit was never pushed.
	pub := publishCheckout(t, origin, "pr")
	if pubHead := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD")); pubHead != pr {
		t.Fatalf("publish checkout at %s, want the PR head %s", pubHead, pr)
	}

	got, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	if got.State != mergeKindConflicted {
		t.Fatalf("state = %q, want conflicted", got.State)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := gitTest(t, pub, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("recreated merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
	if marked, err := os.ReadFile(filepath.Join(pub, "f.txt")); err != nil || !strings.Contains(string(marked), "<<<<<<<") {
		t.Fatalf("recreated f.txt carries no markers: %q (%v)", marked, err)
	}
	// The state file tells the workflow to verify the markers afterwards.
	state, err := os.ReadFile(stateFile)
	if err != nil || string(state) != "state=conflicted\nmerged=true\n" {
		t.Fatalf("merge state = %q (%v)", state, err)
	}
	list, err := os.ReadFile(stateFile + ".files")
	if err != nil || string(list) != "f.txt\n" {
		t.Fatalf("conflicted file list = %q (%v)", list, err)
	}
}

// main moving on between the run and publish is the common case on a busy
// queue: the recorded base tip is then an ancestor of main (compare says
// "ahead"), and publish still recreates the run's merge from the same parents.
func TestPublishRecreatesMergeAfterMainMoved(t *testing.T) {
	r, _, pr, mainTip := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	later := r.commit("later main change", map[string]string{"f.txt": "a\nb\nC\n", "g.txt": "x\n", "h.txt": "new\n", "i.txt": "later\n"}, mainTip)
	r.setBranch("main", later)
	gitTest(t, r.dir, "push", "-q", origin, "refs/heads/main:refs/heads/main")

	pub := publishCheckout(t, origin, "pr")
	got, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge after main moved: %v", err)
	}
	if got.State != mergeKindConflicted {
		t.Fatalf("state = %q, want conflicted", got.State)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := gitTest(t, pub, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("recreated merge parents = %s, want the recorded [%s %s]", parents, pr, mainTip)
	}
}

// A behind-but-clean PR gets a merge commit even when the run merged nothing:
// publish merges the base tip first, then the patch applies on top.
func TestPublishMergesFirstWhenClean(t *testing.T) {
	r, base, pr, mainTip := divergedClean(t)
	origin := r.withOrigin("pr", "main")
	baseFile, _, stateFile := artifactDir(t)
	if err := os.WriteFile(baseFile, []byte(pr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pub := publishCheckout(t, origin, "pr")
	got, err := publishMerge(pub, "pr", "main", baseFile, filepath.Join(t.TempDir(), "absent.json"), stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	if got.State != mergeKindClean {
		t.Fatalf("state = %q, want clean", got.State)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := gitTest(t, pub, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
	_ = base
}

// Issue #171: a PR branch behind main by two commits, plus a run patch
// touching one file, publishes as a merge commit with main's tip as second
// parent and then one work commit. Never a fast-forward: a single-parent
// commit leaves main's changes indistinguishable from the branch's own, and
// the PR's diff replays them.
func TestPublishPRModeKeepsMergeCommit(t *testing.T) {
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"g.txt": "x\n"})
	c1 := r.commit("main 1", map[string]string{"g.txt": "x\n", "main1.txt": "one\n"}, base)
	c2 := r.commit("main 2", map[string]string{"g.txt": "x\n", "main2.txt": "two\n"}, c1)
	r.setBranch("main", c2)
	r.setBranch("pr", base) // the branch is strictly behind main
	r.onBranch("pr")
	// The pushed branch never carries the run's merge commit: publish it
	// before the run makes one.
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	// The run job: the deterministic merge brings main in as an ordinary
	// merge commit, records it, and makes it the base commit.
	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != mergeKindClean {
		t.Fatalf("runMerge: state=%q err=%v; want a clean merge", got.State, err)
	}
	info, err := readMergeInfo(infoFile)
	if err != nil {
		t.Fatalf("the run left no verifiable merge info: %v", err)
	}
	if parents := r.parents(info.Commit); len(parents) != 2 || parents[0] != base || parents[1] != c2 {
		t.Fatalf("run merge parents = %v; want [%s %s]", parents, base, c2)
	}
	if recorded, err := os.ReadFile(baseFile); err != nil || strings.TrimSpace(string(recorded)) != info.Commit {
		t.Fatalf("base commit file = %q (%v); want the merge commit %s", recorded, err, info.Commit)
	}

	// Publish starts from a fresh checkout of the pushed branch head.
	pub := publishCheckout(t, origin, "pr")
	if _, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r)); err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := strings.Fields(gitTest(t, pub, "log", "--format=%P", "-1", head)); len(parents) != 2 || parents[0] != base || parents[1] != c2 {
		t.Fatalf("published merge parents = %v; want [%s %s] -- main's tip as second parent", parents, base, c2)
	}

	// The agent's own patch touches one file; the workflow applies it on the
	// merge commit and commits it as the work commit.
	workBlob := strings.TrimSpace(gitTestIn(t, pub, "agent\n", "hash-object", "-w", "--stdin"))
	mergeTree := strings.TrimSpace(gitTest(t, pub, "rev-parse", head+"^{tree}"))
	entries := gitTest(t, pub, "ls-tree", mergeTree) + "100644 blob " + workBlob + "\twork.txt\n"
	workTree := strings.TrimSpace(gitTestIn(t, pub, entries, "mktree"))
	work := strings.TrimSpace(gitTest(t, pub, "commit-tree", workTree, "-p", head, "-m", "the agent's work"))
	patchFile := filepath.Join(t.TempDir(), "changes.patch")
	if err := os.WriteFile(patchFile, []byte(gitTest(t, pub, "diff", head, work)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, pub, "reset", "-q", "--hard", head)
	gitTest(t, pub, "apply", "--index", "--binary", patchFile)
	gitTest(t, pub, "commit", "-q", "-m", "codeaf: work for #171")

	// git log --graph shows the work commit over the merge commit, and the
	// PR's file list is only the agent's own file.
	workCommit := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := strings.Fields(gitTest(t, pub, "log", "--format=%P", "-1", workCommit)); len(parents) != 1 || parents[0] != head {
		t.Fatalf("work commit parents = %v; want the merge commit alone", parents)
	}
	if files := gitTest(t, pub, "diff", "main...HEAD", "--name-only"); files != "work.txt\n" {
		t.Fatalf("git diff main...HEAD = %q; want only work.txt", files)
	}
	if state, err := os.ReadFile(stateFile); err != nil || string(state) != "state=clean\nmerged=true\n" {
		t.Fatalf("merge state = %q (%v)", state, err)
	}
}

// Without merge info, a patch made against a branch head that is not the one
// publish fetched is still refused as stale.
func TestPublishRefusesStalePatch(t *testing.T) {
	r, base, pr, _ := divergedClean(t)
	origin := r.withOrigin("pr", "main")
	baseFile, _, stateFile := artifactDir(t)
	if err := os.WriteFile(baseFile, []byte(base+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pub := publishCheckout(t, origin, "pr")
	_, err := publishMerge(pub, "pr", "main", baseFile, filepath.Join(t.TempDir(), "absent.json"), stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("publishMerge err = %v, want a stale refusal", err)
	}
	_ = pr
}

// Publish refuses a recorded merge whose recomputed tree does not match the
// tree the run recorded: the patch's base is then unknown.
func TestPublishRefusesTreeMismatch(t *testing.T) {
	r, _, _, _ := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)
	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	// Tamper with the recorded tree, as a run whose artifacts disagree would.
	info, err := readMergeInfo(infoFile)
	if err != nil {
		t.Fatal(err)
	}
	info.Tree = r.tree(map[string]string{"f.txt": "tampered\n"})
	data, _ := json.Marshal(info)
	if err := os.WriteFile(infoFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	pub := publishCheckout(t, origin, "pr")
	_, err = publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("publishMerge err = %v, want a tree mismatch refusal", err)
	}
}

// Publish refuses a recorded merge naming a different PR head than the one it
// fetched, or a base commit that is not the merge commit the info names.
func TestPublishRefusesMergeInfoMismatch(t *testing.T) {
	r, base, pr, _ := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)
	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	// The base commit artifact disagrees with the merge commit the info
	// records.
	pub := publishCheckout(t, origin, "pr")
	if err := os.WriteFile(baseFile, []byte(base+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "base commit") {
		t.Fatalf("base commit mismatch err = %v, want a refusal", err)
	}

	// A branch that moved between the run and publish: the recorded base is
	// not the fetched head.
	r.setBranch("pr", r.commit("moved", map[string]string{"f.txt": "moved\n"}, pr))
	gitTest(t, r.dir, "push", "-qf", origin, "refs/heads/pr:refs/heads/pr")
	pub2 := publishCheckout(t, origin, "pr")
	_, err = publishMerge(pub2, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "another commit") {
		t.Fatalf("stale merge info err = %v, want a base mismatch refusal", err)
	}
}

// Publish refuses a recorded merge whose base tip is not a commit the base
// branch contains: anything else could smuggle content in as "the base".
func TestPublishRefusesForeignBaseTip(t *testing.T) {
	r, base, pr, _ := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	// A commit on a third branch, which main does not contain.
	stranger := r.commit("stranger work", map[string]string{"evil.txt": "evil\n"}, base)
	r.setBranch("stranger", stranger)
	gitTest(t, r.dir, "push", "-q", origin, "refs/heads/stranger:refs/heads/stranger")
	baseFile, _, stateFile := artifactDir(t)
	if err := os.WriteFile(baseFile, []byte(pr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Commit names the value of the base-commit file (the PR head), so the
	// fixture gets past the coherence checks and reaches the one this test
	// is about: a base tip the base branch does not contain.
	foreign := mergeInfo{
		Kind: mergeKindClean, Commit: pr,
		Base: pr, Onto: stranger, Tree: r.tree(map[string]string{"evil.txt": "evil\n"}),
	}
	data, _ := json.Marshal(foreign)
	infoFile := filepath.Join(t.TempDir(), "merge-info.json")
	if err := os.WriteFile(infoFile, data, 0o644); err != nil {
		t.Fatal(err)
	}

	pub := publishCheckout(t, origin, "pr")
	_, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "not contained") {
		t.Fatalf("publishMerge err = %v, want a foreign base tip refusal", err)
	}
}

// A conflict the run never saw (its merge info never existed) is not pushed:
// publish keeps the patch alone rather than publishing marked files.
func TestPublishKeepsPatchWhenMergeConflicts(t *testing.T) {
	r, _, pr, _ := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, _, stateFile := artifactDir(t)
	if err := os.WriteFile(baseFile, []byte(pr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pub := publishCheckout(t, origin, "pr")
	got, err := publishMerge(pub, "pr", "main", baseFile, filepath.Join(t.TempDir(), "absent.json"), stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	if got.State != "none" || got.Commit != "" {
		t.Fatalf("state = %q commit = %q, want no merge", got.State, got.Commit)
	}
	if head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD")); head != pr {
		t.Fatalf("HEAD = %s, want the untouched PR head %s", head, pr)
	}
}

// ---------------------------------------------------------------- verify

func TestVerifyRefusesLeftoverMarkers(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.env")
	marked := "<<<<<<< pr\npr side\n=======\nmain side\n>>>>>>> main\n"
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(marked), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, []byte("state=conflicted\nmerged=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile+".files", []byte("f.txt\nresolved.txt\ndeleted.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The patch was supposed to remove the markers: publish refuses.
	err := verifyMerge(dir, stateFile)
	if err == nil || !strings.Contains(err.Error(), "conflict marker") {
		t.Fatalf("verifyMerge err = %v, want a marker refusal", err)
	}

	// A fully resolved patch publishes: resolved files pass, and a conflicted
	// file the patch deleted counts as resolved.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("pr side\nmain side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "resolved.txt"), []byte("settled\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMerge(dir, stateFile); err != nil {
		t.Fatalf("verifyMerge on a resolved patch: %v", err)
	}

	// A clean or fast-forward merge has nothing to verify.
	if err := os.WriteFile(stateFile, []byte("state=clean\nmerged=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(marked), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyMerge(dir, stateFile); err != nil {
		t.Fatalf("verifyMerge on a clean merge: %v", err)
	}
}

func TestFindConflictMarker(t *testing.T) {
	cases := map[string]bool{
		"<<<<<<< HEAD\n":      true,
		">>>>>>> main\n":      true,
		"// <<<<<<< note\n":   false, // not at the start of the line
		"text\n=======\n":     false,
		"plain content\n":     false,
		"<<<<\n":              false, // too short to be a marker
		"a\n\n>>>>>>>  tag\n": true,
	}
	for text, want := range cases {
		if _, got := findConflictMarker([]byte(text)); got != want {
			t.Errorf("findConflictMarker(%q) = %t, want %t", text, got, want)
		}
	}
}

// ---------------------------------------------------------------- validation

func TestReadMergeInfoValidation(t *testing.T) {
	dir := t.TempDir()
	good := mergeInfo{
		Kind: mergeKindConflicted, Commit: strings.Repeat("a", 40), Base: strings.Repeat("b", 40),
		Onto: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40), Files: []string{"f.txt"},
	}
	data, _ := json.Marshal(good)
	path := filepath.Join(dir, "merge-info.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := readMergeInfo(path); err != nil || info.Kind != mergeKindConflicted {
		t.Fatalf("readMergeInfo: %+v %v", info, err)
	}
	// The workflows field (issue #16) is a bounded boolean like the rest.
	good.Workflows = true
	data, _ = json.Marshal(good)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := readMergeInfo(path); err != nil || !info.Workflows {
		t.Fatalf("readMergeInfo: %+v %v; want workflows: true read back", info, err)
	}
	good.Workflows = false
	data, _ = json.Marshal(good)

	cases := map[string]mergeInfo{
		"unknown kind":  {Kind: "weird", Commit: good.Commit, Base: good.Base, Onto: good.Onto, Tree: good.Tree},
		"short sha":     {Kind: mergeKindClean, Commit: "abc", Base: good.Base, Onto: good.Onto, Tree: good.Tree},
		"uppercase sha": {Kind: mergeKindClean, Commit: strings.Repeat("A", 40), Base: good.Base, Onto: good.Onto, Tree: good.Tree},
		"path escape":   {Kind: mergeKindConflicted, Commit: good.Commit, Base: good.Base, Onto: good.Onto, Tree: good.Tree, Files: []string{"../f.txt"}},
		"absolute path": {Kind: mergeKindConflicted, Commit: good.Commit, Base: good.Base, Onto: good.Onto, Tree: good.Tree, Files: []string{"/etc/passwd"}},
		"newline path":  {Kind: mergeKindConflicted, Commit: good.Commit, Base: good.Base, Onto: good.Onto, Tree: good.Tree, Files: []string{"a\nb"}},
	}
	for name, info := range cases {
		data, _ := json.Marshal(info)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readMergeInfo(path); err == nil {
			t.Errorf("readMergeInfo accepted %s", name)
		}
	}
	// A path list over the cap is refused too.
	good.Files = make([]string, 201)
	for i := range good.Files {
		good.Files[i] = "f" + strconv.Itoa(i) + ".txt"
	}
	data, _ = json.Marshal(good)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readMergeInfo(path); err == nil {
		t.Error("readMergeInfo accepted an oversized path list")
	}
	// A missing file is a missing file, not a validation failure: publish
	// tells that case apart.
	if _, err := readMergeInfo(filepath.Join(dir, "absent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readMergeInfo on an absent file = %v", err)
	}
}

func TestMergeStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "state.env")
	if err := writeMergeState(stateFile, mergeOutcome{State: "none"}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(stateFile); err != nil || string(data) != "state=none\nmerged=false\n" {
		t.Fatalf("state = %q (%v)", data, err)
	}
	if _, err := os.Stat(stateFile + ".files"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean merge wrote a file list: %v", err)
	}
	files := []string{"one.txt", "two.txt"}
	if err := writeMergeState(stateFile, mergeOutcome{State: mergeKindConflicted, Files: files}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(stateFile); err != nil || string(data) != "state=conflicted\nmerged=true\n" {
		t.Fatalf("state = %q (%v)", data, err)
	}
	list, err := os.ReadFile(stateFile + ".files")
	if err != nil || string(list) != "one.txt\ntwo.txt\n" {
		t.Fatalf("file list = %q (%v)", list, err)
	}
}

// The full issue #57 round trip: the run commits the conflicted merge and
// CodeAF resolves it, publish recreates the same merge from the same two
// parents, the patch applies on it and removes the markers, and the marker
// check passes — so the branch ends up containing its base with the conflict
// resolved, pushed as a plain descendant of the PR head.
func TestConflictedMergeRoundTrip(t *testing.T) {
	r, _, pr, mainTip := divergedConflict(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	// The run job: conflicted merge committed, base commit rewritten.
	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != mergeKindConflicted {
		t.Fatalf("runMerge: state=%q err=%v", got.State, err)
	}

	// CodeAF resolves the conflict and the package step stages and exports
	// the patch (diff against the merge commit the run made).
	resolved := "a\nB\nC\n"
	if err := os.WriteFile(filepath.Join(r.dir, "f.txt"), []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTest(t, r.dir, "add", "f.txt")
	patch := strings.TrimSpace(gitTest(t, r.dir, "diff", "--cached", "--binary"))
	if !strings.Contains(patch, "b/main") && !strings.Contains(patch, "f.txt") {
		t.Fatalf("patch does not touch f.txt:\n%s", patch)
	}
	in := t.TempDir()
	patchFile := filepath.Join(in, "changes.patch")
	if err := os.WriteFile(patchFile, []byte(patch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Publish, from a fresh checkout still at the pushed PR head.
	pub := publishCheckout(t, origin, "pr")
	out, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	if out.State != mergeKindConflicted {
		t.Fatalf("publish state = %q, want conflicted", out.State)
	}
	// The patch applies strictly on the recreated merge (its tree is the
	// one the patch was diffed against) and removes the markers.
	gitTest(t, pub, "apply", "--index", "--binary", patchFile)
	if staged := gitTest(t, pub, "cat-file", "blob", ":f.txt"); staged != resolved {
		t.Fatalf("staged f.txt = %q, want the resolution %q", staged, resolved)
	}
	if err := verifyMerge(pub, stateFile); err != nil {
		t.Fatalf("verifyMerge after the resolution: %v", err)
	}
	// Sanity: the recreated merge's parents are the fetched head and base tip.
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	parents := gitTest(t, pub, "log", "--format=%P", "-1", head)
	if fields := strings.Fields(parents); fields[0] != pr || fields[1] != mainTip {
		t.Fatalf("recreated merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
}

// A base that changed .github/workflows/ since the branch point is never
// brought in: GitHub refuses a GITHUB_TOKEN push that carries workflow
// changes, so the run proceeds on the bare head and publish pushes the patch
// alone, as before #57.
// divergedWorkflow builds the issue #16 shape: two branches off one base that
// conflict in f.txt, where main also changed .github/workflows/ci.yml since
// the branch point. workflowOnlyMain changes disjoint files instead, so the
// merge is clean, and prWorkflowEdit has the PR branch edit a workflow file
// of its own (which must travel in the patch, never in the merge).
func divergedWorkflow(t *testing.T) (*testRepo, string, string, string) {
	t.Helper()
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\n"})
	pr := r.commit("pr change", map[string]string{"f.txt": "a\nB\nc\n", "g.txt": "x\npr\n"}, base)
	mainTip := r.commitTree(r.indexTree(map[string]string{
		"f.txt":                    "a\nb\nC\n",
		"g.txt":                    "x\n",
		".github/workflows/ci.yml": "on: push\n",
	}), []string{base}, "main changes a workflow and f.txt")
	r.setBranch("pr", pr)
	r.setBranch("main", mainTip)
	r.onBranch("pr")
	return r, base, pr, mainTip
}

// Issue #16, run side: the base branch changed .github/workflows/ and
// conflicts with the branch. The merge is made anyway -- a conflicted merge
// commit -- and recorded with "workflows": true, instead of the old skip that
// left #10 unable to catch up with main.
func TestRunMergeCarriesBaseWorkflowChanges(t *testing.T) {
	r, _, pr, mainTip := divergedWorkflow(t)
	// The merge base is fetched from origin, one level deep, as in production.
	origin := r.withOrigin("pr", "main")
	_ = origin
	baseFile, infoFile, stateFile := artifactDir(t)

	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	if got.State != mergeKindConflicted {
		t.Fatalf("state = %q, want a conflicted merge despite the workflow change", got.State)
	}
	head := r.head()
	if parents := gitTest(t, r.dir, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
	if base, err := os.ReadFile(baseFile); err != nil || strings.TrimSpace(string(base)) != head {
		t.Fatalf("base commit file = %q, %v; want the merge commit %s", base, err, head)
	}
	info, err := readMergeInfo(infoFile)
	if err != nil {
		t.Fatalf("readMergeInfo: %v", err)
	}
	if !info.Workflows {
		t.Fatal("merge info does not record workflows: true")
	}
	if info.Kind != mergeKindConflicted || !strings.Contains(strings.Join(info.Files, ","), "f.txt") {
		t.Fatalf("merge info = %+v; want kind conflicted conflicting in f.txt", info)
	}
	if info.Tree != got.Tree {
		t.Fatalf("recorded tree %s, want %s", info.Tree, got.Tree)
	}
	_ = stateFile
}

// Issue #16, publish side, approved: with the run's merge info and
// HARNESS_APPROVED=true, publish recreates the merge -- the tree hash it
// computes matches the recorded one, and the merge's .github/workflows/
// equals the base branch's byte-for-byte.
func TestPublishApprovedWorkflowsMerge(t *testing.T) {
	r, _, pr, mainTip := divergedWorkflow(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	info, err := readMergeInfo(infoFile)
	if err != nil || !info.Workflows {
		t.Fatalf("readMergeInfo = %+v, %v; want workflows recorded", info, err)
	}
	pub := publishCheckout(t, origin, "pr")
	t.Setenv("HARNESS_APPROVED", "true")
	got, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil {
		t.Fatalf("publishMerge: %v", err)
	}
	if got.State != mergeKindConflicted || got.Tree != info.Tree {
		t.Fatalf("state = %q tree %s; want a conflicted merge with tree %s verified", got.State, got.Tree, info.Tree)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := gitTest(t, pub, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("recreated merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
	if wf := gitTest(t, pub, "cat-file", "blob", head+":.github/workflows/ci.yml"); wf != "on: push\n" {
		t.Fatalf("merge ci.yml = %q, want the base branch's", wf)
	}
	if wf, err := workflowChanges(pub, head, mainTip); err != nil || len(wf) != 0 {
		t.Fatalf("workflowChanges(merge, base) = %q, %v; want none", wf, err)
	}
}

// Issue #16: without a maintainer's approval, a recorded merge carrying
// workflow changes is refused with an error naming the approval -- never
// published as the patch alone, which is how #10 got stuck.
func TestPublishRefusesUnapprovedWorkflowsMerge(t *testing.T) {
	r, _, pr, _ := divergedWorkflow(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	pub := publishCheckout(t, origin, "pr")
	t.Setenv("HARNESS_APPROVED", "false")
	if _, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r)); err == nil || !strings.Contains(err.Error(), "maintainer") {
		t.Fatalf("publishMerge = %v; want an error naming the maintainer approval", err)
	}
	if head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD")); head != pr {
		t.Fatalf("publish head = %s, want the untouched branch %s", head, pr)
	}
}

// Issue #16: even approved, a recreated merge whose workflow files differ
// from the base branch's is refused -- the merge brings in only what the base
// branch already has, and the branch's own workflow edits travel in the
// patch, where the approval check and the workflow policy see them.
func TestPublishRefusesWorkflowMismatch(t *testing.T) {
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\nb\nc\n", "g.txt": "x\n"})
	pr := r.commitTree(r.indexTree(map[string]string{
		"f.txt":                    "a\nb\nc\n",
		"g.txt":                    "x\npr\n",
		".github/workflows/pr.yml": "on: pull_request\n",
	}), []string{base}, "pr edits its own workflow")
	mainTip := r.commitTree(r.indexTree(map[string]string{
		"f.txt":                    "a\nB\nc\n",
		"g.txt":                    "x\n",
		".github/workflows/ci.yml": "on: push\n",
	}), []string{base}, "main changes f.txt and a workflow")
	r.setBranch("pr", pr)
	r.setBranch("main", mainTip)
	r.onBranch("pr")
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	got, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r))
	if err != nil || got.State != mergeKindClean {
		t.Fatalf("runMerge = %+v, %v; want a clean merge", got, err)
	}
	pub := publishCheckout(t, origin, "pr")
	t.Setenv("HARNESS_APPROVED", "true")
	_, err = publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "workflow files differ") {
		t.Fatalf("publishMerge = %v; want a refusal of the workflow mismatch", err)
	}
}

// Issue #16, the fallback publish (no merge info): without approval the base
// is still not brought in when it changed workflows -- the patch alone is
// pushed, as before. With approval, the merge goes through.
func TestPublishWithoutMergeInfoWorkflowsMerge(t *testing.T) {
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\n", "g.txt": "x\n"})
	pr := r.commit("pr change", map[string]string{"f.txt": "a\n", "g.txt": "x\npr\n"}, base)
	mainTip := r.commitTree(r.indexTree(map[string]string{
		"f.txt":                    "a\n",
		"g.txt":                    "x\n",
		"h.txt":                    "new\n",
		".github/workflows/ci.yml": "on: push\n",
	}), []string{base}, "main changes a workflow")
	r.setBranch("pr", pr)
	r.setBranch("main", mainTip)
	r.onBranch("pr")
	origin := r.withOrigin("pr", "main")

	baseFile, _, stateFile := artifactDir(t)
	if err := os.WriteFile(baseFile, []byte(pr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	infoFile := filepath.Join(t.TempDir(), "absent.json")

	// Without approval: the patch alone, the branch untouched.
	pub := publishCheckout(t, origin, "pr")
	got, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil || got.State != "none" {
		t.Fatalf("publishMerge = %+v, %v; want no merge without approval", got, err)
	}
	if head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD")); head != pr {
		t.Fatalf("publish head = %s, want the untouched branch %s", head, pr)
	}

	// With approval: the base comes in as a two-parent merge commit.
	t.Setenv("HARNESS_APPROVED", "true")
	got, err = publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err != nil || got.State != mergeKindClean {
		t.Fatalf("publishMerge (approved) = %+v, %v; want a clean merge", got, err)
	}
	head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
	if parents := gitTest(t, pub, "log", "--format=%P", "-1", head); strings.Fields(parents)[0] != pr || strings.Fields(parents)[1] != mainTip {
		t.Fatalf("merge parents = %s, want [%s %s]", parents, pr, mainTip)
	}
	if wf := gitTest(t, pub, "cat-file", "blob", head+":.github/workflows/ci.yml"); wf != "on: push\n" {
		t.Fatalf("merge ci.yml = %q, want the base branch's", wf)
	}
}

// indexTree writes a full snapshot through a scratch index. mktree cannot
// name a nested path, so a tree that carries .github/workflows/ files is
// built this way; a space or a non-ASCII byte in a name goes through too,
// and a non-UTF-8 name would be pushed into git unvalidated either way.
func (r *testRepo) indexTree(files map[string]string) string {
	r.t.Helper()
	blob := func(content string) string {
		return strings.TrimSpace(gitTestIn(r.t, r.dir, content, "hash-object", "-w", "--stdin"))
	}
	index := filepath.Join(r.t.TempDir(), "index")
	env := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
		out, err := cmd.CombinedOutput()
		if err != nil {
			r.t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env("update-index", "--add", "--cacheinfo", "100644,"+blob(files[name])+","+name)
	}
	return env("write-tree")
}

// Issue #343: workflowChanges read git's --name-only list with Fields, so a
// workflow path containing a space was reported as two files that do not
// exist, and merge decisions landed on the wrong path set. It pins the fix:
// the list is split on lines, as boundedNames reads it, and the C-style
// quoting git applies to a non-ASCII name under core.quotePath (on by
// default) is stripped, so the change is reported as exactly the two real
// paths.
func TestWorkflowChangesSplitsNamesOnLines(t *testing.T) {
	r := newTestRepo(t)
	base := r.commit("base", map[string]string{"f.txt": "a\n"})
	changed := r.commitTree(r.indexTree(map[string]string{
		"f.txt":                        "a\n",
		".github/workflows/a b.yml":    "on: push\n",
		".github/workflows/\u00e9.yml": "on: push\n",
	}), []string{base}, "changes workflows with awkward names")
	files, err := workflowChanges(r.dir, base, changed)
	if err != nil {
		t.Fatalf("workflowChanges: %v", err)
	}
	sort.Strings(files)
	want := []string{".github/workflows/a b.yml", ".github/workflows/\u00e9.yml"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("workflowChanges = %q; want exactly %q", files, want)
	}
}

// Issue #171: a tampered or missing merge-info.json, with the run's recorded
// base merge present in the base-commit artifact, is refused -- never
// published as a single commit whose parents cannot show what came from the
// base.
func TestPublishPRModeRefusesUnverifiableMerge(t *testing.T) {
	r, _, pr, _ := divergedClean(t)
	origin := r.withOrigin("pr", "main")
	baseFile, infoFile, stateFile := artifactDir(t)

	// The run made its base merge and rewrote the base-commit artifact to the
	// merge commit; the merge info is what publish verifies it against. The
	// branch head publish fetched never carried the merge.
	if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
		t.Fatalf("runMerge: %v", err)
	}
	recorded, err := os.ReadFile(baseFile)
	if err != nil {
		t.Fatal(err)
	}
	baseCommit := strings.TrimSpace(string(recorded))
	if baseCommit == pr {
		t.Fatalf("base commit %s is the branch head; the run recorded no base merge", baseCommit)
	}

	// Tampered: the info no longer validates as the run's record.
	if err := os.WriteFile(infoFile, []byte(`{"kind":"squashed"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pub := publishCheckout(t, origin, "pr")
	_, err = publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("publishMerge err = %v; want a tampered merge info refusal", err)
	}
	if head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD")); head != pr {
		t.Fatalf("publish head = %s; want the untouched branch %s", head, pr)
	}

	// Missing: the base merge is present in the base-commit artifact, but the
	// info that verifies it is gone. Publish refuses with the reason, rather
	// than pushing a single commit.
	if err := os.Remove(infoFile); err != nil {
		t.Fatal(err)
	}
	pub2 := publishCheckout(t, origin, "pr")
	_, err = publishMerge(pub2, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r))
	if err == nil || !strings.Contains(err.Error(), "no verifiable merge info") {
		t.Fatalf("publishMerge err = %v; want a missing merge info refusal", err)
	}
	if head := strings.TrimSpace(gitTest(t, pub2, "rev-parse", "HEAD")); head != pr {
		t.Fatalf("publish head = %s; want the untouched branch %s", head, pr)
	}
}

// The regression from run 39094290325 (issue #171): the run's gate left no
// merge info (its base carried a workflow edit, as #162's did), and the patch
// replayed the base's changes -- #161/#164's snyk-linear-sync code, #162's
// workflow edit and #166's queue gate -- beside the agent's own fix. Before
// the fix, publish kept the patch alone and the workflow applied it as one
// commit on the old head: the squash. After it, publish refuses the replay,
// and a re-run whose gate passes records the merge, which publish recreates
// as a merge commit.
func TestRegressionSquashFromRun39094290325(t *testing.T) {
	// Builds the run's shape through an index, because mktree takes no nested
	// paths and main's gain includes #162's workflow edit.
	build := func(t *testing.T, withWorkflow bool) (*testRepo, string, string, string) {
		r := newTestRepo(t)
		base := r.commit("base", map[string]string{"f.txt": "a\nb\nc\n"})
		pr := r.commit("pr fix", map[string]string{"f.txt": "a\nb\nc\n", "fix.go": "package fix\n"}, base)
		index := filepath.Join(t.TempDir(), "index")
		blob := func(content string) string {
			return strings.TrimSpace(gitTestIn(t, r.dir, content, "hash-object", "-w", "--stdin"))
		}
		env := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = r.dir
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		env("update-index", "--add", "--cacheinfo", "100644,"+blob("a\nb\nsnyk\n")+",f.txt")
		env("update-index", "--add", "--cacheinfo", "100644,"+blob("package snyk\n")+",snyk.go")
		env("update-index", "--add", "--cacheinfo", "100644,"+blob("package queue\n")+",queue.go")
		if withWorkflow {
			env("update-index", "--add", "--cacheinfo", "100644,"+blob("on: push\n")+",.github/workflows/ci.yml")
		}
		mainTip := r.commitTree(env("write-tree"), []string{base}, "main: snyk-linear-sync (#161, #164), queue gate (#166), workflow edit (#162)")
		r.setBranch("main", mainTip)
		r.setBranch("pr", pr)
		r.onBranch("pr")
		return r, base, pr, mainTip
	}
	// The agent's tree: the fix beside a replay of main's changes, as the
	// package step would export it from a run whose session merged.
	agentTree := func(t *testing.T, r *testRepo, withWorkflow bool) string {
		index := filepath.Join(t.TempDir(), "index")
		blob := func(content string) string {
			return strings.TrimSpace(gitTestIn(t, r.dir, content, "hash-object", "-w", "--stdin"))
		}
		env := func(args ...string) string {
			cmd := exec.Command("git", args...)
			cmd.Dir = r.dir
			cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git %v: %v: %s", args, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		env("read-tree", r.head()+"^{tree}") // the branch head the run recorded
		env("update-index", "--cacheinfo", "100644,"+blob("a\nb\nsnyk\n")+",f.txt")
		// The agent's own work beside the replay: it continued its fix.
		env("update-index", "--cacheinfo", "100644,"+blob("package fix\n// the agent's own work\n")+",fix.go")
		env("update-index", "--add", "--cacheinfo", "100644,"+blob("package snyk\n")+",snyk.go")
		env("update-index", "--add", "--cacheinfo", "100644,"+blob("package queue\n")+",queue.go")
		if withWorkflow {
			env("update-index", "--add", "--cacheinfo", "100644,"+blob("on: push\n")+",.github/workflows/ci.yml")
		}
		return env("write-tree")
	}

	t.Run("the squash before the fix", func(t *testing.T) {
		r, _, pr, _ := build(t, true)
		origin := r.withOrigin("pr", "main")
		baseFile, _, stateFile := artifactDir(t)
		if err := os.WriteFile(baseFile, []byte(pr+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// The run's gate left no merge info: its artifact carries none.
		patchFile := filepath.Join(t.TempDir(), "changes.patch")
		patch := gitTest(t, r.dir, "diff", pr, r.commitTree(agentTree(t, r, true), []string{pr}, "the run's export"))
		if err := os.WriteFile(patchFile, []byte(patch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Before the fix: publish kept the patch alone (its base changed a
		// workflow, so the push token could not carry the merge) and the
		// workflow applied it on the old head as one commit -- main's changes
		// squashed into the work commit, `git log --graph` a straight line.
		pub := publishCheckout(t, origin, "pr")
		gitTest(t, pub, "apply", "--index", "--binary", patchFile)
		gitTest(t, pub, "commit", "-q", "-m", "codeaf: work for #163")
		squashed := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
		if parents := strings.Fields(gitTest(t, pub, "log", "--format=%P", "-1", squashed)); len(parents) != 1 {
			t.Fatalf("the squash has %v parents; want the single parent that made the PR replay main", parents)
		}
		files := gitTest(t, pub, "diff", pr, squashed, "--name-only")
		for _, want := range []string{".github/workflows/ci.yml", "fix.go", "queue.go", "snyk.go"} {
			if !strings.Contains(files, want) {
				t.Fatalf("the squashed commit lacks %s (files: %s)", want, files)
			}
		}

		// After the fix: publish refuses the replay instead -- nothing is
		// applied, and the branch head never moves.
		pub2 := publishCheckout(t, origin, "pr")
		_, err := publishMerge(pub2, "pr", "main", baseFile, filepath.Join(t.TempDir(), "absent.json"), stateFile, patchFile, dagCompare(r))
		if err == nil || !strings.Contains(err.Error(), "replays") {
			t.Fatalf("publishMerge err = %v; want a replay refusal", err)
		}
		if head := strings.TrimSpace(gitTest(t, pub2, "rev-parse", "HEAD")); head != pr {
			t.Fatalf("publish head = %s; want the untouched branch %s", head, pr)
		}
	})

	t.Run("the merge commit after the fix", func(t *testing.T) {
		r, _, pr, mainTip := build(t, false) // a re-run whose gate passes
		origin := r.withOrigin("pr", "main")
		baseFile, infoFile, stateFile := artifactDir(t)
		if _, err := runMerge(r.dir, "pr", "main", baseFile, infoFile, dagCompare(r)); err != nil {
			t.Fatalf("runMerge: %v", err)
		}
		if _, err := readMergeInfo(infoFile); err != nil {
			t.Fatalf("the re-run left no verifiable merge info: %v", err)
		}
		// Publish recreates the recorded merge from the same two parents.
		pub := publishCheckout(t, origin, "pr")
		if _, err := publishMerge(pub, "pr", "main", baseFile, infoFile, stateFile, "", dagCompare(r)); err != nil {
			t.Fatalf("publishMerge: %v", err)
		}
		head := strings.TrimSpace(gitTest(t, pub, "rev-parse", "HEAD"))
		if parents := strings.Fields(gitTest(t, pub, "log", "--format=%P", "-1", head)); len(parents) != 2 || parents[0] != pr || parents[1] != mainTip {
			t.Fatalf("recreated merge parents = %v; want [%s %s]", parents, pr, mainTip)
		}
		// The same replay patch no longer applies on the merged tree: main's
		// changes are in it, so the squash is dead for good.
		patchFile := filepath.Join(t.TempDir(), "changes.patch")
		patch := gitTest(t, r.dir, "diff", pr, r.commitTree(agentTree(t, r, true), []string{pr}, "the run's export"))
		if err := os.WriteFile(patchFile, []byte(patch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := gitRun(pub, "apply", "--index", "--binary", patchFile); err == nil {
			t.Fatal("the replay patch applied on the merged tree; the squash is still reachable")
		}
	})
}
