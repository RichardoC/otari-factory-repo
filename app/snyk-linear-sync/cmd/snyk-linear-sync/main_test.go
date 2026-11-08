package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

// errLinearWrite is the error the failing Linear client reports for every
// state-changing write: the batch call and each per-item retry alike. It
// models a Linear host that persistently rejects writes.
var errLinearWrite = errors.New("linear rejected the write")

// testFinding is one open Snyk finding in a project the snapshot lists, so a
// run against an empty Linear workspace has state-changing work to plan:
// exactly one ticket create.
func testFinding() model.Finding {
	return model.Finding{
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
	}
}

// testConfig is the minimal configuration the sync service needs: one worker
// and the due-date windows, mirroring the internal/sync tests' fixtures.
func testConfig() config.Config {
	return config.Config{
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
}

// testSnyk serves the one-finding snapshot.
type testSnyk struct{}

func (testSnyk) LoadSnapshot(context.Context) (model.SnykSnapshot, error) {
	return model.SnykSnapshot{
		Findings:   []model.Finding{testFinding()},
		ProjectIDs: map[string]struct{}{"project-a": {}},
	}, nil
}

// recordingCache is an empty in-memory CacheStore: every run starts from no
// cached state and saves whatever snapshot the run produced.
type recordingCache struct {
	saved int
}

func (c *recordingCache) Load(context.Context) (cache.Snapshot, error) {
	return cache.Snapshot{}, nil
}

func (c *recordingCache) Save(context.Context, cache.Snapshot) error {
	c.saved++
	return nil
}

func (c *recordingCache) IgnoreEvictions() cache.IgnoreEvictionCounts {
	return cache.IgnoreEvictionCounts{}
}

// failingLinear reads fine and fails every state-changing write — the batch
// call and every per-item retry — so the run cannot apply its plan. It counts
// the CreateIssues calls so the test can prove the per-item fallback ran and
// still failed, which is what makes failed_ops permanent.
type failingLinear struct {
	createCalls int
}

func (f *failingLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (f *failingLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (f *failingLinear) CreateIssues(context.Context, []model.DesiredIssue) ([]int, error) {
	f.createCalls++
	return nil, errLinearWrite
}

func (f *failingLinear) UpdateIssues(context.Context, []model.IssueUpdate) error {
	return errLinearWrite
}

func (f *failingLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, errLinearWrite
}

func (f *failingLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// healthyLinear reads fine, records every write, and succeeds at each. It
// exists so the nil-return arm is not vacuous: a nil from runSync that never
// attempted a write would prove nothing about a run that applied its plan.
type healthyLinear struct {
	created []model.DesiredIssue
	updated []model.IssueUpdate
}

func (h *healthyLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (h *healthyLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (h *healthyLinear) CreateIssues(_ context.Context, desired []model.DesiredIssue) ([]int, error) {
	h.created = append(h.created, desired...)
	return nil, nil
}

func (h *healthyLinear) UpdateIssues(_ context.Context, updates []model.IssueUpdate) error {
	h.updated = append(h.updated, updates...)
	return nil
}

func (h *healthyLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, nil
}

func (h *healthyLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// TestRunSyncFailsWhenLinearWritesRejectEveryRetry drives runSync with a
// Linear client whose writes always fail and asserts the run reports an error
// naming failed_ops, after the sync complete summary has been logged.
func TestRunSyncFailsWhenLinearWritesRejectEveryRetry(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &failingLinear{}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err == nil {
		t.Fatalf("runSync() = nil, want an error: every Linear write failed, so a nil would report a run that quietly dropped its work")
	}
	if !strings.Contains(err.Error(), "failed_ops=1") {
		t.Fatalf("runSync() error = %q, want it to name failed_ops=1", err)
	}
	if linear.createCalls < 2 {
		t.Fatalf("CreateIssues called %d time(s), want at least 2: the batch call and a per-item retry; a single call would mean failed_ops counted a write the sync never retried", linear.createCalls)
	}
	// The summary must reach the log before the exit status is decided, so the
	// counts are visible on a failing run too.
	if !strings.Contains(logs.String(), "sync complete") || !strings.Contains(logs.String(), "failed_ops=1") {
		t.Fatalf("summary log does not report the completed sync with failed_ops=1:\n%s", logs.String())
	}
}

// TestRunSyncReturnsNilWithoutFailures is the control twin: the same run
// against a Linear client whose writes succeed returns nil. It asserts a write
// was genuinely attempted and applied, so the nil is the outcome of a run that
// applied its plan, not of one that never tried.
func TestRunSyncReturnsNilWithoutFailures(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &healthyLinear{}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err != nil {
		t.Fatalf("runSync() error = %v, want nil for a run whose writes all applied", err)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1: the run must have attempted and applied a write for the nil return to mean anything", len(linear.created))
	}
	if !strings.Contains(logs.String(), "failed_ops=0") {
		t.Fatalf("summary log does not report failed_ops=0:\n%s", logs.String())
	}
}

// ambiguousLinear models a Linear host whose batch create fails after the
// mutation may have committed: the error alone cannot tell the sync what
// landed, so it reconciles against ExistingFingerprints. Both outcomes of
// that lookup are tested below: when the lookup names the ticket as landed
// the run is clean, and when the lookup itself fails the whole batch is
// counted in failed_ops without a per-item retry.
type ambiguousLinear struct {
	createCalls     int
	reconcileLanded map[string]struct{}
	reconcileErr    error
}

func (a *ambiguousLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (a *ambiguousLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return a.reconcileLanded, a.reconcileErr
}

func (a *ambiguousLinear) CreateIssues(context.Context, []model.DesiredIssue) ([]int, error) {
	a.createCalls++
	return nil, errLinearWrite
}

func (a *ambiguousLinear) UpdateIssues(context.Context, []model.IssueUpdate) error {
	return nil
}

func (a *ambiguousLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, nil
}

func (a *ambiguousLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// TestRunSyncSucceedsWhenFailedBatchCreateLanded is the exit-0 twin of the
// always-failing test: the batch create fails, but the reconciliation lookup
// shows the ticket landed anyway, so nothing is retried, failed_ops stays 0
// and the run reports success — the operation applied, it just took two calls.
func TestRunSyncSucceedsWhenFailedBatchCreateLanded(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &ambiguousLinear{
		reconcileLanded: map[string]struct{}{testFinding().Fingerprint: {}},
	}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err != nil {
		t.Fatalf("runSync() error = %v, want nil: the ticket landed despite the batch error, so there is no unapplied work", err)
	}
	if linear.createCalls != 1 {
		t.Fatalf("CreateIssues called %d time(s), want 1: a batch create that reconciles as landed must not be retried per item", linear.createCalls)
	}
	if !strings.Contains(logs.String(), "failed_ops=0") {
		t.Fatalf("summary log does not report failed_ops=0:\n%s", logs.String())
	}
}

// TestRunSyncCountsWholeBatchWhenReconciliationFails covers the arm the
// always-failing test cannot reach: the batch create fails and the
// reconciliation lookup fails too, so there is no safe way to tell what
// landed. The whole batch is counted in failed_ops and the run fails, without
// any per-item retry.
func TestRunSyncCountsWholeBatchWhenReconciliationFails(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &ambiguousLinear{reconcileErr: errors.New("reconciliation lookup failed")}

	err := runSync(context.Background(), testConfig(), logger, testSnyk{}, linear, &recordingCache{})
	if err == nil {
		t.Fatalf("runSync() = nil, want an error: an irreconcilable batch create leaves unapplied work the next run must pick up")
	}
	if !strings.Contains(err.Error(), "failed_ops=1") {
		t.Fatalf("runSync() error = %q, want it to name failed_ops=1 for the whole batch", err)
	}
	if linear.createCalls != 1 {
		t.Fatalf("CreateIssues called %d time(s), want 1: a batch create whose reconciliation fails must be counted without a per-item retry", linear.createCalls)
	}
}

// blockingLinear parks inside CreateIssues until the context is cancelled, so
// the test can interrupt a run whose write is in flight.
type blockingLinear struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return nil, nil
}

func (b *blockingLinear) ExistingFingerprints(context.Context, []string) (map[string]struct{}, error) {
	return map[string]struct{}{}, nil
}

func (b *blockingLinear) CreateIssues(ctx context.Context, _ []model.DesiredIssue) ([]int, error) {
	b.once.Do(func() { close(b.entered) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockingLinear) UpdateIssues(context.Context, []model.IssueUpdate) error {
	return nil
}

func (b *blockingLinear) PostComments(context.Context, []model.IssueUpdate) ([]int, error) {
	return nil, nil
}

func (b *blockingLinear) LastOpenStates(context.Context, []string) (map[string]model.WorkflowState, error) {
	return map[string]model.WorkflowState{}, nil
}

// TestRunSyncReportsInterruptedRun drives runSync with a Linear client whose
// write is in flight when the context is cancelled, and asserts the interrupt
// becomes the run's error — Run wraps the context error rather than reporting
// success — and that the cache is not refreshed by a partly written plan.
func TestRunSyncReportsInterruptedRun(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	linear := &blockingLinear{entered: make(chan struct{})}
	cacheStore := &recordingCache{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- runSync(ctx, testConfig(), logger, testSnyk{}, linear, cacheStore) }()

	select {
	case <-linear.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CreateIssues was never entered; the run finished before the interrupt could land mid-write")
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("runSync() = nil after the context was cancelled mid-write, want an interrupt error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runSync() error = %v, want it to wrap context.Canceled", err)
		}
		if !strings.Contains(err.Error(), "interrupted") {
			t.Fatalf("runSync() error = %q, want it to say the run was interrupted before all work was written", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runSync did not return after the context was cancelled")
	}
	if cacheStore.saved != 0 {
		t.Fatalf("cache saved %d time(s), want 0: an interrupted run must not refresh the cache over a partly written plan", cacheStore.saved)
	}
}
