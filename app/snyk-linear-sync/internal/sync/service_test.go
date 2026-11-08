package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

type fakeSnyk struct {
	snapshot model.SnykSnapshot
}

func (f fakeSnyk) LoadSnapshot(context.Context) (model.SnykSnapshot, error) {
	return f.snapshot, nil
}

type fakeLinear struct {
	snapshot []model.ExistingIssue
	created  []model.DesiredIssue
	updated  []model.DesiredIssue
	updates  []model.IssueUpdate
	comments []model.IssueUpdate

	// createCallBatches records the desired batch passed to each CreateIssues
	// call, in order, so tests can assert how many calls were made and how
	// they were shaped (e.g. a full batch followed by a single-item retry).
	createCallBatches [][]model.DesiredIssue
	// createFailFingerprints, when non-zero for a fingerprint, causes that
	// many future CreateIssues calls containing that fingerprint to report
	// it as failed (not appended to created) instead of succeeding. This
	// mimics a Linear alias reporting success:false for one item in a batch
	// while its siblings succeed.
	createFailFingerprints map[string]int
	// createFailOutOfRange, when set, makes the next CreateIssues call report
	// one index past the end of the batch as failed, without creating any
	// entry and without an error. It mimics a faulty LinearClient
	// implementation deriving failed indices incorrectly; the real client
	// derives them from the slice it was handed, so they are always valid.
	createFailOutOfRange bool

	// commentCallBatches / commentFailFingerprints mirror the create fields
	// above but for PostComments.
	commentCallBatches      [][]model.IssueUpdate
	commentFailFingerprints map[string]int
	// commentErr, when non-nil, makes every PostComments call fail — the
	// batch call and each per-item retry — mirroring updateErr. It models a
	// transport-level failure where no per-alias data comes back at all.
	commentErr error
	// commentFailOutOfRange mirrors createFailOutOfRange for the comment
	// batch: the next PostComments call reports one failed index beyond the
	// end of the batch instead of indices derived from the updates. It
	// enjoys exactly one call and then resets, so any retry the service
	// makes proceeds normally.
	commentFailOutOfRange bool

	// lastOpenStates / lastOpenStatesErr are what LastOpenStates returns;
	// lastOpenStatesCalls records the issue IDs of each call.
	lastOpenStates      map[string]model.WorkflowState
	lastOpenStatesErr   error
	lastOpenStatesCalls [][]string

	// createBatchErr makes the first (batch) create call fail with an error,
	// simulating an ambiguous transport failure, while single-entry retries
	// succeed. landedFingerprints is what ExistingFingerprints then reports
	// as already present in Linear, and existingErr makes that
	// reconciliation query itself fail.
	createBatchErr     error
	landedFingerprints []string
	existingErr        error
	existingCalls      int

	// updateErr, when non-nil, makes every UpdateIssues call fail; the
	// recording behaviour is kept so a test can still see what was attempted.
	// createErr does the same for CreateIssues, which makes both the batch
	// call and the per-item retries fail.
	updateErr error
	createErr error
	// batchErrAbove, when > 0, makes an UpdateIssues call carrying more than
	// that many updates fail. It models the batch-with-fallback contract: the
	// multi-item batch call fails while every single-item retry succeeds.
	batchErrAbove int
	// updateFailIssueIDs, when non-empty, fails every UpdateIssues call whose
	// batch carries one of these issue IDs — the multi-item batch call and
	// that issue's single-item retry alike — while every other item succeeds.
	// It mirrors a live-label-read refusal, which follows the issue rather
	// than the call: each retry re-reads that issue's labels and refuses
	// again.
	updateFailIssueIDs map[string]bool
	// updateCallBatches records the batch passed to each UpdateIssues call, in
	// order, so a test can prove the per-item fallback actually happened (every
	// call after the failed batch carries exactly one update) rather than
	// inferring it from the recorded writes alone.
	updateCallBatches [][]model.IssueUpdate

	// cancelCreate / cancelUpdate, when non-nil, cancel the run's context the
	// first time CreateIssues / UpdateIssues is called. From that moment the
	// fake answers every call with the context error and records nothing,
	// exactly as a real client does: it models a signal arriving while that
	// Linear batch is in flight.
	cancelCreate func()
	cancelUpdate func()
}

type fakeCache struct {
	snapshot  cache.Snapshot
	saved     cache.Snapshot
	evictions cache.IgnoreEvictionCounts
}

func (f *fakeCache) IgnoreEvictions() cache.IgnoreEvictionCounts {
	return f.evictions
}

func (f *fakeLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return f.snapshot, nil
}

func (f *fakeLinear) ExistingFingerprints(_ context.Context, fingerprints []string) (map[string]struct{}, error) {
	f.existingCalls++
	if f.existingErr != nil {
		return nil, f.existingErr
	}
	wanted := make(map[string]struct{}, len(fingerprints))
	for _, fingerprint := range fingerprints {
		wanted[fingerprint] = struct{}{}
	}
	found := map[string]struct{}{}
	for _, fingerprint := range f.landedFingerprints {
		if _, ok := wanted[fingerprint]; ok {
			found[fingerprint] = struct{}{}
		}
	}
	return found, nil
}

