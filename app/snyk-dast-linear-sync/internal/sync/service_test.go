package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

type fakeSnykDAST struct {
	snapshot model.SnykDASTSnapshot
}

func (f fakeSnykDAST) LoadSnapshot(context.Context) (model.SnykDASTSnapshot, error) {
	return f.snapshot, nil
}

type fakeLinear struct {
	snapshot []model.ExistingIssue
	created  []model.DesiredIssue
	updated  []model.DesiredIssue
	updates  []model.IssueUpdate
	comments []model.IssueUpdate

	// createFailIdx/commentFailIdx simulate a partial batch failure: the first
	// batch call reports these indices as failed, subsequent calls succeed.
	createFailIdx  []int
	commentFailIdx []int
	createCalls    []int
	// createAlwaysFailIdx makes every create call report a per-entry failure,
	// including single-entry retries.
	createAlwaysFailIdx bool

	// createCallBatches records the batch of every CreateIssues call, so tests
	// can assert how much work was started before a cancellation landed.
	createCallBatches [][]model.DesiredIssue
	// cancelCreate, when set, cancels the run's context on the first
	// CreateIssues call, modelling a run interrupted mid-write.
	cancelCreate context.CancelFunc
	// mu guards the fields CreateIssues records, which workers may write
	// concurrently when the pool has more than one worker.
	mu sync.Mutex

	// createBatchErr makes the first (batch) create call fail with an error,
	// simulating an ambiguous transport failure. landedFingerprints is what
	// ExistingFingerprints then reports as already present in Linear, and
	// existingErr makes that reconciliation query itself fail.
	createBatchErr     error
	landedFingerprints []string
	existingErr        error
	existingCalls      int

	// commentBatchErr makes the batch PostComments call fail with an error,
	// simulating an ambiguous outcome. commentCalls records the size of every
	// PostComments call so a test can assert no retry happened.
	commentBatchErr error
	commentCalls    []int

	// updateErr, when non-nil, makes every UpdateIssues call fail — the batch
	// call and each per-entry retry — while the attempted writes are still
	// recorded so a test can see what was attempted. It models a
	// transport-level failure where no per-alias data comes back at all.
	updateErr error
	// updateFailFingerprints makes every UpdateIssues call that carries one of
	// these fingerprints fail, batch or single-entry retry alike, while calls
	// with only other fingerprints succeed. It models a per-ticket Linear
	// failure that survives the retry.
	updateFailFingerprints map[string]struct{}
}

type fakeCache struct {
	snapshot cache.Snapshot
	saved    cache.Snapshot
}

func (f *fakeLinear) LoadSnapshot(context.Context) ([]model.ExistingIssue, error) {
	return f.snapshot, nil
}

func (f *fakeLinear) ExistingFingerprints(_ context.Context, fingerprints []string) (map[string]struct{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recordExistingFingerprintsLocked(fingerprints)
}

// recordExistingFingerprintsLocked is the body of ExistingFingerprints; it
// assumes f.mu is held so it can be called from a method that already locked.
func (f *fakeLinear) recordExistingFingerprintsLocked(fingerprints []string) (map[string]struct{}, error) {
	f.existingCalls++
	if f.existingErr != nil {
		return nil, f.existingErr
	}
	wanted := map[string]struct{}{}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.createIssuesLocked(ctx, desired)
}

// createIssuesLocked is the body of CreateIssues; it assumes f.mu is held.
func (f *fakeLinear) createIssuesLocked(_ context.Context, desired []model.DesiredIssue) ([]int, error) {
	f.createCallBatches = append(f.createCallBatches, append([]model.DesiredIssue(nil), desired...))
	if f.cancelCreate != nil {
		// Simulate a run interrupted while this batch was in flight.
		cancel := f.cancelCreate
		f.cancelCreate = nil
		cancel()
	}
	if f.createBatchErr != nil && len(desired) > 1 {
		// Only the multi-entry batch call fails; single-entry retries succeed.
		err := f.createBatchErr
		f.createBatchErr = nil
		return nil, err
	}
	f.created = append(f.created, desired...)
	if f.createAlwaysFailIdx {
		// Every call, retries included, reports a per-entry failure with a nil
		// error — the case that used to be silently dropped.
		failed := make([]int, len(desired))
		for i := range desired {
			failed[i] = i
		}
		return failed, nil
	}
	if f.createFailIdx != nil {
		f.createCalls = append(f.createCalls, len(desired))
		failed := f.createFailIdx
		// Only the first batch call reports failures, so a retry can succeed.
		f.createFailIdx = nil
		return failed, nil
	}
	return nil, nil
}

func (f *fakeLinear) UpdateIssues(_ context.Context, updates []model.IssueUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updateIssuesLocked(updates)
}

// updateIssuesLocked is the body of UpdateIssues; it assumes f.mu is held. A
// single-lock public entry point keeps the retry loops from re-entering it
// (f.mu is a plain sync.Mutex and is not reentrant).
func (f *fakeLinear) updateIssuesLocked(updates []model.IssueUpdate) error {
	for _, update := range updates {
		if _, ok := f.updateFailFingerprints[update.Desired.Fingerprint]; ok {
			return fmt.Errorf("update failed for %s", update.Desired.Fingerprint)
		}
	}
	if f.updateErr != nil {
		for _, update := range updates {
			f.updated = append(f.updated, update.Desired)
			f.updates = append(f.updates, update)
		}
		return f.updateErr
	}
	for _, update := range updates {
		f.updated = append(f.updated, update.Desired)
		f.updates = append(f.updates, update)
	}
	return nil
}

func (f *fakeLinear) PostComments(_ context.Context, updates []model.IssueUpdate) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCommentsLocked(updates)
}

// postCommentsLocked is the body of PostComments; it assumes f.mu is held.
func (f *fakeLinear) postCommentsLocked(updates []model.IssueUpdate) ([]int, error) {
	f.commentCalls = append(f.commentCalls, len(updates))
	if f.commentBatchErr != nil {
		err := f.commentBatchErr
		f.commentBatchErr = nil
		return nil, err
	}
	f.comments = append(f.comments, updates...)
	if f.commentFailIdx != nil {
		failed := f.commentFailIdx
		f.commentFailIdx = nil
		return failed, nil
	}
	return nil, nil
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
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					TargetHost:        "example.com",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
					CreatedAt:         time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
				},
				{
					Fingerprint:       "snyk-dast:target-b:finding-2",
					SnykDASTFindingID: "su2d3k-2",
					TargetID:          "target-b",
					TargetName:        "Other App",
					IssueTitle:        "SQL Injection",
					Severity:          "low",
					Status:            model.FindingIgnored,
					IssueURL:          "https://plus.probely.app/targets/target-b/findings/2",
					CreatedAt:         time.Date(2026, time.August, 1, 9, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{
				"target-a": {},
				"target-b": {},
				"target-z": {},
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
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "old resolved issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-z:finding-9",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)

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
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					TargetHost:        "example.com",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
					IssueAPIURL:       "https://api.probely.com/findings/1/",
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0])
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
			SnykDASTHashes: map[string]string{
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

	service := New(cfg, logger, snykdast, linear, cacheStore)

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
// ignored in Snyk DAST (desired state Cancelled) is moved to Cancelled even
// when its ticket was manually parked in "Todo" and the cache claims nothing
// changed.
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

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Information Disclosure",
					Severity:          "low",
					Status:            model.FindingIgnored,
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0])
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
			SnykDASTHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snykdast, linear, cacheStore)
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

// TestRunCacheStillSkipsNonTerminalStateDivergence locks in the narrow scope
// of the terminal-transition cache guard: an open finding whose ticket
// diverges only in a non-terminal state must still be cache-suppressed when
// its hashes are unchanged.
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

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen, // desired Todo — non-terminal
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0]) // State == Todo
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
			SnykDASTHashes: map[string]string{
				desired.Fingerprint: desiredIssueHash(desired),
			},
			LinearHashes: map[string]string{
				desired.Fingerprint: existingIssueHash(existing),
			},
		},
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{existing}}

	service := New(cfg, logger, snykdast, linear, cacheStore)
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

func TestRunCancelsMissingIssueWhenTargetDeleted(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "missing target issue",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-z:finding-9",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)

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

func TestRunCancelsMissingIssueWhenTargetDeletedEvenIfCached(t *testing.T) {
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
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{
				"target-a": {},
			},
		},
	}
	existing := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "missing target issue",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: "snyk-dast:target-z:finding-9",
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

	service := New(cfg, logger, snykdast, linear, cacheStore)

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

	if needsUpdate(existing, desired) {
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

func TestDesiredIssueDueDateUsesCreatedAt(t *testing.T) {
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
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "critical",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.August, 11, 23, 30, 0, 0, time.FixedZone("minus0500", -5*60*60)),
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
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	if desired.DueDate != "2026-01-31" {
		t.Fatalf("desired due date = %q, want %q (raw SLA date for past issues)", desired.DueDate, "2026-01-31")
	}
}

