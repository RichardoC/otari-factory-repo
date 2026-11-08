package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/tesslio/snyk-dast-linear-sync/internal/cache"
	"github.com/tesslio/snyk-dast-linear-sync/internal/config"
	"github.com/tesslio/snyk-dast-linear-sync/internal/model"
)

type SnykDASTClient interface {
	LoadSnapshot(ctx context.Context) (model.SnykDASTSnapshot, error)
}

type LinearClient interface {
	LoadSnapshot(ctx context.Context) ([]model.ExistingIssue, error)
	// CreateIssues returns the indices of entries that failed so the caller can
	// retry only those; a non-nil error means no per-entry outcome is known.
	CreateIssues(ctx context.Context, desired []model.DesiredIssue) ([]int, error)
	UpdateIssues(ctx context.Context, updates []model.IssueUpdate) error
	// PostComments returns the indices (into updates) whose comment failed.
	PostComments(ctx context.Context, updates []model.IssueUpdate) ([]int, error)
	// ExistingFingerprints reports which of the given fingerprints already have a
	// live issue in Linear, so an ambiguous write can be reconciled before retry.
	ExistingFingerprints(ctx context.Context, fingerprints []string) (map[string]struct{}, error)
}

type CacheStore interface {
	Load(ctx context.Context) (cache.Snapshot, error)
	Save(ctx context.Context, snapshot cache.Snapshot) error
}

type Service struct {
	cfg      config.Config
	logger   *slog.Logger
	snykdast SnykDASTClient
	linear   LinearClient
	cache    CacheStore

	// executeHook, when non-nil, runs at the entry of every executeJob call
	// before any work starts, and a non-nil error stops the worker pool with
	// that error. It exists only as a test seam: it lets a test inject an
	// independent worker failure (one that is not a context cancellation) and
	// so pin the g.Wait() guard in Run. It is nil in production.
	executeHook func(job) error
}

const (
	progressLogEvery = 1000
	createBatchSize  = 10
)

var linearAutoLinkPattern = regexp.MustCompile(`\[([^\]]+)\]\((?:<)?([^)\n>]+)(?:>)?\)`)

// blankLineRunPattern matches one or more blank lines. Linear's markdown
// serializer inserts a blank line after headings (and between some blocks),
// so blank-line structure is not stable across a write/read round trip.
var blankLineRunPattern = regexp.MustCompile(`\n(?:[ \t]*\n)+`)