func (f *fakeLinear) CreateIssues(ctx context.Context, desired []model.DesiredIssue) ([]int, error) {
	if f.cancelCreate != nil {
		cancel := f.cancelCreate
		f.cancelCreate = nil
		cancel()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.createCallBatches = append(f.createCallBatches, append([]model.DesiredIssue(nil), desired...))
	if f.createBatchErr != nil && len(desired) > 1 {
		return nil, f.createBatchErr
	}
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createFailOutOfRange {
		f.createFailOutOfRange = false
		return []int{len(desired)}, nil
	}
	var failed []int
	for i, d := range desired {
		if f.createFailFingerprints[d.Fingerprint] > 0 {
			f.createFailFingerprints[d.Fingerprint]--
			failed = append(failed, i)
			continue
		}
		f.created = append(f.created, d)
	}
	return failed, nil
}

func (f *fakeLinear) UpdateIssues(ctx context.Context, updates []model.IssueUpdate) error {
	if f.cancelUpdate != nil {
		cancel := f.cancelUpdate
		f.cancelUpdate = nil
		cancel()
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f.updateCallBatches = append(f.updateCallBatches, append([]model.IssueUpdate(nil), updates...))
	if f.batchErrAbove > 0 && len(updates) > f.batchErrAbove {
		return errors.New("batch update failed")
	}
	for _, update := range updates {
		if f.updateFailIssueIDs[update.Existing.ID] {
			return fmt.Errorf("refusing to update Linear issue %s: its current labels could not be read, so labels the sync does not own cannot be preserved", update.Existing.Identifier)
		}
	}
	for _, update := range updates {
		f.updated = append(f.updated, update.Desired)
		f.updates = append(f.updates, update)
	}
	return f.updateErr
}

func (f *fakeLinear) PostComments(_ context.Context, updates []model.IssueUpdate) ([]int, error) {
	f.commentCallBatches = append(f.commentCallBatches, append([]model.IssueUpdate(nil), updates...))
	if f.commentFailOutOfRange {
		f.commentFailOutOfRange = false
		return []int{len(updates)}, nil
	}
	if f.commentErr != nil {
		return nil, f.commentErr
	}
	var failed []int
	for i, u := range updates {
		if f.commentFailFingerprints[u.Desired.Fingerprint] > 0 {
			f.commentFailFingerprints[u.Desired.Fingerprint]--
			failed = append(failed, i)
			continue
		}
		f.comments = append(f.comments, u)
	}
	return failed, nil
}

func (f *fakeLinear) LastOpenStates(_ context.Context, issueIDs []string) (map[string]model.WorkflowState, error) {
	f.lastOpenStatesCalls = append(f.lastOpenStatesCalls, append([]string(nil), issueIDs...))
	out := map[string]model.WorkflowState{}
	for _, id := range issueIDs {
		if state, ok := f.lastOpenStates[id]; ok {
			out[id] = state
		}
	}
	return out, f.lastOpenStatesErr
}

func (f *fakeCache) Load(context.Context) (cache.Snapshot, error) {
	return f.snapshot, nil
}

func (f *fakeCache) Save(_ context.Context, snapshot cache.Snapshot) error {
	f.saved = snapshot
	return nil
}

func TestRunPlansCreateUpdateAndResolve(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Outdated package",
					PackageName: "github.com/example/pkg",
					Severity:    "high",
					Status:      model.FindingOpen,
					IssueURL:    "https://example.test/issue-1",
					CreatedAt:   time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-b:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-b",
					ProjectName: "Project B",
					IssueTitle:  "Ignored issue",
					Severity:    "low",
					Status:      model.FindingIgnored,
					IssueURL:    "https://example.test/issue-2",
					CreatedAt:   time.Date(2026, time.August, 1, 9, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{
				"project-a": {},
				"project-b": {},
				"project-z": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "old resolved issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-z:issue-9",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if len(linear.updated) != 2 {
		t.Fatalf("updated = %d, want 2", len(linear.updated))
	}
	if linear.created[0].DueDate != "2026-10-30" {
		t.Fatalf("created due date = %q, want %q", linear.created[0].DueDate, "2026-10-30")
	}
	if !containsDesiredState(linear.updated, model.StateDone) {
		t.Fatalf("updated states = %#v, want one %q", desiredStates(linear.updated), model.StateDone)
	}
}

// TestRunMatchesFingerprintMangledByLinearMarkdown is the regression test
// for the hourly close-and-recreate loop: Linear's editor rewrites the
// stored fingerprint's "__main__.py" as "**main**.py", so without
// canonicalization the open ticket never matches the live finding — every
// run resolves the "orphaned" ticket and creates a fresh copy. With
// canonicalization the mangled ticket must match: no create, no resolve.
func TestRunMatchesFingerprintMangledByLinearMarkdown(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: model.Fingerprint("project-a", "issue-1", "sim/__main__.py"),
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Path Traversal",
					Severity:    "low",
					Status:      model.FindingOpen,
					IssueURL:    "https://example.test/issue-1",
					CreatedAt:   time.Date(2026, time.July, 5, 14, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{
				"project-a": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Todo",
				// Stored form as returned by the Linear API after its
				// markdown round-trip mangled the written __main__.py.
				Fingerprint: "snyk:project-a:issue-1:sim/**main**.py",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 0 {
		t.Fatalf("PlannedCreates = %d, want 0 (mangled fingerprint must match existing ticket)", result.PlannedCreates)
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0 (matched ticket must not be resolved as missing)", result.PlannedResolves)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0", len(linear.created))
	}
	if containsDesiredState(linear.updated, model.StateDone) {
		t.Fatalf("updated states = %#v, matched open ticket must not be transitioned to Done", desiredStates(linear.updated))
	}
}

// TestNormalizeDescriptionForCompareTreatsLinearBoldRewriteAsEqual verifies
// that a description written with underscore emphasis compares equal to the
// asterisk form Linear stores, so re-matched tickets do not trigger a
// description rewrite on every run.
func TestNormalizeDescriptionForCompareTreatsLinearBoldRewriteAsEqual(t *testing.T) {
	written := "File: sim/__main__.py\n\n<!-- snyk-linear-sync\nfingerprint: snyk:p:i:sim/__main__.py\n-->"
	stored := "File: sim/**main**.py\n\n<!-- snyk-linear-sync\nfingerprint: snyk:p:i:sim/**main**.py\n-->"

	if normalizeDescriptionForCompare(written) != normalizeDescriptionForCompare(stored) {
		t.Fatalf("normalizeDescriptionForCompare: written and Linear-stored forms should compare equal\nwritten: %q\nstored:  %q",
			normalizeDescriptionForCompare(written), normalizeDescriptionForCompare(stored))
	}
}

// failedUpdateConfig is the configuration the failed-write cache tests share:
// worker pool of one and the default due-date windows, so the desired issue
// (and therefore its hash) is the one desiredIssue computes.
func failedUpdateConfig() config.Config {
	return config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
}

// failedUpdateSnyk is the Snyk snapshot every test below drives Run with: one
// open finding whose ticket already exists but is stale.
func failedUpdateSnyk() fakeSnyk {
	return fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1:lodash@1.0.0",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Outdated package",
					PackageName: "lodash",
					Severity:    "high",
					Status:      model.FindingOpen,
					IssueURL:    "https://example.test/issue-1",
					CreatedAt:   time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{
				"project-a": {},
			},
		},
	}
}

// TestRunFailedUpdateIsRetriedOnNextRun is the end-to-end regression test for
// a failed Linear write being cached as reconciled. Run 1 fails the update and
// must leave the fingerprint out of both cache maps, so run 2 — which only sees
// run 1's saved snapshot, exactly as a real second invocation would — plans and
// performs the update instead of skipping it on the cache fast-path.
func TestRunFailedUpdateIsRetriedOnNextRun(t *testing.T) {
	cfg := failedUpdateConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := failedUpdateSnyk()
	fingerprint := snyk.snapshot.Findings[0].Fingerprint

	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SNYK-100",
		Title:       "old title",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
	// Run keeps the ticket's non-terminal Linear state (preserve marker), so
	// this is the hash Run caches for the matched, unchanged ticket.
	desired := desiredIssue(cfg, snyk.snapshot.Findings[0])
	desired.PreserveState = true
	linearSnapshot := []model.ExistingIssue{existing}
	store := &fakeCache{}

	// Run 1: every update fails.
	failing := &fakeLinear{snapshot: linearSnapshot, updateErr: errors.New("linear unavailable")}
	result, err := New(cfg, logger, snyk, failing, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("run 1: PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if result.FailedOps != 1 {
		t.Fatalf("run 1: FailedOps = %d, want 1", result.FailedOps)
	}
	if _, ok := store.saved.SnykHashes[fingerprint]; ok {
		t.Fatalf("run 1: SnykHashes[%q] = %q, want the failed fingerprint absent",
			fingerprint, store.saved.SnykHashes[fingerprint])
	}
	if hash, ok := store.saved.LinearHashes[fingerprint]; ok {
		t.Fatalf("run 1: LinearHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}

	// Run 2: same inputs, healthy Linear, and only run 1's saved cache.
	healthy := &fakeLinear{snapshot: linearSnapshot}
	secondStore := &fakeCache{snapshot: store.saved}
	second, err := New(cfg, logger, snyk, healthy, secondStore).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if second.PlannedUpdates != 1 {
		t.Fatalf("run 2: PlannedUpdates = %d, want 1 (the failed update must be retried)", second.PlannedUpdates)
	}
	if len(healthy.updated) != 1 {
		t.Fatalf("run 2: updated = %d, want 1", len(healthy.updated))
	}
	if second.FailedOps != 0 {
		t.Fatalf("run 2: FailedOps = %d, want 0", second.FailedOps)
	}
	if _, ok := secondStore.saved.SnykHashes[fingerprint]; !ok {
		t.Fatalf("run 2: SnykHashes[%q] missing after a successful write", fingerprint)
	}
	if _, ok := secondStore.saved.LinearHashes[fingerprint]; !ok {
		t.Fatalf("run 2: LinearHashes[%q] missing after a successful write", fingerprint)
	}
	if want := desiredIssueHash(desired); secondStore.saved.SnykHashes[fingerprint] != want {
		t.Fatalf("run 2: SnykHashes[%q] = %q, want %q", fingerprint, secondStore.saved.SnykHashes[fingerprint], want)
	}
	if want := existingIssueHash(existing); secondStore.saved.LinearHashes[fingerprint] != want {
		t.Fatalf("run 2: LinearHashes[%q] = %q, want the initial snapshot's hash %q",
			fingerprint, secondStore.saved.LinearHashes[fingerprint], want)
	}
}

// TestRunFailedCreateIsNotCached verifies that a finding whose creation failed
// is left out of the Snyk hash cache, so the next run tries the create again
// instead of caching a ticket that does not exist.
func TestRunFailedCreateIsNotCached(t *testing.T) {
	cfg := failedUpdateConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := failedUpdateSnyk()
	fingerprint := snyk.snapshot.Findings[0].Fingerprint

	linear := &fakeLinear{createErr: errors.New("linear unavailable")}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1", result.FailedOps)
	}
	if hash, ok := store.saved.SnykHashes[fingerprint]; ok {
		t.Fatalf("SnykHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
}

// TestRunFailedResolveIsNotCached verifies that a ticket whose closure failed
// keeps its Linear hash out of the cache, so the next run re-resolves it rather
// than treating the stale open ticket as reconciled. A resolve records the
// ticket's own fingerprint, which has no Snyk hash at all this run — that
// absence is part of the assertion too.
func TestRunFailedResolveIsNotCached(t *testing.T) {
	cfg := failedUpdateConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := failedUpdateSnyk()
	// Only a project ID, no findings: the registered ticket's finding is gone.
	snyk.snapshot.Findings = nil
	fingerprint := "snyk:project-a:issue-1:lodash@1.0.0"

	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SNYK-100",
		Title:       "outdated package",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
	linear := &fakeLinear{
		snapshot:  []model.ExistingIssue{existing},
		updateErr: errors.New("linear unavailable"),
	}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1", result.FailedOps)
	}
	if hash, ok := store.saved.LinearHashes[fingerprint]; ok {
		t.Fatalf("LinearHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
	if hash, ok := store.saved.SnykHashes[fingerprint]; ok {
		t.Fatalf("SnykHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
}

// TestRunSuccessfulRunCachesEveryFingerprint guards the exclusion logic against
// over-deleting: with no write failing, both maps still carry the fingerprint
// after a single run.
func TestRunSuccessfulRunCachesEveryFingerprint(t *testing.T) {
	cfg := failedUpdateConfig()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := failedUpdateSnyk()
	fingerprint := snyk.snapshot.Findings[0].Fingerprint

	desired := desiredIssue(cfg, snyk.snapshot.Findings[0])
	// Run preserves the ticket's non-terminal state, so the cached hash
	// carries the preserve marker.
	desired.PreserveState = true
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SNYK-100",
		Title:       "old title",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	store := &fakeCache{}

	result, err := New(cfg, logger, snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0", result.FailedOps)
	}
	if want := desiredIssueHash(desired); store.saved.SnykHashes[fingerprint] != want {
		t.Fatalf("SnykHashes[%q] = %q, want %q", fingerprint, store.saved.SnykHashes[fingerprint], want)
	}
	// The fake Linear snapshot is unchanged, so the refreshed Linear snapshot
	// still yields the ticket's pre-update hash.
	if want := existingIssueHash(existing); store.saved.LinearHashes[fingerprint] != want {
		t.Fatalf("LinearHashes[%q] = %q, want %q", fingerprint, store.saved.LinearHashes[fingerprint], want)
	}
}

func TestRunSkipsCachedUnchangedIssue(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:  "snyk:project-a:issue-1",
					SnykIssueID:  "issue-1",
					SnykIssueKey: "SNYK-ISSUE-1",
					ProjectID:    "project-a",
					ProjectName:  "Project A",
					IssueTitle:   "Outdated package",
					PackageName:  "github.com/example/pkg",
					Severity:     "high",
					Status:       model.FindingOpen,
					IssueURL:     "https://app.snyk.io/org/example/project/project-a#issue-SNYK-ISSUE-1",
					IssueAPIURL:  "https://api.snyk.io/rest/orgs/example/issues/issue-1?version=2024-10-15",
					CreatedAt:    time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{
				"project-a": {},
			},
		},
	}
	desired := desiredIssue(cfg, snyk.snapshot.Findings[0])
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Todo",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{existing},
	}

	service := New(cfg, logger, snyk, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

// TestRunCancelsIgnoredFindingEvenIfCached verifies that a finding which is
// ignored in Snyk (desired state Cancelled) is moved to Cancelled even when its
// ticket was manually parked in "Todo" and the cache claims nothing changed.
// Regression test: the per-finding cache fast-path previously skipped these,
// leaving wont-fix-ignored tickets stuck open indefinitely.
func TestRunCancelsIgnoredFindingEvenIfCached(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:  "snyk:project-a:issue-1",
					SnykIssueID:  "issue-1",
					SnykIssueKey: "SNYK-ISSUE-1",
					ProjectID:    "project-a",
					ProjectName:  "Project A",
					IssueTitle:   "Base image vulnerability",
					PackageName:  "glibc/libc6",
					Severity:     "low",
					Status:       model.FindingIgnored,
					CreatedAt:    time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	// desired.State is Cancelled (ignored); the ticket was manually moved to
	// "Todo", so its state diverges from desired.
	desired := desiredIssue(cfg, snyk.snapshot.Findings[0])
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Todo",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	// Cache claims the issue is unchanged since last run — the masking condition
	// that previously suppressed the cancellation.
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snyk, linear, cacheStore)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

// TestRunCacheStillSkipsNonTerminalStateDivergence locks in the narrow scope of
// the terminal-transition cache guard: an open finding whose ticket diverges
// only in a non-terminal state must still be cache-suppressed when its hashes
// are unchanged. (A broader !needsUpdate guard would incorrectly re-update it.)
func TestRunCacheStillSkipsNonTerminalStateDivergence(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:  "snyk:project-a:issue-1",
					SnykIssueID:  "issue-1",
					SnykIssueKey: "SNYK-ISSUE-1",
					ProjectID:    "project-a",
					ProjectName:  "Project A",
					IssueTitle:   "Outdated package",
					PackageName:  "github.com/example/pkg",
					Severity:     "high",
					Status:       model.FindingOpen, // desired Todo — non-terminal
					CreatedAt:    time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	desired := desiredIssue(cfg, snyk.snapshot.Findings[0]) // State == Todo
	// Ticket is in a *different open* state; needsUpdate is true, but the
	// transition is non-terminal so the cache should keep suppressing it.
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       desired.Title,
		Description: desired.Description,
		DueDate:     desired.DueDate,
		StateName:   "Triage",
		Fingerprint: desired.Fingerprint,
		Priority:    desired.Priority,
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snyk, linear, cacheStore)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (non-terminal divergence stays cached)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunCancelsMissingIssueWhenProjectDeleted(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			ProjectIDs: map[string]struct{}{
				"project-a": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "missing project issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-z:issue-9",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

func TestRunCancelsMissingIssueWhenProjectDeletedEvenIfCached(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{
			Workers: 1,
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			ProjectIDs: map[string]struct{}{
				"project-a": {},
			},
		},
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "missing project issue",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: "snyk:project-z:issue-9",
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			LinearHashes: map[string]string{
				existing.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{existing},
	}

	service := New(cfg, logger, snyk, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

func TestNeedsUpdateUsesCaseInsensitiveLabels(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		State:       model.StateTodo,
		Priority:    2,
	}

	if needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = true, want false")
	}
}

func containsDesiredState(desired []model.DesiredIssue, state model.IssueState) bool {
	for _, issue := range desired {
		if issue.State == state {
			return true
		}
	}
	return false
}

func desiredStates(desired []model.DesiredIssue) []model.IssueState {
	out := make([]model.IssueState, 0, len(desired))
	for _, issue := range desired {
		out = append(out, issue.State)
	}
	return out
}

func TestDesiredIssueDueDateUsesSnykCreatedAt(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// Use a CreatedAt that produces a future due date so the guard against
	// past due dates does not kick in.
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		IssueType:   "code",
		Severity:    "critical",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.August, 11, 23, 30, 0, 0, time.FixedZone("minus0500", -5*60*60)),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-08-27" {
		t.Fatalf("desired due date = %q, want %q", desired.DueDate, "2026-08-27")
	}
}

func TestDesiredIssueDueDateLeavesPastSLAAsIs(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// CreatedAt is far in the past, so CreatedAt + 30 days is also in the past.
	// The due date is left as the raw SLA date (past dates show as overdue
	// in Linear and indicate how long the issue has exceeded its SLA).
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-01-31" {
		t.Fatalf("desired due date = %q, want %q (raw SLA date for past issues)", desired.DueDate, "2026-01-31")
	}
}

// TestDesiredIssueDueDateUsesUpdatedOnReusedIssueID verifies that when Snyk
// reuses an issue ID for a new code occurrence (updated_at >> created_at),
// the SLA clock restarts from updated_at rather than using the stale
// created_at. Without this, a freshly-detected occurrence gets a months-old
// due date and is immediately past due despite the code being only days old.
func TestDesiredIssueDueDateUsesUpdatedOnReusedIssueID(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// Snyk created the issue ID in January, but the current occurrence was
	// detected in July (updated_at). The 90-day gap exceeds the 45-day
	// medium SLA, so the SLA clock should restart from updated_at.
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1:src/file.py",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		Severity:    "medium",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.January, 22, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.July, 9, 17, 32, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	// July 9 + 45 days = August 23
	want := "2026-08-23"
	if desired.DueDate != want {
		t.Fatalf("desired due date = %q, want %q (SLA from updated_at, not created_at)", desired.DueDate, want)
	}
	if !strings.Contains(desired.DueDateReason, "updated_at") {
		t.Fatalf("due date reason = %q, want it to mention updated_at", desired.DueDateReason)
	}
}

// TestDesiredIssueDueDateDoesNotUseUpdatedAtForRecentIssue verifies that
// when updated_at is close to created_at (within the SLA window), the SLA
// clock uses created_at as before. This avoids false positives from routine
// re-scans that bump updated_at by a day or two.
func TestDesiredIssueDueDateDoesNotUseUpdatedAtForRecentIssue(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// created_at and updated_at are only 10 days apart — within the 90-day
	// low SLA window, so the SLA clock should use created_at.
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1:src/file.py",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		Severity:    "low",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.July, 11, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	// July 1 + 90 days = September 29 (using created_at, NOT updated_at)
	want := "2026-09-29"
	if desired.DueDate != want {
		t.Fatalf("desired due date = %q, want %q (SLA from created_at, not updated_at)", desired.DueDate, want)
	}
	if !strings.Contains(desired.DueDateReason, "issue creation") {
		t.Fatalf("due date reason = %q, want it to mention issue creation", desired.DueDateReason)
	}
}

// TestDesiredIssueDueDateIgnoredAtZeroUpdatedAt verifies that the updated_at
// fallback does not fire when updated_at is zero (missing from API response).
func TestDesiredIssueDueDateIgnoredAtZeroUpdatedAt(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectName: "Project A",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		// UpdatedAt is zero — should fall back to created_at behavior
	}

	desired := desiredIssue(cfg, finding)

	// Jan 1 + 30 days = Jan 31 (uses created_at, no updated_at fallback)
	if desired.DueDate != "2026-01-31" {
		t.Fatalf("desired due date = %q, want 2026-01-31 (created_at fallback when updated_at is zero)", desired.DueDate)
	}
}

func TestDesiredIssueDueDateLeavesPastExpiredSnoozeAsIs(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// IgnoreExpiresAt is in the past (snooze already expired),
	// so IgnoreExpiresAt + 30 days is also in the past. The due date is
	// left as the raw SLA date.
	finding := model.Finding{
		Fingerprint:     "snyk:project-a:issue-1",
		SnykIssueID:     "issue-1",
		ProjectName:     "Project A",
		Severity:        "high",
		Status:          model.FindingOpen,
		CreatedAt:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		IgnoreExpiresAt: time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-03-31" {
		t.Fatalf("desired due date = %q, want %q (raw SLA date from expired snooze)", desired.DueDate, "2026-03-31")
	}
}

func TestDesiredIssueAddsGitHubSourceLinksWhenConfigured(t *testing.T) {
	cfg := config.Config{
		Source: config.SourceConfig{
			Provider: "github",
		},
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:     "snyk-automation",
				Tool:        map[string]string{"code": "snyk-code"},
				ToolDefault: "snyk-automation",
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk:project-a:issue-1",
		SnykIssueID:       "issue-1",
		SnykIssueKey:      "SNYK-CODE-ISSUE-1",
		IssueType:         "code",
		ProjectID:         "project-a",
		ProjectName:       "Project A",
		IssueTitle:        "Path Traversal",
		Severity:          "high",
		Status:            model.FindingOpen,
		IssueAPIURL:       "https://api.example.test/issue-1",
		IssueURL:          "https://app.example.test/issue-1",
		Repository:        "owner/repo",
		ProjectReference:  "main",
		SourceFile:        "src/main.go",
		SourceCommitID:    "abc123",
		SourceLineStart:   10,
		SourceColumnStart: 2,
		SourceLineEnd:     12,
		SourceColumnEnd:   8,
	}

	desired := desiredIssue(cfg, finding)

	if desired.Title != "Snyk: [high] owner/repo: Path Traversal in main.go" {
		t.Fatalf("title = %q, want %q", desired.Title, "Snyk: [high] owner/repo: Path Traversal in main.go")
	}
	if !strings.Contains(desired.Description, "## Path Traversal [HIGH]") {
		t.Fatalf("description missing heading: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Repository: [owner/repo](https://github.com/owner/repo)") {
		t.Fatalf("description missing GitHub repository link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Ref: `main` at [`abc123`](https://github.com/owner/repo/commit/abc123)") {
		t.Fatalf("description missing ref line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "[src/main.go (line 10:2 to 12:8)](https://github.com/owner/repo/blob/abc123/src/main.go#L10-L12)") {
		t.Fatalf("description missing GitHub source file link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Snyk: [Open issue](https://app.example.test/issue-1)") {
		t.Fatalf("description missing Snyk link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "API: [Issue details](https://api.example.test/issue-1)") {
		t.Fatalf("description missing API link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Status: `open`") {
		t.Fatalf("description missing status line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "managed_labels: snyk-automation,snyk-code") {
		t.Fatalf("description missing managed labels metadata: %s", desired.Description)
	}
	if len(desired.ManagedLabels) != 2 || desired.ManagedLabels[0] != "snyk-automation" || desired.ManagedLabels[1] != "snyk-code" {
		t.Fatalf("ManagedLabels = %#v, want [snyk-automation snyk-code]", desired.ManagedLabels)
	}
}

func TestIssueDescriptionEmbedsSnykIssueDetails(t *testing.T) {
	cfg := config.Config{
		Source: config.SourceConfig{Provider: "github"},
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk:project-a:issue-1",
		SnykIssueID:       "issue-1",
		SnykIssueKey:      "SNYK-JS-LODASH-1",
		IssueType:         "package_vulnerability",
		ProjectID:         "project-a",
		ProjectName:       "owner/repo(main):package-lock.json",
		IssueTitle:        "Prototype Pollution",
		Severity:          "high",
		Status:            model.FindingOpen,
		IssueURL:          "https://app.example.test/issue-1",
		IssueAPIURL:       "https://api.example.test/issue-1",
		PackageName:       "lodash",
		VulnerableVersion: "4.17.20",
		FixedVersion:      "4.17.21",
		CVSS:              7.5,
		Classes: []model.IssueClass{
			{ID: "CWE-1321", Source: "CWE"},
			{ID: "CWE-915", Source: "CWE"},
		},
		CVEs:              []string{"CVE-2020-8203", "CVE-2021-23337"},
		Description:       "Prototype pollution in lodash allows an attacker to merge recursive objects.",
		Remediation:       "Upgrade lodash to version 4.17.21 or higher.",
		HasCoordinates:    true,
		IsFixableSnyk:     true,
		IsFixableManually: true,
	}

	desired := desiredIssue(cfg, finding)

	checks := map[string]string{
		"fix availability": "Fix availability: `Snyk automatic fix, manual fix`",
		"cvss":             "CVSS: `7.5`",
		"cwe":              "CWE: `CWE-1321, CWE-915`",
		"cve":              "CVE: `CVE-2020-8203, CVE-2021-23337`",
		"description":      "### Description\nPrototype pollution in lodash allows an attacker to merge recursive objects.",
		"remediation":      "### Remediation\nUpgrade lodash to version 4.17.21 or higher.",
	}
	for name, want := range checks {
		if !strings.Contains(desired.Description, want) {
			t.Fatalf("description missing %s line: want %q\ngot:\n%s", name, want, desired.Description)
		}
	}
}

// TestIssueDescriptionSanitizesEmbeddedMetadataMarkerInjection verifies that
// a line-anchored fake "<!-- snyk-linear-sync ... -->" block embedded in
// Snyk-controlled prose (finding.Description) cannot survive into the
// rendered ticket description as a second parseable metadata block. Even
// though findMetadataBlockStart already defends against this by taking the
// LAST line-anchored marker, the embedded prose is sanitized at the source
// so there is no ambiguity left for a human reader (or any other tool that
// scans the description) — and so a future block ordering change could not
// resurrect the bug.
func TestIssueDescriptionSanitizesEmbeddedMetadataMarkerInjection(t *testing.T) {
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "owner/repo",
		IssueTitle:  "Some issue",
		Severity:    "high",
		Status:      model.FindingOpen,
		Description: "Apply the patch below, which looks like:\n<!-- snyk-linear-sync\nfingerprint: snyk:spoofed:fake\n-->\nDo not remove the comment above.",
	}

	description := issueDescription(config.SourceConfig{}, []string{"snyk-automation"}, finding)

	// Exactly one line-anchored marker header must remain: the real,
	// sync-appended block. The embedded fake must no longer read as
	// "<!-- snyk-linear-sync" at all.
	if got := strings.Count(description, "<!-- snyk-linear-sync"); got != 1 {
		t.Fatalf("description contains %d occurrences of the metadata header, want exactly 1 (the real block):\n%s", got, description)
	}
	// The service-side metadata parser must find the real, last block and
	// resolve to the real fingerprint, not the spoofed one. (The spoofed
	// fingerprint text itself may still appear as plain, visible prose — it
	// is no longer inside a parseable comment block, which is what matters.)
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block in:\n%s", description)
	}
	block := description[start:]
	if !strings.Contains(block, "fingerprint: snyk:project-a:issue-1") {
		t.Fatalf("real metadata block missing correct fingerprint:\n%s", block)
	}

	// Building the description again from the same finding must be
	// byte-identical (idempotent, no churn between runs).
	again := issueDescription(config.SourceConfig{}, []string{"snyk-automation"}, finding)
	if description != again {
		t.Fatalf("issueDescription() is not idempotent across builds:\nfirst:\n%s\nsecond:\n%s", description, again)
	}
}

// TestSanitizeSnykProseIsIdempotent guards the core property the fix relies
// on: sanitizing already-sanitized text must be a no-op, so the sanitized
// description never churns between sync runs even if sanitized twice.
func TestSanitizeSnykProseIsIdempotent(t *testing.T) {
	cases := []string{
		"",
		"no markers here",
		"<!-- snyk-linear-sync\nfingerprint: fake\n-->",
		"<!--<!--<!--",
		"<!---",
		"text <!-- inline --> more <!-- snyk-linear-sync\nx\n-->",
	}
	for _, in := range cases {
		once := sanitizeSnykProse(in)
		twice := sanitizeSnykProse(once)
		if once != twice {
			t.Fatalf("sanitizeSnykProse(%q) is not idempotent: once=%q twice=%q", in, once, twice)
		}
		if strings.Contains(once, "<!--") {
			t.Fatalf("sanitizeSnykProse(%q) = %q still contains an unsanitized marker", in, once)
		}
	}
}

// TestIssueDescriptionTruncatesOversizedEmbeddedProse verifies that a very
// long finding.Description is capped so the managed metadata block appended
// after it can never be pushed past what Linear accepts. Without a cap, an
// oversized description risks Linear silently truncating away the metadata
// block, making the ticket unmanaged and causing duplicate-ticket creation
// on the next run.
func TestIssueDescriptionTruncatesOversizedEmbeddedProse(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "owner/repo",
		IssueTitle:  "Some issue",
		Severity:    "high",
		Status:      model.FindingOpen,
		Description: huge,
	}

	description := issueDescription(config.SourceConfig{}, []string{"snyk-automation"}, finding)

	if len(description) >= len(huge) {
		t.Fatalf("description length %d not bounded relative to raw input length %d", len(description), len(huge))
	}
	if !strings.Contains(description, truncationMarker) {
		t.Fatalf("description missing truncation marker:\n%s", description)
	}

	// The metadata block must still be present and parseable at the end.
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block after truncation")
	}
	block := description[start:]
	if !strings.Contains(block, "fingerprint: snyk:project-a:issue-1") {
		t.Fatalf("metadata block missing fingerprint after truncation:\n%s", block)
	}
	if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
		t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
	}

	// Building twice from the same finding must be byte-identical.
	again := issueDescription(config.SourceConfig{}, []string{"snyk-automation"}, finding)
	if description != again {
		t.Fatalf("issueDescription() is not idempotent across builds for oversized input")
	}
}

func TestIssueDescriptionOmitsFixAvailabilityWithoutCoordinates(t *testing.T) {
	cfg := config.Config{
		Source: config.SourceConfig{Provider: "github"},
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "owner/repo",
		IssueTitle:  "Some issue",
		Severity:    "medium",
		Status:      model.FindingOpen,
		IssueURL:    "https://app.example.test/issue-1",
		// HasCoordinates false, no fixable flags, no CVSS, no classes/CVEs, no description/remediation.
	}

	desired := desiredIssue(cfg, finding)

	if strings.Contains(desired.Description, "Fix availability:") {
		t.Fatalf("description should omit fix availability when Snyk reports no coordinates:\n%s", desired.Description)
	}
	if strings.Contains(desired.Description, "CVSS:") {
		t.Fatalf("description should omit CVSS when not reported:\n%s", desired.Description)
	}
	if strings.Contains(desired.Description, "CWE:") {
		t.Fatalf("description should omit CWE when none reported:\n%s", desired.Description)
	}
	if strings.Contains(desired.Description, "### Description") {
		t.Fatalf("description should omit Description section when not reported:\n%s", desired.Description)
	}
	if strings.Contains(desired.Description, "### Remediation") {
		t.Fatalf("description should omit Remediation section when not reported:\n%s", desired.Description)
	}
}

func TestIssueDescriptionIncludesKubernetesClusterAndNamespace(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk:project-k8s:issue-1",
		SnykIssueID:       "issue-1",
		ProjectID:         "project-k8s",
		ProjectName:       "backend/deployment.apps/backend:/app/package.json",
		ProjectOrigin:     "kubernetes",
		ProjectCluster:    "production",
		ProjectNamespace:  "backend",
		ProjectReference:  "637423609965.dkr.ecr.us-east-1.amazonaws.com/central-registry/tessl/backend",
		ProjectTargetFile: "/app/package.json",
		IssueTitle:        "Prototype Pollution",
		Severity:          "high",
		Status:            model.FindingOpen,
		IssueURL:          "https://app.example.test/issue-1",
	}

	desired := desiredIssue(cfg, finding)

	if !strings.Contains(desired.Description, "Cluster: `production`") {
		t.Fatalf("description should include the kubernetes cluster:\n%s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Namespace: `backend`") {
		t.Fatalf("description should include the kubernetes namespace:\n%s", desired.Description)
	}
}

func TestIssueDescriptionOmitsClusterAndNamespaceForOtherOrigins(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "owner/repo",
		// ProjectCluster/ProjectNamespace left empty, as they are for
		// non-kubernetes origins.
		IssueTitle: "Some issue",
		Severity:   "medium",
		Status:     model.FindingOpen,
		IssueURL:   "https://app.example.test/issue-1",
	}

	desired := desiredIssue(cfg, finding)

	if strings.Contains(desired.Description, "Cluster:") {
		t.Fatalf("description should omit cluster when not reported:\n%s", desired.Description)
	}
	if strings.Contains(desired.Description, "Namespace:") {
		t.Fatalf("description should omit namespace when not reported:\n%s", desired.Description)
	}
}

func TestDesiredIssueAddsGitHubProjectTargetFileLinkWhenNoSourceLocationExists(t *testing.T) {
	cfg := config.Config{
		Source: config.SourceConfig{
			Provider: "github",
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk:project-a:issue-1",
		SnykIssueID:       "issue-1",
		IssueType:         "package_vulnerability",
		ProjectID:         "project-a",
		ProjectName:       "owner/repo(main):apps/backend/Dockerfile.dev",
		IssueTitle:        "Integer Overflow or Wraparound",
		Severity:          "critical",
		Status:            model.FindingOpen,
		IssueURL:          "https://app.example.test/issue-1",
		IssueAPIURL:       "https://api.example.test/issue-1",
		Repository:        "owner/repo",
		ProjectReference:  "main",
		ProjectTargetFile: "apps/backend/Dockerfile.dev",
		PackageName:       "zlib/zlib1g",
		SnykIssueKey:      "SNYK-DEBIAN-ZLIB-1",
	}

	desired := desiredIssue(cfg, finding)

	if desired.Title != "Snyk: [critical] owner/repo: Integer Overflow or Wraparound in zlib/zlib1g" {
		t.Fatalf("title = %q, want %q", desired.Title, "Snyk: [critical] owner/repo: Integer Overflow or Wraparound in zlib/zlib1g")
	}
	if !strings.Contains(desired.Description, "Repository: [owner/repo](https://github.com/owner/repo)") {
		t.Fatalf("description missing repository link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Ref: `main`") {
		t.Fatalf("description missing ref line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Target file: [apps/backend/Dockerfile.dev](https://github.com/owner/repo/blob/main/apps/backend/Dockerfile.dev)") {
		t.Fatalf("description missing GitHub project target file link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Package: `zlib/zlib1g`") {
		t.Fatalf("description missing package line: %s", desired.Description)
	}
}

func TestIssueTitleUsesReferenceForNonGitHubTargetFileFindings(t *testing.T) {
	finding := model.Finding{
		IssueTitle:        "Use of Default Credentials",
		Severity:          "critical",
		ProjectOrigin:     "kubernetes",
		ProjectReference:  "ghcr.io/berriai/litellm-database",
		ProjectTargetFile: "/app/pyproject.toml",
		PackageName:       "mlflow",
	}

	title := issueTitle(finding)

	if title != "Snyk: [critical] ghcr.io/berriai/litellm-database: Use of Default Credentials in mlflow" {
		t.Fatalf("title = %q, want %q", title, "Snyk: [critical] ghcr.io/berriai/litellm-database: Use of Default Credentials in mlflow")
	}
}

func TestUpsertManagedMetadataRemovesVisibleFingerprintFooter(t *testing.T) {
	description := "Status: `open`\n\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->\nFingerprint: snyk:project-a:issue-1"

	got := upsertManagedMetadata(description, managedMetadata{Fingerprint: "snyk:project-a:issue-1", ManagedLabels: []string{"snyk-automation", "snyk-code"}})

	if strings.Contains(got, "Fingerprint: snyk:project-a:issue-1") {
		t.Fatalf("upsertManagedMetadata() left visible fingerprint footer: %s", got)
	}
	if !strings.Contains(got, "managed_labels: snyk-automation,snyk-code") {
		t.Fatalf("upsertManagedMetadata() missing managed labels metadata: %s", got)
	}
}

// TestUpsertManagedMetadataIgnoresInlineMarker verifies that the metadata
// marker appearing mid-sentence in user text does not corrupt the description.
// The marker must be at the start of a line to be treated as a metadata block.
func TestUpsertManagedMetadataIgnoresInlineMarker(t *testing.T) {
	description := "See <!-- snyk-linear-sync notes --> for details\n\nStatus: `open`"

	got := upsertManagedMetadata(description, managedMetadata{Fingerprint: "snyk:project-a:issue-1", ManagedLabels: []string{"snyk-automation"}})

	// The inline marker should NOT be replaced; it should remain intact.
	if !strings.Contains(got, "See <!-- snyk-linear-sync notes -->") {
		t.Fatalf("upsertManagedMetadata() corrupted inline marker: %s", got)
	}
	// A proper metadata block should be appended at the end.
	if !strings.Contains(got, "<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1") {
		t.Fatalf("upsertManagedMetadata() missing proper metadata block: %s", got)
	}
}

func TestFindMetadataBlockStartAnchoredToLineBoundary(t *testing.T) {
	cases := []struct {
		name        string
		description string
		wantIdx     int
	}{
		{
			name:        "marker at start of description",
			description: "<!-- snyk-linear-sync\nfingerprint: test\n-->",
			wantIdx:     0,
		},
		{
			name:        "marker at start of line",
			description: "Some text\n<!-- snyk-linear-sync\nfingerprint: test\n-->",
			wantIdx:     10, // after "Some text\n"
		},
		{
			name:        "marker mid-sentence should not match",
			description: "See <!-- snyk-linear-sync notes --> for details",
			wantIdx:     -1,
		},
		{
			name:        "no marker at all",
			description: "Just a normal description",
			wantIdx:     -1,
		},
		{
			name:        "inline marker followed by real block",
			description: "See <!-- snyk-linear-sync notes -->\n<!-- snyk-linear-sync\nfingerprint: test\n-->",
			wantIdx:     36, // the real block, not the inline one
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findMetadataBlockStart(tc.description)
			if got != tc.wantIdx {
				t.Fatalf("findMetadataBlockStart() = %d, want %d", got, tc.wantIdx)
			}
		})
	}
}

// TestFindMetadataBlockStartReturnsLastLineAnchoredOccurrence verifies that
// when TWO line-anchored marker blocks are present, the LAST one is returned,
// not the first. This mirrors the fix applied to the equivalent function in
// internal/linear/client.go: Snyk-controlled prose embedded in the ticket
// body (e.g. quoted remediation text) can itself contain a line-anchored
// marker block, and the sync always appends the real managed block last.
func TestFindMetadataBlockStartReturnsLastLineAnchoredOccurrence(t *testing.T) {
	description := "<!-- snyk-linear-sync\nfingerprint: fake\n-->\n<!-- snyk-linear-sync\nfingerprint: real\n-->"
	want := strings.LastIndex(description, "<!-- snyk-linear-sync")

	got := findMetadataBlockStart(description)
	if got != want {
		t.Fatalf("findMetadataBlockStart() = %d, want %d (last line-anchored occurrence)", got, want)
	}
}

// TestUpsertManagedMetadataKeepsRealTrailingBlockWithFakeEarlierBlock verifies
// that when a ticket description contains an earlier, line-anchored block
// that merely looks like the managed metadata marker (e.g. copied into
// Snyk-controlled prose such as remediation text) followed by the real
// sync-managed block, upsertManagedMetadata updates the REAL (last) block and
// leaves the fake block untouched. Returning the first occurrence here would
// corrupt the fake prose and, in the Linear client's equivalent function,
// hijack fingerprint/label extraction and cause duplicate tickets.
func TestUpsertManagedMetadataKeepsRealTrailingBlockWithFakeEarlierBlock(t *testing.T) {
	description := strings.Join([]string{
		"### Remediation",
		"Apply the patch below, which looks like:",
		"<!-- snyk-linear-sync",
		"fingerprint: snyk:spoofed:fake",
		"-->",
		"",
		"<!-- snyk-linear-sync",
		"fingerprint: snyk:project-a:issue-1",
		"-->",
	}, "\n")

	got := upsertManagedMetadata(description, managedMetadata{Fingerprint: "snyk:project-a:issue-2", ManagedLabels: []string{"snyk-automation"}})

	// The fake earlier block must be left alone.
	if !strings.Contains(got, "fingerprint: snyk:spoofed:fake") {
		t.Fatalf("upsertManagedMetadata() removed the fake earlier block: %s", got)
	}
	// The real trailing block must have been replaced with the new fingerprint,
	// not left stale.
	if strings.Contains(got, "fingerprint: snyk:project-a:issue-1") {
		t.Fatalf("upsertManagedMetadata() left the stale real block in place: %s", got)
	}
	if !strings.Contains(got, "fingerprint: snyk:project-a:issue-2") {
		t.Fatalf("upsertManagedMetadata() missing updated fingerprint in real block: %s", got)
	}
	if strings.Count(got, "fingerprint: snyk:project-a:issue-2") != 1 {
		t.Fatalf("expected exactly one occurrence of new fingerprint (only the real block touched): %s", got)
	}
	if !strings.Contains(got, "managed_labels: snyk-automation") {
		t.Fatalf("upsertManagedMetadata() missing managed labels: %s", got)
	}
}

func TestNeedsUpdateDetectsManagedLabelChange(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "title",
		Description:   "description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		ManagedLabels: []string{"old-label"},
		Labels: []model.IssueLabel{
			{ID: "label-1", Name: "old-label"},
		},
		Priority: 2,
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "description",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		ManagedLabels: []string{"new-label"},
		Priority:      2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestManagedLabelsUsesConfiguredToolMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:     "snyk-automation",
		Tool:        map[string]string{"code": "snyk-code"},
		ToolDefault: "snyk-automation",
	}, model.Finding{IssueType: "code"})

	if len(labels) != 2 || labels[0] != "snyk-automation" || labels[1] != "snyk-code" {
		t.Fatalf("managedLabels() = %#v, want [snyk-automation snyk-code]", labels)
	}
}

func TestManagedLabelsFallsBackToConfiguredDefault(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:     "snyk-automation",
		ToolDefault: "snyk-fallback",
	}, model.Finding{IssueType: "custom"})

	if len(labels) != 2 || labels[0] != "snyk-automation" || labels[1] != "snyk-fallback" {
		t.Fatalf("managedLabels() = %#v, want [snyk-automation snyk-fallback]", labels)
	}
}

func TestManagedLabelsUsesConfiguredOriginMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed: "snyk-automation",
		Origin:  map[string]string{"kubernetes": "snyk-kubernetes", "github": "snyk-github"},
	}, model.Finding{ProjectOrigin: "kubernetes"})

	if len(labels) != 2 || labels[0] != "snyk-automation" || labels[1] != "snyk-kubernetes" {
		t.Fatalf("managedLabels() = %#v, want [snyk-automation snyk-kubernetes]", labels)
	}
}

func TestManagedLabelsFallsBackToConfiguredOriginDefault(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:       "snyk-automation",
		OriginDefault: "snyk-origin",
	}, model.Finding{ProjectOrigin: "github"})

	if len(labels) != 2 || labels[0] != "snyk-automation" || labels[1] != "snyk-origin" {
		t.Fatalf("managedLabels() = %#v, want [snyk-automation snyk-origin]", labels)
	}
}

func TestManagedLabelsAddsAwaitingFixLabel(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:     "snyk-automation",
		AwaitingFix: "triage-dependency",
	}, model.Finding{Status: model.FindingAwaitingFix})

	found := false
	for _, l := range labels {
		if l == "triage-dependency" {
			found = true
		}
	}
	if !found {
		t.Fatalf("managedLabels() = %#v, want triage-dependency for awaiting-fix issue", labels)
	}
}

func TestManagedLabelsOmitsAwaitingFixLabelWhenOff(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:     "snyk-automation",
		AwaitingFix: "", // off
	}, model.Finding{Status: model.FindingAwaitingFix})

	for _, l := range labels {
		if l == "triage-dependency" {
			t.Fatalf("managedLabels() should not include triage-dependency when AwaitingFix is off")
		}
	}
}

func TestManagedLabelsOmitsAwaitingFixLabelForOpenIssue(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:     "snyk-automation",
		AwaitingFix: "triage-dependency",
	}, model.Finding{Status: model.FindingOpen})

	for _, l := range labels {
		if l == "triage-dependency" {
			t.Fatalf("managedLabels() should not include triage-dependency for open issues")
		}
	}
}

// TestManagedLabelsEmptyWhenLabelManagementOff is the issue #333 guard: with
// LINEAR_MANAGED_LABEL=off and no other label configuration, the derived labels
// keep their non-empty defaults, so a naive managed set would still assert
// snyk-automation and triage-dependency. Turning management off must turn off
// the whole set — labels and the "why" reasons alike.
func TestManagedLabelsEmptyWhenLabelManagementOff(t *testing.T) {
	labelCfg := config.LabelConfig{
		Managed:       "",
		Tool:          map[string]string{"code": "snyk-code"},
		ToolDefault:   "snyk-automation",
		Origin:        map[string]string{"github": "snyk-github"},
		OriginDefault: "snyk-origin",
		AwaitingFix:   "triage-dependency",
	}
	finding := model.Finding{
		Status:        model.FindingAwaitingFix,
		IssueType:     "code",
		ProjectOrigin: "github",
	}

	if labels := managedLabels(labelCfg, finding); len(labels) != 0 {
		t.Fatalf("managedLabels() = %#v, want none when label management is off", labels)
	}
	if reasons := buildLabelReasons(labelCfg, finding); len(reasons) != 0 {
		t.Fatalf("buildLabelReasons() = %#v, want none when label management is off", reasons)
	}
}

// TestDesiredIssueWritesNoManagedLabelsWhenManagementOff checks the whole
// desired issue, not just the helper: the label set and the metadata block the
// description carries must both come out empty.
func TestDesiredIssueWritesNoManagedLabelsWhenManagementOff(t *testing.T) {
	cfg := githubCfg()
	cfg.Linear.Labels = config.LabelConfig{
		Managed:     "",
		ToolDefault: "snyk-automation",
		AwaitingFix: "triage-dependency",
	}

	finding := scanFinding("issue-1", commitA, 10)
	finding.Status = model.FindingAwaitingFix

	desired := desiredIssue(cfg, finding)

	if len(desired.ManagedLabels) != 0 {
		t.Fatalf("ManagedLabels = %#v, want none when label management is off", desired.ManagedLabels)
	}
	if len(desired.LabelReasons) != 0 {
		t.Fatalf("LabelReasons = %#v, want none when label management is off", desired.LabelReasons)
	}
	if strings.Contains(desired.Description, "managed_labels:") {
		t.Fatalf("metadata block records managed labels with management off:\n%s", desired.Description)
	}
}

// TestRunPlansNoLabelMutationWhenManagementOff drives the run: a finding that
// would otherwise gain snyk-automation and a default tool label must create a
// ticket whose desired label set is empty, so no label resolution or mutation
// is planned.
func TestRunPlansNoLabelMutationWhenManagementOff(t *testing.T) {
	cfg := githubCfg()
	cfg.Linear.Labels = config.LabelConfig{
		Managed:     "",
		ToolDefault: "snyk-automation",
		AwaitingFix: "triage-dependency",
	}
	sim := &linearSim{issues: map[string]*model.ExistingIssue{}}
	store := &fakeCache{}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{scanFinding("issue-1", commitA, 10)},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, sim, store).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(sim.created) != 1 {
		t.Fatalf("created = %d issues, want 1", len(sim.created))
	}
	created := sim.created[0]
	if len(created.ManagedLabels) != 0 {
		t.Fatalf("created ManagedLabels = %#v, want none", created.ManagedLabels)
	}
	if strings.Contains(created.Description, "managed_labels:") {
		t.Fatalf("created metadata block records managed labels:\n%s", created.Description)
	}
}