func TestDesiredIssueDueDateUsesAcceptanceExpiryWhenSnoozed(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Due: config.DueDateConfig{
				HighDays: 30,
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
		IgnoreExpiresAt:   time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	// IgnoreExpiresAt wins over CreatedAt: June 1 + 30 days = July 1.
	if desired.DueDate != "2026-07-01" {
		t.Fatalf("desired due date = %q, want %q (acceptance expiry + high offset)", desired.DueDate, "2026-07-01")
	}
}

func TestDesiredIssueRendersDASTContext(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:           "snyk-dast-automation",
				TargetType:        map[string]string{"api": "snyk-dast-api"},
				TargetTypeDefault: "snyk-dast-web",
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example API",
		TargetType:        "api",
		TargetURL:         "https://api.example.com",
		TargetHost:        "api.example.com",
		IssueTitle:        "Broken Access Control",
		Severity:          "high",
		Status:            model.FindingOpen,
		IssueURL:          "https://plus.probely.app/targets/target-a/findings/1",
		IssueAPIURL:       "https://api.probely.com/findings/1/",
		FindingURL:        "https://api.example.com/users/42",
		Method:            "get",
		Path:              "users/42",
		Parameter:         "id",
		InsertionPoint:    "parameter",
		CWE:               "CWE-284",
		CWEName:           "Improper Access Control",
		CVSS:              7.5,
	}

	desired := desiredIssue(cfg, finding)

	if desired.Title != "Snyk DAST: [high] api.example.com: Broken Access Control at GET users/42" {
		t.Fatalf("title = %q", desired.Title)
	}
	if !strings.Contains(desired.Description, "## Broken Access Control [HIGH]") {
		t.Fatalf("description missing heading: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "URL: [GET users/42](https://api.example.com/users/42)") {
		t.Fatalf("description missing vulnerable URL line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Method: `GET`") {
		t.Fatalf("description missing method line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Path: `users/42`") {
		t.Fatalf("description missing path line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Parameter: `id` (Parameter)") {
		t.Fatalf("description missing parameter line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "CWE: `CWE-284` (Improper Access Control)") {
		t.Fatalf("description missing CWE line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "CVSS: `7.5`") {
		t.Fatalf("description missing CVSS line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Snyk DAST: [Open finding](https://plus.probely.app/targets/target-a/findings/1)") {
		t.Fatalf("description missing Snyk DAST link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "API: [Finding details](https://api.probely.com/findings/1/)") {
		t.Fatalf("description missing API link: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Status: `open`") {
		t.Fatalf("description missing status line: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "managed_labels: snyk-dast-api,snyk-dast-automation") {
		t.Fatalf("description missing managed labels metadata: %s", desired.Description)
	}
	// NormalizeManagedLabelNames sorts labels alphabetically, so snyk-dast-api
	// precedes snyk-dast-automation regardless of insertion order.
	if len(desired.ManagedLabels) != 2 || desired.ManagedLabels[0] != "snyk-dast-api" || desired.ManagedLabels[1] != "snyk-dast-automation" {
		t.Fatalf("ManagedLabels = %#v, want [snyk-dast-api snyk-dast-automation]", desired.ManagedLabels)
	}
}

func TestDesiredIssueRendersSnykCodeCorrelation(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed: "snyk-dast-automation",
			},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example API",
		TargetHost:        "api.example.com",
		IssueTitle:        "SQL Injection",
		Severity:          "high",
		Status:            model.FindingOpen,
		Method:            "get",
		Path:              "users/42",
		CorrelationMarkdown: []string{
			"**Snyk Code:** [SQL Injection in `src/db/query.go:42`](https://app.snyk.io/org/example/project/abc#issue-SNYK-GO-1234)",
			"Repository: `owner/repo` — confirmed correlation",
		},
	}

	desired := desiredIssue(cfg, finding)

	if !strings.Contains(desired.Description, "### Snyk Code correlation") {
		t.Fatalf("description missing Snyk Code correlation heading: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Linked Snyk Code source vulnerability responsible for this runtime finding.") {
		t.Fatalf("description missing correlation subtitle: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "SQL Injection in `src/db/query.go:42`") {
		t.Fatalf("description missing correlation markdown block: %s", desired.Description)
	}
	if !strings.Contains(desired.Description, "Repository: `owner/repo`") {
		t.Fatalf("description missing second correlation block: %s", desired.Description)
	}
}

func TestDesiredIssueOmitsCorrelationSectionWhenEmpty(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{Managed: "snyk-dast-automation"},
		},
	}
	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "medium",
		Status:            model.FindingOpen,
	}

	desired := desiredIssue(cfg, finding)

	if strings.Contains(desired.Description, "Snyk Code correlation") {
		t.Fatalf("description should not contain correlation section when empty: %s", desired.Description)
	}
}

func TestDesiredIssueUsesTargetTypeDefaultLabel(t *testing.T) {
	cfg := config.Config{
		Linear: config.LinearConfig{
			Labels: config.LabelConfig{
				Managed:           "snyk-dast-automation",
				TargetTypeDefault: "snyk-dast-web",
			},
		},
	}
	finding := model.Finding{
		Fingerprint: "snyk-dast:target-a:finding-1",
		TargetType:  "single",
		IssueTitle:  "XSS",
		Severity:    "medium",
		Status:      model.FindingOpen,
	}

	desired := desiredIssue(cfg, finding)

	found := false
	for _, label := range desired.ManagedLabels {
		if label == "snyk-dast-web" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ManagedLabels = %#v, want snyk-dast-web fallback", desired.ManagedLabels)
	}
}

func TestIssueTitleUsesTargetNameWhenNoHost(t *testing.T) {
	finding := model.Finding{
		IssueTitle: "TLS Misconfiguration",
		Severity:   "medium",
		TargetName: "Marketing Site",
		Path:       "login",
		Method:     "post",
	}

	title := issueTitle(finding)

	if title != "Snyk DAST: [medium] Marketing Site: TLS Misconfiguration at POST login" {
		t.Fatalf("title = %q", title)
	}
}

func TestUpsertManagedMetadataRemovesVisibleFingerprintFooter(t *testing.T) {
	description := "Status: `open`\n\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->\nFingerprint: snyk-dast:target-a:finding-1"

	got := upsertManagedMetadata(description, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation", "snyk-dast-api"})

	if strings.Contains(got, "Fingerprint: snyk-dast:target-a:finding-1") {
		t.Fatalf("upsertManagedMetadata() left visible fingerprint footer: %s", got)
	}
	if !strings.Contains(got, "managed_labels: snyk-dast-api,snyk-dast-automation") {
		t.Fatalf("upsertManagedMetadata() missing managed labels metadata: %s", got)
	}
}

// TestUpsertManagedMetadataIgnoresInlineMarker verifies that the metadata
// marker appearing mid-sentence in user text does not corrupt the description.
func TestUpsertManagedMetadataIgnoresInlineMarker(t *testing.T) {
	description := "See <!-- snyk-dast-linear-sync notes --> for details\n\nStatus: `open`"

	got := upsertManagedMetadata(description, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation"})

	if !strings.Contains(got, "See <!-- snyk-dast-linear-sync notes -->") {
		t.Fatalf("upsertManagedMetadata() corrupted inline marker: %s", got)
	}
	if !strings.Contains(got, "<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1") {
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
			description: "<!-- snyk-dast-linear-sync\nfingerprint: test\n-->",
			wantIdx:     0,
		},
		{
			name:        "marker at start of line",
			description: "Some text\n<!-- snyk-dast-linear-sync\nfingerprint: test\n-->",
			wantIdx:     10,
		},
		{
			name:        "marker mid-sentence should not match",
			description: "See <!-- snyk-dast-linear-sync notes --> for details",
			wantIdx:     -1,
		},
		{
			name:        "no marker at all",
			description: "Just a normal description",
			wantIdx:     -1,
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

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestManagedLabelsUsesConfiguredCheckTypeMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:   "snyk-dast-automation",
		CheckType: map[string]string{"passive": "snyk-dast-passive", "active": "snyk-dast-active"},
	}, model.Finding{CheckType: "passive"})

	found := false
	for _, l := range labels {
		if l == "snyk-dast-passive" {
			found = true
		}
	}
	if !found {
		t.Fatalf("managedLabels() = %#v, want snyk-dast-passive for passive check", labels)
	}
}

func TestManagedLabelsUsesConfiguredTargetTypeMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed:    "snyk-dast-automation",
		TargetType: map[string]string{"api": "snyk-dast-api"},
	}, model.Finding{TargetType: "api"})

	// Labels are normalized (sorted) alphabetically.
	if len(labels) != 2 || labels[0] != "snyk-dast-api" || labels[1] != "snyk-dast-automation" {
		t.Fatalf("managedLabels() = %#v, want [snyk-dast-api snyk-dast-automation]", labels)
	}
}

func TestManagedLabelsOmitsTargetTypeLabelWhenNoMapping(t *testing.T) {
	labels := managedLabels(config.LabelConfig{
		Managed: "snyk-dast-automation",
	}, model.Finding{TargetType: "single"})

	if len(labels) != 1 || labels[0] != "snyk-dast-automation" {
		t.Fatalf("managedLabels() = %#v, want [snyk-dast-automation]", labels)
	}
}

// TestManagedLabelsWithMappingsEmptyWhenLabelManagementOff guards the case
// where LINEAR_MANAGED_LABEL=off is set but the check-type and target-type
// label mappings, or their defaults, stay configured: a naive managed set
// would still assert the derived labels, fail the run resolving them, and
// never remove them from tickets. Turning management off must turn off the
// whole set — labels and the "why" reasons alike.
func TestManagedLabelsWithMappingsEmptyWhenLabelManagementOff(t *testing.T) {
	tests := []struct {
		name     string
		labelCfg config.LabelConfig
	}{
		{
			name: "mappings set",
			labelCfg: config.LabelConfig{
				CheckType:  map[string]string{"passive": "snyk-dast-passive"},
				TargetType: map[string]string{"api": "snyk-dast-api"},
			},
		},
		{
			name: "defaults set",
			labelCfg: config.LabelConfig{
				CheckTypeDefault:  "snyk-dast-check-default",
				TargetTypeDefault: "snyk-dast-target-default",
			},
		},
		{
			name: "mappings and defaults set",
			labelCfg: config.LabelConfig{
				CheckType:         map[string]string{"passive": "snyk-dast-passive"},
				TargetType:        map[string]string{"api": "snyk-dast-api"},
				CheckTypeDefault:  "snyk-dast-check-default",
				TargetTypeDefault: "snyk-dast-target-default",
			},
		},
	}

	finding := model.Finding{CheckType: "passive", TargetType: "api"}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if labels := managedLabels(tc.labelCfg, finding); len(labels) != 0 {
				t.Fatalf("managedLabels() = %#v, want none when label management is off", labels)
			}
			if reasons := buildLabelReasons(tc.labelCfg, finding); len(reasons) != 0 {
				t.Fatalf("buildLabelReasons() = %#v, want none when label management is off", reasons)
			}
		})
	}
}

func TestNeedsUpdateClearsDueDateWhenDesiredIsEmpty(t *testing.T) {
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
		DueDate:     "",
		State:       model.StateTodo,
		Priority:    2,
	}

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true (must clear stale due date)")
	}
}

func TestNeedsUpdateSkipsDueDateWhenBothEmpty(t *testing.T) {
	existing := model.ExistingIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		StateName:   "Todo",
		Priority:    2,
	}
	desired := model.DesiredIssue{
		Title:       "title",
		Description: "description",
		DueDate:     "",
		State:       model.StateTodo,
		Priority:    2,
	}

	if needsUpdate(existing, desired) {
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

	if !needsUpdate(existing, desired) {
		t.Fatal("needsUpdate() = false, want true")
	}
}

func TestIdentifierNum(t *testing.T) {
	cases := []struct {
		input string
		want  int
	}{
		{"PROB-1", 1},
		{"SEC-42", 42},
		{"SNYK-11596", 11596},
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

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
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

	cancelledIDs := cancelledIdentifiers(linear.updates)
	if len(cancelledIDs) != 1 || cancelledIDs[0] != "SNYK-20" {
		t.Fatalf("cancelled identifiers = %v, want [SNYK-20]", cancelledIDs)
	}
}

func TestRunDuplicateCancellationIsIdempotentWhenAlreadyCancelled(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Cancelled",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old body\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.CancelledDuplicates != 0 {
		t.Fatalf("CancelledDuplicates = %d, want 0 (already cancelled)", result.CancelledDuplicates)
	}
}

func TestRunCancelsDuplicateAndStillSyncsCanonical(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "issue-a",
				Identifier:  "SNYK-20",
				Title:       "stale title",
				Description: "d\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
			{
				ID:          "issue-b",
				Identifier:  "SNYK-10",
				Title:       "stale title",
				Description: "d\n<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\n-->",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}
	if !containsStr(updatedIdentifiers(linear.updates), "SNYK-10") {
		t.Fatalf("SNYK-10 (canonical) was not updated; updates: %v", updatedIdentifiers(linear.updates))
	}
	if !containsStr(cancelledIdentifiers(linear.updates), "SNYK-20") {
		t.Fatalf("SNYK-20 (duplicate) was not cancelled; cancelled: %v", cancelledIdentifiers(linear.updates))
	}
}

// minimalCfg returns the smallest valid Config needed to run the service in tests.
func minimalCfg() config.Config {
	return config.Config{
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

// emptySnykDASTSnapshot is a snapshot whose target and finding lists came
// back empty: the documented symptom of an API key without the view_target
// permission, of a team filter matching no targets, or of an outage.
func emptySnykDASTSnapshot() model.SnykDASTSnapshot {
	return model.SnykDASTSnapshot{}
}

func openManagedIssues() []model.ExistingIssue {
	return []model.ExistingIssue{
		{
			ID:          "existing-1",
			Identifier:  "SEC-1",
			Title:       "finding on project-a",
			Description: "old description",
			StateName:   "Todo",
			Fingerprint: "snyk-dast:project-a:finding-1",
		},
		{
			ID:          "existing-2",
			Identifier:  "SEC-2",
			Title:       "finding on project-b",
			Description: "old description",
			StateName:   "In Progress",
			Fingerprint: "snyk-dast:project-b:finding-2",
		},
	}
}

func cacheSavedIsEmpty(saved cache.Snapshot) bool {
	return saved.SchemaSignature == "" && len(saved.SnykDASTHashes) == 0 && len(saved.LinearHashes) == 0
}

func TestRunRefusesToResolveFromEmptySnapshot(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{snapshot: emptySnykDASTSnapshot()}
	cacheStore := &fakeCache{
		// Pre-load a plausible cache so a stray save would be observable.
		snapshot: cache.Snapshot{
			SchemaSignature: managedSchemaSignature(),
			SnykDASTHashes:  map[string]string{"snyk-dast:project-a:finding-1": "hash"},
			LinearHashes:    map[string]string{"snyk-dast:project-a:finding-1": "hash"},
		},
	}
	linear := &fakeLinear{snapshot: openManagedIssues()}

	service := New(cfg, logger, snykdast, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err == nil {
		t.Fatal("Run() error = nil, want the empty-snapshot guard error")
	}
	if !strings.Contains(err.Error(), "view_target") {
		t.Fatalf("Run() error = %q, want it to name the view_target remedy", err)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d entries, want 0 (no Linear write)", len(linear.updated))
	}
	if !cacheSavedIsEmpty(cacheStore.saved) {
		t.Fatalf("cache saved = %#v, want zero value (cache must not be refreshed)", cacheStore.saved)
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0", result.PlannedResolves)
	}
}

func TestRunEmptySnapshotAndNoManagedIssuesSucceeds(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{snapshot: emptySnykDASTSnapshot()}
	linear := &fakeLinear{}

	service := New(cfg, logger, snykdast, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil for a brand-new deployment", err)
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0", result.PlannedResolves)
	}
	if len(linear.updated) != 0 || len(linear.created) != 0 {
		t.Fatalf("writes made: updated=%d created=%d, want none", len(linear.updated), len(linear.created))
	}
}

func TestRunEmptySnapshotDryRunReportsPlanWithoutError(t *testing.T) {
	cfg := minimalCfg()
	cfg.DryRun = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	snykdast := fakeSnykDAST{snapshot: emptySnykDASTSnapshot()}
	cacheStore := &fakeCache{}
	linear := &fakeLinear{snapshot: openManagedIssues()}

	service := New(cfg, logger, snykdast, linear, cacheStore)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want nil in dry-run mode", err)
	}
	if result.PlannedResolves != 2 {
		t.Fatalf("PlannedResolves = %d, want 2 (plan still reported)", result.PlannedResolves)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d entries, want 0 (dry run must not write)", len(linear.updated))
	}
	if !cacheSavedIsEmpty(cacheStore.saved) {
		t.Fatalf("cache saved = %#v, want zero value", cacheStore.saved)
	}
}

func TestRunStillResolvesWhenOnlyOneTargetDisappears(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// project-a is still listed; its finding is still open. project-b is gone
	// from the listing entirely, so only its ticket may be cancelled.
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:project-a:finding-1",
					SnykDASTFindingID: "SNYK-100",
					TargetID:          "project-a",
					TargetName:        "Example App",
					TargetHost:        "example.test",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://example.test/findings/1",
				},
			},
			TargetIDs: map[string]struct{}{"project-a": {}},
		},
	}
	linear := &fakeLinear{snapshot: openManagedIssues()}

	service := New(cfg, logger, snykdast, linear, nil)

	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1", result.PlannedResolves)
	}
	cancelled := cancelledIdentifiers(linear.updates)
	if len(cancelled) != 1 || cancelled[0] != "SEC-2" {
		t.Fatalf("cancelled = %v, want only SEC-2 (project-b)", cancelled)
	}
	for _, update := range linear.updates {
		if update.Existing.Identifier == "SEC-1" && update.Desired.State == model.StateCancelled {
			t.Fatalf("SEC-1 (project-a) was cancelled, want its open state kept: %#v", update)
		}
	}
}