var markdownEscapePattern = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!~])`)

type RunResult struct {
	Findings            int
	ExistingIssues      int
	Conflicts           int
	PlannedCreates      int64
	PlannedUpdates      int64
	PlannedResolves     int64
	CancelledDuplicates int64
	// FailedOps counts state-changing operations that did not apply: creates,
	// updates, resolves and duplicate cancellations.
	FailedOps int64
	// DeferredTickets counts managed tickets held back for this run because
	// their finding could not be normalized (the Snyk DAST finding carries no
	// target id). They are neither resolved nor recreated: the finding is
	// still reported by Snyk DAST, so closing the ticket would record a false
	// resolution.
	DeferredTickets int64
	// FailedComments counts change comments that could not be posted. These are
	// tracked separately from FailedOps because a missing comment leaves the
	// synced issue state correct — the comment is explanatory, and the feature is
	// opt-in via LINEAR_COMMENTS — so it should not read as a failed sync
	// operation. It is counted rather than only logged so a run cannot report
	// success while silently dropping comments.
	FailedComments int64
}

// Err reports whether the run left state-changing work permanently unapplied,
// so the caller can exit non-zero. A run that logs a failure but reports success
// is worse than a noisy one: nothing downstream notices the drift, and the next
// scheduled run reports success again.
//
// Only permanent failures count. FailedOps is incremented after the per-entry
// retry has itself failed (or, for an ambiguous batch create, after the
// reconciliation query failed and the entries were deliberately left for the
// next run), so a batch that fails and then succeeds on retry is a clean run and
// returns nil here.
//
// FailedComments is deliberately excluded. A missing change comment leaves the
// synced issue state correct — the comment is explanatory, and the feature is
// opt-in via LINEAR_COMMENTS — so it is logged and counted but does not fail the
// run.
func (r RunResult) Err() error {
	if r.FailedOps > 0 {
		return fmt.Errorf("%d Linear operation(s) could not be applied; see the error log for details", r.FailedOps)
	}
	return nil
}

func New(cfg config.Config, logger *slog.Logger, snykdast SnykDASTClient, linear LinearClient, cacheStore CacheStore) *Service {
	return &Service{
		cfg:      cfg,
		logger:   logger,
		snykdast: snykdast,
		linear:   linear,
		cache:    cacheStore,
	}
}

// Run loads both snapshots, plans the sync and executes it with a pool of
// workers. Its contract on interruption: when ctx is cancelled (a signal, a
// deadline, a caller's cancellation), the run stops starting new jobs, returns
// an error wrapping the context error, and the cache is left untouched — so
// the next run re-examines everything against Linear's live state instead of
// trusting a cache that describes work that was never written.
func (s *Service) Run(ctx context.Context) (RunResult, error) {
	runCtx := ctx
	var (
		snykdastSnapshot model.SnykDASTSnapshot
		findings         []model.Finding
		existingIssues   []model.ExistingIssue
	)
	cacheEnabled := s.cache != nil && !s.cfg.Cache.BypassCache
	cacheSignature := managedSchemaSignature()
	cacheSnapshot := cache.Snapshot{
		SnykDASTHashes: map[string]string{},
		LinearHashes:   map[string]string{},
	}

	if s.cfg.Cache.BypassCache {
		s.logger.Info("bypassing sync cache for this run")
	} else if s.cache != nil {
		loaded, err := s.cache.Load(ctx)
		if err != nil {
			return RunResult{}, err
		}
		s.logger.Info("loaded sync cache", slog.Time("saved_at", loaded.SavedAt))
		if loaded.SchemaSignature != "" && loaded.SchemaSignature != cacheSignature {
			cacheEnabled = false
			s.logger.Info("ignoring sync cache because managed schema changed",
				slog.String("cached_signature", loaded.SchemaSignature),
				slog.String("current_signature", cacheSignature),
			)
		} else {
			cacheSnapshot = loaded
		}
	}

	s.logger.Info("loading Snyk DAST findings and Linear snapshot")
	loadGroup, loadCtx := errgroup.WithContext(ctx)
	loadGroup.Go(func() error {
		var err error
		snykdastSnapshot, err = s.snykdast.LoadSnapshot(loadCtx)
		if err != nil {
			return err
		}
		findings = snykdastSnapshot.Findings
		return err
	})
	loadGroup.Go(func() error {
		var err error
		existingIssues, err = s.linear.LoadSnapshot(loadCtx)
		return err
	})
	if err := loadGroup.Wait(); err != nil {
		return RunResult{}, err
	}
	s.logger.Info("loaded source data",
		slog.Int("findings", len(findings)),
		slog.Int("existing_issues", len(existingIssues)),
	)

	existingByFingerprint := map[string]model.ExistingIssue{}
	var duplicatesToCancel []model.ExistingIssue
	for _, issue := range existingIssues {
		if issue.Fingerprint != "" {
			if prior, exists := existingByFingerprint[issue.Fingerprint]; exists {
				canonical, duplicate := preferCanonicalDuplicate(prior, issue, s.cfg.Linear.States)
				s.logger.Warn("duplicate fingerprint found on Linear issues, will cancel the non-canonical copy",
					slog.String("fingerprint", issue.Fingerprint),
					slog.String("canonical", canonical.Identifier),
					slog.String("duplicate", duplicate.Identifier),
				)
				existingByFingerprint[issue.Fingerprint] = canonical
				duplicatesToCancel = append(duplicatesToCancel, duplicate)
				continue
			}
			existingByFingerprint[issue.Fingerprint] = issue
		}
	}

	// An empty Snyk DAST snapshot is not evidence that every finding went
	// away. The documented symptom of an API key without the view_target
	// permission — and of a SNYK_DAST_TEAM filter that matches no targets, or
	// of an outage that returns success with no results — is a silently empty
	// target and finding list. Resolving on such a snapshot would cancel every
	// managed ticket in one run, so refuse instead: the only safe reading of
	// "no targets at all" alongside existing managed issues is that the
	// snapshot is not a truthful source right now. A brand-new deployment with
	// no managed issues yet is unaffected. This is deliberately not a
	// percentage threshold: a partial listing is a separate concern.
	if len(snykdastSnapshot.TargetIDs) == 0 && len(existingByFingerprint) > 0 {
		message := "the Snyk DAST snapshot contains no targets while managed Linear issues exist; " +
			"this is the documented symptom of an API key without the view_target permission " +
			"(target and finding queries silently return empty results), or of " +
			"SNYK_DAST_TEAM matching no targets; " +
			"grant view_target to the API key, or fix SNYK_DAST_TEAM, before running the sync again"
		if s.cfg.DryRun {
			// A dry run must still show the plan it would have executed, but
			// the operator has to see that a live run would refuse.
			s.logger.Error(message)
		} else {
			return RunResult{}, fmt.Errorf("%s", message)
		}
	}

	// deferredFindingIDs are the findings the loader skipped because their
	// embedded target summary carried no id. Such a finding has no fingerprint
	// of its own, so its existing tickets cannot be matched by full
	// fingerprint; they are matched by the finding-id part instead. Treating
	// their tickets as resolved would close live tickets on incomplete source
	// data, so they are held back for this run — not resolved, not recreated.
	deferredFindingIDs := make(map[string]struct{}, len(snykdastSnapshot.DeferredFindingIDs))
	for _, findingID := range snykdastSnapshot.DeferredFindingIDs {
		deferredFindingIDs[findingID] = struct{}{}
	}

	desiredByFingerprint := make(map[string]model.DesiredIssue, len(findings))
	snykdastHashes := make(map[string]string, len(findings))
	for _, finding := range findings {
		desired := desiredIssue(s.cfg, finding)

		if existing, ok := existingByFingerprint[finding.Fingerprint]; ok {
			// Respect manual Backlog override: if a user moved an open ticket from
			// Todo to Backlog, don't move it back on subsequent syncs.
			if desired.State == model.StateTodo && isConfiguredBacklogState(existing.StateName, s.cfg.Linear.States.Backlog) {
				desired.State = model.StateBacklog
			}
			// Respect manual non-terminal state override: when both the desired
			// model state and the existing Linear state are non-terminal, preserve
			// the user's chosen Linear state. This prevents the sync from dragging
			// an issue back to the configured open state (e.g. "Triage") when a
			// user has manually moved it to "Todo", "In Progress", or any other
			// non-terminal state. It also handles the case where the existing
			// state already matches the configured state, avoiding false-positive
			// state-change detection due to model state names ("todo") differing
			// from configured Linear state names ("Triage").
			if isNonTerminalModelState(desired.State) && isNonTerminalLinearState(existing.StateName, s.cfg.Linear.States) && existing.ArchivedAt == nil {
				desired.PreserveState = true
			}
		}

		desiredByFingerprint[finding.Fingerprint] = desired
		snykdastHashes[finding.Fingerprint] = desiredIssueHash(desired)
	}

	currentLinearHashes := make(map[string]string, len(existingByFingerprint))
	for fingerprint, issue := range existingByFingerprint {
		currentLinearHashes[fingerprint] = existingIssueHash(issue)
	}

	jobs := make(chan job)
	var result RunResult
	var failedWrites failedFingerprints
	result.Findings = len(findings)
	result.ExistingIssues = len(existingIssues)
	result.Conflicts = len(duplicatesToCancel)
	result.DeferredTickets = deferredTicketCount(existingByFingerprint, deferredFindingIDs)
	var queuedJobs int64

	g, workerCtx := errgroup.WithContext(runCtx)
	for i := 0; i < s.cfg.Sync.Workers; i++ {
		g.Go(func() error {
			for job := range jobs {
				if err := s.executeJob(workerCtx, job, &result, &failedWrites); err != nil {
					return err
				}
			}
			return nil
		})
	}

	g.Go(func() error {
		defer close(jobs)

		seen := make(map[string]struct{}, len(desiredByFingerprint))
		createBatch := make([]model.DesiredIssue, 0, createBatchSize)
		updateBatch := make([]model.IssueUpdate, 0, createBatchSize)
		for fingerprint, desired := range desiredByFingerprint {
			seen[fingerprint] = struct{}{}
			existing, ok := existingByFingerprint[fingerprint]
			if !ok {
				createBatch = append(createBatch, desired)
				if len(createBatch) == createBatchSize {
					if err := s.enqueue(workerCtx, jobs, job{kind: jobCreateBatch, desiredBatch: append([]model.DesiredIssue(nil), createBatch...)}); err != nil {
						return err
					}
					s.logQueueProgress(&queuedJobs, int64(len(createBatch)))
					createBatch = createBatch[:0]
				}
				continue
			}
			// If the finding is still closed on the Snyk DAST side, the archived
			// ticket already records that and there is nothing to do — this is
			// what stops the sync from minting a fresh duplicate for every
			// archived ticket on every run.
			//
			// If the finding has come back (desired state is open again), this
			// creates a replacement ticket rather than reopening the archived
			// one. That is a deliberate choice, not an API limitation: Linear
			// does expose issueUnarchive(id), which has no documented
			// precondition and would let the original ticket be restored and
			// updated, preserving its history and comments. Switching to
			// unarchive-then-update is a viable alternative if keeping one
			// ticket per recurring finding is preferred over a fresh ticket per
			// recurrence.
			if existing.ArchivedAt != nil {
				if isNonTerminalModelState(desired.State) {
					s.logger.Info("archived Linear issue cannot be reopened, creating a replacement",
						slog.String("issue", existing.Identifier),
						slog.String("fingerprint", fingerprint),
						slog.String("desired_state", string(desired.State)),
					)
					createBatch = append(createBatch, desired)
					if len(createBatch) == createBatchSize {
						if err := s.enqueue(workerCtx, jobs, job{kind: jobCreateBatch, desiredBatch: append([]model.DesiredIssue(nil), createBatch...)}); err != nil {
							return err
						}
						s.logQueueProgress(&queuedJobs, int64(len(createBatch)))
						createBatch = createBatch[:0]
					}
				}
				continue
			}
			// The cache fast-path may not suppress a pending move into a terminal
			// state: a finding that became fixed/ignored (desired Done/Cancelled)
			// while its ticket sat in an open column must still be closed, even
			// if its source/Linear hashes are unchanged since the last run. Benign
			// open-state divergences stay cache-suppressed as before.
			if cacheEnabled && cacheSnapshot.SnykDASTHashes[fingerprint] == snykdastHashes[fingerprint] && cacheSnapshot.LinearHashes[fingerprint] == currentLinearHashes[fingerprint] && !pendingTerminalTransition(existing, desired) {
				continue
			}
			if needsUpdate(existing, desired) {
				update := model.IssueUpdate{Existing: existing, Desired: desired}
				update.Diff = ComputeDiff(existing, desired)
				updateBatch = append(updateBatch, update)
				if len(updateBatch) == createBatchSize {
					if err := s.enqueue(workerCtx, jobs, job{kind: jobUpdate, updateBatch: append([]model.IssueUpdate(nil), updateBatch...)}); err != nil {
						return err
					}
					s.logQueueProgress(&queuedJobs, int64(len(updateBatch)))
					updateBatch = updateBatch[:0]
				}
			}
		}
		if len(createBatch) > 0 {
			if err := s.enqueue(workerCtx, jobs, job{kind: jobCreateBatch, desiredBatch: append([]model.DesiredIssue(nil), createBatch...)}); err != nil {
				return err
			}
			s.logQueueProgress(&queuedJobs, int64(len(createBatch)))
		}
		if len(updateBatch) > 0 {
			if err := s.enqueue(workerCtx, jobs, job{kind: jobUpdate, updateBatch: append([]model.IssueUpdate(nil), updateBatch...)}); err != nil {
				return err
			}
			s.logQueueProgress(&queuedJobs, int64(len(updateBatch)))
		}

		resolveBatch := make([]model.IssueUpdate, 0, createBatchSize)
		for fingerprint, existing := range existingByFingerprint {
			if _, ok := seen[fingerprint]; ok {
				continue
			}
			// Archived issues are treated as terminal and are not mutated in
			// place; restoring one would require issueUnarchive first.
			if existing.ArchivedAt != nil {
				continue
			}
			// The finding behind this ticket was skipped during the load (its
			// embedded target had no id), so its absence from the snapshot is
			// not evidence that it is gone. Leave the ticket alone this run.
			if findingID, ok := FingerprintFindingID(existing.Fingerprint); ok {
				if _, deferred := deferredFindingIDs[findingID]; deferred {
					s.logger.Warn("deferring resolve of a ticket whose finding could not be normalized (embedded target has no id)",
						slog.String("issue", existing.Identifier),
						slog.String("fingerprint", existing.Fingerprint),
						slog.String("finding_id", findingID))
					continue
				}
			}
			desiredState, stateReason := missingFindingState(existing.Fingerprint, snykdastSnapshot.TargetIDs, snykdastSnapshot.InactiveTargetIDs)
			resolved := model.DesiredIssue{
				Fingerprint:   existing.Fingerprint,
				Title:         existing.Title,
				Description:   upsertManagedMetadata(existing.Description, existing.Fingerprint, existing.ManagedLabels),
				DueDate:       existing.DueDate,
				State:         desiredState,
				StateReason:   stateReason,
				ManagedLabels: existing.ManagedLabels,
				Priority:      existing.Priority,
			}
			if needsUpdate(existing, resolved) {
				resolvedUpdate := model.IssueUpdate{Existing: existing, Desired: resolved}
				resolvedUpdate.Diff = ComputeDiff(existing, resolved)
				resolveBatch = append(resolveBatch, resolvedUpdate)
				if len(resolveBatch) == createBatchSize {
					if err := s.enqueue(workerCtx, jobs, job{kind: jobResolve, updateBatch: append([]model.IssueUpdate(nil), resolveBatch...)}); err != nil {
						return err
					}
					s.logQueueProgress(&queuedJobs, int64(len(resolveBatch)))
					resolveBatch = resolveBatch[:0]
				}
			}
		}
		if len(resolveBatch) > 0 {
			if err := s.enqueue(workerCtx, jobs, job{kind: jobResolve, updateBatch: append([]model.IssueUpdate(nil), resolveBatch...)}); err != nil {
				return err
			}
			s.logQueueProgress(&queuedJobs, int64(len(resolveBatch)))
		}

		cancelBatch := make([]model.IssueUpdate, 0, createBatchSize)
		for _, duplicate := range duplicatesToCancel {
			// An archived duplicate is already closed and immutable.
			if duplicate.ArchivedAt != nil {
				continue
			}
			desired := model.DesiredIssue{
				Fingerprint:   duplicate.Fingerprint,
				Title:         duplicate.Title,
				Description:   duplicate.Description,
				DueDate:       duplicate.DueDate,
				State:         model.StateCancelled,
				StateReason:   "duplicate of another managed issue",
				ManagedLabels: duplicate.ManagedLabels,
				Priority:      duplicate.Priority,
			}
			if needsUpdate(duplicate, desired) {
				cancelUpdate := model.IssueUpdate{Existing: duplicate, Desired: desired}
				cancelUpdate.Diff = ComputeDiff(duplicate, desired)
				cancelBatch = append(cancelBatch, cancelUpdate)
				if len(cancelBatch) == createBatchSize {
					if err := s.enqueue(workerCtx, jobs, job{kind: jobCancelDuplicate, updateBatch: append([]model.IssueUpdate(nil), cancelBatch...)}); err != nil {
						return err
					}
					s.logQueueProgress(&queuedJobs, int64(len(cancelBatch)))
					cancelBatch = cancelBatch[:0]
				}
			}
		}
		if len(cancelBatch) > 0 {
			if err := s.enqueue(workerCtx, jobs, job{kind: jobCancelDuplicate, updateBatch: append([]model.IssueUpdate(nil), cancelBatch...)}); err != nil {
				return err
			}
			s.logQueueProgress(&queuedJobs, int64(len(cancelBatch)))
		}

		return nil
	})

	waitErr := g.Wait()
	// An interrupted run has written only part of its plan: the findings whose
	// jobs never ran look reconciled in a refreshed cache simply because their
	// ticket was untouched. Returning before the cache save below is what keeps
	// such a run from hiding them from the next one.
	//
	// g.Wait() alone is not enough to notice an interruption. When the context
	// was already cancelled before Run started, or the cancellation landed
	// while no job was in flight, errgroup never sees an error and returns nil,
	// so the wait error is checked alongside the run context's own error.
	interruptionErr := waitErr
	if interruptionErr == nil {
		interruptionErr = runCtx.Err()
	}
	if errors.Is(interruptionErr, context.Canceled) || errors.Is(interruptionErr, context.DeadlineExceeded) {
		return result, fmt.Errorf("sync interrupted before all work was written: %w", interruptionErr)
	}
	if waitErr != nil {
		return result, waitErr
	}

	if !s.cfg.DryRun && s.cache != nil {
		// Refresh the cache even if some Linear operations failed. Snyk DAST data
		// is still valid and should be cached so the next run does not have to
		// re-fetch everything. Linear hashes are taken from a fresh snapshot when
		// possible; if the reload fails we fall back to the hashes from the
		// initial load. A fingerprint whose write failed this run is left out of
		// both maps, so the next run compares it against live data again and
		// retries the write instead of skipping it on the cache fast-path.
		cacheLinearHashes := currentLinearHashes
		if result.PlannedCreates > 0 || result.PlannedUpdates > 0 || result.PlannedResolves > 0 {
			refreshedIssues, err := s.linear.LoadSnapshot(runCtx)
			if err != nil {
				s.logger.Warn("failed to refresh Linear snapshot, using current hashes for cache",
					"error", err,
				)
			} else {
				cacheLinearHashes = linearHashesByFingerprint(refreshedIssues)
			}
		}
		uncachedFailedCount := 0
		for fingerprint := range snykdastHashes {
			if failedWrites.has(fingerprint) {
				delete(snykdastHashes, fingerprint)
				delete(cacheLinearHashes, fingerprint)
				uncachedFailedCount++
			}
		}
		// A failed resolve or duplicate cancel can also leave a Linear hash
		// behind for a fingerprint that has no Snyk hash this run.
		for fingerprint := range cacheLinearHashes {
			if failedWrites.has(fingerprint) {
				delete(cacheLinearHashes, fingerprint)
				uncachedFailedCount++
			}
		}
		nextSnapshot := cache.Snapshot{
			SavedAt:         time.Now().UTC(),
			SchemaSignature: cacheSignature,
			SnykDASTHashes:  snykdastHashes,
			LinearHashes:    cacheLinearHashes,
		}
		if err := s.cache.Save(runCtx, nextSnapshot); err != nil {
			return result, err
		}
		s.logger.Info("refreshed sync cache",
			slog.Time("saved_at", nextSnapshot.SavedAt),
			slog.Int("snyk_dast_rows", len(nextSnapshot.SnykDASTHashes)),
			slog.Int("linear_rows", len(nextSnapshot.LinearHashes)),
			slog.Int("uncached_failed", uncachedFailedCount),
			slog.Int64("failed_ops", result.FailedOps),
		)
	}

	return result, nil
}

type jobKind string

const (
	jobCreateBatch     jobKind = "create"
	jobUpdate          jobKind = "update"
	jobResolve         jobKind = "resolve"
	jobCancelDuplicate jobKind = "cancel-duplicate"
)

type job struct {
	kind         jobKind
	desiredBatch []model.DesiredIssue
	updateBatch  []model.IssueUpdate
}

// failedFingerprints collects the fingerprints of the items whose Linear write
// failed during a run. It is written from the worker goroutines and read once
// the pool has finished, hence the mutex. Fingerprints whose write failed are
// excluded from the refreshed cache so the next run compares them against live
// data and retries them instead of being skipped by the cache fast-path.
type failedFingerprints struct {
	mu  sync.Mutex
	set map[string]struct{}
}

func (f *failedFingerprints) add(fps ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.set == nil {
		f.set = make(map[string]struct{}, len(fps))
	}
	for _, fp := range fps {
		f.set[fp] = struct{}{}
	}
}

func (f *failedFingerprints) has(fp string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.set[fp]
	return ok
}

// createOne retries a single issue create. A per-entry failure reported through
// the failed-index return is treated exactly like an error: the issue does not
// exist either way, so the run must not count it as done. Ignoring the indices
// here would let a failed retry pass silently and the run report success for
// work that never landed.
func (s *Service) createOne(ctx context.Context, desired model.DesiredIssue) error {
	failed, err := s.linear.CreateIssues(ctx, []model.DesiredIssue{desired})
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("Linear reported the create as failed without returning an error")
	}
	return nil
}

// commentOne retries a single change comment, with the same reasoning as
// createOne.
func (s *Service) commentOne(ctx context.Context, update model.IssueUpdate) error {
	failed, err := s.linear.PostComments(ctx, []model.IssueUpdate{update})
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("Linear reported the comment as failed without returning an error")
	}
	return nil
}

// retryUnlandedCreates reconciles an ambiguous batch-create failure. It asks
// Linear which of the batch's fingerprints now have a live issue and re-creates
// only the rest, so entries that were committed before the response was lost are
// not duplicated.
//
// If the reconciliation query itself fails there is no safe way to tell what
// landed, so nothing is retried: the entries are counted as failed and the next
// run creates whatever is genuinely missing, since the finding is still reported
// by Snyk DAST and will be absent from the next snapshot. Missing a ticket for
// one cycle is recoverable; a duplicate ticket is not automatically undone until
// the duplicate-cancellation pass sees both copies.
func (s *Service) retryUnlandedCreates(ctx context.Context, batch []model.DesiredIssue, result *RunResult, failed *failedFingerprints) {
	fingerprints := make([]string, 0, len(batch))
	for _, desired := range batch {
		fingerprints = append(fingerprints, desired.Fingerprint)
	}

	landed, err := s.linear.ExistingFingerprints(ctx, fingerprints)
	if err != nil {
		atomic.AddInt64(&result.FailedOps, int64(len(batch)))
		// No safe way to tell what landed, so every entry is treated as failed
		// and kept out of the cache: the next run re-creates whatever is
		// genuinely missing.
		failed.add(fingerprints...)
		s.logger.Error("could not reconcile an ambiguous batch create against Linear; not retrying, the next run will create whatever is missing",
			slog.Int("batch_size", len(batch)),
			slog.Any("error", err),
		)
		return
	}

	for _, desired := range batch {
		if _, ok := landed[desired.Fingerprint]; ok {
			s.logger.Info("issue was created despite the batch error, not retrying",
				slog.String("fingerprint", desired.Fingerprint),
			)
			continue
		}
		if err := s.createOne(ctx, desired); err != nil {
			atomic.AddInt64(&result.FailedOps, 1)
			failed.add(desired.Fingerprint)
			s.logger.Error("failed to create issue",
				slog.String("fingerprint", desired.Fingerprint),
				slog.Any("error", err),
			)
		}
	}
}

// inRange guards indexing by a failed-index list. The Linear client derives the
// indices from the slice it was handed, so they are always valid in practice,
// but LinearClient is an interface and an out-of-range index would panic the
// worker rather than fail one entry.
func inRange(idx, length int) bool {
	return idx >= 0 && idx < length
}

// enqueue hands a job to the worker pool and reports the reason it could not
// be queued. The context is the workers' own: when a worker fails, errgroup
// cancels it and every worker returns. A plain send on the unbuffered jobs
// channel would then block forever — nobody is left to receive — and Run
// would never return. Waiting on the context instead lets the producer stop,
// close the channel and unwind.
func (s *Service) enqueue(ctx context.Context, jobs chan<- job, j job) error {
	select {
	case jobs <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) executeJob(ctx context.Context, job job, result *RunResult, failed *failedFingerprints) error {
	// The test seam runs first, before any counter or client call, so an
	// injected failure behaves exactly like a worker error that stopped the
	// pool: the job leaves no trace and Run must notice it through g.Wait().
	// Nil in production.
	if s.executeHook != nil {
		if err := s.executeHook(job); err != nil {
			return err
		}
	}
	// A cancelled context means the run is over: touch no counter and make no
	// Linear call — every one of them would fail, and each item of the batch
	// would be counted in FailedOps as if the write had been attempted.
	if err := ctx.Err(); err != nil {
		return err
	}
	switch job.kind {
	case jobCreateBatch:
		creates := atomic.AddInt64(&result.PlannedCreates, int64(len(job.desiredBatch)))
		s.logExecutionProgress("create", creates)
		if s.cfg.Verbose {
			for _, desired := range job.desiredBatch {
				s.logger.Info("planned create",
					slog.String("fingerprint", desired.Fingerprint),
					slog.String("title", desired.Title),
					slog.String("state", string(desired.State)),
					slog.String("due_date", desired.DueDate),
					slog.Int("priority", desired.Priority),
					slog.String("labels", strings.Join(desired.ManagedLabels, ",")),
					slog.String("description", desired.Description),
				)
			}
		}
		if s.cfg.DryRun {
			return nil
		}
		failedIdx, err := s.linear.CreateIssues(ctx, job.desiredBatch)
		switch {
		case err != nil:
			// The outcome is ambiguous: a transport-level failure does not prove
			// Linear rejected the mutation, because it may have been committed
			// before the response was lost. Retrying blindly would duplicate every
			// entry that actually landed, so ask Linear what exists first and
			// retry only what is genuinely missing.
			s.logger.Warn("batch create failed with an ambiguous outcome, reconciling against Linear before retry",
				slog.Int("batch_size", len(job.desiredBatch)),
				slog.Any("error", err),
			)
			s.retryUnlandedCreates(ctx, job.desiredBatch, result, failed)
		case len(failedIdx) > 0:
			// Partial failure: only the reported entries still need creating.
			// Retrying the whole batch would duplicate the ones that succeeded.
			s.logger.Warn("batch create partially failed, retrying only the failed issues",
				slog.Int("failed_count", len(failedIdx)),
				slog.Int("batch_size", len(job.desiredBatch)),
			)
			for _, idx := range failedIdx {
				if !inRange(idx, len(job.desiredBatch)) {
					atomic.AddInt64(&result.FailedOps, 1)
					s.logger.Error("Linear client reported an out-of-range failed create index",
						slog.Int("index", idx),
						slog.Int("batch_size", len(job.desiredBatch)),
					)
					continue
				}
				desired := job.desiredBatch[idx]
				if err := s.createOne(ctx, desired); err != nil {
					atomic.AddInt64(&result.FailedOps, 1)
					failed.add(desired.Fingerprint)
					s.logger.Error("failed to create issue",
						slog.String("fingerprint", desired.Fingerprint),
						slog.Any("error", err),
					)
				}
			}
		}
		// Once the context is done the batch call and its fallback fail fast
		// (their failures are counted, but an interrupted run's counts and
		// cache are never saved). Stop the worker so no further job starts.
		return ctx.Err()
	case jobUpdate:
		updates := atomic.AddInt64(&result.PlannedUpdates, int64(len(job.updateBatch)))
		s.logExecutionProgress("update", updates)
		if s.cfg.Verbose {
			for _, update := range job.updateBatch {
				s.logger.Info("planned update",
					slog.String("issue", update.Existing.Identifier),
					slog.String("fingerprint", update.Desired.Fingerprint),
					slog.String("title", update.Desired.Title),
					slog.String("state", string(update.Desired.State)),
					slog.String("due_date", update.Desired.DueDate),
					slog.Int("priority", update.Desired.Priority),
					slog.String("labels", strings.Join(update.Desired.ManagedLabels, ",")),
				)
			}
		}
		if s.cfg.DryRun {
			return nil
		}
		if err := s.linear.UpdateIssues(ctx, job.updateBatch); err != nil {
			s.logger.Warn("batch update failed, retrying issues individually",
				slog.Int("batch_size", len(job.updateBatch)),
				slog.Any("error", err),
			)
			for _, update := range job.updateBatch {
				if err := s.linear.UpdateIssues(ctx, []model.IssueUpdate{update}); err != nil {
					atomic.AddInt64(&result.FailedOps, 1)
					// Record both fingerprints: they differ when a ticket's
					// fingerprint was rewritten, and the cache is keyed by the
					// desired fingerprint at write time and by the ticket's own
					// fingerprint on later runs.
					failed.add(update.Desired.Fingerprint, update.Existing.Fingerprint)
					s.logger.Error("failed to update issue",
						slog.String("issue", update.Existing.Identifier),
						slog.String("fingerprint", update.Desired.Fingerprint),
						slog.Any("error", err),
					)
				}
			}
		} else if s.cfg.Linear.CommentsEnabled {
			failedIdx, err := s.linear.PostComments(ctx, job.updateBatch)
			switch {
			case err != nil:
				// Ambiguous outcome, as above — but a comment carries no
				// fingerprint to reconcile against, and a duplicated change
				// comment is more disruptive than a missing one (the issue state
				// itself is already correct). So these are counted and not
				// retried.
				atomic.AddInt64(&result.FailedComments, int64(len(job.updateBatch)))
				s.logger.Warn("batch comment post failed with an ambiguous outcome, not retrying to avoid duplicate comments",
					slog.Int("batch_size", len(job.updateBatch)),
					slog.Any("error", err),
				)
			case len(failedIdx) > 0:
				// Retry only the comments that failed, so the ones that already
				// posted are not duplicated on the issue.
				s.logger.Warn("batch comment post partially failed, retrying only the failed comments",
					slog.Int("failed_count", len(failedIdx)),
					slog.Int("batch_size", len(job.updateBatch)),
				)
				for _, idx := range failedIdx {
					if !inRange(idx, len(job.updateBatch)) {
						atomic.AddInt64(&result.FailedComments, 1)
						s.logger.Warn("Linear client reported an out-of-range failed comment index",
							slog.Int("index", idx),
							slog.Int("batch_size", len(job.updateBatch)),
						)
						continue
					}
					update := job.updateBatch[idx]
					if err := s.commentOne(ctx, update); err != nil {
						atomic.AddInt64(&result.FailedComments, 1)
						s.logger.Warn("failed to post change comment",
							slog.String("issue", update.Existing.Identifier),
							slog.String("fingerprint", update.Desired.Fingerprint),
							slog.Any("error", err),
						)
					}
				}
			}
		}
		// Once the context is done the batch call and its fallback fail fast
		// (their failures are counted, but an interrupted run's counts and
		// cache are never saved). Stop the worker so no further job starts.
		return ctx.Err()
	case jobResolve:
		resolves := atomic.AddInt64(&result.PlannedResolves, int64(len(job.updateBatch)))
		s.logExecutionProgress("resolve", resolves)
		if s.cfg.Verbose {
			for _, update := range job.updateBatch {
				s.logger.Info("planned resolve",
					slog.String("issue", update.Existing.Identifier),
					slog.String("fingerprint", update.Desired.Fingerprint),
					slog.String("state", string(update.Desired.State)),
					slog.String("reason", update.Desired.StateReason),
				)
			}
		}
		if s.cfg.DryRun {
			return nil
		}
		if err := s.linear.UpdateIssues(ctx, job.updateBatch); err != nil {
			s.logger.Warn("batch resolve failed, retrying issues individually",
				slog.Int("batch_size", len(job.updateBatch)),
				slog.Any("error", err),
			)
			for _, update := range job.updateBatch {
				if err := s.linear.UpdateIssues(ctx, []model.IssueUpdate{update}); err != nil {
					atomic.AddInt64(&result.FailedOps, 1)
					// Record both fingerprints: they differ when a ticket's
					// fingerprint was rewritten, and the cache is keyed by the
					// desired fingerprint at write time and by the ticket's own
					// fingerprint on later runs.
					failed.add(update.Desired.Fingerprint, update.Existing.Fingerprint)
					s.logger.Error("failed to resolve issue",
						slog.String("issue", update.Existing.Identifier),
						slog.String("fingerprint", update.Desired.Fingerprint),
						slog.Any("error", err),
					)
				}
			}
		}
		// Once the context is done the batch call and its fallback fail fast
		// (their failures are counted, but an interrupted run's counts and
		// cache are never saved). Stop the worker so no further job starts.
		return ctx.Err()
	case jobCancelDuplicate:
		cancels := atomic.AddInt64(&result.CancelledDuplicates, int64(len(job.updateBatch)))
		s.logExecutionProgress("cancel-duplicate", cancels)
		if s.cfg.Verbose {
			for _, update := range job.updateBatch {
				s.logger.Info("planned cancel duplicate",
					slog.String("issue", update.Existing.Identifier),
					slog.String("fingerprint", update.Desired.Fingerprint),
				)
			}
		}
		if s.cfg.DryRun {
			return nil
		}
		if err := s.linear.UpdateIssues(ctx, job.updateBatch); err != nil {
			s.logger.Warn("batch cancel-duplicate failed, retrying issues individually",
				slog.Int("batch_size", len(job.updateBatch)),
				slog.Any("error", err),
			)
			for _, update := range job.updateBatch {
				if err := s.linear.UpdateIssues(ctx, []model.IssueUpdate{update}); err != nil {
					atomic.AddInt64(&result.FailedOps, 1)
					// Record both fingerprints: they differ when a ticket's
					// fingerprint was rewritten, and the cache is keyed by the
					// desired fingerprint at write time and by the ticket's own
					// fingerprint on later runs.
					failed.add(update.Desired.Fingerprint, update.Existing.Fingerprint)
					s.logger.Error("failed to cancel duplicate issue",
						slog.String("issue", update.Existing.Identifier),
						slog.String("fingerprint", update.Desired.Fingerprint),
						slog.Any("error", err),
					)
				}
			}
		}
		// Once the context is done the batch call and its fallback fail fast
		// (their failures are counted, but an interrupted run's counts and
		// cache are never saved). Stop the worker so no further job starts.
		return ctx.Err()
	default:
		return fmt.Errorf("unknown job kind %q", job.kind)
	}
}

func (s *Service) logQueueProgress(counter *int64, delta int64) {
	queued := atomic.AddInt64(counter, delta)
	if queued == 1 || queued%progressLogEvery == 0 {
		s.logger.Info("queued sync work", slog.Int64("jobs", queued))
	}
}

func (s *Service) logExecutionProgress(kind string, completed int64) {
	if completed == 1 || completed%progressLogEvery == 0 {
		s.logger.Info("sync progress",
			slog.String("kind", kind),
			slog.Int64("completed", completed),
		)
	}
}

func desiredIssue(cfg config.Config, finding model.Finding) model.DesiredIssue {
	dueDate, dueDateBase, dueDateReason := issueDueDate(cfg.Linear.Due, finding)
	return model.DesiredIssue{
		Fingerprint:   finding.Fingerprint,
		Title:         issueTitle(finding),
		Description:   issueDescription(managedLabels(cfg.Linear.Labels, finding), finding),
		DueDate:       dueDate,
		DueDateBase:   dueDateBase,
		State:         issueState(finding.Status),
		StateReason:   stateReason(finding.Status),
		DueDateReason: dueDateReason,
		ManagedLabels: managedLabels(cfg.Linear.Labels, finding),
		LabelReasons:  buildLabelReasons(cfg.Linear.Labels, finding),
		Priority:      issuePriority(finding.Severity),
	}
}

// issueTitle renders a scan-friendly Linear title. The context is the scanned
// target (host or name) and the subject is the HTTP method + path of the
// vulnerable request, which is the most useful locator for a DAST finding.
// The rendered title is capped by truncateIssueTitle: TargetHost, TargetName
// and Path come from the scan API (see snykdast.Findings, where Path is
// copied from the scan payload unmodified), so a long request path or target
// name would otherwise exceed Linear's title limit and make that finding's
// issue-create fail on every run, permanently.
func issueTitle(finding model.Finding) string {
	contextLabel := issueTitleContext(finding)
	severity := strings.ToLower(strings.TrimSpace(finding.Severity))
	title := strings.TrimSpace(finding.IssueTitle)
	subject := issueTitleSubject(finding)
	if contextLabel == "" {
		if subject == "" {
			return truncateIssueTitle(fmt.Sprintf("Snyk DAST: [%s] %s", severity, title))
		}
		return truncateIssueTitle(fmt.Sprintf("Snyk DAST: [%s] %s at %s", severity, title, subject))
	}
	if subject == "" {
		return truncateIssueTitle(fmt.Sprintf("Snyk DAST: [%s] %s: %s", severity, contextLabel, title))
	}
	return truncateIssueTitle(fmt.Sprintf("Snyk DAST: [%s] %s: %s at %s", severity, contextLabel, title, subject))
}

// maxIssueTitleRunes caps the rendered title at Linear's title limit (255
// characters), measured in runes so multibyte characters are not miscounted.
// The cap is applied to the fully rendered title, after every API-controlled
// field (severity, issue title, target, request path) has been concatenated.
const maxIssueTitleRunes = 255

// issueTitleTruncationMarker is appended when a rendered title is truncated,
// so readers understand why the text is cut off.
const issueTitleTruncationMarker = "..."

// truncateIssueTitle caps title at maxIssueTitleRunes runes, never splitting
// a multibyte rune and appending issueTitleTruncationMarker when truncation
// occurs (the marker replaces the last rune, so the result is never longer
// than the limit). It is a pure function of title, so the same finding always
// renders a byte-identical title — required for the sync's compare/hash
// pipeline to stay stable and not churn tickets between runs.
func truncateIssueTitle(title string) string {
	if utf8.RuneCountInString(title) <= maxIssueTitleRunes {
		return title
	}
	runes := []rune(title)
	return string(runes[:maxIssueTitleRunes-utf8.RuneCountInString(issueTitleTruncationMarker)]) + issueTitleTruncationMarker
}

func issueTitleContext(finding model.Finding) string {
	if host := strings.TrimSpace(finding.TargetHost); host != "" {
		return host
	}
	if name := strings.TrimSpace(finding.TargetName); name != "" {
		return name
	}
	return ""
}

func issueTitleSubject(finding model.Finding) string {
	method := strings.ToUpper(strings.TrimSpace(finding.Method))
	path := strings.TrimSpace(finding.Path)
	switch {
	case path != "" && method != "":
		return method + " " + path
	case path != "":
		return path
	case strings.TrimSpace(finding.FindingURL) != "":
		return truncateForTitle(finding.FindingURL)
	default:
		return ""
	}
}

// truncateForTitle caps a URL used in prose to 80 runes. It counts and cuts
// runes rather than bytes so a multibyte URL is neither miscounted nor split
// mid-rune (which would produce invalid UTF-8 in the title).
func truncateForTitle(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) <= 80 {
		return value
	}
	return string([]rune(value)[:77]) + "..."
}

// sanitizeSnykProse neutralizes HTML-comment openings in Snyk-controlled free
// text by replacing "<!--" with "<!- -", mirroring embedSnykProse in the
// sibling app/snyk-linear-sync. Without this, scanned-application content that
// reaches finding.Fix or finding.CorrelationMarkdown could carry a
// line-anchored "<!-- snyk-dast-linear-sync" marker and a fake "Fingerprint:"
// line, spoofing the ticket fingerprint or hiding it. The replacement never
// reintroduces the substring "<!--" (the inserted space always separates the
// two dashes), so the transformation is idempotent:
// sanitizeSnykProse(sanitizeSnykProse(x)) == sanitizeSnykProse(x). That keeps
// description comparison/hashing stable between runs.
func sanitizeSnykProse(text string) string {
	return strings.ReplaceAll(text, "<!--", "<!- -")
}

// singleLine prepares a scan-controlled value that renders on a single line
// of the ticket description (heading, Host, Path, Parameter, rendered labels
// and hrefs, identifiers). It trims surrounding space, flattens carriage
// returns and newlines to single spaces, and neutralizes HTML-comment
// openings via sanitizeSnykProse. Newline flattening is the anti-spoofing
// step that matters for these fields: findMetadataBlockStart matches a
// line-anchored "<!-- snyk-dast-linear-sync" marker, so a newline inside an
// otherwise backticked field would let a fake marker — and a fake
// "Fingerprint:" line — onto its own line even though sanitizeSnykProse had
// already rewritten the "<!--" itself. Flattening rather than escaping keeps
// the value on one rendered line in every client. Like the prose helpers, it
// is a pure function of its input, so description comparison and hashing stay
// stable between runs.
func singleLine(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	value = strings.ReplaceAll(value, "\n", " ")
	return sanitizeSnykProse(value)
}

// boundedSingleLine is singleLine plus truncateForTitle, for the rendered
// labels and identifier fields that are bounded at the source rather than by
// the description-body bound. truncateForTitle appends a single-line "..."
// marker, never the multi-line prose truncationMarker, so a bounded value can
// never carry the multi-line marker into a backtick span.
func boundedSingleLine(value string) string {
	return truncateForTitle(singleLine(value))
}

// maxEmbeddedProseRunes bounds how much of Snyk DAST's free-text Fix field
// and CorrelationMarkdown blocks is embedded verbatim in the ticket
// description. Linear enforces its own description length limit; since the
// managed metadata block (containing the fingerprint used for
// deduplication) is always appended LAST, unbounded Snyk prose would risk
// pushing that block past Linear's limit and silently truncating it away.
// That would make the ticket unmanaged (extractFingerprint finds nothing)
// and cause the next run to create a duplicate. The cap is conservative
// relative to Linear's limit so the metadata block always survives, and
// matches the sibling app/snyk-linear-sync (maxEmbeddedProseRunes).
//
// The per-block cap alone is not a bound on the description: a finding can be
// correlated to any number of Snyk Code vulnerabilities, so N blocks would
// still embed N x maxEmbeddedProseRunes runes. The correlation section is
// therefore additionally bounded by maxEmbeddedCorrelationBlocks below.
const maxEmbeddedProseRunes = 10000

// maxEmbeddedCorrelationBlocks bounds how many finding.CorrelationMarkdown
// blocks are embedded in the description. The per-block cap above is not a
// bound on the description by itself: Snyk DAST returns one block per linked
// Snyk Code vulnerability and does not bound how many, so a heavily correlated
// finding would still embed N x maxEmbeddedProseRunes runes and could push the
// managed metadata block (appended last) past Linear's limit — the
// duplicate-ticket failure the cap exists to prevent. Capping the block count
// makes the embedded correlation section a fixed bound (3 x 10,000 runes plus
// the block separators and truncation markers), and naming the number of
// hidden blocks keeps the omission visible to a reader.
const maxEmbeddedCorrelationBlocks = 3

// truncationMarker is appended when embedded Snyk prose is truncated, so
// readers understand why the text is cut off. It is a stable constant so
// description hashes do not churn between runs.
const truncationMarker = "\n\n_[truncated by snyk-dast-linear-sync]_"

// embedSnykProse prepares Snyk-controlled free text (finding.Fix or a
// finding.CorrelationMarkdown block) for embedding in a ticket description.
// It:
//
//  1. Sanitizes any HTML-comment opening ("<!--") so embedded text cannot
//     look like a second "<!-- snyk-dast-linear-sync ... -->" metadata block
//     (see sanitizeSnykProse above).
//  2. Caps the length so the text can never grow large enough to push the
//     metadata block (appended after it) out of what Linear accepts.
//
// Both steps are pure functions of the input finding text, so calling
// embedSnykProse twice with the same Snyk data always produces
// byte-identical output — required for the sync's compare/hash pipeline
// (normalizeDescriptionForCompare, desiredIssueHash) to stay stable and not
// churn tickets between runs when nothing has actually changed.
func embedSnykProse(text string) string {
	return truncateProse(sanitizeSnykProse(text), maxEmbeddedProseRunes)
}

// embedCorrelationMarkdown prepares finding.CorrelationMarkdown for embedding
// in the description, returning the lines to append after the section heading
// and subtitle (each block preceded by a blank line).
//
// Only the first maxEmbeddedCorrelationBlocks blocks are embedded, each
// sanitized and capped by embedSnykProse; when more blocks are linked, one
// stable line reports how many were left out, e.g.
//
//	_9 more linked Snyk Code vulnerabilities not shown._
//
// So the embedded section stays within a fixed bound no matter how many blocks
// the API returns, while a reader still learns that more correlations exist.
// The result is a pure function of the input blocks, so the compare/hash
// pipeline stays stable and descriptions do not churn between runs.
func embedCorrelationMarkdown(blocks []string) []string {
	shown := blocks
	if len(shown) > maxEmbeddedCorrelationBlocks {
		shown = shown[:maxEmbeddedCorrelationBlocks]
	}
	lines := make([]string, 0, 2*len(shown)+2)
	for _, block := range shown {
		lines = append(lines, "", embedSnykProse(block))
	}
	if omitted := len(blocks) - len(shown); omitted > 0 {
		lines = append(lines, "", correlationOmittedLine(omitted))
	}
	return lines
}

// correlationOmittedLine is the single stable line that stands in for the
// correlation blocks that were not embedded. Naming the count (and
// pluralising it) keeps the text useful to a reader without making the
// description depend on anything but the finding itself.
func correlationOmittedLine(omitted int) string {
	if omitted == 1 {
		return "_1 more linked Snyk Code vulnerability not shown._"
	}
	return fmt.Sprintf("_%d more linked Snyk Code vulnerabilities not shown._", omitted)
}

// truncateProse truncates s to at most maxRunes runes, never splitting a
// multibyte rune, appending truncationMarker when truncation occurs. It is a
// pure function of s and maxRunes, so the same input always yields the same
// output.
func truncateProse(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + truncationMarker
}

// maxDescriptionBodyMarginRunes is the room the description-body bound keeps
// for everything the per-block prose caps do not count: the two section
// headings, the correlation subtitle and omission line, blank separators, the
// title line and the bounded single-line fields (status, CWE, target and
// finding identifiers, and their labels). Those lines are at most a few
// hundred runes in practice; the margin is deliberately generous so the bound
// never cuts into the maximal bounded content below.
const maxDescriptionBodyMarginRunes = 2000

// maxDescriptionBodyRunes bounds the whole rendered description body — every
// line before the managed metadata block. It is derived from the per-block
// caps rather than chosen separately: the largest body the caps themselves
// allow is one Fix plus maxEmbeddedCorrelationBlocks correlation blocks, each
// at maxEmbeddedProseRunes runes, plus the truncation marker each truncated
// block appends. Adding the margin above keeps every maximal bounded section
// inside the bound, so a finding with a maximal Fix and three maximal
// correlation blocks still renders all of them. The result stays far below
// Linear's description limit, so the managed metadata block appended last
// always survives.
//
// The per-block prose caps do not bound the scan-controlled request-context
// fields (heading, Host, Path, Parameter, rendered links and identifiers),
// which Snyk DAST returns unbounded. The overall cap applied before the
// metadata block is appended guarantees the block still fits whichever field
// grows: without it, an oversized path, parameter or URL silently truncates
// the block away, extractFingerprint finds nothing, the ticket is unmanaged,
// and the next run mints a duplicate.
const maxDescriptionBodyRunes = (1+maxEmbeddedCorrelationBlocks)*(maxEmbeddedProseRunes+len(truncationMarker)) + maxDescriptionBodyMarginRunes

// boundDescriptionBody caps the rendered description body at maxRunes runes,
// deterministically, by dropping whole sections. A "### " heading is kept or
// dropped together with the lines that follow it up to the next heading, so a
// section that does not fit never leaves its heading behind with nothing under
// it. Lines before the first section heading (the title and request-context
// fields) are still considered individually: a line that does not fit in the
// remaining budget is dropped and later lines are still considered, so one
// oversized field cannot push the later bounded content (fix text,
// correlation, Snyk links, identifiers) out of the description. Dropping
// whole lines rather than cutting mid-line keeps every rendered line a pure
// function of the finding, never leaves a backtick span unbalanced, and keeps
// the multi-line truncationMarker out of backtick spans: the marker is
// appended once, as its own line, only when at least one line was dropped.
// The budget reserves room for that marker, so the rendered body never
// exceeds maxRunes. The result is a pure function of lines and maxRunes, so
// description comparison and hashing stay stable between runs.
func boundDescriptionBody(lines []string, maxRunes int) []string {
	budget := maxRunes - utf8.RuneCountInString(truncationMarker) - 1
	used := 0
	dropped := false
	bounded := make([]string, 0, len(lines)+1)
	for _, section := range descriptionSections(lines) {
		size := 0
		for _, line := range section {
			size += utf8.RuneCountInString(line) + 1
		}
		if used+size > budget {
			dropped = true
			continue
		}
		used += size
		bounded = append(bounded, section...)
	}
	if dropped {
		bounded = append(bounded, truncationMarker)
	}
	return bounded
}

// descriptionSections groups description lines into the units
// boundDescriptionBody must keep or drop together: each "### " heading and the
// lines after it up to the next heading form one section. Every other line is
// its own unit. Grouping the heading with its body is what keeps a heading
// from surviving after the bound dropped the only content under it.
func descriptionSections(lines []string) [][]string {
	sections := make([][]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if !isDescriptionSectionHeading(lines[i]) {
			sections = append(sections, lines[i:i+1])
			continue
		}
		j := i + 1
		for j < len(lines) && !isDescriptionSectionHeading(lines[j]) {
			j++
		}
		sections = append(sections, lines[i:j])
		i = j - 1
	}
	return sections
}

// isDescriptionSectionHeading reports whether line opens a description
// section whose body must travel with it. The title uses "## " and is a single
// self-contained line, so it is deliberately not matched here.
func isDescriptionSectionHeading(line string) bool {
	return strings.HasPrefix(line, "### ")
}

// issueDescription builds the managed Linear issue body. It is optimized for
// fast developer triage: the vulnerability and severity lead, the target and
// vulnerable request context follow, then the fix and Snyk DAST links, then the
// identifiers and the hidden metadata block used for deduplication.
func issueDescription(managedLabels []string, finding model.Finding) string {
	lines := []string{
		fmt.Sprintf("## %s [%s]", truncateIssueTitle(singleLine(finding.IssueTitle)), strings.ToUpper(singleLine(finding.Severity))),
	}

	if targetLabel, targetHref := targetLink(finding); targetLabel != "" {
		if targetHref != "" {
			lines = append(lines, fmt.Sprintf("Target: [%s](%s)", targetLabel, targetHref))
		} else {
			lines = append(lines, fmt.Sprintf("Target: %s", targetLabel))
		}
	}
	if host := singleLine(finding.TargetHost); host != "" {
		lines = append(lines, fmt.Sprintf("Host: `%s`", host))
	}

	if findingURLLabel, findingURL := vulnerableURLLine(finding); findingURL != "" {
		lines = append(lines, fmt.Sprintf("URL: [%s](%s)", findingURLLabel, findingURL))
	}
	if method := strings.ToUpper(singleLine(finding.Method)); method != "" {
		lines = append(lines, fmt.Sprintf("Method: `%s`", method))
	}
	if path := singleLine(finding.Path); path != "" {
		lines = append(lines, fmt.Sprintf("Path: `%s`", path))
	}
	if param := singleLine(finding.Parameter); param != "" {
		insertion := singleLine(insertionPointLabel(finding.InsertionPoint))
		if insertion != "" {
			lines = append(lines, fmt.Sprintf("Parameter: `%s` (%s)", param, insertion))
		} else {
			lines = append(lines, fmt.Sprintf("Parameter: `%s`", param))
		}
	} else if insertion := singleLine(insertionPointLabel(finding.InsertionPoint)); insertion != "" {
		lines = append(lines, fmt.Sprintf("Insertion point: %s", insertion))
	}

	if cwe := boundedSingleLine(finding.CWE); cwe != "" {
		if name := boundedSingleLine(finding.CWEName); name != "" {
			lines = append(lines, fmt.Sprintf("CWE: `%s` (%s)", cwe, name))
		} else {
			lines = append(lines, fmt.Sprintf("CWE: `%s`", cwe))
		}
	}
	if finding.CVSS > 0 {
		lines = append(lines, fmt.Sprintf("CVSS: `%.1f`", finding.CVSS))
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Status: `%s`", statusDisplayName(finding.Status)))

	if fix := strings.TrimSpace(finding.Fix); fix != "" {
		lines = append(lines, "")
		lines = append(lines, "### Fix")
		lines = append(lines, embedSnykProse(fix))
	}

	if len(finding.CorrelationMarkdown) > 0 {
		lines = append(lines, "")
		lines = append(lines, "### Snyk Code correlation")
		lines = append(lines, "_Linked Snyk Code source vulnerability responsible for this runtime finding._")
		lines = append(lines, embedCorrelationMarkdown(finding.CorrelationMarkdown)...)
	}

	lines = append(lines, "")
	if issueURL := singleLine(finding.IssueURL); issueURL != "" {
		lines = append(lines, fmt.Sprintf("Snyk DAST: [Open finding](%s)", issueURL))
	}
	if apiURL := singleLine(finding.IssueAPIURL); apiURL != "" {
		lines = append(lines, fmt.Sprintf("API: [Finding details](%s)", apiURL))
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Target: `%s` (`%s`)", boundedSingleLine(finding.TargetName), boundedSingleLine(finding.TargetID)))
	lines = append(lines, fmt.Sprintf("Finding ID: `%s`", boundedSingleLine(finding.SnykDASTFindingID)))
	if defID := boundedSingleLine(finding.DefinitionID); defID != "" {
		lines = append(lines, fmt.Sprintf("Definition ID: `%s`", defID))
	}

	// The managed metadata block is appended LAST, after the body has been
	// bounded, so whichever scan-controlled field grows, the block (with the
	// fingerprint used for deduplication) always survives intact.
	lines = boundDescriptionBody(lines, maxDescriptionBodyRunes)
	lines = append(lines, "", metadataBlock(finding.Fingerprint, managedLabels))
	return strings.Join(lines, "\n")
}

// targetLink returns the display label and optional URL for the scanned
// target. The label prefers the target name, falling back to the host; the
// link uses the target's base URL when available. Both come from the scan
// API unbounded: the label is flattened, sanitized and capped by
// boundedSingleLine, and the href is flattened and sanitized by singleLine
// but never truncated — a shortened URL is a wrong link. An oversized href is
// handled by the description-body bound (boundDescriptionBody), which drops
// the whole rendered line rather than the link target.
func targetLink(finding model.Finding) (string, string) {
	label := boundedSingleLine(finding.TargetName)
	if label == "" {
		label = boundedSingleLine(finding.TargetHost)
	}
	link := singleLine(finding.TargetURL)
	if link == "" {
		link = singleLine(finding.FindingURL)
	}
	return label, link
}

// vulnerableURLLine returns a short label and the full vulnerable URL for the
// finding. The label is the method + path when available (more readable than a
// long query-string URL), otherwise a truncated form of the URL itself; it is
// capped by truncateForTitle because a scanned path can be arbitrarily long.
// The href is flattened and sanitized by singleLine but never truncated — a
// shortened URL is a wrong link — and an oversized href is handled by the
// description-body bound (boundDescriptionBody), which drops the whole
// rendered line rather than the link target.
func vulnerableURLLine(finding model.Finding) (string, string) {
	url := singleLine(finding.FindingURL)
	if url == "" {
		return "", ""
	}
	method := strings.ToUpper(singleLine(finding.Method))
	path := singleLine(finding.Path)
	var label string
	switch {
	case path != "" && method != "":
		label = method + " " + path
	case path != "":
		label = path
	default:
		label = truncateForTitle(url)
	}
	return truncateForTitle(label), url
}

func insertionPointLabel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "cookie":
		return "Cookie"
	case "parameter", "arbitrary_url_param":
		return "Parameter"
	case "header":
		return "Header"
	case "url_folder", "url_filename":
		return "URL Path"
	case "json_parameter":
		return "JSON Parameter"
	case "request_body":
		return "Request Body"
	case "multipart_parameter":
		return "Multipart Parameter"
	case "graphql_parameter":
		return "GraphQL Parameter"
	case "non_standard_parameter":
		return "Non-standard Parameter"
	default:
		return strings.TrimSpace(value)
	}
}

func metadataBlock(fingerprint string, managedLabels []string) string {
	lines := []string{
		"<!-- snyk-dast-linear-sync",
		fmt.Sprintf("fingerprint: %s", fingerprint),
	}
	if labels := model.NormalizeManagedLabelNames(managedLabels); len(labels) > 0 {
		lines = append(lines, fmt.Sprintf("managed_labels: %s", strings.Join(labels, ",")))
	}
	lines = append(lines, "-->")
	return strings.Join(lines, "\n")
}

func issueState(status model.FindingStatus) model.IssueState {
	switch status {
	case model.FindingIgnored:
		return model.StateCancelled
	case model.FindingSnoozed:
		return model.StateTodo
	case model.FindingFixed:
		return model.StateDone
	default:
		return model.StateTodo
	}
}

func stateReason(status model.FindingStatus) string {
	switch status {
	case model.FindingOpen:
		return "Snyk DAST reports this finding as not fixed"
	case model.FindingSnoozed:
		return "Snyk DAST reports this finding as accepted with a time-limited acceptance"
	case model.FindingIgnored:
		return "Snyk DAST reports this finding as invalid or permanently accepted"
	case model.FindingFixed:
		return "Snyk DAST reports this finding as fixed"
	default:
		return ""
	}
}

// statusDisplayName renders the FindingStatus value for the Linear issue
// description. The raw constant values are code-internal; the description
// should show what Snyk DAST actually reports.
func statusDisplayName(status model.FindingStatus) string {
	switch status {
	case model.FindingSnoozed:
		return "accepted (time-limited)"
	default:
		return string(status)
	}
}

func issuePriority(severity string) int {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "critical":
		return 1
	case "high":
		return 2
	case "medium":
		return 3
	case "low":
		return 4
	default:
		return 0
	}
}

func issueDueDate(dueCfg config.DueDateConfig, finding model.Finding) (effective, base, reason string) {
	var baseDate time.Time
	var basis string
	switch {
	case !finding.IgnoreExpiresAt.IsZero():
		expiresUTC := finding.IgnoreExpiresAt.UTC()
		baseDate = time.Date(expiresUTC.Year(), expiresUTC.Month(), expiresUTC.Day(), 0, 0, 0, 0, time.UTC)
		basis = "acceptance expiry"
	case !finding.CreatedAt.IsZero():
		createdAtUTC := finding.CreatedAt.UTC()
		baseDate = time.Date(createdAtUTC.Year(), createdAtUTC.Month(), createdAtUTC.Day(), 0, 0, 0, 0, time.UTC)
		basis = "finding creation"
	default:
		return "", "", ""
	}

	return dueDateFromBase(baseDate, basis, dueCfg, finding)
}

// dueDateFromBase calculates the due date from a given base date, severity,
// and SLA offsets. It returns the same value for both the effective due date
// and the cache base so that past-SLA dates remain stable.
func dueDateFromBase(baseDate time.Time, basis string, dueCfg config.DueDateConfig, finding model.Finding) (effective, base, reason string) {
	var days int
	switch issuePriority(finding.Severity) {
	case 1:
		days = dueCfg.CriticalDays
	case 2:
		days = dueCfg.HighDays
	case 3:
		days = dueCfg.MediumDays
	case 4:
		days = dueCfg.LowDays
	default:
		return "", "", ""
	}

	dueDate := baseDate.AddDate(0, 0, days)
	dueDateStr := dueDate.Format(time.DateOnly)

	severityName := strings.ToLower(strings.TrimSpace(finding.Severity))
	if severityName == "" {
		severityName = "unknown"
	}
	reason = fmt.Sprintf("%s severity SLA: %d days from %s", severityName, days, basis)

	// A past due date is left as-is. Linear renders past due dates as
	// "overdue", and the actual past date is more informative than flooring
	// to today: it tells the triager how long the issue has been past its
	// SLA, not just that it is overdue. Flooring to today caused daily
	// churn — each run would advance the floor by one day, triggering a
	// spurious update even when the underlying Snyk DAST data was unchanged.

	return dueDateStr, dueDateStr, reason
}

// pendingTerminalTransition reports whether the issue still needs to move into
// a terminal Linear state (Done/Cancelled) that it is not already in. Such a
// transition must never be hidden by the cache fast-path, otherwise a finding
// that became fixed or ignored while its ticket sat in an open column would
// stay open indefinitely. Open-state divergences are intentionally excluded so
// the cache continues to batch benign churn.
func pendingTerminalTransition(existing model.ExistingIssue, desired model.DesiredIssue) bool {
	if desired.PreserveState {
		return false
	}
	// An archived issue is already terminal and cannot be mutated.
	if existing.ArchivedAt != nil {
		return false
	}
	if desired.State != model.StateDone && desired.State != model.StateCancelled {
		return false
	}
	return model.NormalizeWorkflowStateName(existing.StateName) != model.NormalizeWorkflowStateName(model.StateName(desired.State))
}

func needsUpdate(existing model.ExistingIssue, desired model.DesiredIssue) bool {
	return ComputeDiff(existing, desired).HasChanges()
}

// ComputeDiff returns a diff describing which managed fields changed between
// the existing and desired Linear issue. The caller is responsible for only
// displaying a change when the corresponding field is non-empty (e.g. a
// resolved issue may carry the existing issue's title and description).
func ComputeDiff(existing model.ExistingIssue, desired model.DesiredIssue) *model.IssueDiff {
	d := &model.IssueDiff{}

	if existing.Title != desired.Title {
		d.TitleChanged = true
		d.TitleFrom = existing.Title
		d.TitleTo = desired.Title
	}

	if normalizeDescriptionForCompare(existing.Description) != normalizeDescriptionForCompare(desired.Description) {
		d.DescriptionChanged = true
	}

	if existing.DueDate != desired.DueDate {
		if desired.DueDate != "" || existing.DueDate != "" {
			d.DueDateChanged = true
			d.DueDateFrom = existing.DueDate
			d.DueDateTo = desired.DueDate
		}
	}

	if !desired.PreserveState {
		existingNorm := model.NormalizeWorkflowStateName(existing.StateName)
		desiredNorm := model.NormalizeWorkflowStateName(model.StateName(desired.State))
		if existingNorm != desiredNorm {
			d.StateChanged = true
			d.StateFrom = existing.StateName
			d.StateTo = desiredNorm
		}
	}

	if existing.Priority != desired.Priority {
		d.PriorityChanged = true
		d.PriorityFrom = existing.Priority
		d.PriorityTo = desired.Priority
	}

	existingLabels := make(map[string]struct{}, len(existing.Labels))
	for _, l := range existing.Labels {
		existingLabels[model.NormalizeLabelName(l.Name)] = struct{}{}
	}
	desiredLabelSet := make(map[string]struct{}, len(desired.ManagedLabels))
	for _, l := range desired.ManagedLabels {
		desiredLabelSet[model.NormalizeLabelName(l)] = struct{}{}
	}
	previousManaged := make(map[string]struct{}, len(existing.ManagedLabels))
	for _, l := range existing.ManagedLabels {
		previousManaged[model.NormalizeLabelName(l)] = struct{}{}
	}

	for label := range desiredLabelSet {
		if _, inPrevious := previousManaged[label]; inPrevious {
			continue
		}
		if _, inExisting := existingLabels[label]; !inExisting {
			d.LabelsAdded = append(d.LabelsAdded, label)
		}
	}

	for _, label := range existing.ManagedLabels {
		norm := model.NormalizeLabelName(label)
		if _, exists := desiredLabelSet[norm]; !exists {
			// Only report as removed if the label is actually present on the
			// issue. If it was previously managed but has already been manually
			// removed, reporting it as "removed" produces a misleading change
			// comment even though the mutation is correct (it simply omits the
			// label from the new label set).
			if _, inExisting := existingLabels[norm]; inExisting {
				d.LabelsRemoved = append(d.LabelsRemoved, norm)
			}
		}
	}

	d.LabelsNeedUpdate = len(d.LabelsAdded) > 0 || len(d.LabelsRemoved) > 0

	// Also detect labels that are in the managed set but not actually present
	// on the issue. This covers the case where a label was supposed to be
	// applied in a previous run but the Linear mutation failed. Only check
	// this when we have label data to compare against; an empty Labels
	// list on the existing issue means label data was not loaded.
	if !d.LabelsNeedUpdate && len(existingLabels) > 0 {
		for label := range desiredLabelSet {
			if _, inExisting := existingLabels[label]; !inExisting {
				d.LabelsNeedUpdate = true
				break
			}
		}
	}

	return d
}

func missingFindingState(fingerprint string, activeTargets map[string]struct{}, inactiveTargets map[string]struct{}) (model.IssueState, string) {
	targetID, ok := FingerprintTargetID(fingerprint)
	if !ok {
		return model.StateDone, "this Snyk DAST finding is no longer present"
	}
	if _, exists := activeTargets[targetID]; exists {
		return model.StateDone, "this Snyk DAST finding is no longer present"
	}
	if _, exists := inactiveTargets[targetID]; exists {
		return model.StateCancelled, "the Snyk DAST target has been deactivated"
	}
	// Both deleted and inactive targets result in Cancelled: the issue is no
	// longer actionable regardless of why the target stopped producing findings.
	return model.StateCancelled, "the Snyk DAST target no longer exists"
}

// deferredTicketCount counts the managed tickets whose finding id is in
// deferredIDs, i.e. the tickets the resolve loop holds back for this run.
func deferredTicketCount(existingByFingerprint map[string]model.ExistingIssue, deferredIDs map[string]struct{}) int64 {
	if len(deferredIDs) == 0 {
		return 0
	}
	var count int64
	for fingerprint := range existingByFingerprint {
		if findingID, ok := FingerprintFindingID(fingerprint); ok {
			if _, deferred := deferredIDs[findingID]; deferred {
				count++
			}
		}
	}
	return count
}

// FingerprintFindingID extracts the numeric finding ID portion of a Snyk DAST
// fingerprint. A finding id is only unique within its target, so callers must
// use it for a coarse match (a ticket whose finding could not be normalized),
// never as the deduplication key.
func FingerprintFindingID(fingerprint string) (string, bool) {
	const prefix = "snyk-dast:"
	if !strings.HasPrefix(fingerprint, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(fingerprint, prefix)
	_, findingID, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(findingID) == "" {
		return "", false
	}
	return findingID, true
}

// FingerprintTargetID extracts the target ID portion of a Snyk DAST fingerprint.
func FingerprintTargetID(fingerprint string) (string, bool) {
	const prefix = "snyk-dast:"
	if !strings.HasPrefix(fingerprint, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(fingerprint, prefix)
	targetID, _, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(targetID) == "" {
		return "", false
	}
	return targetID, true
}

func normalizeDescriptionForCompare(description string) string {
	description = strings.TrimSpace(strings.ReplaceAll(description, "\r\n", "\n"))
	description = blankLineRunPattern.ReplaceAllString(description, "\n")
	description = linearAutoLinkPattern.ReplaceAllString(description, "[$1]($2)")
	description = markdownEscapePattern.ReplaceAllString(description, "$1")
	// Linear re-serializes underscore emphasis as asterisks (__x__ -> **x**,
	// _x_ -> *x*). Mapping '*' to '_' on both sides makes the written and stored forms
	// compare equal; it can only ever suppress a spurious difference, never
	// hide a real one, because it is applied to both sides.
	description = strings.ReplaceAll(description, "*", "_")
	description = strings.ReplaceAll(description, "DO NOT EDIT OR REMOVE THIS BLOCK. Used by snyk-dast-linear-sync for deduplication.", "__SNYK_DAST_LINEAR_METADATA_WARNING__")
	description = strings.ReplaceAll(description, "DO NOT EDIT, REMOVE, OR REFORMAT THIS BLOCK. It is required by snyk-dast-linear-sync for deduplication and safe updates.", "__SNYK_DAST_LINEAR_METADATA_WARNING__")
	return description
}

// isConfiguredBacklogState returns true if the existing Linear issue state name
// matches the configured Backlog state (case-insensitive, with normalization
// for common variants like "Canceled" → "Cancelled").
func isConfiguredBacklogState(existingStateName, configuredBacklog string) bool {
	return model.NormalizeWorkflowStateName(existingStateName) == model.NormalizeWorkflowStateName(configuredBacklog)
}

// isNonTerminalModelState reports whether the desired model state is
// non-terminal. Todo and Backlog are non-terminal; Done and Cancelled are
// terminal. When the sync wants a terminal state the transition must always
// be allowed (handled by pendingTerminalTransition), so PreserveState only
// applies to non-terminal desired states.
func isNonTerminalModelState(state model.IssueState) bool {
	return state == model.StateTodo || state == model.StateBacklog
}

// isNonTerminalLinearState reports whether the existing Linear issue state is
// non-terminal (i.e. not the configured Done or Cancelled state). Users can
// freely move issues between non-terminal states as part of triage; the sync
// should not override those manual decisions.
func isNonTerminalLinearState(stateName string, states config.StateConfig) bool {
	normalized := model.NormalizeWorkflowStateName(stateName)
	if normalized == model.NormalizeWorkflowStateName(states.Done) {
		return false
	}
	if normalized == model.NormalizeWorkflowStateName(states.Cancelled) {
		return false
	}
	return true
}

// isTerminalExistingIssue reports whether an existing Linear issue is terminal:
// either in the configured Done/Cancelled state, or auto-archived by Linear.
func isTerminalExistingIssue(existing model.ExistingIssue, states config.StateConfig) bool {
	if existing.ArchivedAt != nil {
		return true
	}
	return !isNonTerminalLinearState(existing.StateName, states)
}

// preferCanonicalDuplicate decides which of two Linear issues sharing a
// fingerprint should be treated as canonical. A non-terminal issue is always
// preferred over a terminal one (Done/Cancelled, or archived): keeping a
// closed copy as canonical would cancel the live ticket and then reopen the
// closed one on the next run, churning state for no reason, and an archived
// copy cannot be mutated at all. Between two issues of the same class the
// lower identifier wins, as it is the older ticket.
func preferCanonicalDuplicate(a, b model.ExistingIssue, states config.StateConfig) (canonical, duplicate model.ExistingIssue) {
	aTerminal := isTerminalExistingIssue(a, states)
	bTerminal := isTerminalExistingIssue(b, states)
	if aTerminal != bTerminal {
		if aTerminal {
			return b, a
		}
		return a, b
	}
	if identifierNum(b.Identifier) < identifierNum(a.Identifier) {
		return b, a
	}
	return a, b
}

func upsertManagedMetadata(description, fingerprint string, managedLabels []string) string {
	description = strings.TrimSpace(strings.ReplaceAll(description, "\r\n", "\n"))
	block := metadataBlock(fingerprint, managedLabels)

	start := findMetadataBlockStart(description)
	if start >= 0 {
		if relEnd := strings.Index(description[start:], "-->"); relEnd >= 0 {
			end := start + relEnd + len("-->")
			description = strings.TrimSpace(description[:start] + block + description[end:])
			description = stripVisibleFingerprintLine(description)
			return description
		}
	}

	if description == "" {
		return block
	}
	description = stripVisibleFingerprintLine(description)
	return strings.TrimSpace(strings.Join([]string{description, "", block}, "\n"))
}

// findMetadataBlockStart locates the snyk-dast-linear-sync metadata block start
// marker in the description, anchored to the beginning of a line. This
// prevents false matches where the marker string appears mid-sentence in
// user-written text (e.g. "See <!-- snyk-dast-linear-sync notes -->"), which
// could corrupt the description if treated as a metadata block.
//
// It returns the LAST line-anchored occurrence, not the first. Ticket
// descriptions embed free-form Snyk-controlled prose (finding.Fix and
// finding.CorrelationMarkdown) ABOVE the real metadata block, since the sync
// always appends the managed metadata block last. If that prose contains a
// line-anchored marker (scanned-application content can reach
// CorrelationMarkdown), returning the first occurrence would hijack this
// function with a bogus block and corrupt the description, and could also
// break ticket matching via extractFingerprint/extractManagedLabels in the
// Linear client. The real, sync-managed block is always the last one.
// Keep this in sync with the equivalent function in internal/linear/client.go.
func findMetadataBlockStart(description string) int {
	header := metadataHeaderStart()
	last := -1
	for i := 0; i <= len(description)-len(header); {
		idx := strings.Index(description[i:], header)
		if idx < 0 {
			break
		}
		absIdx := i + idx
		// The marker must be at the start of a line: either position 0
		// or preceded by a newline.
		if absIdx == 0 || description[absIdx-1] == '\n' {
			last = absIdx
		}
		i = absIdx + 1
	}
	return last
}

func metadataHeaderStart() string {
	return "<!-- snyk-dast-linear-sync"
}

func stripVisibleFingerprintLine(description string) string {
	lines := strings.Split(description, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "Fingerprint:") {
			continue
		}
		filtered = append(filtered, line)
	}
	return strings.TrimSpace(strings.Join(filtered, "\n"))
}

func managedLabels(labelCfg config.LabelConfig, finding model.Finding) []string {
	// Label management off (LINEAR_MANAGED_LABEL=off) turns off the whole
	// managed set, derived labels included. The check-type and target-type
	// labels can still be configured (mappings or defaults), so returning
	// early here is what stops them being asserted — and resolved, and
	// failing the run when the label is missing in Linear — after the
	// operator disabled label management.
	if !labelCfg.Enabled() {
		return nil
	}

	labels := make([]string, 0, 4)
	if managed := strings.TrimSpace(labelCfg.Managed); managed != "" {
		labels = append(labels, managed)
	}

	checkType := strings.ToLower(strings.TrimSpace(finding.CheckType))
	if checkType != "" {
		if mapped := strings.TrimSpace(labelCfg.CheckType[checkType]); mapped != "" {
			labels = append(labels, mapped)
		} else if fallback := strings.TrimSpace(labelCfg.CheckTypeDefault); fallback != "" {
			labels = append(labels, fallback)
		}
	}

	targetType := strings.ToLower(strings.TrimSpace(finding.TargetType))
	if targetType != "" {
		if mapped := strings.TrimSpace(labelCfg.TargetType[targetType]); mapped != "" {
			labels = append(labels, mapped)
		} else if fallback := strings.TrimSpace(labelCfg.TargetTypeDefault); fallback != "" {
			labels = append(labels, fallback)
		}
	}

	return model.NormalizeManagedLabelNames(labels)
}

// buildLabelReasons returns a map from normalized label name to a short reason
// string explaining why that label is included in the managed set. This gives
// change comments a "why" instead of just listing added labels.
func buildLabelReasons(labelCfg config.LabelConfig, finding model.Finding) map[string]string {
	reasons := make(map[string]string)

	// Mirror managedLabels: with label management off no managed label is
	// written, so no label needs a "why" in the change comment either.
	if !labelCfg.Enabled() {
		return reasons
	}

	checkType := strings.ToLower(strings.TrimSpace(finding.CheckType))
	if checkType != "" {
		if mapped, ok := labelCfg.CheckType[checkType]; ok && strings.TrimSpace(mapped) != "" {
			reasons[model.NormalizeLabelName(mapped)] = fmt.Sprintf("Snyk DAST check type is %s", checkType)
		} else if strings.TrimSpace(labelCfg.CheckTypeDefault) != "" {
			reasons[model.NormalizeLabelName(labelCfg.CheckTypeDefault)] = fmt.Sprintf("Snyk DAST check type is %s", checkType)
		}
	}

	targetType := strings.ToLower(strings.TrimSpace(finding.TargetType))
	if targetType != "" {
		if mapped, ok := labelCfg.TargetType[targetType]; ok && strings.TrimSpace(mapped) != "" {
			reasons[model.NormalizeLabelName(mapped)] = fmt.Sprintf("Snyk DAST target type is %s", targetType)
		} else if strings.TrimSpace(labelCfg.TargetTypeDefault) != "" {
			reasons[model.NormalizeLabelName(labelCfg.TargetTypeDefault)] = fmt.Sprintf("Snyk DAST target type is %s", targetType)
		}
	}

	return reasons
}

// identifierNum extracts the numeric suffix from a Linear identifier (e.g. "PROB-42" → 42).
// Returns 0 if the identifier does not contain a dash or the suffix is not a number.
func identifierNum(identifier string) int {
	_, after, ok := strings.Cut(identifier, "-")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(after)
	if err != nil {
		return 0
	}
	return n
}

func linearHashesByFingerprint(issues []model.ExistingIssue) map[string]string {
	out := make(map[string]string, len(issues))
	for _, issue := range issues {
		if issue.Fingerprint == "" {
			continue
		}
		out[issue.Fingerprint] = existingIssueHash(issue)
	}
	return out
}