// TestRunRemovesPreviouslyManagedLabelsWhenManagementTurnedOff is the other
// half of issue #333: README's "the previously managed label is removed if
// label management is turned off". A ticket created while label management was
// on carries snyk-automation and its derived tool/awaiting-fix labels, recorded
// in the managed_labels: metadata line. The operator then sets
// LINEAR_MANAGED_LABEL=off; the next run's desired label set is empty, so every
// previously managed label must be reported removed and the metadata line must
// stop recording them — while a label the sync does not own is left alone.
func TestRunRemovesPreviouslyManagedLabelsWhenManagementTurnedOff(t *testing.T) {
	cfg := githubCfg()
	// The defaults LINEAR_MANAGED_LABEL=off leaves behind: Managed empty, the
	// derived fallbacks still non-empty.
	cfg.Linear.Labels = config.LabelConfig{
		Managed:     "",
		ToolDefault: "snyk-automation",
		AwaitingFix: "triage-dependency",
	}

	finding := scanFinding("issue-1", commitA, 10)
	finding.Status = model.FindingAwaitingFix
	// Written by the previous run, while label management was still on.
	existing := storedIssue("existing-1", "SNYK-1", "Backlog", desiredIssue(githubCfg(), finding))
	existing.Title = "stale title" // force an update, so the label diff is produced
	existing.ManagedLabels = []string{"snyk-automation", "triage-dependency", "snyk-code"}
	existing.Labels = []model.IssueLabel{
		{ID: "l1", Name: "snyk-automation"},
		{ID: "l2", Name: "triage-dependency"},
		{ID: "l3", Name: "snyk-code"},
		{ID: "l4", Name: "human-triage"}, // not the sync's: must survive
	}

	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(linear.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(linear.updates))
	}
	update := linear.updates[0]

	if len(update.Desired.ManagedLabels) != 0 {
		t.Fatalf("Desired.ManagedLabels = %#v, want none with label management off", update.Desired.ManagedLabels)
	}
	if update.Diff == nil {
		t.Fatal("Diff = nil, want the label removal recorded")
	}
	if !update.Diff.LabelsNeedUpdate {
		t.Fatal("Diff.LabelsNeedUpdate = false, want true (previously managed labels must be removed)")
	}
	got := slices.Sorted(slices.Values(update.Diff.LabelsRemoved))
	want := []string{"snyk-automation", "snyk-code", "triage-dependency"}
	if !slices.Equal(got, want) {
		t.Fatalf("Diff.LabelsRemoved = %v, want %v", got, want)
	}
	if slices.Contains(update.Diff.LabelsRemoved, "human-triage") {
		t.Fatalf("Diff.LabelsRemoved = %v, must not name a label the sync does not own", update.Diff.LabelsRemoved)
	}
	if slices.Contains(update.Diff.LabelsAdded, "snyk-automation") ||
		slices.Contains(update.Diff.LabelsAdded, "triage-dependency") {
		t.Fatalf("Diff.LabelsAdded = %v, want no derived label re-added with management off", update.Diff.LabelsAdded)
	}
	if strings.Contains(update.Desired.Description, "managed_labels:") {
		t.Fatalf("desired metadata block still records managed labels:\n%s", update.Desired.Description)
	}
}

func TestNeedsUpdateClearsDueDateWhenDesiredIsEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-07-01",
		StateName:   "Backlog",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "", // cleared for awaiting-fix
		State:       model.StateBacklog,
		Priority:    2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true (must clear stale due date for awaiting-fix)")
	}
}

func TestNeedsUpdateSkipsDueDateWhenBothEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		StateName:   "Backlog",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		State:       model.StateBacklog,
		Priority:    2,
	}

	if needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = true, want false (both due dates empty)")
	}
}

func TestNeedsUpdateIncludesDueDate(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-04-15",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestNeedsUpdateDetectsLinkOnlyDescriptionChange(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "Repository: owner/repo",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "Repository: [owner/repo](https://github.com/owner/repo)",
		DueDate:     "2026-04-01",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestIdentifierNum(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"SNYK-1", 1},
		{"SNYK-42", 42},
		{"SNYK-11596", 11596},
		{"SEC-999", 999},
		{"nodash", 0},
		{"", 0},
		{"SNYK-abc", 0},
	}
	for _, tc := range cases {
		if got := identifierNum(tc.input); got != tc.want {
			t.Errorf("identifierNum(%q) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestRunCancelsDuplicateFingerprintKeepsLowerIdentifier(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	// SNYK-20 and SNYK-10 share the same fingerprint — concurrent-run duplicate.
	// SNYK-10 is older (lower number) and should be kept as canonical.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}

	// SNYK-20 (the higher-identifier duplicate) must be cancelled.
	cancelledIDs := cancelledIdentifiers(linear.updates)
	if len(cancelledIDs) != 1 || cancelledIDs[0] != "SNYK-20" {
		t.Fatalf("cancelled identifiers = %v, want [SNYK-20]", cancelledIDs)
	}
}

func TestRunDuplicateCancellationIsIdempotentWhenAlreadyCancelled(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	// SNYK-20 is already Cancelled — a previous run already cleaned it up.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	// No update needed since SNYK-20 is already Cancelled.
	if result.CancelledDuplicates != 0 {
		t.Fatalf("CancelledDuplicates = %d, want 0 (already cancelled)", result.CancelledDuplicates)
	}
	if len(cancelledIdentifiers(linear.updates)) != 0 {
		t.Fatalf("expected no cancel mutations, got: %v", linear.updates)
	}
}

func TestRunThreeWayDuplicateCancelsTwoKeepsLowest(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{ID: "issue-c", Identifier: "SNYK-30", Title: "t", Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->", StateName: "Todo", Fingerprint: "snyk:project-a:issue-1"},
			{ID: "issue-a", Identifier: "SNYK-10", Title: "t", Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->", StateName: "Todo", Fingerprint: "snyk:project-a:issue-1"},
			{ID: "issue-b", Identifier: "SNYK-20", Title: "t", Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->", StateName: "Todo", Fingerprint: "snyk:project-a:issue-1"},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 2 {
		t.Fatalf("Conflicts = %d, want 2", result.Conflicts)
	}
	if result.CancelledDuplicates != 2 {
		t.Fatalf("CancelledDuplicates = %d, want 2", result.CancelledDuplicates)
	}

	cancelled := cancelledIdentifiers(linear.updates)
	if len(cancelled) != 2 {
		t.Fatalf("cancelled count = %d, want 2: %v", len(cancelled), cancelled)
	}
	for _, id := range cancelled {
		if id == "SNYK-10" {
			t.Fatalf("SNYK-10 (lowest) must not be cancelled; got cancelled: %v", cancelled)
		}
	}
}

func TestRunCancelsDuplicateAndStillSyncsCanonical(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	// SNYK-10 is canonical with a stale title — it should be updated.
	// SNYK-20 is the duplicate — it should be cancelled.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "stale title",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "stale title",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}
	// Canonical (SNYK-10) must have been updated.
	if !containsStr(updatedIdentifiers(linear.updates), "SNYK-10") {
		t.Fatalf("SNYK-10 (canonical) was not updated; updates: %v", updatedIdentifiers(linear.updates))
	}
	// Duplicate (SNYK-20) must have been cancelled.
	if !containsStr(cancelledIdentifiers(linear.updates), "SNYK-20") {
		t.Fatalf("SNYK-20 (duplicate) was not cancelled; cancelled: %v", cancelledIdentifiers(linear.updates))
	}
}

// TestRunPrefersNonTerminalTicketAsCanonical is a regression test for the
// duplicate-ticket incident: SNYK-100 has the lower identifier but is
// Cancelled (terminal); SNYK-200 has a higher identifier but is Todo
// (non-terminal). The non-terminal ticket must be picked as canonical
// regardless of identifier number. Picking the terminal ticket as canonical
// would fire the reopen guard on every run (the finding is open), dropping
// the fingerprint from the index and creating a brand-new ticket each time --
// a self-sustaining loop that minted one duplicate per run forever (this is
// how 48 duplicates accumulated for one Snyk issue in production).
func TestRunPrefersNonTerminalTicketAsCanonical(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-200",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 0 {
		t.Fatalf("PlannedCreates = %d, want 0 (must match the non-terminal SNYK-200 instead of creating a new ticket)", result.PlannedCreates)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0: %v", len(linear.created), linear.created)
	}
	if result.CancelledDuplicates != 0 {
		t.Fatalf("CancelledDuplicates = %d, want 0 (SNYK-100 is already terminal; cancelling it is a pointless mutation)", result.CancelledDuplicates)
	}
	cancelled := cancelledIdentifiers(linear.updates)
	if containsStr(cancelled, "SNYK-100") {
		t.Fatalf("SNYK-100 must not be re-cancelled; cancelled: %v", cancelled)
	}
	if containsStr(cancelled, "SNYK-200") {
		t.Fatalf("SNYK-200 (canonical) must not be cancelled; cancelled: %v", cancelled)
	}
}

// TestRunTerminalCanonicalConvergesAfterReopenGuardCreate is a regression test
// for the incident where the fingerprint's only existing ticket is terminal
// (Cancelled): run 1 correctly fires the reopen guard and creates a new
// ticket (expected/documented behavior). Run 2 must then converge: with both
// the old terminal ticket and the newly-created non-terminal ticket sharing
// the fingerprint, the non-terminal ticket becomes canonical, so no further
// ticket is created and nothing is cancelled. Before the canonical-selection
// fix, run 2 would instead pick the lower-identifier Cancelled ticket as
// canonical, fire the reopen guard again, and create yet another ticket --
// an unbounded loop that minted one duplicate per run.
func TestRunTerminalCanonicalConvergesAfterReopenGuardCreate(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "CVE-2026-1234",
		Severity:    "high",
		Status:      model.FindingOpen,
	}

	// --- Run 1: only a Cancelled ticket exists for this fingerprint. The
	// reopen guard must fire (documented expected behavior): the terminal
	// ticket is not reused, and a new ticket is created instead.
	snyk1 := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear1 := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
			},
		},
	}
	service1 := New(cfg, logger, snyk1, linear1, nil)
	result1, err := service1.Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if result1.PlannedCreates != 1 || len(linear1.created) != 1 {
		t.Fatalf("run 1: PlannedCreates = %d, created = %d, want 1 each (reopen guard should create a new ticket)", result1.PlannedCreates, len(linear1.created))
	}
	created := linear1.created[0]

	// --- Run 2: both the original Cancelled ticket and the newly-created
	// Todo ticket now exist, sharing the fingerprint, as they would once the
	// create from run 1 has landed in Linear. No new ticket should be
	// created, and neither ticket should be cancelled: this is the loop
	// terminating.
	snyk2 := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear2 := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
			},
			{
				ID:            "issue-b",
				Identifier:    "SNYK-300",
				Title:         created.Title,
				Description:   created.Description,
				DueDate:       created.DueDate,
				StateName:     "Todo",
				Fingerprint:   created.Fingerprint,
				ManagedLabels: created.ManagedLabels,
				Priority:      created.Priority,
			},
		},
	}
	service2 := New(cfg, logger, snyk2, linear2, nil)
	result2, err := service2.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}

	if result2.PlannedCreates != 0 {
		t.Fatalf("run 2: PlannedCreates = %d, want 0 (must converge, not create yet another duplicate)", result2.PlannedCreates)
	}
	if len(linear2.created) != 0 {
		t.Fatalf("run 2: created = %d, want 0: %v", len(linear2.created), linear2.created)
	}
	cancelled := cancelledIdentifiers(linear2.updates)
	if len(cancelled) != 0 {
		t.Fatalf("run 2: cancelled = %v, want none (flapping loop must terminate)", cancelled)
	}
}

// TestRunAllTerminalDuplicatesKeepsLowestIDNoCancelMutation covers the
// all-terminal duplicate case: both existing tickets are already Cancelled.
// The lowest identifier is still picked as canonical (existing behavior for
// same-class duplicates), the reopen guard fires on it producing exactly one
// new ticket, and neither pre-existing terminal ticket receives a pointless
// cancel mutation.
func TestRunAllTerminalDuplicatesKeepsLowestIDNoCancelMutation(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "CVE-2026-1234",
					Severity:    "high",
					Status:      model.FindingOpen,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-200",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "d\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-a:issue-1",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if result.CancelledDuplicates != 0 {
		t.Fatalf("CancelledDuplicates = %d, want 0 (both tickets are already terminal)", result.CancelledDuplicates)
	}
	if len(cancelledIdentifiers(linear.updates)) != 0 {
		t.Fatalf("expected no cancel mutations, got: %v", linear.updates)
	}
}

// minimalCfg returns the smallest valid Config needed to run the service in tests.
func minimalCfg() config.Config {
	return config.Config{
		Linear: config.LinearConfig{
			States: config.StateConfig{
				Todo:      "Todo",
				Backlog:   "Backlog",
				Done:      "Done",
				Cancelled: "Cancelled",
			},
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
}

func cancelledIdentifiers(updates []model.IssueUpdate) []string {
	var out []string
	for _, u := range updates {
		if u.Desired.State == model.StateCancelled {
			out = append(out, u.Existing.Identifier)
		}
	}
	return out
}

func updatedIdentifiers(updates []model.IssueUpdate) []string {
	out := make([]string, 0, len(updates))
	for _, u := range updates {
		out = append(out, u.Existing.Identifier)
	}
	return out
}

func containsStr(slice []string, s string) bool {
	return slices.Contains(slice, s)
}

// TestRunCancelsIssuesWhenProjectBecomesInactive verifies that managed Linear
// issues are cancelled when their Snyk project is de-activated (inactive).
func TestRunCancelsIssuesWhenProjectBecomesInactive(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// project-inactive has been de-activated; it still exists in Snyk but its
	// issues must be cancelled in Linear.
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{},
			ProjectIDs: map[string]struct{}{
				"project-active": {},
			},
			InactiveProjectIDs: map[string]struct{}{
				"project-inactive": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "issue from inactive project",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-inactive:issue-9",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

// TestRunCancelsIssuesWhenProjectBecomesInactiveEvenIfCached verifies that the
// cache does not prevent cancellation when a project transitions to inactive.
func TestRunCancelsIssuesWhenProjectBecomesInactiveEvenIfCached(t *testing.T) {
	cfg := config.Config{
		Cache: config.CacheConfig{},
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{},
			ProjectIDs: map[string]struct{}{
				"project-active": {},
			},
			InactiveProjectIDs: map[string]struct{}{
				"project-inactive": {},
			},
		},
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "issue from inactive project",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: "snyk:project-inactive:issue-9",
	}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			LinearHashes: map[string]string{
				existing.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{existing},
	}

	service := New(cfg, logger, snyk, linear, cacheStore)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateCancelled {
		t.Fatalf("resolved state = %q, want %q", linear.updated[0].State, model.StateCancelled)
	}
}

// TestRunDoesNotCreateIssuesForInactiveProjectFindings verifies that no new
// Linear issues are created for findings belonging to inactive Snyk projects.
// (In practice the Snyk client excludes these findings, but the service should
// not act on them even if they somehow appear in the snapshot.)
func TestRunDoesNotCreateIssuesForInactiveProjectFindings(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// A finding whose project is inactive — it must not trigger a create.
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{},
			ProjectIDs: map[string]struct{}{
				"project-active": {},
			},
			InactiveProjectIDs: map[string]struct{}{
				"project-inactive": {},
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{}}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 0 {
		t.Fatalf("PlannedCreates = %d, want 0 (inactive project findings must not be created)", result.PlannedCreates)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0", len(linear.created))
	}
}

// TestRunInactiveProjectAlreadyCancelledIsIdempotent verifies that an issue
// already in the Cancelled state is not mutated again on a subsequent run.
func TestRunInactiveProjectAlreadyCancelledIsIdempotent(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{},
			ProjectIDs: map[string]struct{}{
				"project-active": {},
			},
			InactiveProjectIDs: map[string]struct{}{
				"project-inactive": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "issue from inactive project",
				Description: "old description\n<!-- snyk-linear-sync\nfingerprint: snyk:project-inactive:issue-9\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk:project-inactive:issue-9",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0 (already cancelled)", result.PlannedResolves)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (no mutation needed)", len(linear.updated))
	}
}

func TestRunKeepsTemporaryIgnoreOpenWithExtendedDueDate(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ignoreExpires := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:     "snyk:project-a:issue-1",
					SnykIssueID:     "issue-1",
					ProjectID:       "project-a",
					ProjectName:     "Project A",
					IssueTitle:      "CVE-2026-1234",
					Severity:        "high",
					Status:          model.FindingOpen,
					CreatedAt:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
					IgnoreExpiresAt: ignoreExpires,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk: [high] CVE-2026-1234",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Temporary ignore should NOT be cancelled or moved to Backlog.
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]
	if updated.State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q", updated.State, model.StateTodo)
	}
	// Due date should be calculated from IgnoreExpiresAt (2026-06-01) + 30 days for high = 2026-07-01
	if updated.DueDate != "2026-07-01" {
		t.Fatalf("updated due date = %q, want %q", updated.DueDate, "2026-07-01")
	}
}

func TestDesiredIssueDueDateUsesIgnoreExpiresAt(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	finding := model.Finding{
		Fingerprint:     "snyk:project-a:issue-1",
		SnykIssueID:     "issue-1",
		ProjectName:     "Project A",
		IssueType:       "code",
		Severity:        "high",
		Status:          model.FindingOpen,
		CreatedAt:       time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		IgnoreExpiresAt: time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-07-01" {
		t.Fatalf("desired due date = %q, want %q", desired.DueDate, "2026-07-01")
	}
	if desired.State != model.StateTodo {
		t.Fatalf("desired state = %q, want %q", desired.State, model.StateTodo)
	}
}

func TestRunRespectsManualBacklogMove(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Backlog",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Backlog override should prevent state-only update)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunUpdatesTitleButKeepsBacklogState(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Backlog",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (title changed, state kept in Backlog)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateBacklog {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateBacklog)
	}
}

func TestRunDoesNotOverrideBacklogForFixedFindings(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingFixed,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Backlog",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (fixed finding should move to Done)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].State != model.StateDone {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateDone)
	}
}

func TestRunRespectsManualTodoMove(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Todo override should prevent state-only update)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

func TestRunUpdatesTitleButKeepsTodoState(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (title changed, state kept in Todo)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].PreserveState != true {
		t.Fatalf("updated[0].PreserveState = %v, want true", linear.updated[0].PreserveState)
	}
}

func TestIsConfiguredBacklogState(t *testing.T) {
	cases := []struct {
		existing   string
		configured string
		want       bool
	}{
		{"Backlog", "Backlog", true},
		{"backlog", "Backlog", true},
		{"BACKLOG", "Backlog", true},
		{"Todo", "Backlog", false},
		{"Done", "Backlog", false},
		{"Cancelled", "Backlog", false},
		{"", "Backlog", false},
		{"Backlog", "", false},
	}
	for _, tc := range cases {
		got := isConfiguredBacklogState(tc.existing, tc.configured)
		if got != tc.want {
			t.Errorf("isConfiguredBacklogState(%q, %q) = %v, want %v", tc.existing, tc.configured, got, tc.want)
		}
	}
}

// TestDesiredIssueDueDateUsesIgnoreExpiryForExpiredSnooze verifies that when an
// expired snooze still produces a future due date (IgnoreExpiresAt + offset),
// the due date is set correctly. When the result would be in the past, the
// due date is omitted instead (see TestDesiredIssueDueDateOmitsPastDueDateFromExpiredSnooze).
func TestDesiredIssueDueDateUsesIgnoreExpiryForExpiredSnooze(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
	}
	// The snooze expired on a date that still produces a future due date
	// when the offset is added.
	ignoreExpiresAt := time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC)
	finding := model.Finding{
		Fingerprint:     "snyk:project-a:issue-1",
		SnykIssueID:     "issue-1",
		ProjectName:     "Project A",
		IssueType:       "code",
		Severity:        "high",
		Status:          model.FindingOpen,
		CreatedAt:       time.Date(2026, time.April, 10, 8, 29, 14, 0, time.UTC),
		IgnoreExpiresAt: ignoreExpiresAt,
	}

	desired := desiredIssue(cfg, finding)

	// Due date should be IgnoreExpiresAt (August 29) + 30 days = September 28.
	if desired.DueDate != "2026-09-28" {
		t.Fatalf("desired due date = %q, want %q (ignore expiry + high offset)", desired.DueDate, "2026-09-28")
	}
}

// TestDesiredIssueDisregardIfFixableMapsToBacklog verifies that an issue with
// disregardIfFixable=true is mapped to FindingAwaitingFix, placed in Backlog
// with no due date, and receives the triage-dependency label.
func TestDesiredItemDisregardIfFixableMapsToBacklog(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	finding := model.Finding{
		Fingerprint:        "snyk:project-a:issue-1",
		SnykIssueID:        "issue-1",
		ProjectName:        "Project A",
		IssueType:          "package_vulnerability",
		Severity:           "medium",
		Status:             model.FindingAwaitingFix,
		CreatedAt:          time.Date(2026, time.April, 30, 11, 59, 47, 0, time.UTC),
		DisregardIfFixable: true,
	}

	desired := desiredIssue(cfg, finding)

	if desired.State != model.StateBacklog {
		t.Fatalf("desired state = %q, want %q for disregard-if-fixable issue", desired.State, model.StateBacklog)
	}
	if desired.DueDate != "" {
		t.Fatalf("desired due date = %q, want empty for disregard-if-fixable issue", desired.DueDate)
	}
	if desired.DueDateBase != "" {
		t.Fatalf("desired due date base = %q, want empty for disregard-if-fixable issue", desired.DueDateBase)
	}
	labelFound := false
	for _, label := range desired.ManagedLabels {
		if label == "triage-dependency" {
			labelFound = true
		}
	}
	if !labelFound {
		t.Fatalf("managed labels = %v, want triage-dependency for disregard-if-fixable issue", desired.ManagedLabels)
	}
	if !strings.Contains(desired.Description, "ignored (no fix available)") {
		t.Fatalf("description should contain 'ignored (no fix available)', got: %s", desired.Description)
	}
}

// TestNeedsUpdateAlwaysCorrectsDueDate verifies that the sync always flags
// a due date update when the desired date differs from the existing one.
// Snyk is authoritative: the sync must correct manual overrides and stale
// dates, even if the desired date is a floored "today" value.
func TestNeedsUpdateAlwaysCorrectsDueDate(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-07-15", // manually-overridden future date
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-01-31", // Snyk-derived SLA date (authoritative)
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true (Snyk-derived due date must correct manual override)")
	}
}

// TestNeedsUpdateStillDetectsDueDateChangeWhenBothNonEmpty verifies that the
// due date change detection still works when both dates are non-empty.
func TestNeedsUpdateStillDetectsDueDateChangeWhenBothNonEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-07-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "2026-07-15",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired, config.StateConfig{}) {
		t.Fatal("needsUpdate() = false, want true (due dates differ)")
	}
}

// TestRunSetsRawSLADateOnNewIssueWithOldCreatedAt verifies that when a new
// Snyk finding has an old CreatedAt, the sync creates the Linear issue with
// the raw SLA date (past dates show as overdue in Linear and indicate how long
// the issue has exceeded its SLA).
func TestRunSetsFlooredDueDateOnNewIssueWithOldCreatedAt(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Old issue",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), // old → due date would be past
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{}}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if linear.created[0].DueDate != "2026-01-31" {
		t.Fatalf("created due date = %q, want %q (raw SLA date for past issues)", linear.created[0].DueDate, "2026-01-31")
	}
}

// TestRunCorrectsOverriddenDueDateWithAuthoritativeCalculation verifies that
// when the Snyk-derived due date differs from the Linear due date, the sync
// always updates it — Snyk is authoritative, even over manual overrides.
func TestRunCorrectsOverriddenDueDateWithAuthoritativeCalculation(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Old issue",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     "2026-07-15", // manually-overridden future date
				StateName:   "Todo",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The sync must correct the manually-overridden due date to the
	// authoritative Snyk-derived SLA date.
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (Snyk due date must correct manual override)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].DueDate != "2026-01-31" {
		t.Fatalf("updated due date = %q, want %q (authoritative Snyk SLA date)", linear.updated[0].DueDate, "2026-01-31")
	}
}

// TestRunUpdatedAtFallbackDueDateIsStickyAcrossRuns is a regression test for
// due-date churn: once the updated_at re-detection fallback fires (see
// issueDueDate), Snyk bumps updated_at on every routine re-scan, not just
// genuine re-detections. Without stickiness, the due date would advance every
// run, producing an endless stream of Linear due-date updates and
// change-comments. Once a matched ticket already has a due date, a second run
// must keep it fixed even though the finding's updated_at has moved on.
func TestRunUpdatedAtFallbackDueDateIsStickyAcrossRuns(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// created_at is far enough in the past, and updated_at far enough beyond
	// created_at plus the high-severity SLA window (30 days), that the
	// updated_at re-detection fallback fires on both runs.
	finding1 := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "CVE-2026-1234",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	snyk1 := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding1},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear1 := &fakeLinear{snapshot: []model.ExistingIssue{}}

	service1 := New(cfg, logger, snyk1, linear1, nil)
	result1, err := service1.Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if result1.PlannedCreates != 1 || len(linear1.created) != 1 {
		t.Fatalf("run 1: PlannedCreates = %d, created = %d, want 1 each", result1.PlannedCreates, len(linear1.created))
	}
	created := linear1.created[0]
	// Jan 1 2026 (updated_at) + 30 days = Jan 31 2026.
	if created.DueDate != "2026-01-31" {
		t.Fatalf("run 1: created due date = %q, want %q", created.DueDate, "2026-01-31")
	}

	// --- Run 2: the finding's updated_at has advanced by a day, as it would
	// from a routine Snyk re-scan (not a genuine re-detection). The existing
	// ticket already has the due date set from run 1.
	finding2 := finding1
	finding2.UpdatedAt = finding1.UpdatedAt.AddDate(0, 0, 1)

	snyk2 := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding2},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear2 := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "issue-a",
				Identifier:    "SNYK-500",
				Title:         created.Title,
				Description:   created.Description,
				DueDate:       created.DueDate,
				StateName:     "Todo",
				Fingerprint:   created.Fingerprint,
				ManagedLabels: created.ManagedLabels,
				Priority:      created.Priority,
			},
		},
	}
	service2 := New(cfg, logger, snyk2, linear2, nil)
	result2, err := service2.Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}

	if result2.PlannedUpdates != 0 {
		t.Fatalf("run 2: PlannedUpdates = %d, want 0 (due date must not churn once set)", result2.PlannedUpdates)
	}
	if len(linear2.updated) != 0 {
		t.Fatalf("run 2: updated = %v, want none", linear2.updated)
	}
}