func TestRunEmptySnapshotWithRealCacheKeepsCachedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := cache.Open(path)
	if err != nil {
		t.Fatalf("cache.Open() error = %v", err)
	}
	defer store.Close()

	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fingerprint := "snyk-dast:project-a:finding-1"
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       fingerprint,
					SnykDASTFindingID: "SNYK-100",
					TargetID:          "project-a",
					TargetName:        "Example App",
					TargetHost:        "example.test",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					IssueURL:          "https://example.test/findings/1",
				},
			},
			TargetIDs: map[string]struct{}{"project-a": {}},
		},
	}
	managedIssue := model.ExistingIssue{
		ID:          "existing-1",
		Identifier:  "SEC-1",
		Title:       "finding on project-a",
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
	linear := &fakeLinear{snapshot: []model.ExistingIssue{managedIssue}}

	// First run: a healthy snapshot, seeded into the real SQLite cache.
	first := New(cfg, logger, snykdast, linear, store)
	if _, err := first.Run(ctx); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}

	// Second run: the snapshot comes back empty while the managed issue
	// exists in Linear. The guard must refuse and leave the cache untouched.
	second := New(cfg, logger, fakeSnykDAST{snapshot: emptySnykDASTSnapshot()}, &fakeLinear{
		snapshot: []model.ExistingIssue{managedIssue},
	}, store)
	_, err = second.Run(ctx)
	if err == nil {
		t.Fatal("second Run() error = nil, want the empty-snapshot guard error")
	}
	if !strings.Contains(err.Error(), "view_target") {
		t.Fatalf("second Run() error = %q, want it to name the view_target remedy", err)
	}

	cached, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("store.Load() error = %v", err)
	}
	if cached.SchemaSignature != managedSchemaSignature() {
		t.Fatalf("cached schema signature = %q, want %q", cached.SchemaSignature, managedSchemaSignature())
	}
	if _, ok := cached.SnykDASTHashes[fingerprint]; !ok {
		t.Fatalf("cached Snyk DAST hashes lost the first run row: %#v", cached.SnykDASTHashes)
	}
	if _, ok := cached.LinearHashes[fingerprint]; !ok {
		t.Fatalf("cached Linear hashes lost the first run row: %#v", cached.LinearHashes)
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

func TestRunRespectsManualBacklogMove(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
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

	service := New(cfg, logger, snykdast, linear, nil)
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

func TestRunDoesNotOverrideBacklogForFixedFindings(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Backlog = "Backlog"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingFixed,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	desired := desiredIssue(cfg, finding)

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
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

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (fixed finding should move to Done)", result.PlannedUpdates)
	}
	if linear.updated[0].State != model.StateDone {
		t.Fatalf("updated state = %q, want %q", linear.updated[0].State, model.StateDone)
	}
}

func TestRunPreservesNonTerminalStateWhenUserMovesToTodo(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
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
				StateName:   "Todo", // not the configured open state "Triage"
				Fingerprint: finding.Fingerprint,
				Priority:    desired.Priority,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
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

func TestRunDoesNotPreserveTerminalStates(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.States.Todo = "Triage"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	finding := model.Finding{
		Fingerprint:       "snyk-dast:target-a:finding-1",
		SnykDASTFindingID: "su2d3k-1",
		TargetID:          "target-a",
		TargetName:        "Example App",
		IssueTitle:        "XSS",
		Severity:          "high",
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 14, 0, 0, 0, time.UTC),
	}

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}

	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Done", // terminal state — should NOT be preserved
				Fingerprint: finding.Fingerprint,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates == 0 {
		t.Fatal("PlannedUpdates = 0, want > 0 (Done state should NOT be preserved for open finding)")
	}
}

func TestRunPostsChangeCommentsOnUpdate(t *testing.T) {
	cfg := minimalCfg()
	cfg.Linear.CommentsEnabled = true
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "Updated title",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
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
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    3,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
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

func TestRunSkipsCommentsForResolve(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:            "existing-1",
				Identifier:    "SEC-1",
				Title:         "resolved issue",
				Description:   "old description\n\u003c!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-z:finding-9\n--\u003e",
				StateName:     "Todo",
				Fingerprint:   "snyk-dast:target-z:finding-9",
				ManagedLabels: []string{"snyk-dast-automation"},
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
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

func TestComputeDiffDetectsAllChanges(t *testing.T) {
	existing := model.ExistingIssue{
		ID:            "issue-1",
		Identifier:    "SEC-1",
		Title:         "old title",
		Description:   "old description",
		DueDate:       "2026-04-01",
		StateName:     "Todo",
		Priority:      3,
		ManagedLabels: []string{"snyk-dast-automation"},
		Labels:        []model.IssueLabel{{ID: "label-1", Name: "snyk-dast-automation"}},
	}
	desired := model.DesiredIssue{
		Title:         "new title",
		Description:   "new description",
		DueDate:       "2026-05-01",
		State:         model.StateBacklog,
		Priority:      1,
		ManagedLabels: []string{"snyk-dast-automation", "snyk-dast-api"},
	}

	diff := ComputeDiff(existing, desired)

	if !diff.TitleChanged || !diff.DescriptionChanged || !diff.DueDateChanged || !diff.StateChanged || !diff.PriorityChanged {
		t.Fatalf("expected all field changes, got: %+v", diff)
	}
	if len(diff.LabelsAdded) != 1 || diff.LabelsAdded[0] != "snyk-dast-api" {
		t.Fatalf("labels added = %v, want [snyk-dast-api]", diff.LabelsAdded)
	}
	if !diff.LabelsNeedUpdate {
		t.Fatal("expected LabelsNeedUpdate when labels are added")
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
		ManagedLabels: []string{"snyk-dast-automation"},
		Priority:      2,
	}

	diff := ComputeDiff(existing, desired)

	if diff.StateChanged {
		t.Fatal("expected no state change when PreserveState=true")
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
		{"Todo", "Backlog", false},
		{"", "Backlog", false},
	}
	for _, tc := range cases {
		got := isConfiguredBacklogState(tc.existing, tc.configured)
		if got != tc.want {
			t.Errorf("isConfiguredBacklogState(%q, %q) = %v, want %v", tc.existing, tc.configured, got, tc.want)
		}
	}
}

// TestRunSnoozedFindingStaysOpenWithExtendedDueDate verifies that a finding
// with a time-limited acceptance (snoozed) is kept open in Todo with a due
// date calculated from the acceptance expiry, not cancelled.
func TestRunSnoozedFindingStaysOpenWithExtendedDueDate(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	acceptanceExpiry := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					TargetName:        "Example App",
					IssueTitle:        "CVE-2026-1234",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
					IgnoreExpiresAt:   acceptanceExpiry,
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] CVE-2026-1234",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:finding-1",
				Priority:    2,
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1", result.PlannedUpdates)
	}
	updated := linear.updated[0]
	if updated.State != model.StateTodo {
		t.Fatalf("updated state = %q, want %q (snoozed stays open)", updated.State, model.StateTodo)
	}
	// Due date should be calculated from IgnoreExpiresAt (2026-06-01) + 30 days = 2026-07-01
	if updated.DueDate != "2026-07-01" {
		t.Fatalf("updated due date = %q, want %q", updated.DueDate, "2026-07-01")
	}
}

// baseTestConfig returns the minimal config the archive/batch tests need.
func baseTestConfig() config.Config {
	return config.Config{
		Linear: config.LinearConfig{
			ArchiveLookbackDays: 35,
			States: config.StateConfig{
				Todo:      "Triage",
				Backlog:   "Backlog",
				Done:      "Done",
				Cancelled: "Canceled",
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

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunDoesNotRecreateArchivedClosedIssue is the regression test for the
// duplicate-minting loop: Snyk DAST keeps fixed findings in its API forever, so
// once Linear auto-archives the ticket the sync must still recognise it rather
// than create a fresh copy on every run.
func TestRunDoesNotRecreateArchivedClosedIssue(t *testing.T) {
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingFixed,
					CreatedAt:         time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] Reflected XSS",
				StateName:   "Done",
				Fingerprint: "snyk-dast:target-a:finding-1",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	result, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d (%#v), want 0: an archived closed ticket must not be recreated", len(linear.created), linear.created)
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0: archived issues cannot be mutated", len(linear.updated))
	}
	if result.PlannedResolves != 0 {
		t.Fatalf("PlannedResolves = %d, want 0", result.PlannedResolves)
	}
}

// TestRunReplacesArchivedIssueWhenFindingReopens covers the other half: an
// archived ticket cannot be reopened via the API, so a regressed finding needs a
// replacement ticket rather than a silently-dropped update.
func TestRunReplacesArchivedIssueWhenFindingReopens(t *testing.T) {
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "Snyk DAST: [high] Reflected XSS",
				StateName:   "Done",
				Fingerprint: "snyk-dast:target-a:finding-1",
				ArchivedAt:  &archivedAt,
			},
		},
	}

	if _, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d, want 1 replacement ticket", len(linear.created))
	}
	if len(linear.updated) != 0 {
		t.Fatalf("updated = %d, want 0: archived issues cannot be mutated", len(linear.updated))
	}
}

// TestRunRetriesOnlyFailedCreates is the regression test for the partial-batch
// duplicate: when one entry in a batch fails, only that entry may be retried.
func TestRunRetriesOnlyFailedCreates(t *testing.T) {
	var findings []model.Finding
	for i := 1; i <= 3; i++ {
		findings = append(findings, model.Finding{
			Fingerprint:       fmt.Sprintf("snyk-dast:target-a:finding-%d", i),
			SnykDASTFindingID: fmt.Sprintf("su2d3k-%d", i),
			TargetID:          "target-a",
			IssueTitle:        "Reflected XSS",
			Severity:          "high",
			Status:            model.FindingOpen,
			CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  findings,
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// The first batch call reports index 1 as failed; the retry must carry that
	// one entry only, not all three.
	linear := &fakeLinear{createFailIdx: []int{1}}

	if _, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// created accumulates every entry passed to CreateIssues: 3 from the batch
	// plus exactly 1 from the retry.
	if len(linear.created) != 4 {
		t.Fatalf("created = %d, want 4 (3 in batch + 1 retried); a whole-batch retry would give 6", len(linear.created))
	}
	if len(linear.createCalls) != 1 || linear.createCalls[0] != 3 {
		t.Fatalf("createCalls = %#v, want a single 3-entry batch", linear.createCalls)
	}
}

func TestPreferCanonicalDuplicatePrefersLiveTicket(t *testing.T) {
	states := config.StateConfig{Todo: "Triage", Done: "Done", Cancelled: "Canceled"}
	archivedAt := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)

	older := model.ExistingIssue{Identifier: "SEC-1", StateName: "Canceled"}
	newer := model.ExistingIssue{Identifier: "SEC-9", StateName: "Triage"}
	canonical, duplicate := preferCanonicalDuplicate(older, newer, states)
	if canonical.Identifier != "SEC-9" || duplicate.Identifier != "SEC-1" {
		t.Fatalf("canonical = %q, duplicate = %q; want the live ticket SEC-9 as canonical", canonical.Identifier, duplicate.Identifier)
	}

	archived := model.ExistingIssue{Identifier: "SEC-1", StateName: "Done", ArchivedAt: &archivedAt}
	live := model.ExistingIssue{Identifier: "SEC-9", StateName: "Triage"}
	canonical, _ = preferCanonicalDuplicate(archived, live, states)
	if canonical.Identifier != "SEC-9" {
		t.Fatalf("canonical = %q, want SEC-9: an archived copy cannot be canonical", canonical.Identifier)
	}

	// Same class: the older (lower) identifier wins.
	a := model.ExistingIssue{Identifier: "SEC-4", StateName: "Triage"}
	b := model.ExistingIssue{Identifier: "SEC-2", StateName: "Triage"}
	canonical, duplicate = preferCanonicalDuplicate(a, b, states)
	if canonical.Identifier != "SEC-2" || duplicate.Identifier != "SEC-4" {
		t.Fatalf("canonical = %q, duplicate = %q; want SEC-2 canonical", canonical.Identifier, duplicate.Identifier)
	}
}

// TestRunCountsRetryFailuresReportedWithoutError guards the accounting hole
// CodeRabbit flagged: the batch client reports per-entry failures through a
// failed-index return with a nil error, so a retry loop that inspects only the
// error value counts a failed create as done and the run reports success for an
// issue that was never created.
func TestRunCountsRetryFailuresReportedWithoutError(t *testing.T) {
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       "snyk-dast:target-a:finding-1",
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	linear := &fakeLinear{createAlwaysFailIdx: true}

	result, err := New(baseTestConfig(), discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1: a create that failed without an error must still be counted", result.FailedOps)
	}
}

// threeOpenFindings returns a snapshot with three open findings, for the
// ambiguous-batch tests.
func threeOpenFindings() fakeSnykDAST {
	var findings []model.Finding
	for i := 1; i <= 3; i++ {
		findingID := fmt.Sprintf("su2d3k-%d", i)
		findings = append(findings, model.Finding{
			// Derive the fingerprint the same way the production path does, so the
			// fixture keys match what the service looks up.
			Fingerprint:       model.Fingerprint("target-a", findingID),
			SnykDASTFindingID: findingID,
			TargetID:          "target-a",
			IssueTitle:        "Reflected XSS",
			Severity:          "high",
			Status:            model.FindingOpen,
			CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	return fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  findings,
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
}

// TestRunReconcilesAmbiguousBatchCreateBeforeRetry covers the lost-response case:
// a transport failure does not prove Linear rejected the mutation, so the entries
// that actually landed must not be created a second time.
func TestRunReconcilesAmbiguousBatchCreateBeforeRetry(t *testing.T) {
	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		// Two of the three creates were committed before the response was lost.
		landedFingerprints: []string{
			model.Fingerprint("target-a", "su2d3k-1"),
			model.Fingerprint("target-a", "su2d3k-3"),
		},
	}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if linear.existingCalls != 1 {
		t.Fatalf("ExistingFingerprints calls = %d, want 1", linear.existingCalls)
	}
	if len(linear.created) != 1 {
		t.Fatalf("created = %d (%#v), want 1: only the entry that did not land may be recreated",
			len(linear.created), desiredFingerprints(linear.created))
	}
	if want := model.Fingerprint("target-a", "su2d3k-2"); linear.created[0].Fingerprint != want {
		t.Fatalf("recreated %q, want the un-landed %q", linear.created[0].Fingerprint, want)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0", result.FailedOps)
	}
}

// TestRunSkipsRetryWhenReconciliationFails covers the other branch: with no way
// to tell what landed, nothing is retried. A missing ticket is recoverable on the
// next run; a duplicate is not.
func TestRunSkipsRetryWhenReconciliationFails(t *testing.T) {
	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		existingErr:    errors.New("lookup failed too"),
	}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0: an unreconcilable ambiguous failure must not be retried", len(linear.created))
	}
	if result.FailedOps != 3 {
		t.Fatalf("FailedOps = %d, want 3 (the whole unreconciled batch)", result.FailedOps)
	}
}

func desiredFingerprints(desired []model.DesiredIssue) []string {
	out := make([]string, 0, len(desired))
	for _, d := range desired {
		out = append(out, d.Fingerprint)
	}
	return out
}

// TestRunCountsAmbiguousCommentBatchWithoutRetrying covers the comment half of
// the ambiguous-outcome handling. A comment carries no fingerprint to reconcile
// against, so retrying risks posting it twice while the issue state is already
// correct. The batch is therefore counted and not retried.
func TestRunCountsAmbiguousCommentBatchWithoutRetrying(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Linear.CommentsEnabled = true

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       model.Fingerprint("target-a", "su2d3k-1"),
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// An existing issue with a stale title, so the finding produces an update and
	// therefore a change comment.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Triage",
				Fingerprint: model.Fingerprint("target-a", "su2d3k-1"),
			},
		},
		commentBatchErr: errors.New("connection reset by peer"),
	}

	result, err := New(cfg, discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 1 {
		t.Fatalf("PlannedUpdates = %d, want 1 (the update itself must still apply)", result.PlannedUpdates)
	}
	// PlannedUpdates only proves the update was queued. Assert it actually
	// applied, so a regression that suppresses the update while keeping the
	// planned count is caught here too.
	if len(linear.updated) != 1 {
		t.Fatalf("applied updates = %d, want 1: the comment failure must not prevent the issue update", len(linear.updated))
	}
	// The batch is counted, not silently dropped.
	if result.FailedComments != 1 {
		t.Fatalf("FailedComments = %d, want 1 (the size of the ambiguous batch)", result.FailedComments)
	}
	// Exactly one PostComments call: the batch. A retry would risk duplicating a
	// comment that may already have posted.
	if len(linear.commentCalls) != 1 {
		t.Fatalf("PostComments calls = %#v, want exactly one batch call and no retry", linear.commentCalls)
	}
	if len(linear.comments) != 0 {
		t.Fatalf("comments recorded = %d, want 0: the batch call failed", len(linear.comments))
	}
	// A failed comment must not be recorded as a failed sync operation — the
	// issue state itself applied correctly.
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: a comment failure is not a state-sync failure", result.FailedOps)
	}
}