// TestRunUpdatedAtFallbackStillSetsDueDateWhenNeverSet verifies that a
// matched ticket which never had a due date still gets one computed from the
// updated_at fallback -- the stickiness added to prevent due-date churn must
// not suppress setting a due date for the first time.
func TestRunUpdatedAtFallbackStillSetsDueDateWhenNeverSet(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "CVE-2026-1234",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}
	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-500",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     "", // never set
				StateName:   "Todo",
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (due date must be set for the first time)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if linear.updated[0].DueDate != "2026-01-31" {
		t.Fatalf("updated due date = %q, want %q", linear.updated[0].DueDate, "2026-01-31")
	}
}

// TestRunAwaitingFixIssueGoesToBacklogWithNoDueDate verifies the full flow for
// a Snyk finding with disregardIfFixable=true: the sync creates the Linear
// issue in Backlog with no due date and the triage-dependency label.
func TestRunAwaitingFixIssueGoesToBacklogWithNoDueDate(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:        "snyk:project-a:issue-1",
					SnykIssueID:        "issue-1",
					ProjectID:          "project-a",
					ProjectName:        "Project A",
					IssueTitle:         "XSS in postcss",
					IssueType:          "package_vulnerability",
					Severity:           "medium",
					Status:             model.FindingAwaitingFix,
					CreatedAt:          time.Date(2026, time.April, 24, 20, 20, 42, 0, time.UTC),
					DisregardIfFixable: true,
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{}}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1", result.PlannedCreates)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	created := linear.created[0]
	if created.State != model.StateBacklog {
		t.Fatalf("created state = %q, want %q", created.State, model.StateBacklog)
	}
	if created.DueDate != "" {
		t.Fatalf("created due date = %q, want empty for awaiting-fix issue", created.DueDate)
	}
	labelFound := false
	for _, label := range created.ManagedLabels {
		if label == "triage-dependency" {
			labelFound = true
		}
	}
	if !labelFound {
		t.Fatalf("created managed labels = %v, want triage-dependency", created.ManagedLabels)
	}
	if !strings.Contains(created.Description, "ignored (no fix available)") {
		t.Fatalf("description should contain 'ignored (no fix available)'")
	}
}

// TestRunAwaitingFixIssueMovedFromTodoToBacklog verifies that when an existing
// issue was previously synced as Todo (before the awaiting-fix feature) and
// the Snyk finding now maps to FindingAwaitingFix, the sync moves it to
// Backlog, clears the due date, and adds the triage-dependency label.
func TestRunAwaitingFixIssueMovedFromTodoToBacklog(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:        "snyk:project-a:issue-1",
		SnykIssueID:        "issue-1",
		ProjectID:          "project-a",
		ProjectName:        "Project A",
		IssueTitle:         "XSS in postcss",
		IssueType:          "package_vulnerability",
		Severity:           "medium",
		Status:             model.FindingAwaitingFix,
		CreatedAt:          time.Date(2026, time.April, 24, 20, 20, 42, 0, time.UTC),
		DisregardIfFixable: true,
	}

	desired := desiredIssue(cfg, finding)

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         desired.Title,
				Description:   desired.Description,
				DueDate:       "2026-06-08", // old due date from before awaiting-fix
				StateName:     "Todo",       // was in Todo before
				Fingerprint:   finding.Fingerprint,
				Priority:      desired.Priority,
				ManagedLabels: []string{"snyk-automation"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]
	if updated.State != model.StateBacklog {
		t.Fatalf("updated state = %q, want %q", updated.State, model.StateBacklog)
	}
	if updated.DueDate != "" {
		t.Fatalf("updated due date = %q, want empty (cleared for awaiting-fix)", updated.DueDate)
	}
	labelFound := false
	for _, label := range updated.ManagedLabels {
		if label == "triage-dependency" {
			labelFound = true
		}
	}
	if !labelFound {
		t.Fatalf("updated managed labels = %v, want triage-dependency", updated.ManagedLabels)
	}
}

func TestComputeDiffDetectsAllChanges(t *testing.T) {
	existing := model.ExistingIssue{
		ID:            "issue-1",
		Identifier:    "SEC-1",
		Title:         "old title",
		Description:   "old description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      3,
		ManagedLabels: []string{"snyk-automation"},
		Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-automation"}},
	}
	desired := model.DesiredIssue{
		Title:         "new title",
		Description:   "new description",
		DueDate:       "2026-05-01",
		State:         model.StateBacklog,
		Priority:      1,
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if !diff.TitleChanged {
		t.Fatal("expected TitleChanged")
	}
	if diff.TitleFrom != "old title" || diff.TitleTo != "new title" {
		t.Fatalf("title diff = %q → %q", diff.TitleFrom, diff.TitleTo)
	}
	if !diff.DescriptionChanged {
		t.Fatal("expected DescriptionChanged")
	}
	if !diff.DueDateChanged {
		t.Fatal("expected DueDateChanged")
	}
	if diff.DueDateFrom != "2026-04-01" || diff.DueDateTo != "2026-05-01" {
		t.Fatalf("due date diff = %q → %q", diff.DueDateFrom, diff.DueDateTo)
	}
	if !diff.StateChanged {
		t.Fatal("expected StateChanged")
	}
	if diff.StateTo != "backlog" {
		t.Fatalf("state to = %q", diff.StateTo)
	}
	if !diff.PriorityChanged {
		t.Fatal("expected PriorityChanged")
	}
	if diff.PriorityFrom != 3 || diff.PriorityTo != 1 {
		t.Fatalf("priority diff = %d → %d", diff.PriorityFrom, diff.PriorityTo)
	}
	if len(diff.LabelsAdded) != 1 || diff.LabelsAdded[0] != "snyk-code" {
		t.Fatalf("labels added = %v, want [snyk-code]", diff.LabelsAdded)
	}
	if len(diff.LabelsRemoved) != 0 {
		t.Fatalf("labels removed = %v, want []", diff.LabelsRemoved)
	}
	if !diff.LabelsNeedUpdate {
		t.Fatal("expected LabelsNeedUpdate when labels are added")
	}
}

func TestComputeDiffDetectsNoChanges(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "same title",
		Description:   "same description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      2,
		ManagedLabels: []string{"snyk-automation"},
	}
	desired := model.DesiredIssue{
		Title:         "same title",
		Description:   "same description",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		Priority:      2,
		ManagedLabels: []string{"snyk-automation"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if diff.TitleChanged || diff.DescriptionChanged || diff.DueDateChanged ||
		diff.StateChanged || diff.PriorityChanged || diff.LabelsNeedUpdate ||
		len(diff.LabelsAdded) > 0 || len(diff.LabelsRemoved) > 0 {
		t.Fatalf("expected no changes, got: %+v", diff)
	}
}

func TestComputeDiffDetectsLabelRemoval(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      2,
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
		Labels: []model.IssueLabel{
			{ID: "l1", Name: "snyk-automation"},
			{ID: "l2", Name: "snyk-code"},
		},
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		Priority:      2,
		ManagedLabels: []string{"snyk-automation"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if len(diff.LabelsRemoved) != 1 || diff.LabelsRemoved[0] != "snyk-code" {
		t.Fatalf("labels removed = %v, want [snyk-code]", diff.LabelsRemoved)
	}
	if !diff.LabelsNeedUpdate {
		t.Fatal("expected LabelsNeedUpdate when labels are removed")
	}
}

func TestComputeDiffNoStateChangeWhenPreserveState(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "desc",
		DueDate:     "2026-04-01",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		State:         model.StateBacklog,
		PreserveState: true,
		ManagedLabels: []string{"snyk-automation"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if diff.StateChanged {
		t.Fatal("expected no state change when PreserveState=true")
	}
}

func TestComputeDiffDetectsLabelNotOnIssue(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      2,
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
		// snyk-code is managed but not actually on the issue (failed to apply).
		Labels: []model.IssueLabel{
			{ID: "l1", Name: "snyk-automation"},
		},
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		Priority:      2,
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if !diff.LabelsNeedUpdate {
		t.Fatal("expected LabelsNeedUpdate when managed label is not on issue")
	}
	if len(diff.LabelsAdded) != 0 {
		t.Fatalf("LabelsAdded = %v, want empty (label was in previous managed set)", diff.LabelsAdded)
	}
	if len(diff.LabelsRemoved) != 0 {
		t.Fatalf("LabelsRemoved = %v, want empty", diff.LabelsRemoved)
	}
}

// TestComputeDiffNoFalseLabelRemovalWhenLabelAlreadyGone verifies that a
// previously managed label that is no longer desired AND already manually
// removed from the issue is NOT reported as LabelsRemoved. This prevents
// misleading change comments like "Removed X — no longer applicable" when
// the label isn't actually on the issue anymore.
func TestComputeDiffNoFalseLabelRemovalWhenLabelAlreadyGone(t *testing.T) {
	existing := model.ExistingIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      2,
		ManagedLabels: []string{"snyk-automation", "snyk-code"},
		// snyk-code was previously managed but has been manually removed.
		Labels: []model.IssueLabel{
			{ID: "l1", Name: "snyk-automation"},
		},
	}
	desired := model.DesiredIssue{
		Title:         "title",
		Description:   "desc",
		DueDate:       "2026-04-01",
		State:         model.StateTodo,
		Priority:      2,
		ManagedLabels: []string{"snyk-automation"},
	}

	diff := ComputeDiff(existing, desired, config.StateConfig{})

	if len(diff.LabelsRemoved) != 0 {
		t.Fatalf("LabelsRemoved = %v, want empty (snyk-code is not on the issue)", diff.LabelsRemoved)
	}
	// LabelsNeedUpdate should be false because snyk-code is already absent from
	// the issue and snyk-automation is still present. The metadata block change
	// is captured by DescriptionChanged, not LabelsNeedUpdate.
	if diff.LabelsNeedUpdate {
		t.Fatal("expected no LabelsNeedUpdate since the label IDs on the issue are already correct")
	}
}

func TestRunPostsChangeCommentsOnUpdate(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Updated title",
					PackageName: "github.com/example/pkg",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    3,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(linear.comments))
	}
}

// TestRunPreservesNonTerminalStateWhenUserMovesToTodo verifies the core bug
// fix: when the configured open state is "Triage" and a user manually moves
// an open finding's issue to "Todo", the sync must not drag it back to
// "Triage". This was the original isManualTodoState check, now generalized
// to cover any non-terminal state.
func TestRunPreservesNonTerminalStateWhenUserMovesToTodo(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage" // simulate the real workspace config
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	// The user manually moved the issue from Triage to Todo.
	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo", // not the configured open state "Triage"
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Todo state should be preserved)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

// TestRunPreservesNonTerminalStateWhenUserMovesToInProgress verifies that the
// general non-terminal state override also covers custom states like "In
// Progress" — not just the hardcoded "todo" that the old isManualTodoState
// checked for.
func TestRunPreservesNonTerminalStateWhenUserMovesToInProgress(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "In Progress", // custom non-terminal state
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (In Progress state should be preserved)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

// TestRunPreservesTodoWhenFindingIsAwaitingFix verifies that the sync does
// not override a user's manual move from Backlog to Todo when the Snyk
// finding is still in "awaiting fix" status. The old isManualTodoState check
// only fired when desired.State was Todo; when the finding was awaiting fix
// (desired Backlog), the check was bypassed and the issue was dragged back
// to Backlog.
func TestRunPreservesTodoWhenFindingIsAwaitingFix(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:        "snyk:project-a:issue-1",
		SnykIssueID:        "issue-1",
		ProjectID:          "project-a",
		ProjectName:        "Project A",
		IssueTitle:         "Outdated package",
		Severity:           "high",
		Status:             model.FindingAwaitingFix,
		CreatedAt:          time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		DisregardIfFixable: true,
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	// The user manually moved the issue from Backlog to Todo despite the
	// finding still being awaiting-fix. The sync must respect this.
	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         desired.Title,
				Description:   desired.Description,
				DueDate:       "", // awaiting-fix has no due date
				StateName:     "Todo",
				Fingerprint:   finding.Fingerprint,
				Priority:      desired.Priority,
				ManagedLabels: desired.ManagedLabels,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (Todo state should be preserved even when finding is awaiting fix)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0", len(linear.updated))
	}
}

// TestRunDoesNotPreserveTerminalStates verifies that a terminal (Done) ticket
// is never reopened when Snyk reports the finding as open. Instead of updating
// the closed ticket back to an open state, the sync creates a new ticket. This
// is the reopen guard: Snyk reusing a problem-type issueID is not a directive
// to reopen a closed Linear ticket.
func TestRunDoesNotPreserveTerminalStates(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	cfg.Linear.States.Done = "Done"
	cfg.Linear.States.Cancelled = "Cancelled"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Done", // terminal state — must not be reopened
				Fingerprint: finding.Fingerprint,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// A new ticket should be created, not an update to the closed one.
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (should create new ticket, not reopen Done)", result.PlannedCreates)
	}
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (must not reopen terminal ticket)", result.PlannedUpdates)
	}
}

// TestRunPreservesNonTerminalStateWithOtherFieldChanges verifies that when
// PreserveState is set due to a non-terminal state override, an update that
// changes OTHER fields (like title) still omits stateId from the mutation
// so the user's state choice is preserved.
func TestRunPreservesNonTerminalStateWithOtherFieldChanges(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "New title from Snyk",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Stale title", // different from desired
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo", // user moved to Todo — should be preserved
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (title changed, state preserved)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	// The state should be preserved as Todo via PreserveState.
	// The user moved the issue to Todo; the sync should not override it
	// back to the configured open state (Triage).
	if linear.updated[0].State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q (Todo state should be preserved when other fields change)", linear.updated[0].State, model.StateTodo)
	}
}

// TestRunNonTerminalOverrideDoesNotFireWhenStateMatchesConfig verifies that
// PreserveState is NOT set when the existing state already matches the
// configured state for the desired model state. This prevents the sync from
// unnecessarily adding ":preserve" to the Snyk hash.
func TestRunNonTerminalOverrideDoesNotFireWhenStateMatchesConfig(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Outdated package",
		Severity:    "high",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	desired := desiredIssue(cfg, finding)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Triage", // matches configured Todo state
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (no override needed when state matches config)", result.PlannedUpdates)
	}
}

func TestRunSkipsCommentsForResolve(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "resolved issue",
				Description:   "old description\n\u003c!-- snyk-linear-sync\nfingerprint: snyk:project-z:issue-9\n--\u003e",
				StateName:     "Todo",
				Fingerprint:   "snyk:project-z:issue-9",
				ManagedLabels: []string{"snyk-automation"},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	if len(linear.comments) != 0 {
		t.Fatalf("comments = %d, want 0 (no comments for resolve)", len(linear.comments))
	}
}

// TestRunAwaitingFixToOpenRecalculatesDueDateFromFixAvailability verifies that
// when a Snyk finding transitions from awaiting-fix to open (fix became
// available), the due date is recalculated from today instead of the
// original created_at — because the old SLA date is meaningless when the
// team couldn't act on the issue while no fix existed.
func TestRunAwaitingFixToOpenRecalculatesDueDateFromFixAvailability(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen,                                      // fix is now available
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), // months ago
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "Snyk: [high] XSS in postcss",
				Description:   "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation,triage-dependency\n-->",
				DueDate:       "",        // was cleared for awaiting-fix
				StateName:     "Backlog", // was in Backlog for awaiting-fix
				Fingerprint:   finding.Fingerprint,
				Priority:      2,
				ManagedLabels: []string{"snyk-automation", "triage-dependency"}, // had the awaiting-fix label
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}, {ID: "l2", Name: "triage-dependency"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]

	// Due date should be calculated from today + 30 days (high severity SLA),
	// NOT from 2026-01-01 + 30 days (which would give 2026-01-31, a meaningless
	// past date for an issue that was blocked for months).
	expectedDueDate := time.Now().UTC().AddDate(0, 0, 30)
	expectedStr := time.Date(expectedDueDate.Year(), expectedDueDate.Month(), expectedDueDate.Day(), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	if updated.DueDate != expectedStr {
		t.Fatalf("updated due date = %q, want %q (SLA from fix availability, not from months-old created_at)", updated.DueDate, expectedStr)
	}

	// The triage-dependency label must be gone from the desired set and
	// reported as removed in the diff: a loop over the desired set alone would
	// pass vacuously for any set that does not happen to contain it.
	for _, label := range updated.ManagedLabels {
		if label == "triage-dependency" {
			t.Fatalf("updated managed labels should not contain triage-dependency after fix is available: %v", updated.ManagedLabels)
		}
	}
	if len(linear.updates) != 1 || linear.updates[0].Diff == nil {
		t.Fatalf("updates = %d, want one diffed update", len(linear.updates))
	}
	if !slices.Contains(linear.updates[0].Diff.LabelsRemoved, "triage-dependency") {
		t.Fatalf("Diff.LabelsRemoved = %v, want it to contain triage-dependency", linear.updates[0].Diff.LabelsRemoved)
	}
	// Label management is on here, so the global label stays: this is what
	// separates this test from the turn-off case, where it is removed too.
	if slices.Contains(linear.updates[0].Diff.LabelsRemoved, "snyk-automation") {
		t.Fatalf("Diff.LabelsRemoved = %v, must not contain snyk-automation while label management is on", linear.updates[0].Diff.LabelsRemoved)
	}

	// The sync parked this ticket in Backlog itself, so the manual Backlog
	// override must not apply: the ticket moves back to Todo, and the update
	// must actually carry the state change instead of preserving Backlog.
	if updated.State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q (fix arrived, ticket must leave Backlog)", updated.State, model.StateTodo)
	}
	if updated.PreserveState {
		t.Fatalf("updated PreserveState = true, want false (Backlog was the sync's own placement, not a manual move)")
	}
	if len(linear.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(linear.updates))
	}
	if !linear.updates[0].Diff.StateChanged {
		t.Fatalf("Diff.StateChanged = false, want true (state must be sent to Linear)")
	}
	if linear.updates[0].Diff.StateTo != "todo" {
		t.Fatalf("Diff.StateTo = %q, want %q", linear.updates[0].Diff.StateTo, "todo")
	}
}

// TestRunAwaitingFixToOpenKeepsManuallyChosenInProgress verifies that only the
// Backlog state the sync chose for an awaiting-fix finding is overridden when
// the fix arrives. A person who moved that ticket to another non-terminal
// state (here In Progress) made a real triage decision, and the sync keeps it:
// the label is still removed and the due date recalculated, but no stateId is
// sent.
func TestRunAwaitingFixToOpenKeepsManuallyChosenInProgress(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen,                                      // fix is now available
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), // months ago
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "Snyk: [high] XSS in postcss",
				Description:   "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation,triage-dependency\n-->",
				DueDate:       "",            // was cleared for awaiting-fix
				StateName:     "In Progress", // a person moved it here, not the sync
				Fingerprint:   finding.Fingerprint,
				Priority:      2,
				ManagedLabels: []string{"snyk-automation", "triage-dependency"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}, {ID: "l2", Name: "triage-dependency"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]

	if !updated.PreserveState {
		t.Fatalf("updated PreserveState = false, want true (a manual non-terminal state must be kept)")
	}
	if len(linear.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(linear.updates))
	}
	if linear.updates[0].Diff.StateChanged {
		t.Fatalf("Diff.StateChanged = true, want false (the user's In Progress state must not be overridden)")
	}

	// The label removal happens regardless of which state the ticket sits in:
	// the awaiting-fix condition is over, so the label goes. It must be gone
	// from the desired set and reported as removed in the diff: a loop over the
	// desired set alone would pass vacuously for a set that does not contain it.
	for _, label := range updated.ManagedLabels {
		if label == "triage-dependency" {
			t.Fatalf("updated managed labels should not contain triage-dependency after fix is available: %v", updated.ManagedLabels)
		}
	}
	if len(linear.updates) != 1 || linear.updates[0].Diff == nil {
		t.Fatalf("updates = %d, want one diffed update", len(linear.updates))
	}
	if !slices.Contains(linear.updates[0].Diff.LabelsRemoved, "triage-dependency") {
		t.Fatalf("Diff.LabelsRemoved = %v, want it to contain triage-dependency", linear.updates[0].Diff.LabelsRemoved)
	}
	// Label management is on here, so the global label stays: this is what
	// separates this test from the turn-off case, where it is removed too.
	if slices.Contains(linear.updates[0].Diff.LabelsRemoved, "snyk-automation") {
		t.Fatalf("Diff.LabelsRemoved = %v, must not contain snyk-automation while label management is on", linear.updates[0].Diff.LabelsRemoved)
	}
}

// TestRunAwaitingFixLabelOffLeavesBacklogAlone pins the backward-compatibility
// contract for tickets written before the awaiting_fix marker existed: a
// Backlog ticket whose metadata block carries neither the awaiting-fix label
// (the label is off) nor the marker cannot be told apart from a manual Backlog
// move, so the sync leaves it in Backlog. No one-time mass update fires on
// upgrade; a legacy ticket only becomes eligible for the automatic move after
// the sync rewrites its description (any later awaiting-fix run adds the
// marker).
func TestRunAwaitingFixLabelOffLeavesBacklogAlone(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "" // LINEAR_AWAITING_FIX_LABEL=off
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen, // fix is now available
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "stale title", // title change so an update is produced
				Description:   "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation\n-->",
				DueDate:       "",
				StateName:     "Backlog",
				Fingerprint:   finding.Fingerprint,
				Priority:      2,
				ManagedLabels: []string{"snyk-automation"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (title changed)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]

	if !updated.PreserveState {
		t.Fatalf("updated PreserveState = false, want true (no awaiting-fix label: Backlog stays put)")
	}
	if updated.State != model.StateBacklog {
		t.Fatalf("updated state = %q, want %q", updated.State, model.StateBacklog)
	}
}

// TestRunAwaitingFixLabelOffMarkerMovesBacklogToTodo verifies that the
// awaiting_fix marker in the metadata block — which the sync writes whenever
// a finding is in the awaiting-fix state, regardless of the label
// configuration — lets the sync recognize its own Backlog placement even with
// LINEAR_AWAITING_FIX_LABEL=off. When the fix arrives the ticket moves back to
// Todo with a recalculated due date, and the marker is cleared.
func TestRunAwaitingFixLabelOffMarkerMovesBacklogToTodo(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "" // LINEAR_AWAITING_FIX_LABEL=off
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen,                                      // fix is now available
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC), // months ago
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk: [high] XSS in postcss",
				Description: "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation\nawaiting_fix: yes\n-->",
				DueDate:     "",        // was cleared for awaiting-fix
				StateName:   "Backlog", // the sync parked it here for the awaiting-fix finding
				Fingerprint: finding.Fingerprint,
				Priority:    2,
				// No awaiting-fix label recorded: the label is off. Only the
				// marker can identify this as the sync's own placement.
				ManagedLabels: []string{"snyk-automation"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}},
				// What extractAwaitingFix returns for the description above.
				AwaitingFix: true,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]

	// The marker identifies the Backlog placement as the sync's own, so the
	// manual Backlog override must not apply: the ticket moves to Todo and
	// the update carries the state change.
	if updated.State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q (awaiting_fix marker, fix arrived)", updated.State, model.StateTodo)
	}
	if updated.PreserveState {
		t.Fatalf("updated PreserveState = true, want false (Backlog was the sync's own placement)")
	}
	if len(linear.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(linear.updates))
	}
	if !linear.updates[0].Diff.StateChanged {
		t.Fatalf("Diff.StateChanged = false, want true (state must be sent to Linear)")
	}

	// Due date is recalculated from fix availability, not from the months-old
	// created_at the SLA was originally based on.
	expectedDueDate := time.Now().UTC().AddDate(0, 0, 30)
	expectedStr := time.Date(expectedDueDate.Year(), expectedDueDate.Month(), expectedDueDate.Day(), 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
	if updated.DueDate != expectedStr {
		t.Fatalf("updated due date = %q, want %q (SLA from fix availability)", updated.DueDate, expectedStr)
	}

	// The marker has served its purpose and must not outlive the
	// awaiting-fix state.
	if strings.Contains(updated.Description, "awaiting_fix") {
		t.Fatalf("awaiting_fix marker should be cleared once the fix arrived:\n%s", updated.Description)
	}
}

// TestRunAwaitingFixStaysInBacklogWhileStillAwaiting verifies that nothing
// moves while the find is still missing: the ticket stays in Backlog with no
// due date, even when the title changes enough to produce an update.
func TestRunAwaitingFixStaysInBacklogWhileStillAwaiting(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingAwaitingFix, // still no fix
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "stale title", // title change so an update is produced
				Description:   "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation,triage-dependency\n-->",
				DueDate:       "",
				StateName:     "Backlog",
				Fingerprint:   finding.Fingerprint,
				Priority:      2,
				ManagedLabels: []string{"snyk-automation", "triage-dependency"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}, {ID: "l2", Name: "triage-dependency"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (title changed)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	updated := linear.updated[0]

	if updated.State != model.StateBacklog {
		t.Fatalf("updated state = %q, want %q", updated.State, model.StateBacklog)
	}
	if updated.DueDate != "" {
		t.Fatalf("updated due date = %q, want \"\" (SLA paused while the fix is still missing)", updated.DueDate)
	}
}

// TestRunAwaitingFixToOpenPostsStateChangeComment drives the first scenario
// through the real entry point with comments enabled, so the whole run —
// match, override logic, diff, update, comment — is exercised. The pick-up at
// Backlog is the sync's own placement, so the change comment must explain the
// move out of Backlog and mention the label it removed.
func TestRunAwaitingFixToOpenPostsStateChangeComment(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	cfg.Linear.Labels.AwaitingFix = "triage-dependency"
	cfg.Linear.Labels.Managed = "snyk-automation"
	cfg.Linear.CommentsEnabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "XSS in postcss",
		IssueType:   "package_vulnerability",
		Severity:    "high",
		Status:      model.FindingOpen, // fix is now available
		CreatedAt:   time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "Snyk: [high] XSS in postcss",
				Description:   "old body\n<!-- snyk-linear-sync\nfingerprint: snyk:project-a:issue-1\nmanaged_labels: snyk-automation,triage-dependency\n-->",
				DueDate:       "",
				StateName:     "Backlog",
				Fingerprint:   finding.Fingerprint,
				Priority:      2,
				ManagedLabels: []string{"snyk-automation", "triage-dependency"},
				Labels:        []model.IssueLabel{{ID: "l1", Name: "snyk-automation"}, {ID: "l2", Name: "triage-dependency"}},
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	if len(linear.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(linear.comments))
	}
	comment := linear.comments[0]
	if comment.Diff == nil {
		t.Fatal("comment Diff = nil, want a populated diff")
	}
	if !comment.Diff.StateChanged {
		t.Fatalf("comment Diff.StateChanged = false, want true")
	}
	if !strings.Contains(comment.Desired.StateReason, "fix is now available") {
		t.Fatalf("comment StateReason = %q, want it to contain %q", comment.Desired.StateReason, "fix is now available")
	}
	if !slices.Contains(comment.Diff.LabelsRemoved, "triage-dependency") {
		t.Fatalf("comment Diff.LabelsRemoved = %v, want it to contain triage-dependency", comment.Diff.LabelsRemoved)
	}
}

// TestRunDoesNotReopenTerminalTicketForCoarseFingerprint reproduces the
// SNYK-6582 bug: a Done ticket with the old coarse fingerprint
// snyk:project:issue is matched when Snyk re-reports the same issue ID as
// open. The reopen guard must prevent reusing the closed ticket and instead
// create a fresh one.
func TestRunDoesNotReopenTerminalTicketForCoarseFingerprint(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Done = "Done"
	cfg.Linear.States.Cancelled = "Cancelled"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Path traversal",
		Severity:    "low",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SNYK-6582",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Done",
				Fingerprint: "snyk:project-a:issue-1",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// A new ticket should be created, not an update to the closed one.
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (should create new ticket for reopened finding)", result.PlannedCreates)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (must not reopen terminal ticket)", result.PlannedUpdates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (must not send update for Done ticket)", len(linear.updated))
	}

	// The created ticket should carry the same fingerprint as the finding.
	if linear.created[0].Fingerprint != finding.Fingerprint {
		t.Fatalf("created fingerprint = %q, want %q", linear.created[0].Fingerprint, finding.Fingerprint)
	}
}

// TestRunDoesNotReopenTerminalTicketForFineGrainedFingerprint tests the case
// where Snyk starts reporting a fine-grained fingerprint (with location key)
// for an issue whose old coarse-fingerprint ticket is Done. The closed ticket
// must not be matched via the coarse fallback, and a new ticket must be created.
func TestRunDoesNotReopenTerminalTicketForFineGrainedFingerprint(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Done = "Done"
	cfg.Linear.States.Cancelled = "Cancelled"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1:e2e/prerequisite_gate.py",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Path traversal",
		Severity:    "low",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SNYK-6582",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Done",
				Fingerprint: "snyk:project-a:issue-1", // old coarse fingerprint
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (should create new ticket for new code occurrence)", result.PlannedCreates)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (must not match terminal ticket via coarse fallback)", result.PlannedUpdates)
	}
	// The old Done ticket may be resolved (no-op update keeping it Done),
	// but it must not be reopened. Verify no update changed its state to
	// non-terminal.
	for _, u := range linear.updated {
		if u.State == model.StateTodo || u.State == model.StateBacklog {
			t.Fatalf("terminal ticket was reopened: state=%s", u.State)
		}
	}
}

// TestRunMigratesCoarseFingerprintForNonTerminalTicket tests the migration
// path: a non-terminal ticket with the old coarse fingerprint is updated
// (not duplicated) when Snyk starts reporting a fine-grained fingerprint.
func TestRunMigratesCoarseFingerprintForNonTerminalTicket(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Todo"
	cfg.Linear.States.Done = "Done"
	cfg.Linear.States.Cancelled = "Cancelled"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1:e2e/prerequisite_gate.py",
		SnykIssueID: "issue-1",
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  "Path traversal",
		Severity:    "low",
		Status:      model.FindingOpen,
		CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{finding},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1", // old coarse fingerprint
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// Should update the existing non-terminal ticket, not create a duplicate.
	if result.PlannedCreates != 0 {
		t.Fatalf("PlannedCreates = %d, want 0 (should migrate, not duplicate)", result.PlannedCreates)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (should update existing ticket with new fingerprint)", result.PlannedUpdates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1", len(linear.updated))
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0", len(linear.created))
	}
}

// TestRunMigratesCoarseFingerprintForMultipleFindings verifies that when
// Snyk reports multiple fine-grained findings sharing a coarse prefix and a
// single non-terminal coarse ticket exists, only the first finding migrates
// (updates the existing ticket). The rest create fresh tickets. This prevents
// a race where multiple findings all bind to the same Linear issue.
func TestRunMigratesCoarseFingerprintForMultipleFindings(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	findings := []model.Finding{
		{
			Fingerprint: "snyk:project-a:issue-1:src/fileA.py",
			SnykIssueID: "issue-1",
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Path traversal",
			Severity:    "low",
			Status:      model.FindingOpen,
			CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		},
		{
			Fingerprint: "snyk:project-a:issue-1:src/fileB.py",
			SnykIssueID: "issue-1",
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Path traversal",
			Severity:    "low",
			Status:      model.FindingOpen,
			CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		},
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   findings,
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1", // old coarse fingerprint
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// One finding migrates (updates the existing ticket), the other creates
	// a fresh ticket. Both must not update the same Linear issue.
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (only first finding migrates)", result.PlannedUpdates)
	}
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (second finding creates new ticket)", result.PlannedCreates)
	}

	// Verify no two updates target the same existing issue.
	// (fakeLinear.updated is []DesiredIssue; duplicates would mean the same
	// coarse ticket was matched twice, which the deplete-on-match prevents.)
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want exactly 1 (no duplicate updates)", len(linear.updated))
	}
}

// TestRunDoesNotStealFineGrainedTicketViaCoarseFallback verifies that a
// fine-grained ticket (3-segment fingerprint) is never used as a coarse-
// fallback match for a DIFFERENT finding sharing the same coarse prefix.
// Without this guard, the fileA finding would steal the fileB ticket via
// the coarse index, causing perpetual fingerprint churn.
func TestRunDoesNotStealFineGrainedTicketViaCoarseFallback(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	findings := []model.Finding{
		{
			Fingerprint: "snyk:project-a:issue-1:src/fileA.py",
			SnykIssueID: "issue-1",
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Path traversal",
			Severity:    "low",
			Status:      model.FindingOpen,
			CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		},
		{
			Fingerprint: "snyk:project-a:issue-1:src/fileB.py",
			SnykIssueID: "issue-1",
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Path traversal",
			Severity:    "low",
			Status:      model.FindingOpen,
			CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		},
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   findings,
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}
	// Linear already has a fine-grained ticket for fileB (e.g. from a prior
	// migration or creation). fileA has no ticket yet.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1:src/fileB.py",
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// fileB finding updates its exact-match ticket. fileA finding must NOT
	// steal the fileB ticket via coarse fallback — it should create a new one.
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (only fileB updates its ticket)", result.PlannedUpdates)
	}
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (fileA creates new ticket, not steal fileB's)", result.PlannedCreates)
	}
	if len(linear.updated) != 1 {
		t.Fatalf("updated = %d, want 1 (no ticket stealing)", len(linear.updated))
	}
}

// TestRunDoesNotReopenArchivedTicket verifies that an archived Linear ticket
// is treated as terminal — the sync creates a new ticket instead of trying
// to reopen or update the archived one. Without this, auto-archiving would
// make closed tickets invisible and the sync would create duplicates.
func TestRunDoesNotReopenArchivedTicket(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	findings := []model.Finding{
		{
			Fingerprint: "snyk:project-a:issue-1:src/file.py",
			SnykIssueID: "issue-1",
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Path traversal",
			Severity:    "low",
			Status:      model.FindingOpen,
			CreatedAt:   time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
		},
	}

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   findings,
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	archivedAt := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "archived-1",
				Identifier:  "SNYK-999",
				Title:       "Snyk: [low] Path traversal",
				Description: "old description",
				StateName:   "Done",
				Fingerprint: "snyk:project-a:issue-1:src/file.py",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The archived ticket must NOT be reopened or updated. A new ticket
	// should be created instead.
	if result.PlannedUpdates != 0 {
		t.Fatalf("PlannedUpdates = %d, want 0 (archived ticket must not be updated)", result.PlannedUpdates)
	}
	if result.PlannedCreates != 1 {
		t.Fatalf("PlannedCreates = %d, want 1 (new ticket for finding with archived match)", result.PlannedCreates)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (archived ticket untouched)", len(linear.updated))
	}
}

// TestRunSkipsArchivedTicketInResolveLoop verifies that archived tickets are
// not included in the resolve batch — Linear doesn't allow updating archived
// issues, so trying to resolve them would produce API errors.
func TestRunSkipsArchivedTicketInResolveLoop(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// No findings — the sync should try to resolve all existing tickets.
	// The archived ticket must be skipped.
	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings:   nil,
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	archivedAt := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "open-1",
				Identifier:  "SNYK-100",
				Title:       "Snyk: [low] Some issue",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-2",
			},
			{
				ID:          "archived-1",
				Identifier:  "SNYK-101",
				Title:       "Snyk: [low] Old issue",
				StateName:   "Done",
				Fingerprint: "snyk:project-a:issue-3",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The open ticket should be resolved (no matching finding). The archived
	// ticket should be skipped — not resolved, not updated.
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1 (only the non-archived ticket)", result.PlannedResolves)
	}
}

// TestIsTerminalLinearStateArchived verifies that isTerminalLinearState
// returns true for archived tickets regardless of their workflow state name.
func TestIsTerminalLinearStateArchived(t *testing.T) {
	states := config.StateConfig{Done: "Done", Cancelled: "Cancelled"}
	archivedAt := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)

	// Archived + non-terminal state name → still terminal
	archived := model.ExistingIssue{StateName: "Todo", ArchivedAt: &archivedAt}
	if !isTerminalLinearState(archived, states) {
		t.Fatal("archived ticket with Todo state should be terminal")
	}

	// Not archived + terminal state name → terminal
	done := model.ExistingIssue{StateName: "Done"}
	if !isTerminalLinearState(done, states) {
		t.Fatal("non-archived Done ticket should be terminal")
	}

	// Not archived + non-terminal state name → not terminal
	open := model.ExistingIssue{StateName: "Todo"}
	if isTerminalLinearState(open, states) {
		t.Fatal("non-archived Todo ticket should not be terminal")
	}
}

// TestRunBatchCreatePartialFailureRetriesOnlyFailedIssue verifies that when
// CreateIssues reports a partial failure (one alias failed, siblings
// succeeded), only the failed issue is retried individually. Retrying the
// whole batch — the old behavior — recreated the already-created siblings,
// producing duplicate tickets.
func TestRunBatchCreatePartialFailureRetriesOnlyFailedIssue(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "First finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Second finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		// The first CreateIssues call containing issue-2 reports it as
		// failed (success: false alias) while issue-1 succeeds.
		createFailFingerprints: map[string]int{"snyk:project-a:issue-2": 1},
	}

	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	counts := map[string]int{}
	for _, d := range linear.created {
		counts[d.Fingerprint]++
	}
	if counts["snyk:project-a:issue-1"] != 1 {
		t.Fatalf("issue-1 created %d times, want exactly 1 (batch-sibling must not be recreated on partial failure)", counts["snyk:project-a:issue-1"])
	}
	if counts["snyk:project-a:issue-2"] != 1 {
		t.Fatalf("issue-2 created %d times, want exactly 1 (failed item retried individually)", counts["snyk:project-a:issue-2"])
	}

	if len(linear.createCallBatches) != 2 {
		t.Fatalf("CreateIssues calls = %d, want 2 (initial batch + single-item retry)", len(linear.createCallBatches))
	}
	retry := linear.createCallBatches[1]
	if len(retry) != 1 || retry[0].Fingerprint != "snyk:project-a:issue-2" {
		t.Fatalf("retry batch = %+v, want only the failed issue-2", retry)
	}

	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0 (the individual retry succeeded)", result.FailedOps)
	}
}

// TestRunBatchCreatePartialFailureFailedRetryFailsRun covers a single-item
// retry that is itself reported as failed through the failed-index return
// with a nil top-level error (success: false on the alias). Before the retry
// went through createOne, that nil error was taken as success: the item was
// never counted in FailedOps, never recorded in failedWrites, the run
// reported success and the fingerprint was cached as if it had landed.
func TestRunBatchCreatePartialFailureFailedRetryFailsRun(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "First finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Second finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		// issue-2 is reported as failed on the batch call and again on
		// the single-item retry - both times through the failed-index
		// return, without a top-level error.
		createFailFingerprints: map[string]int{"snyk:project-a:issue-2": 2},
	}
	store := &fakeCache{}

	service := New(cfg, logger, snyk, linear, store)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	counts := map[string]int{}
	for _, d := range linear.created {
		counts[d.Fingerprint]++
	}
	if counts["snyk:project-a:issue-1"] != 1 {
		t.Fatalf("issue-1 created %d times, want exactly 1 (batch-sibling must not be recreated on partial failure)", counts["snyk:project-a:issue-1"])
	}
	if counts["snyk:project-a:issue-2"] != 0 {
		t.Fatalf("issue-2 created %d times, want 0 (both the batch call and the retry reported it as failed)", counts["snyk:project-a:issue-2"])
	}

	if len(linear.createCallBatches) != 2 {
		t.Fatalf("CreateIssues calls = %d, want 2 (initial batch + single-item retry)", len(linear.createCallBatches))
	}
	retry := linear.createCallBatches[1]
	if len(retry) != 1 || retry[0].Fingerprint != "snyk:project-a:issue-2" {
		t.Fatalf("retry batch = %+v, want only the failed issue-2", retry)
	}

	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1 (the retry also failed)", result.FailedOps)
	}
	if err := result.Err(); err == nil {
		t.Fatal("RunResult.Err() = nil, want non-nil (a create that failed twice must fail the run)")
	}

	// The still-missing ticket must not be cached: the fast path of the next
	// run has to compare it against live Snyk data again instead of skipping
	// it as reconciled.
	const failed = "snyk:project-a:issue-2"
	if _, ok := store.saved.SnykHashes[failed]; ok {
		t.Fatalf("saved SnykHashes must not contain %q (a create that failed twice is not reconciled)", failed)
	}
	if _, ok := store.saved.LinearHashes[failed]; ok {
		t.Fatalf("saved LinearHashes must not contain %q (a create that failed twice is not reconciled)", failed)
	}
	if _, ok := store.saved.SnykHashes["snyk:project-a:issue-1"]; !ok {
		t.Fatalf("saved SnykHashes must contain \"snyk:project-a:issue-1\" (its create landed and stays cached)")
	}
}

// TestRunBatchCreateOutOfRangeFailedIndexFailsRun covers a degenerate
// LinearClient that reports a failed index outside the batch (nil error, index
// one past the end, nothing created). The index identifies no batch entry, so
// the sync must count a failed op, fail the run, and keep every fingerprint of
// the batch out of the refreshed cache: caching any of them would let the next
// run's cache fast path skip the ticket the bogus index tried to report as
// never created.
func TestRunBatchCreateOutOfRangeFailedIndexFailsRun(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "First finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Second finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		// The batch create reports index 2 as failed - one past the end of
		// the two-entry batch - with no entry created and no error, so the
		// reported failure matches no batch entry.
		createFailOutOfRange: true,
	}
	store := &fakeCache{}

	service := New(cfg, logger, snyk, linear, store)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(linear.createCallBatches) != 1 {
		t.Fatalf("CreateIssues calls = %d, want 1 (an out-of-range index identifies no batch entry, so no single-item retry is possible)", len(linear.createCallBatches))
	}

	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1 (the out-of-range failed index is counted)", result.FailedOps)
	}
	if err := result.Err(); err == nil {
		t.Fatal("RunResult.Err() = nil, want non-nil (a create reported failed by an index outside the batch must fail the run)")
	}

	// Nothing in the batch is proven to have landed, so no fingerprint of it
	// may be cached: the next run has to compare every entry against live
	// data instead of skipping the still-missing ticket on the fast path.
	for _, fp := range []string{"snyk:project-a:issue-1", "snyk:project-a:issue-2"} {
		if _, ok := store.saved.SnykHashes[fp]; ok {
			t.Fatalf("saved SnykHashes must not contain %q (an out-of-range failed index proves nothing landed)", fp)
		}
		if _, ok := store.saved.LinearHashes[fp]; ok {
			t.Fatalf("saved LinearHashes must not contain %q (an out-of-range failed index proves nothing landed)", fp)
		}
	}
}

// TestRunReconcilesAmbiguousBatchCreateBeforeRetry covers the lost-response
// case: a transport failure does not prove Linear rejected the mutation, so
// the entries that actually landed must not be created a second time.
func TestRunReconcilesAmbiguousBatchCreateBeforeRetry(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "First finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Second finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		// The batch create died with a transport error, but one of its two
		// creates was committed before the response was lost.
		createBatchErr:     errors.New("connection reset by peer"),
		landedFingerprints: []string{"snyk:project-a:issue-1"},
	}

	service := New(minimalCfg(), logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if linear.existingCalls != 1 {
		t.Fatalf("ExistingFingerprints calls = %d, want 1", linear.existingCalls)
	}
	// The batch call errored before recording anything, and the landed
	// entry is skipped by the reconciliation, so the only create the fake
	// ever records is the retry for the un-landed fingerprint.
	if len(linear.created) != 1 {
		t.Fatalf("created = %d (%#v), want 1: only the entry that did not land may be recreated", len(linear.created), linear.created)
	}
	if linear.created[0].Fingerprint != "snyk:project-a:issue-2" {
		t.Fatalf("recreated %q, want the un-landed snyk:project-a:issue-2", linear.created[0].Fingerprint)
	}
	if len(linear.createCallBatches) != 2 {
		t.Fatalf("CreateIssues calls = %d, want 2 (failed batch + single-item retry)", len(linear.createCallBatches))
	}
	retry := linear.createCallBatches[1]
	if len(retry) != 1 || retry[0].Fingerprint != "snyk:project-a:issue-2" {
		t.Fatalf("retry batch = %+v, want only the un-landed issue-2", retry)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: reconciliation and the retry both succeeded", result.FailedOps)
	}
}

// TestRunLeavesUnreconcilableBatchForNextRun covers the other branch of the
// ambiguous-failure contract: with no way to tell what landed, nothing is
// retried and the whole batch is counted as failed. A missing ticket is
// recoverable on the next run; a duplicate is only undone later by the
// duplicate-cancellation pass.
func TestRunLeavesUnreconcilableBatchForNextRun(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "First finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Second finding",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		existingErr:    errors.New("lookup failed too"),
	}

	service := New(minimalCfg(), logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(linear.created) != 0 {
		t.Fatalf("created = %d (%#v), want 0: an unreconcilable ambiguous failure must not be retried", len(linear.created), linear.created)
	}
	if result.FailedOps != 2 {
		t.Fatalf("FailedOps = %d, want 2 (the whole unreconciled batch)", result.FailedOps)
	}
	if len(linear.createCallBatches) != 1 {
		t.Fatalf("CreateIssues calls = %d, want 1: no per-item retry may follow a failed reconciliation", len(linear.createCallBatches))
	}
}

// TestRunBatchCommentPartialFailureRetriesOnlyFailedComment is the
// PostComments equivalent: a partial comment failure must only re-post the
// failed comment. Re-posting comments that already landed leaves duplicate
// change-summary comments (and duplicate notifications) on the issue.
func TestRunBatchCommentPartialFailureRetriesOnlyFailedComment(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Updated title one",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Updated title two",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title one",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    3,
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "stale title two",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-2",
				Priority:    3,
			},
		},
		commentFailFingerprints: map[string]int{"snyk:project-a:issue-2": 1},
	}

	service := New(cfg, logger, snyk, linear, nil)
	if _, err := service.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	counts := map[string]int{}
	for _, u := range linear.comments {
		counts[u.Desired.Fingerprint]++
	}
	if counts["snyk:project-a:issue-1"] != 1 {
		t.Fatalf("issue-1 commented %d times, want exactly 1 (batch-sibling comment must not be re-posted)", counts["snyk:project-a:issue-1"])
	}
	if counts["snyk:project-a:issue-2"] != 1 {
		t.Fatalf("issue-2 commented %d times, want exactly 1 (failed comment retried individually)", counts["snyk:project-a:issue-2"])
	}

	if len(linear.commentCallBatches) != 2 {
		t.Fatalf("PostComments calls = %d, want 2 (initial batch + single-item retry)", len(linear.commentCallBatches))
	}
	retry := linear.commentCallBatches[1]
	if len(retry) != 1 || retry[0].Desired.Fingerprint != "snyk:project-a:issue-2" {
		t.Fatalf("comment retry batch = %+v, want only the failed issue-2 update", retry)
	}
}

// TestRunBatchCommentOutOfRangeFailedIndexCountsFailedComment pins the second
// half of the failed-index contract for PostComments: a faultily implemented
// LinearClient can report a failed index outside the batch. It names no batch
// entry, so indexing with it would panic the worker goroutine and take the
// whole run down; instead the run must count one failed comment, keep its
// updates, and complete.
func TestRunBatchCommentOutOfRangeFailedIndexCountsFailedComment(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true

	snyk := fakeSnyk{
		snapshot: model.SnykSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint: "snyk:project-a:issue-1",
					SnykIssueID: "issue-1",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Updated title one",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint: "snyk:project-a:issue-2",
					SnykIssueID: "issue-2",
					ProjectID:   "project-a",
					ProjectName: "Project A",
					IssueTitle:  "Updated title two",
					Severity:    "high",
					Status:      model.FindingOpen,
					CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			ProjectIDs: map[string]struct{}{"project-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title one",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-1",
				Priority:    3,
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "stale title two",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk:project-a:issue-2",
				Priority:    3,
			},
		},
		commentFailOutOfRange: true,
	}

	service := New(cfg, discardLogger(), snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want a completed run", err)
	}
	if result.FailedComments != 1 {
		t.Fatalf("FailedComments = %d, want 1 (the out-of-range index counts one failed comment)", result.FailedComments)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0 (the updates themselves landed)", result.FailedOps)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("result.Err() = %v, want nil (a failed comment must not fail the run)", err)
	}
	updatedFingerprints := make(map[string]int, len(linear.updated))
	for _, desired := range linear.updated {
		updatedFingerprints[desired.Fingerprint]++
	}
	for _, fingerprint := range []string{"snyk:project-a:issue-1", "snyk:project-a:issue-2"} {
		if updatedFingerprints[fingerprint] != 1 {
			t.Fatalf("update of %s = %d times, want exactly 1 (the run must finish its other work)", fingerprint, updatedFingerprints[fingerprint])
		}
	}
	if len(linear.commentCallBatches) != 1 {
		t.Fatalf("PostComments calls = %d, want 1 (the out-of-range index names no batch entry, so no comment is retried)", len(linear.commentCallBatches))
	}
}

// storedIssue models what the next run reads back after Linear stores
// desired: the same description, with the metadata fields the Linear client
// would parse out of it (fingerprint, identity, closed_reason, labels).
func storedIssue(id, identifier, stateName string, desired model.DesiredIssue) model.ExistingIssue {
	issue := model.ExistingIssue{
		ID:            id,
		Identifier:    identifier,
		Title:         desired.Title,
		Description:   desired.Description,
		DueDate:       desired.DueDate,
		StateName:     stateName,
		Fingerprint:   desired.Fingerprint,
		ManagedLabels: desired.ManagedLabels,
		Priority:      desired.Priority,
	}
	start := findMetadataBlockStart(desired.Description)
	if start < 0 {
		return issue
	}
	for line := range strings.SplitSeq(desired.Description[start:], "\n") {
		if after, ok := strings.CutPrefix(line, "identity: "); ok {
			issue.Identity = after
		}
		if after, ok := strings.CutPrefix(line, "closed_reason: "); ok {
			issue.ClosedReason = after
		}
		if after, ok := strings.CutPrefix(line, "awaiting_fix: "); ok {
			issue.AwaitingFix = after == "yes"
		}
	}
	return issue
}

// recreatedProjectFinding returns the same Snyk finding as seen in a project
// with the given ID: Snyk recreating a project keeps its name, target and
// issue key but mints a new project ID and new issue IDs.
func recreatedProjectFinding(projectID, issueID string) model.Finding {
	return model.Finding{
		Fingerprint:       model.Fingerprint(projectID, issueID, "glibc@2.41-12"),
		SnykIssueID:       issueID,
		SnykIssueKey:      "SNYK-DEBIAN13-GLIBC-19383357",
		ProjectID:         projectID,
		ProjectName:       "tesslio/monorepo(main):harness/kikimora/Dockerfile.simulators",
		ProjectOrigin:     "github",
		ProjectReference:  "main",
		ProjectTargetFile: "harness/kikimora/Dockerfile.simulators",
		IssueTitle:        "Integer Overflow",
		PackageName:       "glibc",
		VulnerableVersion: "2.41-12",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	}
}

// withClosedReason records closedReason in the ticket's metadata block, as
// the resolve loop does when it cancels a ticket whose project vanished.
func withClosedReason(issue model.ExistingIssue, closedReason string) model.ExistingIssue {
	issue.Description = upsertManagedMetadata(issue.Description, managedMetadata{
		Fingerprint:   issue.Fingerprint,
		Identity:      issue.Identity,
		ManagedLabels: issue.ManagedLabels,
		ClosedReason:  closedReason,
	})
	issue.ClosedReason = closedReason
	return issue
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunRebindsOpenTicketAfterProjectRecreation(t *testing.T) {
	cfg := minimalCfg()
	oldFinding := recreatedProjectFinding("project-old", "issue-old")
	newFinding := recreatedProjectFinding("project-new", "issue-new")
	if model.FindingIdentity(oldFinding) == "" || model.FindingIdentity(oldFinding) != model.FindingIdentity(newFinding) {
		t.Fatalf("identity must survive project recreation: old %q new %q", model.FindingIdentity(oldFinding), model.FindingIdentity(newFinding))
	}

	// The old project vanished in the same run the new one appeared, while
	// its ticket was still open (a human had moved it to In Progress).
	existing := storedIssue("existing-1", "SNYK-100", "In Progress", desiredIssue(cfg, oldFinding))
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{newFinding},
		ProjectIDs: map[string]struct{}{"project-new": {}},
	}}

	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Rebound != 1 {
		t.Fatalf("Rebound = %d, want 1", result.Rebound)
	}
	if result.PlannedCreates != 0 || len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0 (the old ticket must be rebound, not copied)", len(linear.created))
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0 (the rebound ticket must not also be cancelled)", result.PlannedResolves)
	}
	if len(linear.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(linear.updates))
	}
	update := linear.updates[0]
	if update.Existing.ID != "existing-1" {
		t.Fatalf("updated %s, want existing-1", update.Existing.ID)
	}
	if update.Desired.Fingerprint != newFinding.Fingerprint {
		t.Fatalf("fingerprint = %q, want the new project's %q", update.Desired.Fingerprint, newFinding.Fingerprint)
	}
	if !strings.Contains(update.Desired.Description, "fingerprint: "+newFinding.Fingerprint) || !strings.Contains(update.Desired.Description, "(`project-new`)") {
		t.Fatalf("description not rewritten from the new finding:\n%s", update.Desired.Description)
	}
	if !update.Desired.PreserveState {
		t.Fatalf("PreserveState = false, want true (the human's open state is kept)")
	}

	// Next run: the ticket now carries the new fingerprint and converges.
	linear2 := &fakeLinear{snapshot: []model.ExistingIssue{storedIssue("existing-1", "SNYK-100", "In Progress", update.Desired)}}
	result2, err := New(cfg, discardLogger(), snyk, linear2, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if result2.Rebound != 0 || len(linear2.created) != 0 || len(linear2.updates) != 0 {
		t.Fatalf("run 2: rebound=%d created=%d updates=%d, want all 0", result2.Rebound, len(linear2.created), len(linear2.updates))
	}
}