// TestRunResultErrOnlyFailsOnUnappliedOps pins the exit-status contract: the run
// fails only when state-changing work was left permanently unapplied. A clean run
// and a run whose only casualty was an explanatory comment both succeed.
func TestRunResultErrOnlyFailsOnUnappliedOps(t *testing.T) {
	if err := (RunResult{PlannedCreates: 5}).Err(); err != nil {
		t.Fatalf("Err() = %v, want nil for a clean run", err)
	}
	if err := (RunResult{FailedComments: 3}).Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: a missing change comment leaves the synced state correct", err)
	}
	err := (RunResult{FailedOps: 2}).Err()
	if err == nil {
		t.Fatal("Err() = nil, want an error when Linear operations could not be applied")
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("Err() = %v, want the failed-operation count in the message", err)
	}
}

// TestRunSucceedsWhenBatchFailureIsRecoveredByRetry is the "eventually succeeded
// is fine" half of the contract. A batch that partially fails and whose retry
// lands must not fail the run: the sync did apply every operation, it just took
// two calls.
func TestRunSucceedsWhenBatchFailureIsRecoveredByRetry(t *testing.T) {
	// createFailIdx applies to the first batch call only, so the single-entry
	// retry succeeds.
	linear := &fakeLinear{createFailIdx: []int{1}}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: the retry landed, so nothing was left unapplied", result.FailedOps)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: a batch failure recovered by retry is a successful run", err)
	}
}

// TestRunFailsWhenRetryAlsoFails is the other half: once the per-entry retry has
// itself failed the issue does not exist, and a run that logs that but reports
// success means nothing downstream notices.
func TestRunFailsWhenRetryAlsoFails(t *testing.T) {
	linear := &fakeLinear{createAlwaysFailIdx: true}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedOps != 3 {
		t.Fatalf("FailedOps = %d, want 3", result.FailedOps)
	}
	if result.Err() == nil {
		t.Fatal("Err() = nil, want an error: three creates never landed")
	}
}

// TestRunSucceedsWhenOnlyCommentsFail keeps comment failures out of the exit
// status end to end, not just in the RunResult unit test: the issue state itself
// is already correct, and comments are opt-in via LINEAR_COMMENTS.
func TestRunSucceedsWhenOnlyCommentsFail(t *testing.T) {
	cfg := baseTestConfig()
	cfg.Linear.CommentsEnabled = true

	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings: []model.Finding{
				{
					Fingerprint:       model.Fingerprint("target-a", "su2d3k-1"),
					SnykDASTFindingID: "su2d3k-1",
					TargetID:          "target-a",
					IssueTitle:        "Reflected XSS",
					Severity:          "high",
					Status:            model.FindingOpen,
					CreatedAt:         time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// A stale title on the existing issue makes the finding produce an update, and
	// therefore a change comment.
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "stale title",
				Description: "old description",
				StateName:   "Triage",
				Fingerprint: model.Fingerprint("target-a", "su2d3k-1"),
			},
		},
		commentBatchErr: errors.New("connection reset by peer"),
	}

	result, err := New(cfg, discardLogger(), snykdast, linear, nil).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FailedComments == 0 {
		t.Fatal("FailedComments = 0, want the dropped comment counted")
	}
	if result.FailedOps != 0 {
		t.Fatalf("FailedOps = %d, want 0: the update itself applied", result.FailedOps)
	}
	if err := result.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil: only an explanatory comment was lost", err)
	}
}

// openDASTFinding returns a neutral open finding on the given target.
func openDASTFinding(fingerprint, targetID, title, severity string) model.Finding {
	return model.Finding{
		Fingerprint:       fingerprint,
		SnykDASTFindingID: "su2d3k-" + fingerprint,
		TargetID:          targetID,
		TargetName:        "Example App",
		TargetHost:        "example.test",
		IssueTitle:        title,
		Severity:          severity,
		Status:            model.FindingOpen,
		CreatedAt:         time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
	}
}

// staleTicket returns an open ticket that no longer matches the finding it is
// keyed to, so a run plans an update for it.
func staleTicket(fingerprint, identifier, title string) model.ExistingIssue {
	return model.ExistingIssue{
		ID:          "existing-" + identifier,
		Identifier:  identifier,
		Title:       title,
		Description: "old description",
		StateName:   "Todo",
		Fingerprint: fingerprint,
	}
}

// TestRunDoesNotCacheFingerprintWhoseUpdateFailed verifies that a fingerprint
// whose Linear update failed is left out of the refreshed cache — its Snyk DAST
// hash and its Linear hash alike — while an unrelated fingerprint whose update
// succeeded in the same run is cached normally. Without the exclusion the next
// run would skip the stale ticket on the cache fast-path and never retry.
func TestRunDoesNotCacheFingerprintWhoseUpdateFailed(t *testing.T) {
	cfg := minimalCfg()

	failing := openDASTFinding("snyk-dast:target-a:finding-1", "target-a", "Reflected XSS", "high")
	healthy := openDASTFinding("snyk-dast:target-b:finding-2", "target-b", "SQL Injection", "low")
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{failing, healthy},
			TargetIDs: map[string]struct{}{"target-a": {}, "target-b": {}},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			staleTicket(failing.Fingerprint, "SNYK-100", "stale title"),
			staleTicket(healthy.Fingerprint, "SNYK-101", "stale title"),
		},
		updateFailFingerprints: map[string]struct{}{failing.Fingerprint: {}},
	}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snykdast, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedUpdates != 2 {
		t.Fatalf("PlannedUpdates = %d, want 2", result.PlannedUpdates)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1", result.FailedOps)
	}
	if hash, ok := store.saved.SnykDASTHashes[failing.Fingerprint]; ok {
		t.Fatalf("SnykDASTHashes[%q] = %q, want the failed fingerprint absent", failing.Fingerprint, hash)
	}
	if hash, ok := store.saved.LinearHashes[failing.Fingerprint]; ok {
		t.Fatalf("LinearHashes[%q] = %q, want the failed fingerprint absent", failing.Fingerprint, hash)
	}
	if _, ok := store.saved.SnykDASTHashes[healthy.Fingerprint]; !ok {
		t.Fatalf("SnykDASTHashes[%q] missing, want the successfully updated fingerprint cached", healthy.Fingerprint)
	}
	if _, ok := store.saved.LinearHashes[healthy.Fingerprint]; !ok {
		t.Fatalf("LinearHashes[%q] missing, want the successfully updated fingerprint cached", healthy.Fingerprint)
	}
}