func TestRunRebindsSyncCancelledTicketAfterProjectRecreation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inactive   bool
		wantReason string
	}{
		{name: "project deleted", wantReason: model.ClosedReasonProjectMissing},
		{name: "project deactivated", inactive: true, wantReason: model.ClosedReasonProjectDeactivated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			oldFinding := recreatedProjectFinding("project-old", "issue-old")
			newFinding := recreatedProjectFinding("project-new", "issue-new")
			existing := storedIssue("existing-1", "SNYK-100", "Todo", desiredIssue(cfg, oldFinding))

			// Run 1: the old project disappears and nothing replaces it yet.
			// The sync cancels the ticket and records why.
			snapshot1 := model.SnykSnapshot{ProjectIDs: map[string]struct{}{"project-other": {}}}
			if tc.inactive {
				snapshot1.InactiveProjectIDs = map[string]struct{}{"project-old": {}}
			}
			linear1 := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
			if _, err := New(cfg, discardLogger(), fakeSnyk{snapshot: snapshot1}, linear1, nil).Run(context.Background()); err != nil {
				t.Fatalf("run 1: Run() error = %v", err)
			}
			if len(linear1.updates) != 1 || linear1.updates[0].Desired.State != model.StateCancelled {
				t.Fatalf("run 1: want one cancel, got %v", linear1.updates)
			}
			cancelled := storedIssue("existing-1", "SNYK-100", "Cancelled", linear1.updates[0].Desired)
			if cancelled.ClosedReason != tc.wantReason {
				t.Fatalf("run 1: closed_reason = %q, want %q:\n%s", cancelled.ClosedReason, tc.wantReason, cancelled.Description)
			}
			if cancelled.Identity != existing.Identity {
				t.Fatalf("run 1: identity = %q, want it kept (%q)", cancelled.Identity, existing.Identity)
			}

			// Run 2 (possibly weeks later): the recreated project appears.
			snapshot2 := model.SnykSnapshot{
				Findings:   []model.Finding{newFinding},
				ProjectIDs: map[string]struct{}{"project-other": {}, "project-new": {}},
			}
			if tc.inactive {
				snapshot2.InactiveProjectIDs = map[string]struct{}{"project-old": {}}
			}
			linear2 := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
			result2, err := New(cfg, discardLogger(), fakeSnyk{snapshot: snapshot2}, linear2, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("run 2: Run() error = %v", err)
			}
			if result2.Rebound != 1 || len(linear2.created) != 0 {
				t.Fatalf("run 2: rebound=%d created=%d, want 1 and 0", result2.Rebound, len(linear2.created))
			}
			if len(linear2.updates) != 1 {
				t.Fatalf("run 2: updates = %d, want 1", len(linear2.updates))
			}
			update := linear2.updates[0]
			if update.Desired.State != model.StateTodo || !update.Desired.Reopen || !update.Diff.StateChanged {
				t.Fatalf("run 2: state=%q reopen=%v stateChanged=%v, want todo/true/true", update.Desired.State, update.Desired.Reopen, update.Diff.StateChanged)
			}
			if update.Desired.Fingerprint != newFinding.Fingerprint {
				t.Fatalf("run 2: fingerprint = %q, want %q", update.Desired.Fingerprint, newFinding.Fingerprint)
			}
			if strings.Contains(update.Desired.Description, "closed_reason:") {
				t.Fatalf("run 2: closed_reason must be cleared once the ticket is active again:\n%s", update.Desired.Description)
			}
		})
	}
}

func TestRunDoesNotRebindWhenOldProjectStillActive(t *testing.T) {
	cfg := minimalCfg()
	// Two live projects scanning the same target: each keeps its own ticket.
	oldFinding := recreatedProjectFinding("project-a", "issue-a")
	newFinding := recreatedProjectFinding("project-b", "issue-b")
	existing := storedIssue("existing-1", "SNYK-100", "Todo", desiredIssue(cfg, oldFinding))
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{newFinding},
		ProjectIDs: map[string]struct{}{"project-a": {}, "project-b": {}},
	}}

	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Rebound != 0 || len(linear.created) != 1 {
		t.Fatalf("rebound=%d created=%d, want 0 and 1", result.Rebound, len(linear.created))
	}
	for _, u := range linear.updates {
		if u.Existing.ID == "existing-1" && u.Desired.Fingerprint != existing.Fingerprint {
			t.Fatalf("active project's ticket was rebound: %+v", u.Desired)
		}
	}
}

func TestRunDoesNotRebindTicketClosedByFixOrPerson(t *testing.T) {
	for _, tc := range []struct {
		name         string
		state        string
		closedReason string
	}{
		{name: "done by fix or person", state: "Done"},
		{name: "cancelled by person", state: "Cancelled"},
		// A person moved a sync-cancelled ticket to Done: their decision wins.
		{name: "sync-cancelled then moved to done", state: "Done", closedReason: model.ClosedReasonProjectMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			existing := storedIssue("existing-1", "SNYK-100", tc.state, desiredIssue(cfg, recreatedProjectFinding("project-old", "issue-old")))
			existing = withClosedReason(existing, tc.closedReason)
			linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
			snyk := fakeSnyk{snapshot: model.SnykSnapshot{
				Findings:   []model.Finding{recreatedProjectFinding("project-new", "issue-new")},
				ProjectIDs: map[string]struct{}{"project-new": {}},
			}}

			result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Rebound != 0 || len(linear.created) != 1 {
				t.Fatalf("rebound=%d created=%d, want 0 and 1", result.Rebound, len(linear.created))
			}
			for _, u := range linear.updates {
				if isNonTerminalModelState(u.Desired.State) {
					t.Fatalf("closed ticket was reopened: %+v", u.Desired)
				}
			}
		})
	}
}

func TestRunDoesNotRebindArchivedTicket(t *testing.T) {
	cfg := minimalCfg()
	archivedAt := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	existing := storedIssue("existing-1", "SNYK-100", "Cancelled", desiredIssue(cfg, recreatedProjectFinding("project-old", "issue-old")))
	existing = withClosedReason(existing, model.ClosedReasonProjectMissing)
	existing.ArchivedAt = &archivedAt
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{recreatedProjectFinding("project-new", "issue-new")},
		ProjectIDs: map[string]struct{}{"project-new": {}},
	}}

	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Rebound != 0 || len(linear.created) != 1 || len(linear.updates) != 0 {
		t.Fatalf("rebound=%d created=%d updates=%d, want 0/1/0", result.Rebound, len(linear.created), len(linear.updates))
	}
}

func TestRunRebindCandidatesAreDeterministicAndSingleUse(t *testing.T) {
	cfg := minimalCfg()
	older := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	// Three tickets share the identity (earlier recreations left copies).
	// The same old fingerprint on several tickets would be a duplicate
	// conflict, so each sits in a different vanished project.
	openOlder := storedIssue("open-older", "SNYK-10", "Todo", desiredIssue(cfg, recreatedProjectFinding("project-gone-1", "issue-1")))
	openOlder.CreatedAt = &older
	openNewer := storedIssue("open-newer", "SNYK-20", "Todo", desiredIssue(cfg, recreatedProjectFinding("project-gone-2", "issue-2")))
	openNewer.CreatedAt = &newer
	cancelled := storedIssue("cancelled", "SNYK-30", "Cancelled", desiredIssue(cfg, recreatedProjectFinding("project-gone-3", "issue-3")))
	cancelled = withClosedReason(cancelled, model.ClosedReasonProjectMissing)
	cancelled.CreatedAt = &newer

	// Two findings with the same identity (two recreated projects with the
	// same name) take the two best candidates, one each, in order.
	first := recreatedProjectFinding("project-new-1", "issue-new-1")
	second := recreatedProjectFinding("project-new-2", "issue-new-2")

	for range 20 {
		linear := &fakeLinear{snapshot: []model.ExistingIssue{cancelled, openOlder, openNewer}}
		snyk := fakeSnyk{snapshot: model.SnykSnapshot{
			Findings:   []model.Finding{first, second},
			ProjectIDs: map[string]struct{}{"project-new-1": {}, "project-new-2": {}},
		}}
		result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if result.Rebound != 2 || len(linear.created) != 0 {
			t.Fatalf("rebound=%d created=%d, want 2 and 0", result.Rebound, len(linear.created))
		}
		bound := map[string]string{}
		var resolved []string
		for _, u := range linear.updates {
			if u.Desired.Fingerprint == u.Existing.Fingerprint {
				resolved = append(resolved, u.Existing.ID)
				continue
			}
			bound[u.Desired.Fingerprint] = u.Existing.ID
		}
		if bound[first.Fingerprint] != "open-newer" || bound[second.Fingerprint] != "open-older" {
			t.Fatalf("bindings = %v, want first->open-newer, second->open-older", bound)
		}
		// The leftover sync-cancelled ticket is left alone.
		if len(resolved) != 0 {
			t.Fatalf("resolved = %v, want none", resolved)
		}
	}
}

func TestRunIdentityBackfillHappensOnce(t *testing.T) {
	cfg := minimalCfg()
	finding := recreatedProjectFinding("project-a", "issue-a")
	desired := desiredIssue(cfg, finding)
	identityLine := "identity: " + model.FindingIdentity(finding) + "\n"
	if !strings.Contains(desired.Description, identityLine) {
		t.Fatalf("desired description lacks the identity line:\n%s", desired.Description)
	}

	// A ticket written before the identity line existed.
	legacy := desired
	legacy.Description = strings.Replace(desired.Description, identityLine, "", 1)
	existing := storedIssue("existing-1", "SNYK-100", "Todo", legacy)
	if existing.Identity != "" {
		t.Fatalf("legacy ticket unexpectedly has identity %q", existing.Identity)
	}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	linear1 := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	if _, err := New(cfg, discardLogger(), snyk, linear1, nil).Run(context.Background()); err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if len(linear1.updates) != 1 {
		t.Fatalf("run 1: updates = %d, want 1 (one-time identity backfill)", len(linear1.updates))
	}
	diff := linear1.updates[0].Diff
	if !diff.DescriptionChanged || !diff.MetadataOnlyDescriptionChange {
		t.Fatalf("run 1: diff = %+v, want a metadata-only description change", diff)
	}

	linear2 := &fakeLinear{snapshot: []model.ExistingIssue{storedIssue("existing-1", "SNYK-100", "Todo", linear1.updates[0].Desired)}}
	if _, err := New(cfg, discardLogger(), snyk, linear2, nil).Run(context.Background()); err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if len(linear2.updates) != 0 {
		t.Fatalf("run 2: updates = %d, want 0 (backfill must happen exactly once)", len(linear2.updates))
	}
}

func TestRunSkipsUpdatesToArchivedMatchedTicket(t *testing.T) {
	cfg := minimalCfg()
	finding := recreatedProjectFinding("project-a", "issue-a")
	finding.Status = model.FindingFixed
	legacy := desiredIssue(cfg, finding)
	legacy.Description = "stale description"
	existing := storedIssue("existing-1", "SNYK-100", "Done", legacy)
	archivedAt := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	existing.ArchivedAt = &archivedAt
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.updates) != 0 || len(linear.created) != 0 {
		t.Fatalf("updates=%d created=%d, want 0 (Linear rejects updates to archived issues)", len(linear.updates), len(linear.created))
	}
}

func TestMetadataBlockClosedReasonRoundTrips(t *testing.T) {
	description := upsertManagedMetadata("body", managedMetadata{
		Fingerprint:   "snyk:project-a:issue-1:pkg@1.0.0",
		Identity:      "0123456789abcdef0123456789abcdef",
		ManagedLabels: []string{"snyk-automation"},
		ClosedReason:  model.ClosedReasonProjectDeactivated,
	})
	got := storedIssue("id", "SNYK-1", "Cancelled", model.DesiredIssue{Description: description})
	if got.Identity != "0123456789abcdef0123456789abcdef" || got.ClosedReason != model.ClosedReasonProjectDeactivated {
		t.Fatalf("identity=%q closed_reason=%q:\n%s", got.Identity, got.ClosedReason, description)
	}
	// Rewriting without a closed reason clears it.
	cleared := upsertManagedMetadata(description, managedMetadata{Fingerprint: "snyk:project-a:issue-1:pkg@1.0.0"})
	if strings.Contains(cleared, "closed_reason") || strings.Contains(cleared, "identity:") {
		t.Fatalf("stale metadata left behind:\n%s", cleared)
	}
}

func TestMetadataBlockAwaitingFixRoundTrips(t *testing.T) {
	description := upsertManagedMetadata("body", managedMetadata{
		Fingerprint:   "snyk:project-a:issue-1",
		ManagedLabels: []string{"snyk-automation"},
		AwaitingFix:   true,
	})
	if !strings.Contains(description, "awaiting_fix: yes") {
		t.Fatalf("awaiting_fix marker missing:\n%s", description)
	}
	got := storedIssue("id", "SNYK-1", "Backlog", model.DesiredIssue{Description: description})
	if !got.AwaitingFix {
		t.Fatalf("AwaitingFix = false, want true:\n%s", description)
	}
	// Rewriting the block for a finding that left the awaiting-fix state
	// clears the marker.
	cleared := upsertManagedMetadata(description, managedMetadata{Fingerprint: "snyk:project-a:issue-1"})
	if strings.Contains(cleared, "awaiting_fix") {
		t.Fatalf("stale awaiting_fix marker left behind:\n%s", cleared)
	}
	got = storedIssue("id", "SNYK-1", "Todo", model.DesiredIssue{Description: cleared})
	if got.AwaitingFix {
		t.Fatalf("AwaitingFix = true, want false after clear:\n%s", cleared)
	}
}

// TestMetadataBlockWritesAwaitingFixMarkerWithLabelOff verifies the marker is
// written regardless of the awaiting-fix label configuration — that
// independence is the whole point of the marker.
func TestMetadataBlockWritesAwaitingFixMarkerWithLabelOff(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.Labels.AwaitingFix = "" // off
	finding := model.Finding{
		Fingerprint: "snyk:project-a:issue-1",
		Status:      model.FindingAwaitingFix,
	}
	description := issueDescription(cfg.Source, managedLabels(cfg.Linear.Labels, finding), finding)
	if !strings.Contains(description, "awaiting_fix: yes") {
		t.Fatalf("awaiting_fix marker missing with label off:\n%s", description)
	}
	// An open finding must not carry the marker.
	finding.Status = model.FindingOpen
	description = issueDescription(cfg.Source, managedLabels(cfg.Linear.Labels, finding), finding)
	if strings.Contains(description, "awaiting_fix") {
		t.Fatalf("awaiting_fix marker present for an open finding:\n%s", description)
	}
}

func TestRunResolveLoopRecordsClosedReasonOnlyWhenSyncCloses(t *testing.T) {
	cfg := minimalCfg()
	desired := desiredIssue(cfg, recreatedProjectFinding("project-gone", "issue-1"))
	open := storedIssue("open", "SNYK-1", "Todo", desired)
	done := storedIssue("done", "SNYK-2", "Done", desired)
	done.Fingerprint = "snyk:project-gone:issue-2:glibc@2.41-12"
	fixed := storedIssue("fixed", "SNYK-3", "Todo", desired)
	fixed.Fingerprint = "snyk:project-live:issue-3:glibc@2.41-12"
	linear := &fakeLinear{snapshot: []model.ExistingIssue{open, done, fixed}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{ProjectIDs: map[string]struct{}{"project-live": {}}}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	reasons := map[string]string{}
	for _, u := range linear.updates {
		reasons[u.Existing.ID] = storedIssue("", "", "", u.Desired).ClosedReason
	}
	if reasons["open"] != model.ClosedReasonProjectMissing {
		t.Fatalf("open ticket closed_reason = %q, want %q", reasons["open"], model.ClosedReasonProjectMissing)
	}
	if r, ok := reasons["done"]; ok && r != "" {
		t.Fatalf("ticket already Done before its project vanished got closed_reason %q; that closure was not the sync's", r)
	}
	if reasons["fixed"] != "" {
		t.Fatalf("ticket resolved in an active project got closed_reason %q", reasons["fixed"])
	}
}

func TestRunReopensTicketClosedWhileSnykStillReportsOpen(t *testing.T) {
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name           string
		lastResolvedAt time.Time
	}{
		{name: "never resolved"},
		{name: "resolved before the ticket was created", lastResolvedAt: created.Add(-24 * time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			finding := recreatedProjectFinding("project-a", "issue-a")
			finding.LastResolvedAt = tc.lastResolvedAt
			// An automation marked the ticket Done; Snyk still reports it open.
			existing := storedIssue("existing-1", "SNYK-100", "Done", desiredIssue(cfg, finding))
			existing.CreatedAt = &created
			existing.ClosedAt = &closed
			linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
			snyk := fakeSnyk{snapshot: model.SnykSnapshot{
				Findings:   []model.Finding{finding},
				ProjectIDs: map[string]struct{}{"project-a": {}},
			}}

			result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(linear.created) != 0 || result.PlannedCreates != 0 {
				t.Fatalf("created = %d, want 0 (must reopen, not duplicate)", len(linear.created))
			}
			if len(linear.updates) != 1 {
				t.Fatalf("updates = %d, want 1", len(linear.updates))
			}
			u := linear.updates[0]
			if u.Existing.ID != "existing-1" || u.Desired.State != model.StateTodo || !u.Diff.StateChanged {
				t.Fatalf("update = %+v / diff %+v, want existing-1 moved to todo", u.Desired, u.Diff)
			}
			if !strings.Contains(u.Desired.StateReason, "reopened instead of creating a duplicate") {
				t.Fatalf("StateReason = %q", u.Desired.StateReason)
			}
		})
	}
}

func TestRunDoesNotReopenClosedTicketWhenUnsafe(t *testing.T) {
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	archived := time.Date(2026, time.September, 10, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		mutate func(*model.Finding, *model.ExistingIssue)
	}{
		{
			// Issue-ID reuse: Snyk resolved the old occurrence (then the sync
			// closed the ticket on a later run), and the ID came back. The
			// resolution predates the close, so comparing against the close
			// time would wrongly reopen; it is after the ticket's creation.
			name: "resolved after the ticket was created, before it was closed",
			mutate: func(f *model.Finding, _ *model.ExistingIssue) {
				f.LastResolvedAt = closed.Add(-time.Hour)
			},
		},
		{
			name: "resolved after the ticket was closed",
			mutate: func(f *model.Finding, _ *model.ExistingIssue) {
				f.LastResolvedAt = closed.Add(time.Hour)
			},
		},
		{
			name: "unparsable last_resolved_at",
			mutate: func(f *model.Finding, _ *model.ExistingIssue) {
				f.LastResolvedAtInvalid = true
			},
		},
		{
			name: "missing closed time",
			mutate: func(_ *model.Finding, e *model.ExistingIssue) {
				e.ClosedAt = nil
			},
		},
		{
			name: "missing created time",
			mutate: func(_ *model.Finding, e *model.ExistingIssue) {
				e.CreatedAt = nil
			},
		},
		{
			name: "archived",
			mutate: func(_ *model.Finding, e *model.ExistingIssue) {
				e.ArchivedAt = &archived
			},
		},
		{
			name: "coarse fingerprint",
			mutate: func(f *model.Finding, e *model.ExistingIssue) {
				f.Fingerprint = "snyk:project-a:issue-a"
				e.Fingerprint = f.Fingerprint
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			finding := recreatedProjectFinding("project-a", "issue-a")
			existing := storedIssue("existing-1", "SNYK-100", "Done", desiredIssue(cfg, finding))
			existing.CreatedAt = &created
			existing.ClosedAt = &closed
			tc.mutate(&finding, &existing)
			linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
			snyk := fakeSnyk{snapshot: model.SnykSnapshot{
				Findings:   []model.Finding{finding},
				ProjectIDs: map[string]struct{}{"project-a": {}},
			}}

			if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if len(linear.created) != 1 {
				t.Fatalf("created = %d, want 1 (keep today's behavior: fresh ticket)", len(linear.created))
			}
			for _, u := range linear.updates {
				if isNonTerminalModelState(u.Desired.State) {
					t.Fatalf("closed ticket was reopened: %+v", u.Desired)
				}
			}
		})
	}
}

func TestRunReopenGuardStillAppliesToCoarseMigration(t *testing.T) {
	// A coarse-fallback match never reaches a terminal ticket (the coarse
	// index holds open tickets only), so a Done coarse ticket with complete
	// timestamps is still left closed and a fresh ticket is created.
	cfg := minimalCfg()
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	finding := recreatedProjectFinding("project-a", "issue-a")
	existing := storedIssue("existing-1", "SNYK-100", "Done", desiredIssue(cfg, finding))
	existing.Fingerprint = "snyk:project-a:issue-a"
	existing.CreatedAt = &created
	existing.ClosedAt = &closed
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1", len(linear.created))
	}
	for _, u := range linear.updates {
		if isNonTerminalModelState(u.Desired.State) {
			t.Fatalf("closed coarse ticket was reopened: %+v", u.Desired)
		}
	}
}

func TestComputeDiffAllowsOnlyDeliberateReopen(t *testing.T) {
	states := minimalCfg().Linear.States
	existing := model.ExistingIssue{StateName: "Done"}
	if d := ComputeDiff(existing, model.DesiredIssue{State: model.StateTodo}, states); d.StateChanged {
		t.Fatalf("terminal->open reported without Reopen")
	}
	if d := ComputeDiff(existing, model.DesiredIssue{State: model.StateTodo, Reopen: true}, states); !d.StateChanged {
		t.Fatalf("deliberate reopen not reported as a state change")
	}
}

func TestRunReopenIsNotSuppressedByCacheFastPath(t *testing.T) {
	cfg := minimalCfg()
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	finding := recreatedProjectFinding("project-a", "issue-a")
	desired := desiredIssue(cfg, finding)
	existing := storedIssue("existing-1", "SNYK-100", "Done", desired)
	existing.CreatedAt = &created
	existing.ClosedAt = &closed
	// The previous run already saw this exact Done ticket and finding (for
	// example it created a duplicate instead), so both hashes match.
	cacheStore := &fakeCache{snapshot: cache.Snapshot{
		SchemaSignature: managedSchemaSignature(),
		SnykHashes:      map[string]string{finding.Fingerprint: desiredIssueHash(desired)},
		LinearHashes:    map[string]string{finding.Fingerprint: existingIssueHash(existing)},
	}}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, linear, cacheStore).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.updates) != 1 || linear.updates[0].Desired.State != model.StateTodo {
		t.Fatalf("updates = %v, want the ticket reopened despite matching cache hashes", linear.updates)
	}
}

func TestRunDoesNotRebindAcrossKubernetesClusters(t *testing.T) {
	cfg := minimalCfg()
	// Snyk names Kubernetes projects without the cluster, so staging and
	// prod running the same workload differ only by ProjectCluster.
	k8sFinding := func(projectID, issueID, cluster string) model.Finding {
		f := recreatedProjectFinding(projectID, issueID)
		f.ProjectName = "backend/deployment.apps/api:ghcr.io/tesslio/api"
		f.ProjectOrigin = "kubernetes"
		f.ProjectReference = ""
		f.ProjectTargetFile = ""
		f.ProjectNamespace = "backend"
		f.ProjectCluster = cluster
		return f
	}
	staging := k8sFinding("project-staging", "issue-s", "staging")
	prod := k8sFinding("project-prod", "issue-p", "prod")
	if model.FindingIdentity(staging) == model.FindingIdentity(prod) {
		t.Fatalf("staging and prod findings share an identity")
	}

	// The staging project was deleted earlier and its ticket sync-cancelled;
	// later a prod project with the same workload appears.
	cancelled := withClosedReason(storedIssue("staging-ticket", "SNYK-100", "Cancelled", desiredIssue(cfg, staging)), model.ClosedReasonProjectMissing)
	linear := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{prod},
		ProjectIDs: map[string]struct{}{"project-prod": {}},
	}}

	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Rebound != 0 || len(linear.created) != 1 || len(linear.updates) != 0 {
		t.Fatalf("rebound=%d created=%d updates=%d, want 0/1/0 (no cross-cluster rebind)", result.Rebound, len(linear.created), len(linear.updates))
	}
}

func TestRunReopensNewestTerminalDuplicateInsteadOfCreatingAnother(t *testing.T) {
	cfg := minimalCfg()
	finding := recreatedProjectFinding("project-a", "issue-a")
	// Snyk resolved an earlier occurrence in May; the current open period
	// started after that and is tracked by the newest copy.
	finding.LastResolvedAt = time.Date(2026, time.May, 15, 0, 0, 0, 0, time.UTC)
	desired := desiredIssue(cfg, finding)
	at := func(month time.Month) *time.Time {
		t := time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC)
		return &t
	}
	// The original ticket predates the May resolution; the later copy was
	// created by an earlier run's reopen guard and then closed early by an
	// automation. All copies are terminal, so the lowest identifier (the
	// original) is canonical and fails the created-time check.
	original := storedIssue("original", "SNYK-100", "Done", desired)
	original.CreatedAt, original.ClosedAt = at(time.April), at(time.May)
	middle := storedIssue("middle", "SNYK-200", "Done", desired)
	middle.CreatedAt, middle.ClosedAt = at(time.June), at(time.July)
	newest := storedIssue("newest", "SNYK-300", "Done", desired)
	newest.CreatedAt, newest.ClosedAt = at(time.August), at(time.September)
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	linear := &fakeLinear{snapshot: []model.ExistingIssue{original, newest, middle}}
	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0 (must reopen the newest copy, not add another)", len(linear.created))
	}
	if result.Reopened != 1 || result.CancelledDuplicates != 0 {
		t.Fatalf("reopened=%d cancelledDuplicates=%d, want 1 and 0", result.Reopened, result.CancelledDuplicates)
	}
	if len(linear.updates) != 1 || linear.updates[0].Existing.ID != "newest" || linear.updates[0].Desired.State != model.StateTodo {
		t.Fatalf("updates = %v, want only the newest copy moved to todo (older copies untouched)", updatedIdentifiers(linear.updates))
	}

	// Next run: the reopened copy is canonical, older copies stay terminal
	// and untouched, and nothing new is created.
	reopenedTicket := newest
	reopenedTicket.StateName = "Todo"
	reopenedTicket.ClosedAt = nil
	linear2 := &fakeLinear{snapshot: []model.ExistingIssue{original, reopenedTicket, middle}}
	result2, err := New(cfg, discardLogger(), snyk, linear2, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if len(linear2.created) != 0 || len(linear2.updates) != 0 || result2.Reopened != 0 {
		t.Fatalf("run 2: created=%d updates=%v reopened=%d, want none", len(linear2.created), updatedIdentifiers(linear2.updates), result2.Reopened)
	}
}