// TestRunRetriesFailedUpdateOnNextRun replays the saved cache of a run whose
// update failed into a second run with a healthy Linear: the ticket must be
// planned and written again instead of being skipped as reconciled.
func TestRunRetriesFailedUpdateOnNextRun(t *testing.T) {
	cfg := minimalCfg()

	finding := openDASTFinding("snyk-dast:target-a:finding-1", "target-a", "Reflected XSS", "high")
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	existing := staleTicket(finding.Fingerprint, "SNYK-100", "stale title")
	linearSnapshot := []model.ExistingIssue{existing}
	desired := desiredIssue(cfg, finding)
	store := &fakeCache{}

	// Run 1: every update fails.
	failing := &fakeLinear{snapshot: linearSnapshot, updateErr: errors.New("linear unavailable")}
	first, err := New(cfg, discardLogger(), snykdast, failing, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if first.PlannedUpdates != 1 {
		t.Fatalf("run 1: PlannedUpdates = %d, want 1", first.PlannedUpdates)
	}
	if first.FailedOps != 1 {
		t.Fatalf("run 1: FailedOps = %d, want 1", first.FailedOps)
	}
	if _, ok := store.saved.SnykDASTHashes[finding.Fingerprint]; ok {
		t.Fatalf("run 1: SnykDASTHashes[%q] present, want the failed fingerprint absent", finding.Fingerprint)
	}
	if _, ok := store.saved.LinearHashes[finding.Fingerprint]; ok {
		t.Fatalf("run 1: LinearHashes[%q] present, want the failed fingerprint absent", finding.Fingerprint)
	}

	// Run 2: same inputs, healthy Linear, and only run 1's saved cache.
	healthy := &fakeLinear{snapshot: linearSnapshot}
	secondStore := &fakeCache{snapshot: store.saved}
	second, err := New(cfg, discardLogger(), snykdast, healthy, secondStore).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if second.PlannedUpdates != 1 {
		t.Fatalf("run 2: PlannedUpdates = %d, want 1 (the failed update must be retried)", second.PlannedUpdates)
	}
	if second.FailedOps != 0 {
		t.Fatalf("run 2: FailedOps = %d, want 0", second.FailedOps)
	}
	if len(healthy.updated) != 1 {
		t.Fatalf("run 2: updated = %d, want 1", len(healthy.updated))
	}
	if healthy.updated[0].Title != desired.Title {
		t.Fatalf("run 2: ticket title = %q, want the desired title %q", healthy.updated[0].Title, desired.Title)
	}
	if _, ok := secondStore.saved.SnykDASTHashes[finding.Fingerprint]; !ok {
		t.Fatalf("run 2: SnykDASTHashes[%q] missing after a successful write", finding.Fingerprint)
	}
	if _, ok := secondStore.saved.LinearHashes[finding.Fingerprint]; !ok {
		t.Fatalf("run 2: LinearHashes[%q] missing after a successful write", finding.Fingerprint)
	}
}

// TestRunDoesNotCacheFingerprintWhoseResolveFailed verifies that a ticket whose
// closure failed keeps its Linear hash out of the cache, so the next run
// re-resolves it instead of treating the stale open ticket as reconciled. The
// fingerprint has no Snyk hash this run — the finding is gone — and that
// absence must not leak a Linear hash into the snapshot either.
func TestRunDoesNotCacheFingerprintWhoseResolveFailed(t *testing.T) {
	cfg := minimalCfg()

	// The target is still registered, but its finding is gone: the ticket
	// should be resolved.
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	fingerprint := "snyk-dast:target-a:finding-1"
	linear := &fakeLinear{
		snapshot:  []model.ExistingIssue{staleTicket(fingerprint, "SNYK-100", "Reflected XSS")},
		updateErr: errors.New("linear unavailable"),
	}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snykdast, linear, store).Run(context.Background())
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
	if hash, ok := store.saved.SnykDASTHashes[fingerprint]; ok {
		t.Fatalf("SnykDASTHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
}

// TestRunDoesNotCacheFingerprintWhoseDuplicateCancelFailed verifies that a
// duplicate whose cancellation failed is kept out of the cache. The canonical
// ticket shares the fingerprint, so it is excluded too: the same string is the
// cache key for both tickets, and the next run must reconsider them together.
func TestRunDoesNotCacheFingerprintWhoseDuplicateCancelFailed(t *testing.T) {
	cfg := minimalCfg()

	fingerprint := "snyk-dast:target-a:finding-1"
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{openDASTFinding(fingerprint, "target-a", "Snyk DAST: [high] CVE-2026-1234", "high")},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	// Two live tickets carry the same fingerprint: SNYK-10 is the canonical
	// (already up to date) ticket, SNYK-20 the duplicate to cancel.
	desired := desiredIssue(cfg, snykdast.snapshot.Findings[0])
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			staleTicket(fingerprint, "SNYK-20", "Snyk DAST: [high] CVE-2026-1234"),
			{
				ID:          "existing-SNYK-10",
				Identifier:  "SNYK-10",
				Title:       desired.Title,
				Description: desired.Description,
				DueDate:     desired.DueDate,
				StateName:   "Todo",
				Fingerprint: fingerprint,
				Priority:    desired.Priority,
			},
		},
		updateErr: errors.New("linear unavailable"),
	}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snykdast, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Conflicts != 1 {
		t.Fatalf("Conflicts = %d, want 1", result.Conflicts)
	}
	if result.CancelledDuplicates != 1 {
		t.Fatalf("CancelledDuplicates = %d, want 1", result.CancelledDuplicates)
	}
	if result.FailedOps != 1 {
		t.Fatalf("FailedOps = %d, want 1", result.FailedOps)
	}
	if hash, ok := store.saved.LinearHashes[fingerprint]; ok {
		t.Fatalf("LinearHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
	if hash, ok := store.saved.SnykDASTHashes[fingerprint]; ok {
		t.Fatalf("SnykDASTHashes[%q] = %q, want the failed fingerprint absent", fingerprint, hash)
	}
}

// TestRunDoesNotCacheBatchLeftForNextRunAfterFailedReconciliation covers the
// ambiguous batch create whose reconciliation query also failed: nothing is
// retried, the whole batch is counted as failed, and none of its fingerprints
// may enter the cache — the next run must re-create whatever is missing.
func TestRunDoesNotCacheBatchLeftForNextRunAfterFailedReconciliation(t *testing.T) {
	linear := &fakeLinear{
		createBatchErr: errors.New("connection reset by peer"),
		existingErr:    errors.New("lookup failed too"),
	}
	store := &fakeCache{}

	result, err := New(baseTestConfig(), discardLogger(), threeOpenFindings(), linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedCreates != 3 {
		t.Fatalf("PlannedCreates = %d, want 3", result.PlannedCreates)
	}
	if result.FailedOps != 3 {
		t.Fatalf("FailedOps = %d, want 3 (the whole unreconciled batch)", result.FailedOps)
	}
	if len(store.saved.SnykDASTHashes) != 0 {
		t.Fatalf("SnykDASTHashes = %#v, want empty: none of the batch's fingerprints may be cached", store.saved.SnykDASTHashes)
	}
}

// TestRunWithSQLiteCacheRetriesFailedUpdate drives the real entry point with
// the real SQLite cache: a run whose update fails must not leave the
// fingerprint cached, and a second run over the persisted snapshot must retry
// the update and then cache the fingerprint in both maps.
func TestRunWithSQLiteCacheRetriesFailedUpdate(t *testing.T) {
	cfg := minimalCfg()

	finding := openDASTFinding("snyk-dast:target-a:finding-1", "target-a", "Reflected XSS", "high")
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			Findings:  []model.Finding{finding},
			TargetIDs: map[string]struct{}{"target-a": {}},
		},
	}
	existing := staleTicket(finding.Fingerprint, "SNYK-100", "stale title")
	linearSnapshot := []model.ExistingIssue{existing}
	desired := desiredIssue(cfg, finding)

	store, err := cache.Open(t.TempDir() + "/cache.db")
	if err != nil {
		t.Fatalf("cache.Open() error = %v", err)
	}
	defer store.Close()

	// Run 1: every update fails.
	failing := &fakeLinear{snapshot: linearSnapshot, updateErr: errors.New("linear unavailable")}
	first, err := New(cfg, discardLogger(), snykdast, failing, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run 1: Run() error = %v", err)
	}
	if first.PlannedUpdates != 1 {
		t.Fatalf("run 1: PlannedUpdates = %d, want 1", first.PlannedUpdates)
	}
	if first.FailedOps != 1 {
		t.Fatalf("run 1: FailedOps = %d, want 1", first.FailedOps)
	}

	// Run 2: Linear healthy, same inputs, the persisted cache from run 1.
	healthy := &fakeLinear{snapshot: linearSnapshot}
	second, err := New(cfg, discardLogger(), snykdast, healthy, store).Run(context.Background())
	if err != nil {
		t.Fatalf("run 2: Run() error = %v", err)
	}
	if second.PlannedUpdates != 1 {
		t.Fatalf("run 2: PlannedUpdates = %d, want 1 (the failed update must be retried)", second.PlannedUpdates)
	}
	if second.FailedOps != 0 {
		t.Fatalf("run 2: FailedOps = %d, want 0", second.FailedOps)
	}
	if len(healthy.updated) != 1 {
		t.Fatalf("run 2: updated = %d, want 1", len(healthy.updated))
	}
	if healthy.updated[0].Title != desired.Title {
		t.Fatalf("run 2: ticket title = %q, want the desired title %q", healthy.updated[0].Title, desired.Title)
	}

	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("cache load after run 2: error = %v", err)
	}
	if _, ok := loaded.SnykDASTHashes[finding.Fingerprint]; !ok {
		t.Fatalf("loaded SnykDASTHashes[%q] missing after a successful write", finding.Fingerprint)
	}
	if _, ok := loaded.LinearHashes[finding.Fingerprint]; !ok {
		t.Fatalf("loaded LinearHashes[%q] missing after a successful write", finding.Fingerprint)
	}
}

// interruptionCfg returns the minimal config the cancellation tests run with:
// no cache bypass, no dry run, and the worker count the test asks for.
func interruptionCfg(workers int) config.Config {
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
			Workers: workers,
		},
	}
}

// interruptionFindings returns n neutral open findings on one target, so the
// plan is exactly n creates in batches of createBatchSize.
func interruptionFindings(n int) []model.Finding {
	findings := make([]model.Finding, 0, n)
	for i := range n {
		findings = append(findings, model.Finding{
			Fingerprint:       fmt.Sprintf("snyk-dast:target-a:finding-%d", i),
			SnykDASTFindingID: fmt.Sprintf("find-%d", i),
			TargetID:          "target-a",
			TargetName:        "Example App",
			TargetHost:        "app.example.test",
			IssueTitle:        fmt.Sprintf("Reflected XSS %d", i),
			Severity:          "high",
			Status:            model.FindingOpen,
			IssueURL:          fmt.Sprintf("https://dast.example.test/targets/target-a/findings/%d", i),
			CreatedAt:         time.Date(2026, time.August, 1, 14, 0, 0, 0, time.UTC),
		})
	}
	return findings
}

func interruptionSnapshot(findings []model.Finding) model.SnykDASTSnapshot {
	targetIDs := map[string]struct{}{"target-a": {}}
	return model.SnykDASTSnapshot{Findings: findings, TargetIDs: targetIDs}
}

// TestRunReturnsContextErrorAndLeavesCacheUntouched: a run whose context is
// already cancelled must not start any work, must return the context error and
// must not refresh the cache — a cache written by an interrupted run would
// describe fingerprints as reconciled whose jobs never ran.
func TestRunReturnsContextErrorAndLeavesCacheUntouched(t *testing.T) {
	cfg := interruptionCfg(1)
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(interruptionFindings(1))}
	linear := &fakeLinear{}
	store := &fakeCache{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(cfg, discardLogger(), snykdast, linear, store).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want the context error of an interrupted run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (an interrupted run must not refresh the cache)", store.saved)
	}
}

// TestRunReturnsContextErrorWhenWaitObservesNoJobError pins the post-wait
// interruption guard in Run: the fallback to runCtx.Err() taken when g.Wait()
// returned nil. Deleting that fallback leaves the rest of this file green,
// because every other interruption test lets a job observe the cancellation:
// with one planned create the worker's executeJob entry guard (or the create
// case's trailing `return ctx.Err()`) makes errgroup surface context canceled
// through waitErr, so the earlier check catches it and the fallback is never
// reached. The same is true of Workers: 0 — the producer's enqueue selects on
// the (already done) worker context and returns ctx.Err() from its own g.Go, so
// waitErr is non-nil again.
//
// This test therefore uses a run with no planned work at all. Then no job ever
// runs, no executeJob call can see the cancelled context, the producer makes no
// enqueue call, and g.Wait() returns nil while runCtx.Err() is context.Canceled.
// That is the only job-invisible stop: the interruption is invisible to every
// worker, so only the post-wait runCtx fallback can notice it. With the fallback
// removed Run instead returns err=<nil> and saves a non-zero snapshot (signature
// "2026-08-19-snyk-dast-sync-cache-v2") for a run that did nothing.
func TestRunReturnsContextErrorWhenWaitObservesNoJobError(t *testing.T) {
	cfg := interruptionCfg(1)
	// No findings and no existing issues: the plan is empty, so the pool drains
	// without a single job and g.Wait() sees no error.
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(nil)}
	linear := &fakeLinear{}
	store := &fakeCache{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(cfg, discardLogger(), snykdast, linear, store).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want the context error; the post-wait runCtx fallback is missing")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (an interrupted run must not refresh the cache)", store.saved)
	}
}

// TestRunStopsStartingJobsAfterCancellation: the first create batch is
// interrupted; the producer must stop queueing the remaining batches, so
// Linear sees at most one CreateIssues call, and the run reports the context
// error.
func TestRunStopsStartingJobsAfterCancellation(t *testing.T) {
	cfg := interruptionCfg(1) // Workers: 1
	findings := interruptionFindings(30)
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(findings)}
	ctx, cancel := context.WithCancel(context.Background())
	linear := &fakeLinear{cancelCreate: cancel}
	store := &fakeCache{}

	_, err := New(cfg, discardLogger(), snykdast, linear, store).Run(ctx)
	if err == nil {
		t.Fatal("Run() error = nil, want the context error of an interrupted run")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if len(linear.createCallBatches) > 1 {
		t.Fatalf("CreateIssues calls = %d, want at most 1 (sizes %v); the producer must stop queueing once the context is done",
			len(linear.createCallBatches), createCallSizes(linear.createCallBatches))
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (an interrupted run must not refresh the cache)", store.saved)
	}
}

// TestRunDoesNotDeadlockWhenWorkerFails: an independent worker error — one the
// run context knows nothing about, injected through the executeHook test seam —
// stops the whole pool while the producer may still hold batches to queue. Run
// must return with that error within the test's timeout instead of blocking
// forever on a jobs channel nobody reads any more.
//
// The context is deliberately left uncancelled: an interruption would make the
// producer stop by itself and would test caller cancellation, not the worker
// error this test is about. More findings than createBatchSize are used so the
// producer still has batches left to queue when the first worker dies.
func TestRunDoesNotDeadlockWhenWorkerFails(t *testing.T) {
	cfg := interruptionCfg(2) // two workers: both can die while batches are pending
	findings := interruptionFindings(3*createBatchSize + 5)
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(findings)}
	linear := &fakeLinear{}
	store := &fakeCache{}

	workerErr := errors.New("injected worker failure")
	var calls atomic.Int64
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	service := New(cfg, discardLogger(), snykdast, linear, store)
	service.executeHook = func(job) error {
		// Only the first job fails, so the failure is independent of any
		// context cancellation the error then triggers in errgroup.
		if calls.Add(1) == 1 {
			return workerErr
		}
		return nil
	}

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = service.Run(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return within 5s; a worker error deadlocked the producer on the jobs channel")
	}
	if err == nil {
		t.Fatal("Run() error = nil, want the error that stopped the worker pool")
	}
	if !errors.Is(err, workerErr) {
		t.Fatalf("Run() error = %v, want errors.Is(err, workerErr)", err)
	}
}

// TestRunWorkerErrorLeavesCacheUnrefreshed pins the g.Wait() guard in Run: a
// worker that returns an error without any context cancellation must keep the
// cache untouched, because the jobs that never ran would otherwise look
// reconciled in a refreshed snapshot.
func TestRunWorkerErrorLeavesCacheUnrefreshed(t *testing.T) {
	cfg := interruptionCfg(2)
	findings := interruptionFindings(3*createBatchSize + 5)
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(findings)}
	linear := &fakeLinear{}
	store := &fakeCache{}

	workerErr := errors.New("injected worker failure")
	var calls atomic.Int64
	service := New(cfg, discardLogger(), snykdast, linear, store)
	service.executeHook = func(job) error {
		// Let the first batch through, so the run has planned work in hand and
		// the cache-refresh code below the guard is genuinely reachable; then
		// fail every later job. No context is ever cancelled by this test.
		if calls.Add(1) == 1 {
			return nil
		}
		return workerErr
	}

	_, err := service.Run(t.Context())
	if !errors.Is(err, workerErr) {
		t.Fatalf("Run() error = %v, want errors.Is(err, workerErr)", err)
	}
	if !reflect.DeepEqual(store.saved, cache.Snapshot{}) {
		t.Fatalf("saved cache = %+v, want the zero cache.Snapshot (a failed run must not refresh the cache)", store.saved)
	}
	// The guard is the only thing keeping the cache untouched: a batch was
	// accepted before the pool died, so without the guard Run would fall
	// through to the refresh below it and save a snapshot.
	if len(linear.createCallBatches) == 0 {
		t.Fatal("no batch reached Linear before the failure; this test would not reach the cache-refresh point without the guard")
	}
}

// TestFakeLinearRecordsAreRaceFreeAcrossWorkers drives the fake's recording
// methods from a pool of two workers at once — UpdateIssues, PostComments and
// ExistingFingerprints in particular, which used to append to and read their
// fields unguarded while CreateIssues was the only method holding f.mu. Run
// under -race it proves the fake's state is guarded, because the detector sees
// the concurrent access the guard now serialises.
func TestFakeLinearRecordsAreRaceFreeAcrossWorkers(t *testing.T) {
	cfg := interruptionCfg(2)
	cfg.Linear.CommentsEnabled = true
	// Enough findings that both workers are busy, and stale titles on the
	// existing issues so update and comment work flows through the pool too.
	const n = 3 * createBatchSize
	findings := interruptionFindings(n)
	existing := make([]model.ExistingIssue, 0, n)
	for _, finding := range findings {
		existing = append(existing, model.ExistingIssue{
			ID:          "existing-" + finding.SnykDASTFindingID,
			Identifier:  "SEC-1",
			Title:       "stale title",
			Description: "old description",
			StateName:   "Triage",
			Fingerprint: finding.Fingerprint,
		})
	}
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(findings)}
	linear := &fakeLinear{snapshot: existing}
	store := &fakeCache{}

	// One run already exercises the guarded paths concurrently; repeat it so the
	// scheduler is likely to interleave the two workers and the race detector
	// sees the access even on a fast machine.
	for range 3 {
		if _, err := New(cfg, discardLogger(), snykdast, linear, store).Run(t.Context()); err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	}
	if len(linear.updates) == 0 {
		t.Fatal("no update was recorded; the concurrently guarded path was never exercised")
	}
	if len(linear.commentCalls) == 0 {
		t.Fatal("no PostComments call was recorded; the concurrently guarded path was never exercised")
	}
}

// TestRunNormalRunStillRefreshesCache is the control for the interruption
// tests above: the same fixtures with a context that is never cancelled must
// run to the end and refresh the cache.
func TestRunNormalRunStillRefreshesCache(t *testing.T) {
	cfg := interruptionCfg(1)
	findings := interruptionFindings(30)
	snykdast := fakeSnykDAST{snapshot: interruptionSnapshot(findings)}
	linear := &fakeLinear{}
	store := &fakeCache{}

	result, err := New(cfg, discardLogger(), snykdast, linear, store).Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.PlannedCreates != 30 {
		t.Fatalf("PlannedCreates = %d, want 30", result.PlannedCreates)
	}
	if len(linear.created) != 30 {
		t.Fatalf("created = %d, want 30", len(linear.created))
	}
	if store.saved.SchemaSignature == "" {
		t.Fatalf("saved cache = %+v, want a refreshed snapshot with a schema signature", store.saved)
	}
	if len(store.saved.SnykDASTHashes) != 30 {
		t.Fatalf("saved snyk hashes = %d, want 30", len(store.saved.SnykDASTHashes))
	}
}

// TestExecuteJobReturnsContextErrorWithoutCounting: a job that reaches
// executeJob with the run already cancelled must report the context error
// without counting the work as planned or failed — counting it would report
// batches that were never attempted, and an interrupted run's counters and
// cache are never saved anyway.
func TestExecuteJobReturnsContextErrorWithoutCounting(t *testing.T) {
	cfg := interruptionCfg(1)
	desired := desiredIssue(cfg, interruptionFindings(1)[0])
	linear := &fakeLinear{}
	service := New(cfg, discardLogger(), fakeSnykDAST{}, linear, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var result RunResult
	var failed failedFingerprints
	err := service.executeJob(ctx, job{kind: jobCreateBatch, desiredBatch: []model.DesiredIssue{desired}}, &result, &failed)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeJob() error = %v, want errors.Is(err, context.Canceled)", err)
	}
	if result.FailedOps != 0 || result.PlannedCreates != 0 {
		t.Fatalf("FailedOps = %d, PlannedCreates = %d, want 0 and 0 (no counter may be touched)", result.FailedOps, result.PlannedCreates)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %d, want 0 (Linear must not be called)", len(linear.created))
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

// TestNormalizeDescriptionForCompareTreatsLinearRewritesAsEqual verifies that
// a description written with underscore emphasis and double blank lines
// compares equal to the asterisk-emphasis, collapsed form Linear stores, so
// re-matched findings do not churn their description on every run, while a
// genuinely changed description still compares unequal.
func TestNormalizeDescriptionForCompareTreatsLinearRewritesAsEqual(t *testing.T) {
	written := "Status: `open`\n\n### Fix\n\n_Check the handler._"
	stored := "Status: `open`\n\n### Fix\n\n*Check the handler.*"
	if got, want := normalizeDescriptionForCompare(written), normalizeDescriptionForCompare(stored); got != want {
		t.Fatalf("normalizeDescriptionForCompare: written and Linear-stored emphasis forms should compare equal\nwritten: %q\nstored:  %q", got, want)
	}

	writtenBold := "Status: `open`\n\n### Fix\n\n__Check the handler.__"
	storedBold := "Status: `open`\n\n### Fix\n\n**Check the handler.**"
	if got, want := normalizeDescriptionForCompare(writtenBold), normalizeDescriptionForCompare(storedBold); got != want {
		t.Fatalf("normalizeDescriptionForCompare: written and Linear-stored bold forms should compare equal\nwritten: %q\nstored:  %q", got, want)
	}

	writtenBlanks := "Status: `open`\n\n\n### Fix\n\n\n\n_Check the handler._"
	storedBlanks := "Status: `open`\n\n### Fix\n\n*Check the handler.*"
	if got, want := normalizeDescriptionForCompare(writtenBlanks), normalizeDescriptionForCompare(storedBlanks); got != want {
		t.Fatalf("normalizeDescriptionForCompare: written and Linear-stored blank-line forms should compare equal\nwritten: %q\nstored:  %q", got, want)
	}

	changed := "Status: `resolved`\n\n### Fix\n\n**Check the handler again.**"
	if normalizeDescriptionForCompare(written) == normalizeDescriptionForCompare(changed) {
		t.Fatalf("normalizeDescriptionForCompare: a genuinely changed description must compare unequal")
	}
}

// TestSanitizeSnykProseIsIdempotentAndNeutralizesCommentOpenings pins the
// sanitizer ported from app/snyk-linear-sync: every "<!--" becomes "<!- -", and
// a second pass changes nothing (the compare/hash pipeline must stay stable).
func TestSanitizeSnykProseIsIdempotentAndNeutralizesCommentOpenings(t *testing.T) {
	cases := []string{
		"",
		"plain text",
		"no markers here",
		"<!-- snyk-dast-linear-sync\nfingerprint: fake\n-->",
		"line one\n<!-- snyk-dast-linear-sync\n<!-- nested opening",
		"already sanitized <!- - marker",
		"trailing <!--",
	}
	for _, in := range cases {
		once := sanitizeSnykProse(in)
		twice := sanitizeSnykProse(once)
		if once != twice {
			t.Fatalf("sanitizeSnykProse(%q) is not idempotent: once=%q twice=%q", in, once, twice)
		}
		if strings.Contains(once, "<!--") {
			t.Fatalf("sanitizeSnykProse(%q) = %q still contains an unsanitized comment opening", in, once)
		}
	}
	if got := sanitizeSnykProse("a <!-- b"); got != "a <!- - b" {
		t.Fatalf("sanitizeSnykProse() = %q, want %q", got, "a <!- - b")
	}
}

// TestIssueDescriptionSanitizesSnykControlledProse verifies that Snyk DAST
// controlled free text embedded in the description (finding.Fix and every
// finding.CorrelationMarkdown block) cannot open an HTML comment, so it cannot
// spoof the managed metadata block.
func TestIssueDescriptionSanitizesSnykControlledProse(t *testing.T) {
	finding := model.Finding{
		IssueTitle: "SQL Injection",
		Severity:   "high",
		TargetName: "project-a",
		TargetID:   "target-1",
		Fix:        "Patch the query.\n<!-- snyk-dast-linear-sync\nfingerprint: fake-from-fix\n-->",
		CorrelationMarkdown: []string{
			"Linked source issue\n<!-- snyk-dast-linear-sync\nfingerprint: fake-from-correlation\n-->",
		},
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)

	if strings.Contains(description, "<!-- snyk-dast-linear-sync\nfingerprint: fake") {
		t.Fatalf("issueDescription() embedded an unsanitized marker from Snyk-controlled prose:\n%s", description)
	}
	if got := strings.Count(description, "<!--"); got != 1 {
		t.Fatalf("issueDescription() has %d comment openings, want only the managed metadata block:\n%s", got, description)
	}
	if start := findMetadataBlockStart(description); !strings.Contains(description[start:], "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("issueDescription() managed metadata block is not the last marker:\n%s", description)
	}
}

// TestIssueDescriptionTruncatesOversizedEmbeddedProse verifies that very
// long Snyk DAST controlled free text (finding.Fix and each
// finding.CorrelationMarkdown block) is capped so the managed metadata
// block appended after it can never be pushed past what Linear accepts.
// Without a cap, oversized prose risks Linear silently truncating away the
// metadata block, making the ticket unmanaged and causing duplicate-ticket
// creation on the next run. The truncation marker is a stable constant and
// truncation is a pure function of the input, so building the description
// twice from the same finding is byte-identical and the compare/hash
// pipeline does not churn between runs.
func TestIssueDescriptionTruncatesOversizedEmbeddedProse(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	finding := model.Finding{
		IssueTitle: "SQL Injection",
		Severity:   "high",
		TargetName: "project-a",
		TargetID:   "target-1",
		Fix:        huge,
		CorrelationMarkdown: []string{
			huge,
			"short correlation block survives untouched",
		},
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)

	// Both oversized blocks were truncated; the short correlation block was
	// not, and none of the huge raw input found its way in whole.
	if got := strings.Count(description, truncationMarker); got != 2 {
		t.Fatalf("description has %d truncation markers, want 2 (one per oversized block):\n%s", got, description)
	}
	if !strings.Contains(description, "short correlation block survives untouched") {
		t.Fatalf("description dropped the short correlation block:\n%s", description)
	}
	if len(description) >= len(huge) {
		t.Fatalf("description length %d not bounded relative to raw input length %d", len(description), len(huge))
	}
	// Each embedded block is capped at maxEmbeddedProseRunes runes, so the
	// description stays far below Linear's overall description limit.
	if got := strings.Count(description, "x"); got > 2*maxEmbeddedProseRunes+200 {
		t.Fatalf("description embeds %d prose runes, want each block capped at %d", got, maxEmbeddedProseRunes)
	}

	// The metadata block must still be present and parseable at the end.
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block after truncation")
	}
	block := description[start:]
	if !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("metadata block missing fingerprint after truncation:\n%s", block)
	}
	if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
		t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
	}

	// Building twice from the same finding must be byte-identical, so
	// description hashes do not churn between runs.
	again := issueDescription([]string{"snyk-dast-automation"}, finding)
	if description != again {
		t.Fatalf("issueDescription() is not idempotent across builds for oversized input")
	}
}

// TestIssueDescriptionBoundsOversizedRequestContextFields verifies that the
// scan-controlled single-line fields the prose caps do not cover — Path,
// Parameter and the vulnerable URL — cannot push the managed metadata block
// past Linear's description limit. Snyk DAST returns these fields unbounded;
// without the description-body bound, an oversized path, parameter or URL
// would truncate the block away, extractFingerprint would find nothing, the
// ticket would be unmanaged, and the next run would mint a duplicate. The
// bound drops whole oversized lines — so a URL href is never shortened into a
// wrong link — and the metadata block stays last and parseable. Truncation is
// a pure function of the finding, so building the description twice is
// byte-identical and the compare/hash pipeline does not churn between runs.
func TestIssueDescriptionBoundsOversizedRequestContextFields(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	finding := model.Finding{
		IssueTitle:        "SQL Injection",
		Severity:          "high",
		TargetName:        "project-a",
		TargetID:          "target-1",
		FindingURL:        "https://example.test/vuln?" + huge,
		Path:              huge,
		Parameter:         huge,
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)

	// None of the oversized raw values reached the description whole: the
	// oversized lines were dropped by the description-body bound.
	if got := strings.Count(description, "x"); got >= len(huge) {
		t.Fatalf("description embeds %d oversized-field runes, want the description bounded below the raw input length %d:\n%s", got, len(huge), description)
	}
	if !strings.Contains(description, truncationMarker) {
		t.Fatalf("description is missing the truncation marker after the body bound dropped oversized lines:\n%s", description)
	}
	if strings.Contains(description, "Path: `"+huge) || strings.Contains(description, "Parameter: `"+huge) {
		t.Fatalf("description kept an oversized Path or Parameter line:\n%s", description)
	}

	// The metadata block must still be present and parseable at the end.
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block after bounding request-context fields")
	}
	if block := description[start:]; !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("metadata block missing fingerprint after bounding request-context fields:\n%s", block)
	}
	if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
		t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
	}

	// An update rewrites only the real (last) block, so the ticket stays
	// manageable.
	if got := upsertManagedMetadata(description, "snyk-dast:target-1:finding-1", []string{"snyk-dast-automation"}); got != strings.TrimSpace(description) {
		t.Fatalf("upsertManagedMetadata() rewrote more than the managed block:\n%s", got)
	}

	// Building twice from the same finding must be byte-identical, so
	// description hashes do not churn between runs.
	if again := issueDescription([]string{"snyk-dast-automation"}, finding); description != again {
		t.Fatalf("issueDescription() is not idempotent across builds for oversized request-context fields")
	}
}