// k8sWorkloadFinding is a Kubernetes-integration finding whose cluster is
// known, or unknown when the cluster lookup failed this run.
func k8sWorkloadFinding(projectID, issueID, cluster string, unknown bool) model.Finding {
	f := recreatedProjectFinding(projectID, issueID)
	f.ProjectName = "backend/deployment.apps/api:ghcr.io/tesslio/api"
	f.ProjectOrigin = "kubernetes"
	f.ProjectReference = ""
	f.ProjectTargetFile = ""
	f.ProjectNamespace = "backend"
	f.ProjectCluster = cluster
	f.ProjectClusterUnknown = unknown
	if unknown {
		f.ProjectCluster = ""
	}
	return f
}

func TestRunClusterLookupFailureKeepsStoredIdentityAndCluster(t *testing.T) {
	cfg := minimalCfg()
	known := k8sWorkloadFinding("project-a", "issue-a", "prod", false)
	existing := storedIssue("existing-1", "SNYK-100", "Todo", desiredIssue(cfg, known))
	if existing.Identity == "" || !strings.Contains(existing.Description, "Cluster: `prod`") {
		t.Fatalf("fixture must carry identity and cluster:\n%s", existing.Description)
	}

	failed := k8sWorkloadFinding("project-a", "issue-a", "", true)
	if model.FindingIdentity(failed) != "" {
		t.Fatalf("a failed cluster lookup must give a blank identity")
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:              []model.Finding{failed},
		ProjectIDs:            map[string]struct{}{"project-a": {}},
		ClusterLookupFailures: 1,
	}}

	result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ClusterLookupFailures != 1 {
		t.Fatalf("ClusterLookupFailures = %d, want 1", result.ClusterLookupFailures)
	}
	// Still matched by fingerprint, and the description does not churn.
	if len(linear.created) != 0 || len(linear.updates) != 0 {
		t.Fatalf("created=%d updates=%d, want 0/0 (no identity or cluster churn)", len(linear.created), len(linear.updates))
	}

	// A real change still updates the ticket, keeping identity and cluster.
	failed.Severity = "critical"
	linear2 := &fakeLinear{snapshot: []model.ExistingIssue{existing}}
	snyk.snapshot.Findings = []model.Finding{failed}
	if _, err := New(cfg, discardLogger(), snyk, linear2, nil).Run(context.Background()); err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if len(linear2.updates) != 1 {
		t.Fatalf("run 2: updates = %d, want 1", len(linear2.updates))
	}
	desc := linear2.updates[0].Desired.Description
	if !strings.Contains(desc, "identity: "+existing.Identity) || !strings.Contains(desc, "Cluster: `prod`") {
		t.Fatalf("run 2: stored identity or cluster stripped:\n%s", desc)
	}
}

func TestRunClusterLookupFailureDefersRebindUntilLookupSucceeds(t *testing.T) {
	cfg := minimalCfg()
	oldFinding := k8sWorkloadFinding("project-old", "issue-old", "prod", false)
	oldTicket := storedIssue("old-ticket", "SNYK-100", "Todo", desiredIssue(cfg, oldFinding))

	// Run 1: the project is recreated, but the new project's cluster lookup
	// fails. No rebind, no fresh ticket; the old ticket is cancelled with a
	// sync-recorded closed_reason as usual.
	snyk1 := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:              []model.Finding{k8sWorkloadFinding("project-new", "issue-new", "", true)},
		ProjectIDs:            map[string]struct{}{"project-new": {}},
		ClusterLookupFailures: 1,
	}}
	linear1 := &fakeLinear{snapshot: []model.ExistingIssue{oldTicket}}
	result1, err := New(cfg, discardLogger(), snyk1, linear1, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if result1.Rebound != 0 || len(linear1.created) != 0 || result1.DeferredCreates != 1 {
		t.Fatalf("run 1: rebound=%d created=%d deferred=%d, want 0/0/1", result1.Rebound, len(linear1.created), result1.DeferredCreates)
	}
	if len(linear1.updates) != 1 || linear1.updates[0].Desired.State != model.StateCancelled {
		t.Fatalf("run 1: want the old ticket cancelled, got %v", linear1.updates)
	}
	cancelled := storedIssue("old-ticket", "SNYK-100", "Cancelled", linear1.updates[0].Desired)
	if cancelled.ClosedReason != model.ClosedReasonProjectMissing || cancelled.Identity != oldTicket.Identity {
		t.Fatalf("run 1: closed_reason=%q identity=%q", cancelled.ClosedReason, cancelled.Identity)
	}

	// Run 2: the lookup succeeds and the ticket is rebound and reopened.
	newFinding := k8sWorkloadFinding("project-new", "issue-new", "prod", false)
	snyk2 := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{newFinding},
		ProjectIDs: map[string]struct{}{"project-new": {}},
	}}
	linear2 := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
	result2, err := New(cfg, discardLogger(), snyk2, linear2, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if result2.Rebound != 1 || len(linear2.created) != 0 || len(linear2.updates) != 1 {
		t.Fatalf("run 2: rebound=%d created=%d updates=%d, want 1/0/1", result2.Rebound, len(linear2.created), len(linear2.updates))
	}
	if u := linear2.updates[0]; u.Desired.Fingerprint != newFinding.Fingerprint || u.Desired.State != model.StateTodo {
		t.Fatalf("run 2: update = %+v", u.Desired)
	}
}

func TestRunClusterLookupFailureStillCreatesWhenNoCandidateCouldMatch(t *testing.T) {
	cfg := minimalCfg()
	// No ticket from a vanished project could be this finding's (the only
	// cancelled ticket is for a different workload), so a failing lookup
	// does not hold the finding back: it gets a ticket, without identity.
	other := k8sWorkloadFinding("project-gone", "issue-g", "prod", false)
	other.ProjectName = "backend/deployment.apps/worker:ghcr.io/tesslio/worker"
	cancelled := withClosedReason(storedIssue("other", "SNYK-100", "Cancelled", desiredIssue(cfg, other)), model.ClosedReasonProjectMissing)
	linear := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{k8sWorkloadFinding("project-prod", "issue-p", "", true)},
		ProjectIDs: map[string]struct{}{"project-prod": {}},
	}}
	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 1 || strings.Contains(linear.created[0].Description, "identity:") {
		t.Fatalf("want one ticket created without an identity line, got %v", linear.created)
	}
}

func TestRunClusterLookupFailureDeferralIsCappedAt24Hours(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closedAgo  time.Duration
		noClosedAt bool
		wantDefer  bool
	}{
		{name: "candidate closed an hour ago", closedAgo: time.Hour, wantDefer: true},
		{name: "candidate closed more than 24 hours ago", closedAgo: 25 * time.Hour},
		{name: "candidate closed at an unknown time", noClosedAt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			oldFinding := k8sWorkloadFinding("project-old", "issue-old", "prod", false)
			cancelled := withClosedReason(storedIssue("old-ticket", "SNYK-100", "Cancelled", desiredIssue(cfg, oldFinding)), model.ClosedReasonProjectMissing)
			if !tc.noClosedAt {
				closedAt := time.Now().Add(-tc.closedAgo)
				cancelled.ClosedAt = &closedAt
			}
			linear := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
			snyk := fakeSnyk{snapshot: model.SnykSnapshot{
				Findings:              []model.Finding{k8sWorkloadFinding("project-new", "issue-new", "", true)},
				ProjectIDs:            map[string]struct{}{"project-new": {}},
				ClusterLookupFailures: 1,
			}}

			result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if tc.wantDefer {
				if result.DeferredCreates != 1 || len(linear.created) != 0 {
					t.Fatalf("deferred=%d created=%d, want 1/0", result.DeferredCreates, len(linear.created))
				}
				return
			}
			if result.DeferredCreates != 0 || len(linear.created) != 1 {
				t.Fatalf("deferred=%d created=%d, want 0/1 (the window has passed; create without identity)", result.DeferredCreates, len(linear.created))
			}
			if strings.Contains(linear.created[0].Description, "identity:") {
				t.Fatalf("created ticket must carry no identity:\n%s", linear.created[0].Description)
			}
			if result.Rebound != 0 || len(linear.updates) != 0 {
				t.Fatalf("rebound=%d updates=%d, want 0/0 (candidate left alone)", result.Rebound, len(linear.updates))
			}
		})
	}
}

func TestRunResolveLoopKeepsClosedTicketStateWhenProjectGoes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		inactive bool
	}{
		{name: "project deactivated", inactive: true},
		{name: "project deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			open := storedIssue("open", "SNYK-1", "Todo", desiredIssue(cfg, recreatedProjectFinding("project-gone", "issue-1")))
			// Fixed before Snyk deactivated or deleted its project.
			fixed := storedIssue("fixed", "SNYK-2", "Done", desiredIssue(cfg, recreatedProjectFinding("project-gone", "issue-2")))
			snapshot := model.SnykSnapshot{ProjectIDs: map[string]struct{}{"project-live": {}}}
			if tc.inactive {
				snapshot.InactiveProjectIDs = map[string]struct{}{"project-gone": {}}
			}
			linear := &fakeLinear{snapshot: []model.ExistingIssue{open, fixed}}

			result, err := New(cfg, discardLogger(), fakeSnyk{snapshot: snapshot}, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.PlannedResolves != 1 || len(linear.updates) != 1 {
				t.Fatalf("resolves=%d updates=%v, want only the open ticket cancelled", result.PlannedResolves, updatedIdentifiers(linear.updates))
			}
			if u := linear.updates[0]; u.Existing.ID != "open" || u.Desired.State != model.StateCancelled {
				t.Fatalf("update = %s -> %q, want open -> cancelled", u.Existing.Identifier, u.Desired.State)
			}
		})
	}
}

func TestRunResolveLoopStillMovesClosedTicketToDoneInLiveProject(t *testing.T) {
	cfg := minimalCfg()
	// Cancelled while the finding was ignored; the finding is now gone from
	// a project that still exists, so it was fixed.
	cancelled := storedIssue("cancelled", "SNYK-1", "Cancelled", desiredIssue(cfg, recreatedProjectFinding("project-live", "issue-1")))
	linear := &fakeLinear{snapshot: []model.ExistingIssue{cancelled}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{ProjectIDs: map[string]struct{}{"project-live": {}}}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.updates) != 1 || linear.updates[0].Desired.State != model.StateDone || !linear.updates[0].Diff.StateChanged {
		t.Fatalf("updates = %+v, want the ticket moved to done", linear.updates)
	}
}

func TestRunReopenRestoresStateFromBeforeClosure(t *testing.T) {
	created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
	inProgress := model.WorkflowState{ID: "state-in-progress", Name: "In Progress"}

	for _, tc := range []struct {
		name  string
		setup func(cfg config.Config) (model.ExistingIssue, model.SnykSnapshot)
	}{
		{
			name: "closed while still open in Snyk",
			setup: func(cfg config.Config) (model.ExistingIssue, model.SnykSnapshot) {
				finding := recreatedProjectFinding("project-a", "issue-a")
				existing := storedIssue("existing-1", "SNYK-100", "Cancelled", desiredIssue(cfg, finding))
				existing.CreatedAt = &created
				existing.ClosedAt = &closed
				return existing, model.SnykSnapshot{
					Findings:   []model.Finding{finding},
					ProjectIDs: map[string]struct{}{"project-a": {}},
				}
			},
		},
		{
			name: "rebind after project recreation",
			setup: func(cfg config.Config) (model.ExistingIssue, model.SnykSnapshot) {
				existing := storedIssue("existing-1", "SNYK-100", "Cancelled", desiredIssue(cfg, recreatedProjectFinding("project-old", "issue-old")))
				existing = withClosedReason(existing, model.ClosedReasonProjectDeactivated)
				return existing, model.SnykSnapshot{
					Findings:           []model.Finding{recreatedProjectFinding("project-new", "issue-new")},
					ProjectIDs:         map[string]struct{}{"project-new": {}},
					InactiveProjectIDs: map[string]struct{}{"project-old": {}},
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			existing, snapshot := tc.setup(cfg)
			linear := &fakeLinear{
				snapshot:       []model.ExistingIssue{existing},
				lastOpenStates: map[string]model.WorkflowState{"existing-1": inProgress},
			}

			result, err := New(cfg, discardLogger(), fakeSnyk{snapshot: snapshot}, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Reopened+result.Rebound == 0 || len(linear.created) != 0 || len(linear.updates) != 1 {
				t.Fatalf("reopened=%d rebound=%d created=%d updates=%d, want one reopen", result.Reopened, result.Rebound, len(linear.created), len(linear.updates))
			}
			if !slices.Equal(slices.Concat(linear.lastOpenStatesCalls...), []string{"existing-1"}) {
				t.Fatalf("LastOpenStates calls = %v, want one for existing-1", linear.lastOpenStatesCalls)
			}
			u := linear.updates[0]
			if !u.Desired.Reopen || u.Desired.RestoreState != inProgress {
				t.Fatalf("reopen=%v restore=%+v, want reopen into %+v", u.Desired.Reopen, u.Desired.RestoreState, inProgress)
			}
			if !u.Diff.StateChanged || u.Diff.StateTo != "In Progress" {
				t.Fatalf("diff = %+v, want a state change to In Progress", u.Diff)
			}
			if !strings.Contains(u.Desired.StateReason, "back in the state it was in before it was closed") {
				t.Fatalf("StateReason = %q", u.Desired.StateReason)
			}
		})
	}
}

func TestRunReopenFallsBackToConfiguredOpenStateWithoutHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "no open state in history"},
		{name: "history unreadable", err: errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := minimalCfg()
			created := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
			closed := time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC)
			finding := recreatedProjectFinding("project-a", "issue-a")
			existing := storedIssue("existing-1", "SNYK-100", "Done", desiredIssue(cfg, finding))
			existing.CreatedAt = &created
			existing.ClosedAt = &closed
			linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}, lastOpenStatesErr: tc.err}
			snyk := fakeSnyk{snapshot: model.SnykSnapshot{
				Findings:   []model.Finding{finding},
				ProjectIDs: map[string]struct{}{"project-a": {}},
			}}

			result, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background())
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Reopened != 1 || len(linear.updates) != 1 || result.FailedOps != 0 {
				t.Fatalf("reopened=%d updates=%d failed=%d, want the reopen to go ahead", result.Reopened, len(linear.updates), result.FailedOps)
			}
			u := linear.updates[0]
			if u.Desired.RestoreState != (model.WorkflowState{}) || u.Desired.State != model.StateTodo || u.Diff.StateTo != "todo" {
				t.Fatalf("restore=%+v state=%q stateTo=%q, want the configured open state", u.Desired.RestoreState, u.Desired.State, u.Diff.StateTo)
			}
			if strings.Contains(u.Desired.StateReason, "before it was closed") {
				t.Fatalf("StateReason = %q, must not claim a restore", u.Desired.StateReason)
			}
		})
	}
}

func TestRunReadsHistoryOnlyForReopens(t *testing.T) {
	cfg := minimalCfg()
	finding := recreatedProjectFinding("project-a", "issue-a")
	stale := desiredIssue(cfg, finding)
	stale.Title = "stale title"
	linear := &fakeLinear{snapshot: []model.ExistingIssue{storedIssue("existing-1", "SNYK-100", "Todo", stale)}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}

	if _, err := New(cfg, discardLogger(), snyk, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.updates) != 1 || len(linear.lastOpenStatesCalls) != 0 {
		t.Fatalf("updates=%d history reads=%v, want an update and no history read", len(linear.updates), linear.lastOpenStatesCalls)
	}
}

func TestComputeDiffReportsRestoredState(t *testing.T) {
	states := config.StateConfig{Todo: "Triage", Done: "Done", Cancelled: "Cancelled"}
	existing := model.ExistingIssue{StateName: "Canceled"}
	desired := model.DesiredIssue{State: model.StateTodo, Reopen: true, RestoreState: model.WorkflowState{ID: "state-todo", Name: "Todo"}}
	if d := ComputeDiff(existing, desired, states); !d.StateChanged || d.StateFrom != "Canceled" || d.StateTo != "Todo" {
		t.Fatalf("diff = %+v, want Canceled -> Todo", d)
	}
}

// scanFinding is a Snyk Code finding as one scan of commit reports it, with
// the flagged region starting at line.
func scanFinding(issueID, commit string, line int) model.Finding {
	return model.Finding{
		Fingerprint:       model.Fingerprint("project-a", issueID, "src/app.py"),
		SnykIssueID:       issueID,
		SnykIssueKey:      "key-" + issueID,
		IssueType:         "code",
		ProjectID:         "project-a",
		ProjectName:       "owner/repo(main)",
		ProjectOrigin:     "github",
		ProjectReference:  "main",
		IssueTitle:        "Path Traversal",
		Severity:          "low",
		Status:            model.FindingIgnored,
		IssueURL:          "https://app.example.test/" + issueID,
		IssueAPIURL:       "https://api.example.test/" + issueID,
		Repository:        "owner/repo",
		SourceFile:        "src/app.py",
		SourceCommitID:    commit,
		SourceLineStart:   line,
		SourceColumnStart: 10,
		SourceLineEnd:     line,
		SourceColumnEnd:   14,
		Description:       "Unsanitized input flows into open.",
		CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
	}
}

const (
	commitA = "1111111111111111111111111111111111111111"
	commitB = "2222222222222222222222222222222222222222"
)

var linearStoredLinkPattern = regexp.MustCompile(`\]\((https://[^)]+)\)`)

// linearStored returns description the way Linear stores it after a write:
// the heading's brackets escaped and followed by a blank line, and every link
// target wrapped in angle brackets.
func linearStored(description string) string {
	heading, rest, _ := strings.Cut(description, "\n")
	heading = strings.NewReplacer("[", `\[`, "]", `\]`).Replace(heading)
	return heading + "\n\n" + linearStoredLinkPattern.ReplaceAllString(rest, "](<$1>)")
}

func githubCfg() config.Config {
	cfg := minimalCfg()
	cfg.Source = config.SourceConfig{Provider: "github"}
	return cfg
}

func TestNormalizeDescriptionForCompareMatchesLinearStoredForm(t *testing.T) {
	desired := desiredIssue(githubCfg(), scanFinding("issue-1", commitA, 10)).Description
	stored := linearStored(desired)
	if !strings.Contains(stored, "\\]\n\nRepository: [owner/repo](<https://github.com/owner/repo>)") {
		t.Fatalf("fixture is not in Linear's stored form:\n%s", stored)
	}
	if normalizeDescriptionForCompare(stored) != normalizeDescriptionForCompare(desired) {
		t.Fatalf("stored and written descriptions compare different:\n%s\n---\n%s", normalizeDescriptionForCompare(stored), normalizeDescriptionForCompare(desired))
	}
}

func TestNormalizeDescriptionForCompareIgnoresScanPins(t *testing.T) {
	cfg := githubCfg()
	before := desiredIssue(cfg, scanFinding("issue-1", commitA, 10)).Description
	rescanned := scanFinding("issue-1", commitB, 40)
	rescanned.SourceLineEnd = 44
	after := desiredIssue(cfg, rescanned).Description
	if !strings.Contains(after, commitB) || !strings.Contains(after, "#L40-L44") || !strings.Contains(after, "(line 40:10 to 44:14)") {
		t.Fatalf("fixture does not move the scan pins:\n%s", after)
	}
	if normalizeDescriptionForCompare(linearStored(before)) != normalizeDescriptionForCompare(after) {
		t.Fatalf("a rescan that only moved the commit and lines compares different:\n%s\n---\n%s", normalizeDescriptionForCompare(before), normalizeDescriptionForCompare(after))
	}

	for _, tc := range []struct {
		name   string
		change func(*model.Finding)
	}{
		{name: "status", change: func(f *model.Finding) { f.Status = model.FindingOpen }},
		{name: "branch", change: func(f *model.Finding) { f.ProjectReference = "release" }},
		{name: "source file", change: func(f *model.Finding) { f.SourceFile = "src/other.py" }},
		{name: "commit link in Snyk prose", change: func(f *model.Finding) {
			f.Description = "Fixed upstream in https://github.com/upstream/lib/commit/" + commitB
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := scanFinding("issue-1", commitA, 10)
			tc.change(&changed)
			if normalizeDescriptionForCompare(before) == normalizeDescriptionForCompare(desiredIssue(cfg, changed).Description) {
				t.Fatalf("a %s change was hidden by the scan-pin normalization", tc.name)
			}
		})
	}
}

// linearSim stands in for Linear across runs: it keeps each written
// description in Linear's stored form.
type linearSim struct {
	fakeLinear
	issues map[string]*model.ExistingIssue
	order  []string
}

func (l *linearSim) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	out := make([]model.ExistingIssue, 0, len(l.order))
	for _, id := range l.order {
		out = append(out, *l.issues[id])
	}
	return out, nil
}

func (l *linearSim) UpdateIssues(ctx context.Context, updates []model.IssueUpdate) error {
	for _, u := range updates {
		issue := l.issues[u.Existing.ID]
		issue.Title, issue.Description, issue.DueDate, issue.Priority = u.Desired.Title, linearStored(u.Desired.Description), u.Desired.DueDate, u.Desired.Priority
		issue.ManagedLabels = u.Desired.ManagedLabels
	}
	return l.fakeLinear.UpdateIssues(ctx, updates)
}

func TestRunDoesNotRewriteTicketsOnRescanWithoutChanges(t *testing.T) {
	cfg := githubCfg()
	sim := &linearSim{issues: map[string]*model.ExistingIssue{}}
	for i, issueID := range []string{"issue-1", "issue-2"} {
		f := scanFinding(issueID, commitA, 10+i)
		issue := storedIssue(fmt.Sprintf("existing-%d", i+1), fmt.Sprintf("SNYK-%d", i+1), "Canceled", desiredIssue(cfg, f))
		issue.Description = linearStored(issue.Description)
		sim.issues[issue.ID] = &issue
		sim.order = append(sim.order, issue.ID)
	}
	store := &fakeCache{}

	run := func(name string, findings ...model.Finding) {
		t.Helper()
		sim.updates = nil
		store.snapshot = store.saved
		snyk := fakeSnyk{snapshot: model.SnykSnapshot{Findings: findings, ProjectIDs: map[string]struct{}{"project-a": {}}}}
		if _, err := New(cfg, discardLogger(), snyk, sim, store).Run(context.Background()); err != nil {
			t.Fatalf("%s: Run() error = %v", name, err)
		}
	}

	run("first run, nothing changed", scanFinding("issue-1", commitA, 10), scanFinding("issue-2", commitA, 11))
	if len(sim.updates) != 0 {
		t.Fatalf("first run rewrote %v, want nothing", updatedIdentifiers(sim.updates))
	}
	run("rescan of a new commit", scanFinding("issue-1", commitB, 30), scanFinding("issue-2", commitB, 31))
	if len(sim.updates) != 0 {
		t.Fatalf("rescan rewrote %v, want nothing", updatedIdentifiers(sim.updates))
	}

	fixed := scanFinding("issue-2", commitB, 31)
	fixed.Status = model.FindingFixed
	run("real change", scanFinding("issue-1", commitB, 30), fixed)
	if len(sim.updates) != 1 || sim.updates[0].Existing.ID != "existing-2" {
		t.Fatalf("updates = %v, want only the fixed ticket", updatedIdentifiers(sim.updates))
	}
	if !strings.Contains(sim.issues["existing-2"].Description, commitB) {
		t.Fatalf("a real update must still write the current commit:\n%s", sim.issues["existing-2"].Description)
	}
}

func TestRunCachesHashOfMatchedTicketForDuplicateFingerprint(t *testing.T) {
	cfg := minimalCfg()
	finding := recreatedProjectFinding("project-a", "issue-a")
	desired := desiredIssue(cfg, finding)
	// The open copy is canonical; the older cancelled copy comes later in the
	// snapshot, the way Linear lists newest first.
	canonical := storedIssue("open", "SNYK-200", "Todo", desired)
	olderCopy := storedIssue("old", "SNYK-100", "Cancelled", desired)
	olderCopy.Title = "older title"
	// A ticket with a pending change, so the run writes and refreshes the
	// cache from Linear.
	other := recreatedProjectFinding("project-a", "issue-b")
	stale := desiredIssue(cfg, other)
	stale.Title = "stale title"
	linear := &fakeLinear{snapshot: []model.ExistingIssue{canonical, olderCopy, storedIssue("other", "SNYK-300", "Todo", stale)}}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   []model.Finding{finding, other},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("planned updates = %d, want 1 so the cache is refreshed from Linear", result.PlannedUpdates)
	}
	if got, want := store.saved.LinearHashes[finding.Fingerprint], existingIssueHash(canonical); got != want {
		t.Fatalf("cached Linear hash is not the matched ticket's: got %s, want %s (older copy %s)", got, want, existingIssueHash(olderCopy))
	}
}

// updateCallSizes renders the per-call batch sizes of an UpdateIssues
// sequence, for assertion failure messages.
func updateCallSizes(batches [][]model.IssueUpdate) []int {
	sizes := make([]int, 0, len(batches))
	for _, batch := range batches {
		sizes = append(sizes, len(batch))
	}
	return sizes
}

// executeJobUpdateScenario drives Service.executeJob directly for one
// update-kind job and returns the result and the fake Linear client, so the
// three update kinds can share one table.
func executeJobUpdateScenario(t *testing.T, kind jobKind, cfg config.Config) (RunResult, *fakeLinear) {
	t.Helper()
	batch := make([]model.IssueUpdate, 0, 3)
	for _, ident := range []string{"SNYK-1", "SNYK-2", "SNYK-3"} {
		existing := model.ExistingIssue{
			ID:          "issue-" + ident,
			Identifier:  ident,
			Title:       "stale title",
			Description: "old description",
			StateName:   "Blog",
			Fingerprint: "snyk:project-a:" + ident,
		}
		desired := desiredIssue(cfg, model.Finding{
			Fingerprint: existing.Fingerprint,
			SnykIssueID: ident,
			ProjectID:   "project-a",
			ProjectName: "Project A",
			IssueTitle:  "Outdated package",
			Severity:    "high",
			Status:      model.FindingOpen,
			IssueURL:    "https://example.test/" + ident,
			CreatedAt:   time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
		})
		desired.PreserveState = true
		batch = append(batch, model.IssueUpdate{Existing: existing, Desired: desired})
	}

	linear := &fakeLinear{batchErrAbove: 1}
	service := New(cfg, discardLogger(), fakeSnyk{}, linear, nil)
	var result RunResult
	var failed failedFingerprints
	if err := service.executeJob(context.Background(), job{kind: kind, updateBatch: batch}, &result, &failed); err != nil {
		t.Fatalf("executeJob(%s) error = %v", kind, err)
	}
	return result, linear
}

// TestExecuteJobFallsBackPerItemForEveryUpdateKind pins the batch-with-
// fallback contract for all three update kinds: a failing multi-item batch
// call must be retried item by item, and the retries that succeed are not
// failures. The per-kind counter still counts the whole batch.
func TestExecuteJobFallsBackPerItemForEveryUpdateKind(t *testing.T) {
	cases := []struct {
		kind    jobKind
		counter func(RunResult) int64
	}{
		{jobUpdate, func(r RunResult) int64 { return r.PlannedUpdates }},
		{jobResolve, func(r RunResult) int64 { return r.PlannedResolves }},
		{jobCancelDuplicate, func(r RunResult) int64 { return r.CancelledDuplicates }},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			cfg := minimalCfg()
			cfg.Linear.CommentsEnabled = true
			result, linear := executeJobUpdateScenario(t, tc.kind, cfg)

			if got := tc.counter(result); got != 3 {
				t.Fatalf("counter = %d, want 3 (the whole batch is counted)", got)
			}
			if result.FailedOps != 0 {
				t.Fatalf("FailedOps = %d, want 0 (every single-item retry succeeded)", result.FailedOps)
			}
			if len(linear.updated) != 3 {
				t.Fatalf("updated = %d, want 3 (batch failure must fall back to per-item retries)", len(linear.updated))
			}
			// Comments are deliberately posted only when the batch update call
			// itself succeeded, so the fallback path must not post any.
			if len(linear.comments) != 0 {
				t.Fatalf("comments = %d, want 0 (no comments on the per-item fallback)", len(linear.comments))
			}
		})
	}
}

// TestExecuteJobCountsEveryItemThatFailsIndividually: when the per-item retry
// fails too, every item counts as one FailedOps and both of its fingerprints
// are recorded in the failed set, so the next run retries them.
func TestExecuteJobCountsEveryItemThatFailsIndividually(t *testing.T) {
	for _, kind := range []jobKind{jobUpdate, jobResolve, jobCancelDuplicate} {
		t.Run(string(kind), func(t *testing.T) {
			cfg := minimalCfg()
			batch := make([]model.IssueUpdate, 0, 3)
			var fingerprints []string
			for _, ident := range []string{"SNYK-1", "SNYK-2", "SNYK-3"} {
				existing := model.ExistingIssue{
					ID:          "issue-" + ident,
					Identifier:  ident,
					StateName:   "Blog",
					Fingerprint: "snyk:project-a:" + ident,
				}
				desired := model.DesiredIssue{Fingerprint: existing.Fingerprint, State: model.StateDone, PreserveState: true}
				fingerprints = append(fingerprints, existing.Fingerprint)
				batch = append(batch, model.IssueUpdate{Existing: existing, Desired: desired})
			}
			linear := &fakeLinear{updateErr: errors.New("linear unavailable")}
			service := New(cfg, discardLogger(), fakeSnyk{}, linear, nil)
			var failed failedFingerprints
			var failing RunResult
			if err := service.executeJob(context.Background(), job{kind: kind, updateBatch: batch}, &failing, &failed); err != nil {
				t.Fatalf("executeJob(%s) error = %v", kind, err)
			}
			if failing.FailedOps != 3 {
				t.Fatalf("FailedOps = %d, want 3 (every item of the batch failed individually)", failing.FailedOps)
			}
			for _, fingerprint := range fingerprints {
				if !failed.has(fingerprint) {
					t.Fatalf("failed fingerprint set does not hold %q", fingerprint)
				}
			}
		})
	}
}

// TestExecuteJobCountsOneRefusedIssueWithoutBlockingTheBatch covers the refusal
// the linear client reports when an issue's live labels cannot be read —
// including when the label page is truncated: the multi-item batch call fails,
// and the per-item fallback retries every item, where the refused issue fails
// again while its batch-mates land. The refused issue counts once in FailedOps
// and in the failed-fingerprint set; the rest of the batch is not blocked.
func TestExecuteJobCountsOneRefusedIssueWithoutBlockingTheBatch(t *testing.T) {
	cfg := minimalCfg()
	batch := make([]model.IssueUpdate, 0, 3)
	for _, ident := range []string{"SNYK-1", "SNYK-2", "SNYK-3"} {
		existing := model.ExistingIssue{
			ID:          "issue-" + ident,
			Identifier:  ident,
			StateName:   "Blog",
			Fingerprint: "snyk:project-a:" + ident,
		}
		desired := model.DesiredIssue{Fingerprint: existing.Fingerprint, State: model.StateDone, PreserveState: true}
		batch = append(batch, model.IssueUpdate{Existing: existing, Desired: desired})
	}
	linear := &fakeLinear{updateFailIssueIDs: map[string]bool{"issue-SNYK-2": true}}
	service := New(cfg, discardLogger(), fakeSnyk{}, linear, nil)
	var failed failedFingerprints
	var result RunResult
	if err := service.executeJob(context.Background(), job{kind: jobUpdate, updateBatch: batch}, &result, &failed); err != nil {
		t.Fatalf("executeJob(jobUpdate) error = %v", err)
	}

	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1 (only the refused issue counts)", result.FailedOps)
	}
	if len(linear.updateCallBatches) != 4 {
		t.Fatalf("UpdateIssues calls = %d, want 4 (one batch plus three per-item retries)", len(linear.updateCallBatches))
	}
	if got := len(linear.updateCallBatches[0]); got != 3 {
		t.Fatalf("first UpdateIssues call carried %d updates, want the whole batch of 3", got)
	}
	for i, call := range linear.updateCallBatches[1:] {
		if len(call) != 1 {
			t.Fatalf("per-item retry %d carried %d updates, want 1", i, len(call))
		}
	}
	// The refused issue is absent from the landed writes; its batch-mates are
	// not blocked by it.
	var landedIdentifiers []string
	for _, u := range linear.updates {
		landedIdentifiers = append(landedIdentifiers, u.Existing.Identifier)
	}
	if !slices.Equal(landedIdentifiers, []string{"SNYK-1", "SNYK-3"}) {
		t.Fatalf("landed updates = %v, want SNYK-1 and SNYK-3 (SNYK-2 is refused, the rest not blocked)", landedIdentifiers)
	}
	if !failed.has("snyk:project-a:SNYK-2") {
		t.Fatal("failed fingerprint set does not hold the refused issue's fingerprint")
	}
	if failed.has("snyk:project-a:SNYK-1") || failed.has("snyk:project-a:SNYK-3") {
		t.Fatal("failed fingerprint set holds a fingerprint whose issue landed; only the refused issue belongs there")
	}
}

// TestExecuteJobDryRunCountsWithoutCalling: a dry run counts the work it would
// have done and must not touch Linear at all.
func TestExecuteJobDryRunCountsWithoutCalling(t *testing.T) {
	cases := []struct {
		kind    jobKind
		counter func(RunResult) int64
	}{
		{jobUpdate, func(r RunResult) int64 { return r.PlannedUpdates }},
		{jobResolve, func(r RunResult) int64 { return r.PlannedResolves }},
		{jobCancelDuplicate, func(r RunResult) int64 { return r.CancelledDuplicates }},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			cfg := minimalCfg()
			cfg.DryRun = true
			result, linear := executeJobUpdateScenario(t, tc.kind, cfg)

			if got := tc.counter(result); got != 3 {
				t.Fatalf("counter = %d, want 3 (dry run still counts the planned work)", got)
			}
			if len(linear.updated) != 0 {
				t.Fatalf("updated = %d, want 0 (dry run must not call Linear)", len(linear.updated))
			}
			if result.FailedOps != 0 {
				t.Fatalf("FailedOps = %d, want 0", result.FailedOps)
			}
		})
	}
}

// TestPostCommentsWithFallbackRetriesAllWhenBatchCallFails: a wholesale
// PostComments failure (no per-alias data at all, e.g. a transport failure)
// must retry every comment individually, and stay a warning: no error escapes
// and nothing is counted or recorded as a failed write.
func TestPostCommentsWithFallbackRetriesAllWhenBatchCallFails(t *testing.T) {
	batch := make([]model.IssueUpdate, 0, 2)
	for _, ident := range []string{"SNYK-1", "SNYK-2"} {
		existing := model.ExistingIssue{
			ID:          "issue-" + ident,
			Identifier:  ident,
			StateName:   "Blog",
			Fingerprint: "snyk:project-a:" + ident,
		}
		desired := model.DesiredIssue{Fingerprint: existing.Fingerprint, State: model.StateDone, PreserveState: true}
		batch = append(batch, model.IssueUpdate{Existing: existing, Desired: desired})
	}
	linear := &fakeLinear{commentErr: errors.New("linear transport down")}
	service := New(minimalCfg(), discardLogger(), fakeSnyk{}, linear, nil)
	var result RunResult
	service.postCommentsWithFallback(context.Background(), batch, &result)

	if len(linear.commentCallBatches) != 3 {
		t.Fatalf("PostComments calls = %d, want 3 (failed batch + one retry per item)", len(linear.commentCallBatches))
	}
	if got := len(linear.commentCallBatches[0]); got != 2 {
		t.Fatalf("first PostComments call carried %d updates, want the whole batch of 2", got)
	}
	for i, retry := range linear.commentCallBatches[1:] {
		if len(retry) != 1 {
			t.Fatalf("retry call %d carried %d updates, want exactly 1", i, len(retry))
		}
	}
	if len(linear.comments) != 0 {
		t.Fatalf("comments = %d, want 0 (every attempt failed)", len(linear.comments))
	}
	if result.FailedComments != 2 {
		t.Fatalf("FailedComments = %d, want 2 (both individual retries failed)", result.FailedComments)
	}
}

// TestPostCommentsWithFallbackCountsRetryReportingFailedIndex pins the
// single-comment retry's own failed-index return: a retry that reports its
// comment as failed with a nil error must be counted, not dropped silently.
func TestPostCommentsWithFallbackCountsRetryReportingFailedIndex(t *testing.T) {
	batch := make([]model.IssueUpdate, 0, 2)
	for _, ident := range []string{"SNYK-1", "SNYK-2"} {
		existing := model.ExistingIssue{
			ID:          "issue-" + ident,
			Identifier:  ident,
			StateName:   "Todo",
			Fingerprint: "snyk:project-a:" + ident,
		}
		desired := model.DesiredIssue{Fingerprint: existing.Fingerprint, State: model.StateDone, PreserveState: true}
		batch = append(batch, model.IssueUpdate{Existing: existing, Desired: desired})
	}
	// SNYK-1 fails in the batch and again on its retry; SNYK-2 posts.
	linear := &fakeLinear{commentFailFingerprints: map[string]int{"snyk:project-a:SNYK-1": 2}}
	service := New(minimalCfg(), discardLogger(), fakeSnyk{}, linear, nil)
	var result RunResult
	service.postCommentsWithFallback(context.Background(), batch, &result)

	if len(linear.commentCallBatches) != 2 {
		t.Fatalf("PostComments calls = %d, want 2 (batch + one retry of the failed comment)", len(linear.commentCallBatches))
	}
	if result.FailedComments != 1 {
		t.Fatalf("FailedComments = %d, want 1 (the retry reported its comment failed)", result.FailedComments)
	}
	if len(linear.comments) != 1 || linear.comments[0].Existing.Identifier != "SNYK-2" {
		t.Fatalf("comments = %+v, want only SNYK-2 posted", linear.comments)
	}
}

// TestRunUpdatesResolvesAndCancelsWithBatchFallback drives the real Run entry
// point over inputs that produce all three update kinds at once — an update
// (changed title), a resolve (finding gone from an active project) and a
// duplicate cancel (two open tickets sharing one fingerprint) — with an
// UpdateIssues that fails for every multi-item batch. Every kind must still
// land its planned mutations through the per-item fallback, with no failures.
func TestRunUpdatesResolvesAndCancelsWithBatchFallback(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true
	logger := discardLogger()

	openFindings := func() []model.Finding {
		return []model.Finding{
			{
				Fingerprint: "snyk:project-a:issue-1",
				SnykIssueID: "issue-1",
				ProjectID:   "project-a",
				ProjectName: "Project A",
				IssueTitle:  "First finding",
				Severity:    "high",
				Status:      model.FindingOpen,
				CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
			},
			{
				Fingerprint: "snyk:project-a:issue-2",
				SnykIssueID: "issue-2",
				ProjectID:   "project-a",
				ProjectName: "Project A",
				IssueTitle:  "Second finding",
				Severity:    "high",
				Status:      model.FindingOpen,
				CreatedAt:   time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
			},
		}
	}

	build := func() (fakeSnyk, *fakeLinear) {
		findings := openFindings()
		snyk := fakeSnyk{snapshot: model.SnykSnapshot{
			Findings:   findings,
			ProjectIDs: map[string]struct{}{"project-a": {}},
		}}

		// The first finding's ticket is stale, so it is the update.
		updates := make([]model.ExistingIssue, 0, 1)
		firstDesired := desiredIssue(cfg, findings[0])
		firstStale := firstDesired
		// A stale title differs from the desired one, so needsUpdate is true;
		// the state stays non-terminal, so the update preserves it.
		firstStale.Title = "stale title"
		update := storedIssue("existing-1", "SNYK-1", "Blog", firstStale)
		updates = append(updates, update)
		// Two tickets whose findings are gone from an active project: the
		// resolve loop closes both (done), never the duplicate-cancel loop.
		// Two of them also make the resolve batch two items long, so the
		// multi-item batch call is the one that fails and the per-item
		// fallback is exercised for resolves exactly as for updates.
		resolved := model.ExistingIssue{
			ID:          "existing-9",
			Identifier:  "SNYK-19",
			Title:       "resolved issue",
			Description: "old description",
			StateName:   "Blog",
			Fingerprint: "snyk:project-a:issue-gone",
		}
		orphan := model.ExistingIssue{
			ID:          "existing-8",
			Identifier:  "SNYK-28",
			Title:       "another resolved issue",
			Description: "old description",
			StateName:   "Blog",
			Fingerprint: "snyk:project-a:issue-also-gone",
		}
		// Two open tickets sharing one fingerprint of their own (no finding
		// claims it, so they cannot collide with the update or resolve
		// tickets); the higher identifier is the duplicate the sync cancels.
		// The live finding's ticket must be one of the two copies: the
		// resolve loop skips fingerprints a finding claims, so the pair
		// contributes the duplicate cancel and nothing to the resolve batch,
		// which is exactly what run 158 pins.
		pairDesired := desiredIssue(cfg, findings[1])
		pairStale := pairDesired
		// A stale title on both copies keeps them non-terminal and unequal to
		// the desired issue, so the canonical copy could update; the sync
		// cancels the higher-identifier copy instead.
		pairStale.Title = "stale title"
		canonical := storedIssue("existing-3", "SNYK-3", "Blog", pairStale)
		duplicate := storedIssue("existing-4", "SNYK-4", "Blog", pairStale)

		linear := &fakeLinear{snapshot: append(updates, resolved, orphan, canonical, duplicate), batchErrAbove: 1}
		return snyk, linear
	}

	snyk, linear := build()
	service := New(cfg, logger, snyk, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 2 {
		t.Fatalf("PlannedUpdates = %d, want 2", result.PlannedUpdates)
	}
	if result.PlannedResolves != 2 {
		t.Fatalf("PlannedResolves = %d, want 2 (states %v)", result.PlannedResolves, desiredStates(linear.updated))
	}
	if result.Conflicts != 1 || result.CancelledDuplicates != 1 {
		t.Fatalf("Conflicts = %d, CancelledDuplicates = %d, want 1 and 1 (identifiers %v)", result.Conflicts, result.CancelledDuplicates, updatedIdentifiers(linear.updates))
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0 (per-item fallback must succeed for every kind)", result.FailedOps)
	}
	if len(linear.updated) != 5 {
		t.Fatalf("updated = %d, want 5 (2 updates + 2 resolves + 1 duplicate cancel), states %v", len(linear.updated), desiredStates(linear.updated))
	}

	states := make([]model.IssueState, 0, len(linear.updates))
	for _, update := range linear.updates {
		states = append(states, update.Desired.State)
	}
	// The whole run lands in one order: the two updates (both keeping their
	// non-terminal state), then the two resolves (done, because the project is
	// still active), then the single duplicate cancel (cancelled).
	wantStates := []model.IssueState{
		model.StateTodo, model.StateTodo,
		model.StateDone, model.StateDone,
		model.StateCancelled,
	}
	if !slices.Equal(states, wantStates) {
		t.Fatalf("states in order = %v, want %v (identifiers %v)", states, wantStates, updatedIdentifiers(linear.updates))
	}
	// The two updates are state preserves: the state written is the ticket's
	// own, not one the run computed.
	preserved := 0
	for _, update := range linear.updates {
		if update.Desired.State == model.StateTodo && !update.Desired.PreserveState {
			t.Fatalf("update for %s must preserve state, got PreserveState = false", update.Existing.Identifier)
		}
		if update.Desired.PreserveState {
			preserved++
		}
	}
	if preserved != 2 {
		t.Fatalf("PreserveState updates = %d, want 2 (identifiers %v)", preserved, updatedIdentifiers(linear.updates))
	}

	// Prove the fallback path actually ran, rather than only that the plan was
	// right: every batch larger than one fails against this fake, so a run that
	// lands five writes with FailedOps == 0 can only have done it through
	// single-item calls. A multi-item call that reached Linear would have been
	// counted as a failure, so this pins the shape of the traffic too.
	// Prove the per-item fallback actually ran, not merely that the plan was
	// right. On this fixture a batch larger than one always fails, so a run
	// that lands exactly five writes with FailedOps == 0 can only have landed
	// them through single-item calls. The recorded traffic is the update batch
	// (2) failing, then its two single-item retries, the same for the resolve
	// batch, and finally the duplicate cancel — a single-item call, the only
	// size that kind produces here, which is why the recorded sizes are
	// [2 1 1 2 1 1 1]. The assertion below pins the successful calls, so a
	// regression that batched the writes (or that skipped the fallback and left
	// them unwritten) fails here; the multi-item calls are exactly the two the
	// fake is built to reject.
	wantCallSizes := []int{2, 1, 1, 2, 1, 1, 1}
	if got := updateCallSizes(linear.updateCallBatches); !slices.Equal(got, wantCallSizes) {
		t.Fatalf("UpdateIssues call sizes = %v, want %v (identifiers %v)", got, wantCallSizes, updatedIdentifiers(linear.updates))
	}
	singleItem := 0
	for i, batch := range linear.updateCallBatches {
		if len(batch) == 1 {
			singleItem++
			continue
		}
		// Only the two batch attempts, which the fake rejects, may be
		// multi-item; anything else multi-item would have failed.
		if i != 0 && i != 3 {
			t.Fatalf("UpdateIssues call %d carried %d updates, want 1", i, len(batch))
		}
	}
	if singleItem != 5 {
		t.Fatalf("single-item UpdateIssues calls = %d, want 5 (every write the run landed)", singleItem)
	}
}

// cancellableFinding returns a distinct open finding of project-a, one per i.
func cancellableFinding(i int) model.Finding {
	return model.Finding{
		Fingerprint: fmt.Sprintf("snyk:project-a:issue-%d", i),
		SnykIssueID: fmt.Sprintf("issue-%d", i),
		ProjectID:   "project-a",
		ProjectName: "Project A",
		IssueTitle:  fmt.Sprintf("Outdated package %d", i),
		PackageName: "github.com/example/pkg",
		Severity:    "high",
		Status:      model.FindingOpen,
		IssueURL:    fmt.Sprintf("https://example.test/issue-%d", i),
		CreatedAt:   time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
	}
}

// TestRunStopsCreatingWhenContextCancelled: a signal arrives while the first
// create batch is in flight, so the sync stops there. The plan has 25 creates
// (batches of 10, 10, 5); the cancelled batch call and every later call fail
// because the context is done, and the only calls that may follow are the
// cancelled batch's per-item retries — never a second full batch. The run
// returns the context error and saves no cache: the 15 findings whose jobs
// never ran must be re-examined by the next run instead of looking reconciled.
func TestRunStopsCreatingWhenContextCancelled(t *testing.T) {
	cfg := minimalCfg() // Sync.Workers: 1
	findings := make([]model.Finding, 0, 25)
	for i := range 25 {
		findings = append(findings, cancellableFinding(i))
	}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   findings,
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	linear := &fakeLinear{cancelCreate: cancel}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snyk, linear, store).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want the context error of an interrupted run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(linear.createCallBatches) > 2 {
		t.Fatalf("CreateIssues calls = %d, want at most 2 (the cancelled batch plus its per-item retries), sizes %v",
			len(linear.createCallBatches), createCallSizes(linear.createCallBatches))
	}
	for i, batch := range linear.createCallBatches {
		if len(batch) > 1 && len(batch) != createBatchSize {
			t.Fatalf("CreateIssues call %d carried %d items, want the planned batch %d or a single-item retry",
				i, len(batch), createBatchSize)
		}
	}
	// The first call is the plan's first full batch and it was cancelled; a
	// later call of the same size would mean the producer queued the second
	// batch after the context was already done.
	for i := 1; i < len(linear.createCallBatches); i++ {
		if len(linear.createCallBatches[i]) == createBatchSize {
			t.Fatalf("CreateIssues call %d carried a full batch of %d after cancellation, want single-item retries only (sizes %v)",
				i, createBatchSize, createCallSizes(linear.createCallBatches))
		}
	}
	if result.PlannedCreates >= 25 {
		t.Fatalf("PlannedCreates = %d, want fewer than the whole plan of 25", result.PlannedCreates)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0 (every create call was cancelled)", len(linear.created))
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (an interrupted run must not refresh the cache)", store.saved)
	}
}

// TestRunStopsResolvingWhenContextCancelled runs the same interruption over the
// resolve plan: no findings, 25 open tickets of a still-active project, all of
// which the sync means to close. The first resolve batch is cancelled, so at
// most two batches are planned, the run returns the context error and the cache
// stays unsaved.
func TestRunStopsResolvingWhenContextCancelled(t *testing.T) {
	cfg := minimalCfg() // Sync.Workers: 1
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}
	issues := make([]model.ExistingIssue, 0, 25)
	for i := range 25 {
		issues = append(issues, model.ExistingIssue{
			ID:          fmt.Sprintf("existing-%d", i),
			Identifier:  fmt.Sprintf("SNYK-%d", i),
			Title:       fmt.Sprintf("stale %d", i),
			Description: "old description",
			StateName:   "Todo",
			Fingerprint: fmt.Sprintf("snyk:project-a:gone-%d", i),
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	linear := &fakeLinear{snapshot: issues, cancelUpdate: cancel}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snyk, linear, store).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want the context error of an interrupted run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	// Two batches of ten at most: the cancelled one plus one the producer may
	// already have handed over before it noticed the context was done.
	if result.PlannedResolves > 20 {
		t.Fatalf("PlannedResolves = %d, want at most 20 (the producer must stop queueing once the context is done)", result.PlannedResolves)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (every resolve call was cancelled)", len(linear.updated))
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (an interrupted run must not refresh the cache)", store.saved)
	}
}

// TestExecuteJobReturnsImmediatelyWhenContextAlreadyCancelled: a job reached
// executeJob with the run already cancelled. It must report the context error
// without counting the work as planned and without calling Linear — counting it
// would report a batch of failed writes that was never attempted.
func TestExecuteJobReturnsImmediatelyWhenContextAlreadyCancelled(t *testing.T) {
	cfg := minimalCfg()
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SNYK-1",
		Title:       "stale title",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: "snyk:project-a:issue-1",
	}
	desired := desiredIssue(cfg, cancellableFinding(1))
	desired.PreserveState = true
	linear := &fakeLinear{}
	service := New(cfg, discardLogger(), fakeSnyk{}, linear, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var result RunResult
	var failed failedFingerprints
	err := service.executeJob(ctx, job{kind: jobUpdate, updateBatch: []model.IssueUpdate{{Existing: existing, Desired: desired}}}, &result, &failed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeJob() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if result.PlannedUpdates != 0 || result.FailedOps != 0 {
		t.Fatalf("PlannedUpdates = %d, FailedOps = %d, want 0 and 0 (no counter may be touched)", result.PlannedUpdates, result.FailedOps)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0 (Linear must not be called)", len(linear.updated))
	}
}

// TestRunCompletesAndSavesCacheWhenNotCancelled is the control for the two
// interruption tests above: the very same inputs with a context that is never
// cancelled must run to the end, create every finding and refresh the cache.
func TestRunCompletesAndSavesCacheWhenNotCancelled(t *testing.T) {
	cfg := minimalCfg() // Sync.Workers: 1
	findings := make([]model.Finding, 0, 25)
	for i := range 25 {
		findings = append(findings, cancellableFinding(i))
	}
	snyk := fakeSnyk{snapshot: model.SnykSnapshot{
		Findings:   findings,
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}}
	linear := &fakeLinear{}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snyk, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 25 {
		t.Fatalf("created = %d, want 25", len(linear.created))
	}
	if result.PlannedCreates != 25 {
		t.Fatalf("PlannedCreates = %d, want 25", result.PlannedCreates)
	}
	if store.saved.SchemaSignature == "" {
		t.Fatalf("saved cache = %+v, want a refreshed snapshot with a schema signature", store.saved)
	}
}

// createCallSizes reports the item count of every recorded CreateIssues call.
func createCallSizes(batches [][]model.DesiredIssue) []int {
	sizes := make([]int, 0, len(batches))
	for _, batch := range batches {
		sizes = append(sizes, len(batch))
	}
	return sizes
}

// TestIssueTitleCapsUnboundedFields verifies that a finding whose API fields
// (repository, package name) are long enough to exceed Linear's title limit
// still renders a title within that limit, with a truncation marker, instead
// of producing an issue-create that fails on every run.
func TestIssueTitleCapsUnboundedFields(t *testing.T) {
	finding := model.Finding{
		IssueTitle:  "Use of Default Credentials",
		Severity:    "critical",
		Fingerprint: "snyk:project-a:issue-1",
		Repository:  "owner/" + strings.Repeat("repo-", 60),
		PackageName: "zlib",
	}

	title := issueTitle(finding)

	if got := utf8.RuneCountInString(title); got > maxIssueTitleRunes {
		t.Fatalf("title length = %d runes, want <= %d", got, maxIssueTitleRunes)
	}
	if !strings.HasSuffix(title, issueTitleTruncationMarker) {
		t.Fatalf("title = %q, want truncation marker suffix", title)
	}
	if !strings.HasPrefix(title, "Snyk: [critical] owner/repo-") {
		t.Fatalf("title = %q, want intact prefix", title)
	}
	if issueTitle(finding) != title {
		t.Fatalf("issueTitle is not deterministic for the same finding")
	}
}

// TestRunLogsCacheAgeAndIgnoreEvictions pins the cache observability the run
// reports: the age of the hash snapshot it loaded, and how many cached v1
// ignore rows were discarded as stale or unparseable.
func TestRunLogsCacheAgeAndIgnoreEvictions(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				CriticalDays: 15,
				HighDays:     30,
				MediumDays:   45,
				LowDays:      90,
			},
		},
		Sync: config.SyncConfig{Workers: 1},
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	snyk := fakeSnyk{snapshot: model.SnykSnapshot{}}
	linear := &fakeLinear{}
	cacheStore := &fakeCache{
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SavedAt:         time.Now().UTC().Add(-2 * time.Hour),
		},
		evictions: cache.IgnoreEvictionCounts{
			TTLExpired:  3,
			Unparseable: 1,
		},
	}

	service := New(cfg, logger, snyk, linear, cacheStore)
	if _, err := service.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	out := logs.String()
	for _, want := range []string{"cache_age=", "ignore_evicted_ttl=3", "ignore_evicted_unparseable=1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("logs missing %q:\n%s", want, out)
		}
	}
}