// TestIssueDescriptionSanitizesRequestContextFields verifies that a
// line-anchored "<!-- snyk-dast-linear-sync" marker inside Path or Parameter
// (the class issue #431 hardened for Snyk-controlled prose) cannot reach the
// description either: single-line fields are flattened, so no newline can
// carry a fake marker or a fake "Fingerprint:" line onto its own line, and
// sanitized, so the "<!--" opening itself is neutralized.
func TestIssueDescriptionSanitizesRequestContextFields(t *testing.T) {
	spoof := "<!-- snyk-dast-linear-sync\nfingerprint: fake-from-path\n-->"
	finding := model.Finding{
		IssueTitle:        "SQL Injection",
		Severity:          "high",
		TargetName:        "project-a",
		TargetID:          "target-1",
		Path:              "/api/search?q=" + spoof,
		Parameter:         spoof,
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)

	if got := strings.Count(description, "<!--"); got != 1 {
		t.Fatalf("issueDescription() has %d comment openings, want only the managed metadata block:\n%s", got, description)
	}
	// No line outside the managed metadata block may start the fake marker or
	// a fake fingerprint line: findMetadataBlockStart takes the last
	// line-anchored marker, so a line-anchored copy would hijack block
	// detection even after sanitization.
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no managed block:\n%s", description)
	}
	for line := range strings.SplitSeq(description[:start], "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!-- snyk-dast-linear-sync") || strings.HasPrefix(strings.ToLower(trimmed), "fingerprint:") {
			t.Fatalf("issueDescription() let a line-anchored fake marker or fingerprint line through from a request-context field:\n%s", description)
		}
	}
	if block := description[start:]; !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("issueDescription() managed metadata block is not the last marker:\n%s", description)
	}

	// Building twice from the same finding must be byte-identical, so
	// description hashes do not churn between runs.
	if again := issueDescription([]string{"snyk-dast-automation"}, finding); description != again {
		t.Fatalf("issueDescription() is not idempotent across builds for sanitized request-context fields")
	}
}

// TestIssueDescriptionBoundsTotalEmbeddedCorrelationProse verifies that the
// Snyk Code correlation section as a whole stays within a fixed bound, not
// just each block: Snyk DAST returns one CorrelationMarkdown block per linked
// Snyk Code vulnerability without bounding how many, so many maximum-size
// blocks would otherwise embed N x maxEmbeddedProseRunes runes and push the
// managed metadata block (appended last) past Linear's description limit,
// leaving the ticket unmanaged. Only maxEmbeddedCorrelationBlocks blocks are
// embedded, the rest are named by one stable line, and the description stays a
// pure function of the finding, so its hash does not churn between runs.
func TestIssueDescriptionBoundsTotalEmbeddedCorrelationProse(t *testing.T) {
	const blockCount = 12
	blocks := make([]string, blockCount)
	for i := range blocks {
		blocks[i] = strings.Repeat("x", maxEmbeddedProseRunes)
	}
	finding := model.Finding{
		IssueTitle:          "SQL Injection",
		Severity:            "high",
		TargetName:          "project-a",
		TargetID:            "target-1",
		CorrelationMarkdown: blocks,
		Fingerprint:         "snyk-dast:target-1:finding-1",
		SnykDASTFindingID:   "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)

	// Measure only the correlation section, between its subtitle and the
	// trailing identifiers, so text from other sections cannot skew the count.
	const subtitle = "_Linked Snyk Code source vulnerability responsible for this runtime finding._"
	section := description[strings.Index(description, subtitle)+len(subtitle):]
	section = section[:strings.Index(section, "Target: `project-a`")]

	// The embedded prose is bounded by the block cap times the per-block cap,
	// however many blocks the API returned.
	fixedBound := maxEmbeddedCorrelationBlocks*maxEmbeddedProseRunes + maxEmbeddedProseRunes
	if got := strings.Count(section, "x"); got > fixedBound {
		t.Fatalf("correlation section embeds %d prose runes, want at most the fixed bound %d", got, fixedBound)
	}
	if got, want := strings.Count(section, "x"), maxEmbeddedCorrelationBlocks*maxEmbeddedProseRunes; got != want {
		t.Fatalf("correlation section embeds %d prose runes, want %d (one maximum-size block per embedded slot)", got, want)
	}

	// The blocks that were not embedded are named by one stable line.
	wantOmitted := fmt.Sprintf("_%d more linked Snyk Code vulnerabilities not shown._", blockCount-maxEmbeddedCorrelationBlocks)
	if !strings.Contains(section, wantOmitted) {
		t.Fatalf("correlation section is missing the %q summary line:\n%s", wantOmitted, section)
	}

	// The metadata block must still be last and parseable.
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block after bounding correlation prose")
	}
	if block := description[start:]; !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("metadata block missing fingerprint after bounding correlation prose:\n%s", block)
	}
	if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
		t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
	}

	// An update rewrites only the real (last) block, leaving the bounded prose
	// untouched, so the ticket stays manageable.
	if got := upsertManagedMetadata(description, "snyk-dast:target-1:finding-1", []string{"snyk-dast-automation"}); got != strings.TrimSpace(description) {
		t.Fatalf("upsertManagedMetadata() rewrote more than the managed block:\n%s", got)
	}

	// Building twice from the same finding must be byte-identical, so
	// description hashes do not churn between runs.
	if again := issueDescription([]string{"snyk-dast-automation"}, finding); description != again {
		t.Fatalf("issueDescription() is not idempotent for many correlation blocks")
	}
}

// descriptionBodyBeforeMetadata returns the rendered body above the managed
// metadata block, with the separator newlines that precede the block trimmed,
// so its rune count can be compared with maxDescriptionBodyRunes.
func descriptionBodyBeforeMetadata(t *testing.T, description string) string {
	t.Helper()
	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("description has no managed metadata block:\n%s", description)
	}
	return strings.TrimRight(description[:start], "\n")
}

// assertNoOrphanSectionHeadings fails when a "### " heading in body has no
// non-blank line under it before the next heading. The description-body bound
// must drop a section's heading together with its body rather than leave an
// empty heading in the ticket.
func assertNoOrphanSectionHeadings(t *testing.T, body string) {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "### ") {
			continue
		}
		hasBody := false
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "### ") {
				break
			}
			if strings.TrimSpace(lines[j]) != "" {
				hasBody = true
				break
			}
		}
		if !hasBody {
			t.Fatalf("section heading %q has no body under it:\n%s", line, body)
		}
	}
}

// TestIssueDescriptionRendersMaximalFixAndCorrelationBlocks verifies that the
// description-body bound is at least as large as the content its own per-block
// caps allow. One Fix at maxEmbeddedProseRunes plus maxEmbeddedCorrelationBlocks
// correlation blocks at the same cap must all render in full: the earlier
// bound was lower than those caps, so it silently dropped a whole section and
// could leave its heading behind.
func TestIssueDescriptionRendersMaximalFixAndCorrelationBlocks(t *testing.T) {
	// Each input is just over the per-block cap, so its maximal rendered form
	// is maxEmbeddedProseRunes runes plus the truncation marker. The derived
	// body bound must fit all four rendered blocks and their markers.
	capPlus := maxEmbeddedProseRunes + 1
	fixRendered := strings.Repeat("q", maxEmbeddedProseRunes) + truncationMarker
	blocksRendered := []string{
		strings.Repeat("j", maxEmbeddedProseRunes) + truncationMarker,
		strings.Repeat("k", maxEmbeddedProseRunes) + truncationMarker,
		strings.Repeat("l", maxEmbeddedProseRunes) + truncationMarker,
	}
	finding := model.Finding{
		IssueTitle: "SQL Injection",
		Severity:   "high",
		TargetName: "project-a",
		TargetID:   "target-1",
		Fix:        strings.Repeat("q", capPlus),
		CorrelationMarkdown: []string{
			strings.Repeat("j", capPlus),
			strings.Repeat("k", capPlus),
			strings.Repeat("l", capPlus),
		},
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)
	body := descriptionBodyBeforeMetadata(t, description)

	if !strings.Contains(body, "### Fix") {
		t.Fatalf("description dropped the Fix heading:\n%s", body)
	}
	if !strings.Contains(body, fixRendered) {
		t.Fatalf("description did not render the maximal Fix in full")
	}
	if !strings.Contains(body, "### Snyk Code correlation") {
		t.Fatalf("description dropped the correlation heading:\n%s", body)
	}
	for i, block := range blocksRendered {
		if !strings.Contains(body, block) {
			t.Fatalf("description did not render correlation block %d in full", i)
		}
	}
	// Four blocks were truncated by the per-block cap; the body bound must not
	// add a fifth marker or drop any of them.
	if got := strings.Count(body, truncationMarker); got != 4 {
		t.Fatalf("description has %d truncation markers, want 4 (one per maximal block):\n%s", got, body)
	}
	if got := utf8.RuneCountInString(body); got > maxDescriptionBodyRunes {
		t.Fatalf("body is %d runes, want at most the %d-rune bound", got, maxDescriptionBodyRunes)
	}
	assertNoOrphanSectionHeadings(t, body)

	// The result must stay a pure function of the finding.
	if again := issueDescription([]string{"snyk-dast-automation"}, finding); description != again {
		t.Fatalf("issueDescription() is not idempotent for maximal bounded content")
	}
}

// TestIssueDescriptionDropsSectionHeadingWithItsBody verifies that when the
// description-body bound cannot fit a section, it drops that section's heading
// together with its body instead of leaving an empty heading. A long finding
// URL that fits on its own consumes most of the budget, so the Fix and
// correlation sections no longer fit alongside it.
func TestIssueDescriptionDropsSectionHeadingWithItsBody(t *testing.T) {
	href := "https://example.test/vuln?" + strings.Repeat("q", maxDescriptionBodyRunes-2000)
	fixProse := strings.Repeat("f", maxEmbeddedProseRunes)
	finding := model.Finding{
		IssueTitle: "SQL Injection",
		Severity:   "high",
		TargetName: "project-a",
		TargetID:   "target-1",
		FindingURL: href,
		Fix:        fixProse,
		CorrelationMarkdown: []string{
			strings.Repeat("x", maxEmbeddedProseRunes),
			strings.Repeat("y", maxEmbeddedProseRunes),
			strings.Repeat("z", maxEmbeddedProseRunes),
		},
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)
	body := descriptionBodyBeforeMetadata(t, description)

	if !strings.Contains(body, href) {
		t.Fatalf("description dropped a long href that fits in the bound")
	}
	if strings.Contains(body, "### Fix") {
		t.Fatalf("description kept the Fix heading after dropping its body:\n%s", body)
	}
	if strings.Contains(body, fixProse) {
		t.Fatalf("description kept the oversized Fix body after dropping its heading")
	}
	if strings.Contains(body, "### Snyk Code correlation") {
		t.Fatalf("description kept the correlation heading after dropping its body:\n%s", body)
	}
	if !strings.Contains(body, truncationMarker) {
		t.Fatalf("description is missing the truncation marker after dropping sections:\n%s", body)
	}
	if got := utf8.RuneCountInString(body); got > maxDescriptionBodyRunes {
		t.Fatalf("body is %d runes, want at most the %d-rune bound", got, maxDescriptionBodyRunes)
	}
	assertNoOrphanSectionHeadings(t, body)

	start := findMetadataBlockStart(description)
	if start < 0 {
		t.Fatalf("findMetadataBlockStart() found no block after dropping sections")
	}
	if block := description[start:]; !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
		t.Fatalf("metadata block missing fingerprint after dropping sections:\n%s", block)
	}
	if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
		t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
	}
}

// TestIssueDescriptionKeepsLongHrefThatFits verifies that a long href which
// fits inside the body bound is rendered whole. Hrefs are never truncated — a
// shortened URL is a wrong link — so only the whole-line body bound may drop
// one, and it must not drop one that fits.
func TestIssueDescriptionKeepsLongHrefThatFits(t *testing.T) {
	href := "https://example.test/vuln?" + strings.Repeat("h", 5000)
	finding := model.Finding{
		IssueTitle:        "SQL Injection",
		Severity:          "high",
		TargetName:        "project-a",
		TargetID:          "target-1",
		FindingURL:        href,
		Fingerprint:       "snyk-dast:target-1:finding-1",
		SnykDASTFindingID: "finding-1",
	}

	description := issueDescription([]string{"snyk-dast-automation"}, finding)
	body := descriptionBodyBeforeMetadata(t, description)

	if !strings.Contains(body, "]("+href+")") {
		t.Fatalf("description did not render the long fitting href whole:\n%s", body)
	}
	if strings.Contains(body, truncationMarker) {
		t.Fatalf("description bounded away a long href that fits:\n%s", body)
	}
	if got := utf8.RuneCountInString(body); got > maxDescriptionBodyRunes {
		t.Fatalf("body is %d runes, want at most the %d-rune bound", got, maxDescriptionBodyRunes)
	}
	assertNoOrphanSectionHeadings(t, body)
}

// TestIssueDescriptionBodyBoundAndMetadataAfterOversizedFields verifies that
// the rendered body above the metadata block never exceeds
// maxDescriptionBodyRunes, and that an oversized IssueTitle, DefinitionID or
// IssueURL still leaves the managed metadata block last and parseable. The
// block is the deduplication key, so losing it mints a duplicate ticket on the
// next run.
func TestIssueDescriptionBodyBoundAndMetadataAfterOversizedFields(t *testing.T) {
	huge := strings.Repeat("x", 100000)
	base := model.Finding{
		IssueTitle:          "SQL Injection",
		Severity:            "high",
		TargetName:          "project-a",
		TargetID:            "target-1",
		Fix:                 "Patch the query.",
		CorrelationMarkdown: []string{"Linked source issue"},
		Fingerprint:         "snyk-dast:target-1:finding-1",
		SnykDASTFindingID:   "finding-1",
	}
	cases := []struct {
		name   string
		mutate func(*model.Finding)
	}{
		{"oversized IssueTitle", func(f *model.Finding) { f.IssueTitle = huge }},
		{"oversized DefinitionID", func(f *model.Finding) { f.DefinitionID = huge }},
		{"oversized IssueURL", func(f *model.Finding) { f.IssueURL = "https://example.test/finding/" + huge }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding := base
			tc.mutate(&finding)
			description := issueDescription([]string{"snyk-dast-automation"}, finding)
			body := descriptionBodyBeforeMetadata(t, description)

			if got := utf8.RuneCountInString(body); got > maxDescriptionBodyRunes {
				t.Fatalf("body is %d runes, want at most the %d-rune bound", got, maxDescriptionBodyRunes)
			}
			start := findMetadataBlockStart(description)
			if start < 0 {
				t.Fatalf("findMetadataBlockStart() found no block after an oversized field")
			}
			block := description[start:]
			if !strings.Contains(block, "fingerprint: snyk-dast:target-1:finding-1") {
				t.Fatalf("metadata block missing fingerprint after an oversized field:\n%s", block)
			}
			if !strings.HasSuffix(strings.TrimSpace(description), "-->") {
				t.Fatalf("metadata block is not the last thing in the description:\n%s", description)
			}
			assertNoOrphanSectionHeadings(t, body)

			if again := issueDescription([]string{"snyk-dast-automation"}, finding); description != again {
				t.Fatalf("issueDescription() is not idempotent for an oversized field")
			}
		})
	}
}

// TestUpsertManagedMetadataIgnoresLineAnchoredProseMarker covers the gap the
// mid-sentence tests left: embedded prose above the real block carries a
// line-anchored fake marker and a fake "Fingerprint:" line. Upsert must rewrite
// only the real (last) block, leaving the prose untouched, and must stay
// idempotent so repeated runs do not churn the description.
func TestUpsertManagedMetadataIgnoresLineAnchoredProseMarker(t *testing.T) {
	proseBlock := "<!-- snyk-dast-linear-sync\nfingerprint: fake-from-prose\n-->"
	description := "### Snyk Code correlation\n\n" + proseBlock + "\n" +
		"Fingerprint: fake-from-prose\n\n" +
		"<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\nmanaged_labels: snyk-dast-automation\n-->"

	got := upsertManagedMetadata(description, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation", "snyk-dast-api"})

	// The prose copy survives byte-for-byte: only the real block is rewritten.
	// (The pre-existing footer cleanup still drops the visible "Fingerprint:"
	// line from the body, which is unrelated to block selection.)
	if n := strings.Count(got, proseBlock); n != 1 {
		t.Fatalf("upsertManagedMetadata() left the prose block %d times, want 1 (untouched):\n%s", n, got)
	}
	// Exactly one managed block: the refreshed real block, appended once and
	// last, so extraction still finds it.
	want := "<!-- snyk-dast-linear-sync\nfingerprint: snyk-dast:target-a:finding-1\nmanaged_labels: snyk-dast-api,snyk-dast-automation\n-->"
	if n := strings.Count(got, want); n != 1 {
		t.Fatalf("upsertManagedMetadata() has %d refreshed managed blocks, want exactly 1:\n%s", n, got)
	}
	if !strings.HasSuffix(got, want) {
		t.Fatalf("upsertManagedMetadata() did not leave the real block last:\n%s", got)
	}
	if start := findMetadataBlockStart(got); got[start:] != want {
		t.Fatalf("metadata block start resolves to %q, want the real block:\n%s", got[start:], got)
	}
	if again := upsertManagedMetadata(got, "snyk-dast:target-a:finding-1", []string{"snyk-dast-automation", "snyk-dast-api"}); again != got {
		t.Fatalf("upsertManagedMetadata() is not idempotent (repeated runs churn the description):\nfirst:  %q\nsecond: %q", got, again)
	}
}

// TestIssueTitleCapsUnboundedFields verifies that a finding whose scan-API
// fields (Path, TargetHost) are long enough to exceed Linear's title limit
// still renders a title within that limit, with a truncation marker, instead
// of producing an issue-create that fails on every run.
func TestIssueTitleCapsUnboundedFields(t *testing.T) {
	// Both API-controlled fields are oversized: a multi-hundred-character
	// TargetHost as well as Path. Either alone would push the rendered title
	// past Linear's title limit.
	finding := model.Finding{
		IssueTitle:  "Reflected XSS",
		Severity:    "high",
		Fingerprint: "snyk-dast:target-a:finding-1",
		TargetHost:  "www." + strings.Repeat("h", 300) + ".example.test",
		Method:      "get",
		Path:        "/search?q=" + strings.Repeat("a", 400),
	}

	title := issueTitle(finding)

	if got := utf8.RuneCountInString(title); got > maxIssueTitleRunes {
		t.Fatalf("title length = %d runes, want <= %d", got, maxIssueTitleRunes)
	}
	if !strings.HasSuffix(title, issueTitleTruncationMarker) {
		t.Fatalf("title = %q, want truncation marker suffix", title)
	}
	if !strings.HasPrefix(title, "Snyk DAST: [high] www.") {
		t.Fatalf("title = %q, want intact prefix", title)
	}
	if strings.Contains(title, strings.Repeat("h", 300)) {
		t.Fatalf("title = %q, want host truncated", title)
	}
	if strings.Contains(title, strings.Repeat("a", 400)) {
		t.Fatalf("title = %q, want path truncated", title)
	}

	// A host long enough to matter but short enough to leave room keeps the
	// whole context and subject prefix intact; only the path tail is cut.
	finding.TargetHost = "www." + strings.Repeat("h", 100) + ".example.test"
	title = issueTitle(finding)

	if got := utf8.RuneCountInString(title); got > maxIssueTitleRunes {
		t.Fatalf("title length = %d runes, want <= %d", got, maxIssueTitleRunes)
	}
	if !strings.HasSuffix(title, issueTitleTruncationMarker) {
		t.Fatalf("title = %q, want truncation marker suffix", title)
	}
	if !strings.HasPrefix(title, "Snyk DAST: [high] www."+strings.Repeat("h", 100)+".example.test: Reflected XSS at GET /search?q=") {
		t.Fatalf("title = %q, want intact context and subject prefix", title)
	}
}

// TestIssueTitleTruncationIsStable verifies the rendered title is a pure
// function of the finding, so the compare/hash pipeline does not churn
// tickets between runs.
func TestIssueTitleTruncationIsStable(t *testing.T) {
	finding := model.Finding{
		IssueTitle:  "Reflected XSS",
		Severity:    "high",
		TargetHost:  "example.test",
		Method:      "get",
		Path:        "/" + strings.Repeat("b", 300),
		Fingerprint: "snyk-dast:target-a:finding-1",
	}

	if issueTitle(finding) != issueTitle(finding) {
		t.Fatalf("issueTitle is not deterministic for the same finding")
	}
}

// TestDesiredIssueUnboundedFieldsKeepFingerprintAndMetadata verifies that
// capping the title leaves the fingerprint and managed metadata flow
// unchanged for a finding with oversized scan-API fields.
func TestDesiredIssueUnboundedFieldsKeepFingerprintAndMetadata(t *testing.T) {
	cfg := config.Config{}
	finding := model.Finding{
		IssueTitle:  "Reflected XSS",
		Severity:    "high",
		Fingerprint: "snyk-dast:target-a:finding-1",
		TargetHost:  "www." + strings.Repeat("h", 300) + ".example.test",
		Method:      "get",
		Path:        "/download/" + strings.Repeat("c", 500),
	}

	desired := desiredIssue(cfg, finding)

	if desired.Fingerprint != "snyk-dast:target-a:finding-1" {
		t.Fatalf("Fingerprint = %q, want unchanged", desired.Fingerprint)
	}
	if got := utf8.RuneCountInString(desired.Title); got > maxIssueTitleRunes {
		t.Fatalf("Title length = %d runes, want <= %d", got, maxIssueTitleRunes)
	}
	if !strings.Contains(desired.Description, "fingerprint: snyk-dast:target-a:finding-1") {
		t.Fatalf("description missing fingerprint metadata block:\n%s", desired.Description)
	}
}

// TestTruncateForTitleNeverSplitsRunes verifies the URL cap is rune-safe: a
// multibyte URL is neither miscounted nor cut mid-rune (which would produce
// invalid UTF-8 in the title).
func TestTruncateForTitleNeverSplitsRunes(t *testing.T) {
	long := strings.Repeat("é", 100) // 100 runes, 200 bytes

	got := truncateForTitle(long)

	if got := utf8.RuneCountInString(got); got > 80 {
		t.Fatalf("truncateForTitle length = %d runes, want <= 80", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncateForTitle produced invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncateForTitle = %q, want ... suffix", got)
	}

	short := strings.Repeat("é", 40)
	if got := truncateForTitle(short); got != short {
		t.Fatalf("truncateForTitle(%q) = %q, want unchanged", short, got)
	}
}

// TestRunDefersTicketWhoseFindingHasNoTargetID: a finding whose embedded target
// summary carries no id never reaches the snapshot, so its ticket is absent
// from the "seen" set. Resolving it would close a ticket Snyk DAST still
// reports open. The ticket is matched by the finding-id part of its fingerprint
// (the finding id alone identifies nothing, but it is all the deferral gets) and
// left untouched for the run, while a genuinely missing finding still resolves.
func TestRunDefersTicketWhoseFindingHasNoTargetID(t *testing.T) {
	cfg := minimalCfg()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The one finding in Snyk DAST could not be normalized, so it is absent
	// from Findings and named in DeferredFindingIDs instead.
	snykdast := fakeSnykDAST{
		snapshot: model.SnykDASTSnapshot{
			TargetIDs:          map[string]struct{}{"target-a": {}, "target-b": {}},
			DeferredFindingIDs: []string{"7"},
		},
	}
	linear := &fakeLinear{
		snapshot: []model.ExistingIssue{
			{
				ID:          "existing-1",
				Identifier:  "SEC-1",
				Title:       "finding on project-a",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-a:7",
			},
			{
				ID:          "existing-2",
				Identifier:  "SEC-2",
				Title:       "gone finding on project-b",
				Description: "old description",
				StateName:   "Todo",
				Fingerprint: "snyk-dast:target-b:9",
			},
		},
	}

	service := New(cfg, logger, snykdast, linear, nil)
	result, err := service.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v, want success", err)
	}

	if result.DeferredTickets != 1 {
		t.Fatalf("DeferredTickets = %d, want 1", result.DeferredTickets)
	}
	if len(linear.created) != 0 {
		t.Fatalf("created = %v, want none (a deferred ticket is neither resolved nor recreated)", desiredStates(linear.created))
	}
	for _, update := range linear.updated {
		if update.Fingerprint == "snyk-dast:target-a:7" {
			t.Fatalf("the deferred ticket SEC-1 was moved to %q, want it left in Todo", update.State)
		}
	}
	if !containsDesiredState(linear.updated, model.StateDone) {
		t.Fatalf("updated states = %v, want the genuinely missing finding resolved", desiredStates(linear.updated))
	}
	if result.PlannedResolves != 1 {
		t.Fatalf("PlannedResolves = %d, want 1 (only the finding that is really gone)", result.PlannedResolves)
	}
	if result.Err() != nil {
		t.Fatalf("Err() = %v, want nil", result.Err())
	}
}

// TestRunCacheLogLine checks the "loaded sync cache" log: it reports the
// cache's age (unknown when the cache carries no usable saved_at) and it is
// logged only when the cache is actually used, not when a schema mismatch
// makes the run ignore it.
func TestRunCacheLogLine(t *testing.T) {
	// A non-empty snapshot keeps the run short (nothing to create or
	// resolve) while still exercising the cache-load path.
	findings := model.SnykDASTSnapshot{
		Findings: []model.Finding{{SnykDASTFindingID: "finding-1", TargetID: "target-a", TargetName: "project-a", IssueTitle: "finding on project-a", Severity: "high"}},
	}
	tests := []struct {
		name          string
		snapshot      cache.Snapshot
		wantSubstring string
		wantAbsent    string
	}{
		{
			name: "fresh cache logs a duration",
			snapshot: cache.Snapshot{
				SchemaSignature: managedSchemaSignature(),
				SavedAt:         time.Now().UTC().Add(-5 * time.Minute),
			},
			wantSubstring: "loaded sync cache",
			wantAbsent:    "cache_age=unknown",
		},
		{
			name: "cache without saved_at logs unknown age",
			snapshot: cache.Snapshot{
				SchemaSignature: managedSchemaSignature(),
			},
			wantSubstring: "cache_age=unknown",
		},
		{
			name: "schema mismatch skips the loaded-cache log",
			snapshot: cache.Snapshot{
				SchemaSignature: "old-signature",
				SavedAt:         time.Now().UTC().Add(-5 * time.Minute),
			},
			wantAbsent: "loaded sync cache",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&log, nil))
			snykdast := fakeSnykDAST{snapshot: findings}
			linear := &fakeLinear{}
			cacheStore := &fakeCache{snapshot: tt.snapshot}

			service := New(minimalCfg(), logger, snykdast, linear, cacheStore)
			if _, err := service.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			logged := log.String()
			if tt.wantSubstring != "" && !strings.Contains(logged, tt.wantSubstring) {
				t.Fatalf("log missing %q:\n%s", tt.wantSubstring, logged)
			}
			if tt.wantAbsent != "" && strings.Contains(logged, tt.wantAbsent) {
				t.Fatalf("log must not contain %q:\n%s", tt.wantAbsent, logged)
			}
		})
	}
}
