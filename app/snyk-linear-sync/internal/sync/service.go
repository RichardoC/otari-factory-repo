package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/tesslio/snyk-linear-sync/internal/cache"
	"github.com/tesslio/snyk-linear-sync/internal/config"
	"github.com/tesslio/snyk-linear-sync/internal/model"
)

type SnykClient interface {
	LoadSnapshot(ctx context.Context) (model.SnykSnapshot, error)
}

type LinearClient interface {
	LoadSnapshot(ctx context.Context) ([]model.ExistingIssue, error)
	// CreateIssues returns the indices (into desired) of items whose alias
	// failed, so the caller retries only those instead of the whole batch.
	// A non-nil error means no per-alias data is available (e.g. a
	// transport failure): nothing is known per item, and the mutation may
	// even have been committed before the response was lost. The caller
	// must reconcile which entries landed (ExistingFingerprints) before
	// retrying any of them.
	CreateIssues(ctx context.Context, desired []model.DesiredIssue) ([]int, error)
	// ExistingFingerprints reports which of the given fingerprints already
	// have a live (non-archived) managed issue in Linear. It reconciles the
	// ambiguous batch-create outcome: a non-nil error from CreateIssues
	// carries no per-alias data, and a transport failure does not prove
	// Linear rejected the mutation, so the caller asks what actually landed
	// before retrying any item.
	ExistingFingerprints(ctx context.Context, fingerprints []string) (map[string]struct{}, error)
	UpdateIssues(ctx context.Context, updates []model.IssueUpdate) error
	// PostComments returns the indices (into updates) whose comment failed
	// to post, so the caller retries only those instead of the whole batch.
	// A non-nil error means no per-alias data is available and every update
	// must be retried individually.
	PostComments(ctx context.Context, updates []model.IssueUpdate) ([]int, error)
	// LastOpenStates reads the Linear history of the given closed issues and
	// returns, per issue ID, the open workflow state each was in before it
	// was closed. Issues whose history yields none are absent. The error
	// reports issues whose history could not be read; the map still holds
	// every issue that was read.
	LastOpenStates(ctx context.Context, issueIDs []string) (map[string]model.WorkflowState, error)
}

type CacheStore interface {
	Load(ctx context.Context) (cache.Snapshot, error)
	Save(ctx context.Context, snapshot cache.Snapshot) error
	// IgnoreEvictions reports how many cached v1 ignore rows LoadIgnores
	// discarded as stale or unparseable, so the run can log them.
	IgnoreEvictions() cache.IgnoreEvictionCounts
}

type Service struct {
	cfg    config.Config
	logger *slog.Logger
	snyk   SnykClient
	linear LinearClient
	cache  CacheStore
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

// scanPinnedLinePattern matches the description lines that pin a ticket to the
// scan it was written from: the Ref or Commit line naming the scanned commit,
// and the File line linking the flagged lines at that commit. The patterns
// after it find those pins within such a line.
var (
	scanPinnedLinePattern = regexp.MustCompile(`(?m)^(?:Ref|Commit|File): .*$`)
	scannedCommitPattern  = regexp.MustCompile("(` at \\[?|^Commit: \\[?)`[^`]*`")
	commitPathPattern     = regexp.MustCompile(`/(commit|blob)/[0-9a-f]{7,40}\b`)
	sourceRegionPattern   = regexp.MustCompile(`\(line [0-9:]+(?:-[0-9]+| to [0-9:]+)?\)`)
	lineAnchorPattern     = regexp.MustCompile(`#L[0-9]+(?:-L[0-9]+)?`)
)

var markdownEscapePattern = regexp.MustCompile(`\\([\\` + "`" + `*_{}\[\]()#+\-.!~])`)

type RunResult struct {
	Findings       int
	ExistingIssues int
	// ActiveProjects / InactiveProjects count the Snyk projects in the
	// snapshot. A sudden change in either (projects deleted, deactivated or
	// recreated) explains a burst of cancels, rebinds or creates.
	ActiveProjects   int
	InactiveProjects int
	// ClusterLookupFailures counts Kubernetes projects whose cluster lookup
	// failed this run; their findings get no identity and cannot rebind.
	ClusterLookupFailures int
	Conflicts             int
	// Rebound counts tickets matched to a finding by identity after Snyk
	// recreated the finding's project under a new project ID.
	Rebound int64
	// Reopened counts terminal tickets the sync deliberately reopened: sync-
	// cancelled tickets rebound after project recreation, and tickets closed
	// while Snyk kept reporting the same occurrence as open.
	Reopened int64
	// DeferredCreates counts findings whose ticket creation was held back
	// for the run because their cluster lookup failed while a recent rebind
	// candidate might be theirs (see plausibleRebindCandidate).
	DeferredCreates     int64
	PlannedCreates      int64
	PlannedUpdates      int64
	PlannedResolves     int64
	CancelledDuplicates int64
	FailedOps           int64
	// FailedComments counts change comments the run gave up on: a comment
	// whose individual retry still failed (with an error or a failed index),
	// and a failed index that named no batch entry, so it could not be
	// retried. It is tracked separately from FailedOps
	// because a missing change comment leaves the synced issue state
	// correct — the comment is explanatory, and the feature is opt-in via
	// LINEAR_COMMENTS — so it does not fail the run; it is counted rather
	// than only logged so a run cannot report success while silently
	// dropping comments.
	FailedComments int64
}

func New(cfg config.Config, logger *slog.Logger, snyk SnykClient, linear LinearClient, cacheStore CacheStore) *Service {
	return &Service{
		cfg:    cfg,
		logger: logger,
		snyk:   snyk,
		linear: linear,
		cache:  cacheStore,
	}
}

// Err reports whether the run left state-changing work permanently unapplied,
// so the caller can exit non-zero. A run that logs a failure but reports
// success is worse than a noisy one: nothing downstream notices the drift, and
// the next scheduled run reports success again.
//
// Only permanent failures count. FailedOps is incremented after the per-item
// retry has itself failed, so a batch that fails and then succeeds on retry is
// a clean run and returns nil here. The exception is an ambiguous batch create
// whose reconciliation lookup also fails: the whole batch is counted without a
// retry, because there is no safe way to tell which entries landed. Comment
// failures never reach FailedOps: a missing change comment leaves the synced
// issue state correct, and comments are opt-in via LINEAR_COMMENTS, so they
// are logged as warnings and counted in FailedComments, which never fails
// the run.
func (r RunResult) Err() error {
	if r.FailedOps > 0 {
		return fmt.Errorf("sync finished with failed_ops=%d; see the error log for the Linear operation(s) that could not be applied", r.FailedOps)
	}
	return nil
}

// Run loads both the Snyk and the Linear snapshot, plans the work that brings
// the tickets in line with the findings and executes that plan over a pool of
// cfg.Sync.Workers workers.
//
// An error means the run did not complete: either the source data could not be
// loaded, a worker failed or ctx was cancelled (see below). The cache is
// refreshed only by a run that completed. An interrupted run returns a wrapped
// context error and leaves the cache exactly as it was, so the next run
// re-examines every finding this one did not write; saving the refresh after a
// partial plan would mark never-attempted findings as reconciled and the next
// run would skip them through the cache fast-path.
func (s *Service) Run(ctx context.Context) (RunResult, error) {
	runCtx := ctx
	var (
		snykSnapshot   model.SnykSnapshot
		findings       []model.Finding
		existingIssues []model.ExistingIssue
	)
	cacheEnabled := s.cache != nil && !s.cfg.Cache.BypassCache
	cacheSignature := managedSchemaSignature()
	cacheSnapshot := cache.Snapshot{
		SnykHashes:   map[string]string{},
		LinearHashes: map[string]string{},
	}

	if s.cfg.Cache.BypassCache {
		s.logger.Info("bypassing sync cache for this run")
	} else if s.cache != nil {
		loaded, err := s.cache.Load(ctx)
		if err != nil {
			return RunResult{}, err
		}
		if loaded.SchemaSignature != "" && loaded.SchemaSignature != cacheSignature {
			cacheEnabled = false
			s.logger.Info("ignoring sync cache because managed schema changed",
				slog.String("cached_signature", loaded.SchemaSignature),
				slog.String("current_signature", cacheSignature),
			)
		} else {
			cacheSnapshot = loaded
			cacheAge := "unknown"
			if !loaded.SavedAt.IsZero() {
				cacheAge = time.Since(loaded.SavedAt).Round(time.Second).String()
			}
			s.logger.Info("loaded sync cache",
				slog.String("cache_age", cacheAge),
				slog.Int("snyk_rows", len(loaded.SnykHashes)),
				slog.Int("linear_rows", len(loaded.LinearHashes)),
			)
		}
	}

	s.logger.Info("loading Snyk findings and Linear snapshot")
	loadGroup, loadCtx := errgroup.WithContext(ctx)
	loadGroup.Go(func() error {
		var err error
		snykSnapshot, err = s.snyk.LoadSnapshot(loadCtx)
		if err != nil {
			return err
		}
		findings = snykSnapshot.Findings
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

	// Stored fingerprints are canonicalized before any comparison: Linear's
	// editor rewrites markdown-equivalent sequences in stored descriptions
	// (e.g. __main__.py becomes **main**.py), and computed fingerprints are
	// canonical by construction. extractFingerprint already canonicalizes,
	// but this pass also covers issues arriving from any other source.
	for i := range existingIssues {
		existingIssues[i].Fingerprint = model.CanonicalFingerprint(existingIssues[i].Fingerprint)
	}

	existingByFingerprint := map[string]model.ExistingIssue{}
	// existingByCoarseFingerprint indexes non-terminal tickets by their coarse
	// (location-stripped) fingerprint. It is used only for migration: when a
	// finding carries a new fine-grained fingerprint that no Linear ticket has
	// yet, but a non-terminal ticket with the matching coarse fingerprint
	// exists, we update that ticket rather than creating a duplicate. Terminal
	// tickets are deliberately excluded so that a closed coarse-fingerprint
	// ticket is never reused (reopen guard). Only tickets whose stored
	// fingerprint IS already coarse (no location segment) are candidates — a
	// fine-grained ticket is matched by exact lookup and must never be a
	// coarse-fallback candidate, or two findings sharing a coarse prefix
	// would both bind to it (ticket stealing + perpetual churn).
	existingByCoarseFingerprint := map[string]model.ExistingIssue{}
	var duplicatesToCancel []model.ExistingIssue
	// terminalByFingerprint lists every non-archived terminal ticket per
	// fingerprint, duplicates included. When all copies of a fingerprint are
	// terminal, preferCanonicalDuplicate keeps the lowest identifier (the
	// original ticket), but the copy an automation closed prematurely is the
	// newest one; the reopen check must be able to consider it.
	terminalByFingerprint := map[string][]model.ExistingIssue{}
	for _, issue := range existingIssues {
		if issue.Fingerprint != "" && issue.ArchivedAt == nil && isTerminalLinearState(issue, s.cfg.Linear.States) {
			terminalByFingerprint[issue.Fingerprint] = append(terminalByFingerprint[issue.Fingerprint], issue)
		}
		if issue.Fingerprint != "" {
			if prior, exists := existingByFingerprint[issue.Fingerprint]; exists {
				canonical, duplicate := preferCanonicalDuplicate(prior, issue, s.cfg.Linear.States)
				s.logger.Warn("duplicate fingerprint found on Linear issues, will cancel other copy",
					slog.String("fingerprint", issue.Fingerprint),
					slog.String("canonical", canonical.Identifier),
					slog.String("duplicate", duplicate.Identifier),
				)
				existingByFingerprint[issue.Fingerprint] = canonical
				duplicatesToCancel = append(duplicatesToCancel, duplicate)
				continue
			}
			existingByFingerprint[issue.Fingerprint] = issue
			if isNonTerminalLinearState(issue, s.cfg.Linear.States) {
				coarse := model.CoarseFingerprint(issue.Fingerprint)
				if coarse == issue.Fingerprint {
					if _, exists := existingByCoarseFingerprint[coarse]; !exists {
						existingByCoarseFingerprint[coarse] = issue
					}
				}
			}
		}
	}

	// An empty Snyk snapshot is not evidence that every finding went away.
	// The documented symptom of an API key without the required project read
	// permission, and of an outage that answers success with no results, is a
	// silently empty project and finding list. Resolving on such a snapshot
	// would cancel every managed ticket in one run, so refuse instead: the
	// only safe reading of "no projects at all" alongside existing managed
	// tickets is that the snapshot is not a truthful source right now. The
	// trigger is deliberate: zero active *and* zero inactive projects, never
	// zero findings. An organization whose issues are all fixed still lists
	// its projects, and those tickets must still move to the resolved state.
	// A brand-new deployment with no managed tickets yet is unaffected. This
	// is not a percentage threshold: a partial listing is a separate concern.
	if len(snykSnapshot.ProjectIDs) == 0 && len(snykSnapshot.InactiveProjectIDs) == 0 && len(existingByFingerprint) > 0 {
		message := "the Snyk snapshot contains no projects while managed Linear issues exist; " +
			"this is the documented symptom of an API key that cannot read projects " +
			"(project and issue queries silently return empty results), of an outage, " +
			"or of a SNYK_ORG_ID naming an organization the API key cannot see; " +
			"check SNYK_ORG_ID and the Snyk API key, and Snyk status, before running the sync again"
		if s.cfg.DryRun {
			// A dry run must still show the plan it would have executed, but
			// the operator has to see that a live run would refuse.
			s.logger.Error(message)
		} else {
			return RunResult{}, fmt.Errorf("%s", message)
		}
	}

	// rebindCandidates indexes, by project-independent identity, the tickets
	// a finding may take over after Snyk recreates its project under a new
	// project ID (see rebindCandidatesByIdentity for who qualifies).
	findingFingerprints := make(map[string]struct{}, len(findings))
	for _, finding := range findings {
		findingFingerprints[finding.Fingerprint] = struct{}{}
	}
	rebindCandidates := rebindCandidatesByIdentity(existingByFingerprint, findingFingerprints, snykSnapshot.ProjectIDs, s.cfg.Linear.States)
	// reboundFingerprints records the old fingerprints of rebound tickets so
	// the resolve loop does not also try to close them: the ticket now
	// tracks a live finding under its new fingerprint.
	reboundFingerprints := map[string]struct{}{}
	// deferredFingerprints records findings held back for one run because
	// their cluster lookup failed while a rebind candidate may be theirs
	// (see plausibleRebindCandidate). They are marked seen like any finding.
	deferredFingerprints := map[string]struct{}{}
	candidateClusters := rebindCandidateClusters(rebindCandidates)

	desiredByFingerprint := make(map[string]model.DesiredIssue, len(findings))
	// matchedExisting records the Linear ticket each finding resolved to,
	// whether by exact fingerprint, coarse-fingerprint migration fallback, or
	// identity rebind after project recreation. The job loop uses this
	// instead of existingByFingerprint so that migration-matched and rebound
	// findings update their existing ticket (rewriting its fingerprint)
	// rather than creating a duplicate.
	matchedExisting := make(map[string]model.ExistingIssue, len(findings))
	snykHashes := make(map[string]string, len(findings))
	var rebound, reopened int64
	for _, finding := range findings {
		desired := desiredIssue(s.cfg, finding)

		existing, matched := existingByFingerprint[finding.Fingerprint]
		kind := matchExact
		if !matched {
			// Migration fallback: the finding carries a fine-grained
			// fingerprint no Linear ticket has yet (new code occurrence),
			// but an in-flight ticket with the matching coarse fingerprint
			// may exist. Reuse it so we update rather than duplicate. Only
			// non-terminal tickets are candidates — a closed ticket must
			// never be reused (reopen guard).
			coarse := model.CoarseFingerprint(finding.Fingerprint)
			if coarse != finding.Fingerprint {
				if candidate, ok := existingByCoarseFingerprint[coarse]; ok {
					existing = candidate
					matched = true
					kind = matchCoarse
					// Deplete the coarse index so only the first fine-grained finding
					// reuses this ticket. Subsequent findings with the same coarse
					// prefix (e.g. the same issue type in a different file) create
					// fresh tickets instead of all binding to the same Linear issue,
					// which would race and lose fingerprints.
					delete(existingByCoarseFingerprint, coarse)
				}
			}
		}

		if !matched {
			// Rebind after project recreation: Snyk sometimes recreates a
			// project (same name and target) under a new project ID, which
			// also mints new issue IDs, so no fingerprint matches. The
			// finding's project-independent identity still does. Take over
			// the old project's ticket instead of letting the resolve loop
			// cancel it and creating a copy. Candidates are depleted on use
			// so two findings can never bind to the same ticket.
			if candidate, ok := takeRebindCandidate(rebindCandidates, model.FindingIdentity(finding)); ok {
				existing = candidate
				matched = true
				kind = matchRebind
				reboundFingerprints[candidate.Fingerprint] = struct{}{}
				rebound++
				oldProjectID, _ := FingerprintProjectID(candidate.Fingerprint)
				s.logger.Info("rebinding ticket to finding in recreated Snyk project",
					slog.String("existing", candidate.Identifier),
					slog.String("existing_state", candidate.StateName),
					slog.String("old_project_id", oldProjectID),
					slog.String("new_project_id", finding.ProjectID),
					slog.String("old_fingerprint", candidate.Fingerprint),
					slog.String("new_fingerprint", finding.Fingerprint),
					slog.String("project_name", finding.ProjectName),
				)
			}
		}

		if matched && isTerminalLinearState(existing, s.cfg.Linear.States) && isNonTerminalModelState(desired.State) {
			switch {
			case kind == matchRebind:
				// A terminal rebind candidate is always a ticket the sync
				// itself cancelled because its project disappeared (the
				// candidate index admits no other terminal ticket), so the
				// closure was never a fix or a human decision. The finding is
				// the same one in the recreated project; reopen the ticket.
				desired.Reopen = true
				desired.StateReason = "Snyk recreated this finding's project under a new project ID and still reports the finding as open; reopened the ticket cancelled when the old project disappeared"
			case kind == matchExact && reopenCandidate(terminalByFingerprint[finding.Fingerprint], finding, &existing):
				// The ticket was closed (typically by an automation outside
				// the sync) while Snyk kept reporting this exact occurrence as
				// open. Creating a fresh ticket would mint a duplicate with
				// the identical fingerprint every time this happens, so reuse
				// the ticket; the normal update moves it back to the open
				// state. See continuouslyOpenSinceTicketCreated for why this
				// cannot resurrect a #28 zombie ticket.
				desired.Reopen = true
				desired.StateReason = "Snyk still reports this finding as open; reopened instead of creating a duplicate"
				// The reopened copy becomes canonical; older terminal copies
				// stay untouched (the duplicate-cancel loop skips terminal
				// tickets), and next run preferCanonicalDuplicate keeps this
				// now-open ticket.
				existingByFingerprint[finding.Fingerprint] = existing
				s.logger.Info("reopening ticket closed while Snyk still reports the finding as open",
					slog.String("fingerprint", finding.Fingerprint),
					slog.String("existing", existing.Identifier),
					slog.String("existing_state", existing.StateName),
				)
			default:
				// Reopen guard: never reuse a terminal (Done/Cancelled)
				// ticket when Snyk reports the finding as open/awaiting-fix.
				// Snyk reusing a problem-type issueID across different code
				// is not a directive to reopen a closed Linear ticket; a
				// fresh ticket should be created instead. Treating this as
				// "no match" falls through to the create path. The terminal
				// ticket is also removed from existingByFingerprint so the
				// job-dispatch loop does not send an update that would
				// reopen it.
				s.logger.Info("not reusing closed ticket for reopened finding; creating new ticket",
					slog.String("fingerprint", finding.Fingerprint),
					slog.String("existing", existing.Identifier),
					slog.String("existing_state", existing.StateName),
				)
				delete(existingByFingerprint, finding.Fingerprint)
				matched = false
			}
		}

		if !matched && finding.ProjectClusterUnknown && plausibleRebindCandidate(rebindCandidates, candidateClusters, finding, s.cfg.Linear.States, time.Now()) {
			// The cluster lookup failed, so the finding has no identity and
			// cannot rebind, but a ticket left by a vanished project could be
			// its own. Creating a fresh ticket now would make the next run
			// match that copy exactly and never rebind, which is the
			// cancel+create churn rebinding exists to prevent. Hold the
			// finding back for this run instead; the candidate is cancelled
			// (recording closed_reason) by the resolve loop as usual and is
			// rebound once a lookup succeeds.
			s.logger.Warn("deferring ticket for finding with unknown kubernetes cluster; a rebind candidate may match once the cluster lookup succeeds",
				slog.String("fingerprint", finding.Fingerprint),
				slog.String("project_id", finding.ProjectID),
				slog.String("project_name", finding.ProjectName),
			)
			deferredFingerprints[finding.Fingerprint] = struct{}{}
			continue
		}

		if matched && finding.ProjectClusterUnknown {
			// The cluster lookup failed this run, so the finding has no
			// identity and no cluster. Rendering it as-is would strip the
			// ticket's stored identity and Cluster line, and put them back on
			// the next successful run: two pointless updates per failure.
			// Keep what the ticket already records instead; both are
			// refreshed from Snyk on the next run whose lookup succeeds.
			recovered := finding
			recovered.ProjectCluster = storedCluster(existing.Description)
			desired.Description = issueDescriptionWithIdentity(s.cfg.Source, desired.ManagedLabels, recovered, existing.Identity)
		}

		if matched {
			// A fix arriving for an "ignored until fix available" finding: Snyk
			// flips ignored=false, the issue comes back as open, and the sync
			// must move the ticket it parked in Backlog out again. Computed
			// once here because both the Backlog override below and the
			// due-date recalculation further down depend on it.
			fixNowAvailable := finding.Status == model.FindingOpen && wasAwaitingFix(existing, s.cfg.Linear.Labels.AwaitingFix)
			// The sync itself parked this ticket in Backlog while Snyk reported
			// the ignore as "until fix available". That is not a manual triage
			// decision, so neither override below may treat it as one.
			syncParkedInBacklog := fixNowAvailable && isConfiguredBacklogState(existing.StateName, s.cfg.Linear.States.Backlog)

			// Respect manual Backlog override: if a user moved an open ticket from
			// Todo to Backlog, don't move it back on subsequent syncs. A Backlog
			// placement the sync itself made for an awaiting-fix finding is not a
			// manual move, so it is skipped here and the ticket returns to Todo.
			if desired.State == model.StateTodo && isConfiguredBacklogState(existing.StateName, s.cfg.Linear.States.Backlog) && !syncParkedInBacklog {
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
			if isNonTerminalModelState(desired.State) && isNonTerminalLinearState(existing, s.cfg.Linear.States) && !syncParkedInBacklog {
				desired.PreserveState = true
			}

			// Explain the move out of Backlog in the change comment. Only the
			// state the sync itself chose is overridden: if a person moved the
			// awaiting-fix ticket somewhere else (say In Progress), the general
			// override above still preserves their choice.
			if syncParkedInBacklog {
				desired.StateReason = "Snyk reports a fix is now available; moved out of Backlog"
			}

			// Sticky due date for the updated_at re-detection fallback: Snyk
			// bumps updated_at on routine re-scans, not just genuine
			// re-detections, which would otherwise advance the computed due
			// date every single run once the fallback triggers — endless
			// Linear due-date updates and change-comments. Once the ticket
			// already has a due date, keep it fixed instead of re-deriving it
			// from a moving updated_at. A ticket that never had a due date
			// set still gets one (the condition below is false), and the
			// fix-availability recalculation further below still takes
			// priority when it applies.
			if desired.DueDateUsedUpdatedAtFallback && existing.DueDate != "" {
				desired.DueDate = existing.DueDate
				desired.DueDateBase = existing.DueDate
				desired.DueDateReason = "kept existing due date to avoid churn from Snyk updated_at re-detection"
			}

			// When a fix becomes available for a previously-blocked issue,
			// recalculate the due date from today instead of the original
			// created_at. The original SLA date is meaningless because the
			// team couldn't act on the issue while no fix was available. A
			// fresh SLA from fix-availability gives a meaningful triage
			// deadline without the daily churn that the old floor-to-today
			// caused for all overdue issues.
			if fixNowAvailable {
				desired.DueDate, desired.DueDateBase, desired.DueDateReason = issueDueDateFromFixAvailability(s.cfg.Linear.Due, finding)
			}

		}

		if desired.Reopen {
			reopened++
		}
		desiredByFingerprint[finding.Fingerprint] = desired
		snykHashes[finding.Fingerprint] = desiredIssueHash(desired)
		if matched {
			matchedExisting[finding.Fingerprint] = existing
		}
	}

	if reopened > 0 {
		s.restoreReopenedStates(ctx, desiredByFingerprint, matchedExisting)
	}

	currentLinearHashes := make(map[string]string, len(matchedExisting))
	for fingerprint, issue := range matchedExisting {
		currentLinearHashes[fingerprint] = existingIssueHash(issue)
	}

	jobs := make(chan job)
	var result RunResult
	var failedWrites failedFingerprints
	result.Findings = len(findings)
	result.ExistingIssues = len(existingIssues)
	result.ActiveProjects = len(snykSnapshot.ProjectIDs)
	result.InactiveProjects = len(snykSnapshot.InactiveProjectIDs)
	result.ClusterLookupFailures = snykSnapshot.ClusterLookupFailures
	result.Conflicts = len(duplicatesToCancel)
	result.Rebound = rebound
	result.Reopened = reopened
	result.DeferredCreates = int64(len(deferredFingerprints))
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

		seen := make(map[string]struct{}, len(desiredByFingerprint)+len(reboundFingerprints))
		for fingerprint := range reboundFingerprints {
			seen[fingerprint] = struct{}{}
		}
		for fingerprint := range deferredFingerprints {
			seen[fingerprint] = struct{}{}
			seen[model.CoarseFingerprint(fingerprint)] = struct{}{}
		}
		createBatch := make([]model.DesiredIssue, 0, createBatchSize)
		updateBatch := make([]model.IssueUpdate, 0, createBatchSize)
		for fingerprint, desired := range desiredByFingerprint {
			seen[fingerprint] = struct{}{}
			// Also mark the coarse fingerprint as seen so the resolve loop
			// doesn't try to close a terminal ticket whose coarse fingerprint
			// was superseded by a fine-grained finding (reopen-guard create).
			if coarse := model.CoarseFingerprint(fingerprint); coarse != fingerprint {
				seen[coarse] = struct{}{}
			}
			existing, ok := matchedExisting[fingerprint]
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
			// Linear does not allow updating archived issues; an update would
			// only fail, and fail again every run. Archived tickets are always
			// terminal and are only matched here when the desired state is
			// terminal too, so there is nothing to move.
			if existing.ArchivedAt != nil {
				continue
			}
			// The cache fast-path may not suppress a pending move into a terminal
			// state: a finding that became fixed/ignored (desired Done/Cancelled)
			// while its ticket sat in an open column must still be closed, even
			// if its Snyk/Linear hashes are unchanged since the last run. Benign
			// open-state divergences stay cache-suppressed as before. Nor may it
			// suppress a deliberate reopen, or a ticket matched under a
			// different stored fingerprint (coarse migration or rebind), whose
			// fingerprint must be rewritten: the cache is keyed by fingerprint
			// and says nothing about a ticket that carried another one.
			if cacheEnabled && cacheSnapshot.SnykHashes[fingerprint] == snykHashes[fingerprint] && cacheSnapshot.LinearHashes[fingerprint] == currentLinearHashes[fingerprint] && !pendingTerminalTransition(existing, desired) && !desired.Reopen && existing.Fingerprint == fingerprint {
				continue
			}
			if needsUpdate(existing, desired, s.cfg.Linear.States) {
				update := model.IssueUpdate{Existing: existing, Desired: desired}
				update.Diff = ComputeDiff(existing, desired, s.cfg.Linear.States)
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
			// Skip archived tickets — they're already terminal and Linear
			// doesn't allow updating archived issues. Trying to resolve
			// them would produce API errors.
			if existing.ArchivedAt != nil {
				continue
			}
			desiredState, stateReason := missingFindingState(existing.Fingerprint, snykSnapshot.ProjectIDs, snykSnapshot.InactiveProjectIDs)
			// Record a machine-made closure only when this loop is the one
			// closing the ticket. A ticket that is already terminal was closed
			// by a fix or a person before its project vanished; keep whatever
			// it already records rather than claiming the closure, so it never
			// becomes eligible for a rebind.
			closedReason := existing.ClosedReason
			alreadyClosed := isTerminalLinearState(existing, s.cfg.Linear.States)
			if !alreadyClosed {
				closedReason = projectClosedReason(existing.Fingerprint, snykSnapshot.ProjectIDs, snykSnapshot.InactiveProjectIDs)
			}
			// For the same reason it keeps its closed state. A project going
			// away says nothing about how the ticket was closed, and moving a
			// Done ticket to Cancelled would erase the record that the
			// finding was fixed. A ticket closed in a live project still moves
			// to Done once its finding is gone, as before.
			preserveClosedState := alreadyClosed && desiredState == model.StateCancelled
			resolved := model.DesiredIssue{
				Fingerprint: existing.Fingerprint,
				Title:       existing.Title,
				Description: upsertManagedMetadata(existing.Description, managedMetadata{
					Fingerprint:   existing.Fingerprint,
					Identity:      existing.Identity,
					ManagedLabels: existing.ManagedLabels,
					ClosedReason:  closedReason,
					AwaitingFix:   existing.AwaitingFix,
				}),
				DueDate:       existing.DueDate,
				State:         desiredState,
				PreserveState: preserveClosedState,
				StateReason:   stateReason,
				ManagedLabels: existing.ManagedLabels,
				Priority:      existing.Priority,
			}
			if needsUpdate(existing, resolved, s.cfg.Linear.States) {
				resolvedUpdate := model.IssueUpdate{Existing: existing, Desired: resolved}
				resolvedUpdate.Diff = ComputeDiff(existing, resolved, s.cfg.Linear.States)
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
			// Skip tickets that are already terminal (a configured Done/
			// Cancelled workflow state, or archived). Cancelling an
			// already-cancelled or already-done ticket is a pointless
			// mutation, and archived tickets cannot be mutated via the
			// Linear API at all — attempting it would just produce an API
			// error.
			if isTerminalLinearState(duplicate, s.cfg.Linear.States) {
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
			if needsUpdate(duplicate, desired, s.cfg.Linear.States) {
				cancelUpdate := model.IssueUpdate{Existing: duplicate, Desired: desired}
				cancelUpdate.Diff = ComputeDiff(duplicate, desired, s.cfg.Linear.States)
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

	if err := g.Wait(); err != nil {
		// An interrupted run has written only part of its plan: the findings
		// whose jobs never ran look reconciled in the refreshed cache simply
		// because their ticket was untouched. Returning before the cache save
		// below is what keeps such a run from hiding them from the next one.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return result, fmt.Errorf("sync interrupted before all work was written: %w", err)
		}
		return result, err
	}

	if !s.cfg.DryRun && s.cache != nil {
		// Refresh the cache even if some Linear operations failed. Snyk data and
		// ignore metadata are still valid and should be cached so the next run
		// does not have to re-fetch everything. Linear hashes are taken from a
		// fresh snapshot when possible; if the reload fails we fall back to the
		// hashes from the initial load.
		//
		// Fingerprints whose write failed are left out of both maps below, so
		// the next run compares them against live data again and retries them
		// instead of being skipped by the cache fast-path. That covers both a
		// snapshot of the ticket as it still is (the failed write left it
		// unchanged) and a failed reload, whose fallback hashes are just as
		// stale.
		cacheLinearHashes := currentLinearHashes
		if result.PlannedCreates > 0 || result.PlannedUpdates > 0 || result.PlannedResolves > 0 {
			refreshedIssues, err := s.linear.LoadSnapshot(runCtx)
			if err != nil {
				s.logger.Warn("failed to refresh Linear snapshot, using current hashes for cache",
					"error", err,
				)
			} else {
				cacheLinearHashes = linearHashesByFingerprint(refreshedIssues)
				// A fingerprint shared by several tickets must cache the hash of
				// the ticket the finding is matched to, not whichever copy the
				// snapshot happened to list last.
				refreshedByID := make(map[string]model.ExistingIssue, len(refreshedIssues))
				for _, issue := range refreshedIssues {
					refreshedByID[issue.ID] = issue
				}
				for fingerprint, matched := range matchedExisting {
					if issue, ok := refreshedByID[matched.ID]; ok {
						cacheLinearHashes[fingerprint] = existingIssueHash(issue)
					}
				}
			}
		}
		uncachedFailedCount := 0
		for fingerprint := range snykHashes {
			if failedWrites.has(fingerprint) {
				delete(snykHashes, fingerprint)
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
			SchemaSignature: cacheSignature,
			SnykHashes:      snykHashes,
			LinearHashes:    cacheLinearHashes,
		}
		if err := s.cache.Save(runCtx, nextSnapshot); err != nil {
			return result, err
		}
		ignoreEvictions := s.cache.IgnoreEvictions()
		s.logger.Info("refreshed sync cache",
			slog.Int("snyk_rows", len(nextSnapshot.SnykHashes)),
			slog.Int("linear_rows", len(nextSnapshot.LinearHashes)),
			slog.Int("uncached_failed", uncachedFailedCount),
			slog.Int64("failed_ops", result.FailedOps),
			slog.Int("ignore_evicted_ttl", ignoreEvictions.TTLExpired),
			slog.Int("ignore_evicted_unparseable", ignoreEvictions.Unparseable),
		)
	}

	return result, nil
}

// matchKind records how a finding was matched to an existing ticket. The
// reopen guard treats each differently: only an exact fingerprint match may
// reopen a prematurely closed ticket, and only a rebind may reopen a ticket
// the sync cancelled because its project disappeared.
type matchKind int

const (
	matchExact matchKind = iota
	matchCoarse
	matchRebind
)

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

// createOne retries a single issue create. A per-entry failure reported
// through the failed-index return is treated exactly like an error: the issue
// does not exist either way, so the run must not count it as done. Ignoring
// the indices here would let a failed retry pass silently and the run report
// success for work that never landed.
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

// retryUnlandedCreates reconciles an ambiguous batch-create failure. Linear
// exposes no idempotency key, so a lost response does not prove the mutation
// was rejected: the batch may have committed server-side before the response
// was lost, and retrying every item would duplicate those tickets until only
// the duplicate-cancellation pass converges them away on a later run, after
// duplicate notifications went out. It therefore asks Linear which of the
// batch's fingerprints now have a live issue and re-creates only the rest.
//
// If the reconciliation query itself fails there is no safe way to tell what
// landed, so nothing is retried: the batch is counted in FailedOps and left
// for the next run, which creates whatever is genuinely missing — the finding
// is still reported by Snyk and its absent ticket is missing from the next
// Linear snapshot. Missing a ticket for one cycle is recoverable; a duplicate
// is only undone later by the duplicate-cancellation pass.
func (s *Service) retryUnlandedCreates(ctx context.Context, batch []model.DesiredIssue, result *RunResult, failed *failedFingerprints) {
	fingerprints := make([]string, 0, len(batch))
	for _, desired := range batch {
		fingerprints = append(fingerprints, desired.Fingerprint)
	}

	landed, err := s.linear.ExistingFingerprints(ctx, fingerprints)
	if err != nil {
		atomic.AddInt64(&result.FailedOps, int64(len(batch)))
		// No safe way to tell what landed, so every entry is treated as
		// failed and kept out of the refreshed cache: the next run
		// re-creates whatever is genuinely missing.
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
// but LinearClient is an interface, so an out-of-range index from a faulty
// implementation must count a FailedOp rather than panic the worker goroutine.
// Because such an index identifies no batch entry, the caller also keeps every
// fingerprint of the batch out of the refreshed cache: nothing in it is proven
// to have landed.
func inRange(idx, length int) bool {
	return idx >= 0 && idx < length
}

// updateBatchWithFallback sends batch to Linear in one call and, when the
// call fails, retries every item on its own. It reports whether the batch call
// itself succeeded, counts each item that still fails in FailedOps, and records
// its fingerprints as failed (see failedFingerprints).
func (s *Service) updateBatchWithFallback(ctx context.Context, kind jobKind, verb string, batch []model.IssueUpdate, result *RunResult, failed *failedFingerprints) bool {
	if err := s.linear.UpdateIssues(ctx, batch); err == nil {
		return true
	} else {
		s.logger.Warn("batch "+string(kind)+" failed, retrying issues individually",
			slog.Int("batch_size", len(batch)),
			slog.Any("error", err),
		)
	}
	for _, update := range batch {
		if err := s.linear.UpdateIssues(ctx, []model.IssueUpdate{update}); err != nil {
			atomic.AddInt64(&result.FailedOps, 1)
			// Record both fingerprints: they differ for a
			// coarse-fingerprint migration or an identity rebind, and the
			// cache is keyed by the desired fingerprint at write time and by
			// the ticket's own fingerprint on later runs.
			failed.add(update.Desired.Fingerprint, update.Existing.Fingerprint)
			s.logger.Error("failed to "+verb+" issue",
				slog.String("issue", update.Existing.Identifier),
				slog.String("fingerprint", update.Desired.Fingerprint),
				slog.Any("error", err),
			)
		}
	}
	return false
}

// postCommentsWithFallback posts the change comments for batch in one call and,
// when the call fails, retries every item on its own. A comment failure is not
// a job failure: it is counted in result.FailedComments and logged as a
// warning, not counted in FailedOps.
func (s *Service) postCommentsWithFallback(ctx context.Context, batch []model.IssueUpdate, result *RunResult) {
	retryComment := func(update model.IssueUpdate) {
		// A single-comment retry can fail two ways: an error, or a nil error
		// with the comment's own index in the failed list. Both mean the
		// comment was given up on, so both are counted.
		failed, err := s.linear.PostComments(ctx, []model.IssueUpdate{update})
		if err != nil || len(failed) > 0 {
			atomic.AddInt64(&result.FailedComments, 1)
			s.logger.Warn("failed to post change comment",
				slog.String("issue", update.Existing.Identifier),
				slog.String("fingerprint", update.Desired.Fingerprint),
				slog.Any("error", err),
				slog.Int("failed_count", len(failed)),
			)
		}
	}
	failedIdx, err := s.linear.PostComments(ctx, batch)
	if err != nil {
		// No per-alias data at all (e.g. a transport failure): fall back to
		// retrying every update individually, same as before.
		s.logger.Warn("batch comment post failed, retrying individually",
			slog.Int("batch_size", len(batch)),
			slog.Any("error", err),
		)
		for _, update := range batch {
			retryComment(update)
		}
	} else if len(failedIdx) > 0 {
		// Partial failure: only retry the comments that actually failed - the
		// rest already posted, and retrying them too would leave duplicate
		// notification comments on the issue.
		s.logger.Warn("batch comment post had partial failures, retrying only the failed comments",
			slog.Int("failed_count", len(failedIdx)),
			slog.Int("batch_size", len(batch)),
		)
		for _, idx := range failedIdx {
			if !inRange(idx, len(batch)) {
				// A faultily implemented LinearClient can name a failed
				// index that is no member of the batch. Count it as a
				// failed comment rather than indexing the batch with it,
				// which would panic the worker goroutine and take down
				// the whole run.
				atomic.AddInt64(&result.FailedComments, 1)
				s.logger.Warn("Linear client reported an out-of-range failed comment index; counting a failed comment",
					slog.Int("index", idx),
					slog.Int("batch_size", len(batch)),
				)
				continue
			}
			retryComment(batch[idx])
		}
	}
}

// enqueue hands a job to the worker pool and reports the reason it could not be
// queued. The context is the workers' own: when a worker fails, errgroup
// cancels it and every worker returns. A plain send on the unbuffered jobs
// channel would then block forever — nobody is left to receive — and Run would
// never return. Waiting on the context instead lets the producer stop, close
// the channel and unwind.
func (s *Service) enqueue(ctx context.Context, jobs chan<- job, j job) error {
	select {
	case jobs <- j:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) executeJob(ctx context.Context, job job, result *RunResult, failed *failedFingerprints) error {
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
		if s.cfg.DryRun {
			return nil
		}
		failedIdx, err := s.linear.CreateIssues(ctx, job.desiredBatch)
		if err != nil {
			// No per-alias data at all (e.g. a transport failure, or a 502
			// after Linear had already committed the mutation): nothing is
			// known per item, and retrying blindly would create a second
			// copy of every ticket the batch landed server-side. Reconcile
			// against Linear first; see retryUnlandedCreates.
			s.logger.Warn("batch create failed, reconciling which issues landed before retrying",
				slog.Int("batch_size", len(job.desiredBatch)),
				slog.Any("error", err),
			)
			s.retryUnlandedCreates(ctx, job.desiredBatch, result, failed)
		} else if len(failedIdx) > 0 {
			// Partial failure: the batch call told us exactly which items
			// failed. Only retry those — the rest were already created and
			// retrying them too would produce duplicate tickets.
			s.logger.Warn("batch create had partial failures, retrying only the failed issues",
				slog.Int("failed_count", len(failedIdx)),
				slog.Int("batch_size", len(job.desiredBatch)),
			)
			for _, idx := range failedIdx {
				if !inRange(idx, len(job.desiredBatch)) {
					atomic.AddInt64(&result.FailedOps, 1)
					// The index identifies no batch entry, so no entry of
					// the batch can be proven to have landed. As with an
					// ambiguous create whose reconciliation also fails,
					// every fingerprint is treated as failed and kept out
					// of the refreshed cache: caching any of them could
					// let the next run's cache fast path skip the ticket
					// the bogus index was trying to report as missing.
					batchFingerprints := make([]string, 0, len(job.desiredBatch))
					for _, desired := range job.desiredBatch {
						batchFingerprints = append(batchFingerprints, desired.Fingerprint)
					}
					failed.add(batchFingerprints...)
					s.logger.Error("Linear client reported an out-of-range failed create index; keeping the whole batch out of the refreshed cache",
						slog.Int("index", idx),
						slog.Int("batch_size", len(job.desiredBatch)),
					)
					continue
				}
				desired := job.desiredBatch[idx]
				// createOne treats a failed-index return with a nil error
				// (success: false on the alias) like the error it is; the
				// run must count the item and keep it out of the cache.
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
		n := atomic.AddInt64(&result.PlannedUpdates, int64(len(job.updateBatch)))
		s.logExecutionProgress("update", n)
		if s.cfg.DryRun {
			return nil
		}
		if s.updateBatchWithFallback(ctx, jobUpdate, "update", job.updateBatch, result, failed) && s.cfg.Linear.CommentsEnabled {
			s.postCommentsWithFallback(ctx, job.updateBatch, result)
		}
		// A batch that failed because the context ended must stop the worker,
		// not be retried item by item: see the jobCreateBatch case.
		return ctx.Err()
	case jobResolve:
		n := atomic.AddInt64(&result.PlannedResolves, int64(len(job.updateBatch)))
		s.logExecutionProgress("resolve", n)
		if s.cfg.DryRun {
			return nil
		}
		s.updateBatchWithFallback(ctx, jobResolve, "resolve", job.updateBatch, result, failed)
		// See the jobCreateBatch case: a cancelled batch stops the worker.
		return ctx.Err()
	case jobCancelDuplicate:
		n := atomic.AddInt64(&result.CancelledDuplicates, int64(len(job.updateBatch)))
		s.logExecutionProgress("cancel-duplicate", n)
		if s.cfg.DryRun {
			return nil
		}
		s.updateBatchWithFallback(ctx, jobCancelDuplicate, "cancel duplicate", job.updateBatch, result, failed)
		// See the jobCreateBatch case: a cancelled batch stops the worker.
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
	dueDate, dueDateBase, dueDateReason, usedUpdatedAtFallback := issueDueDate(cfg.Linear.Due, finding)
	// No meaningful SLA while blocked on an upstream fix. When a fix becomes
	// available Snyk flips ignored=false; the next run maps it back to
	// FindingOpen (Todo) and recalculates the due date.
	if finding.Status == model.FindingAwaitingFix {
		dueDate = ""
		dueDateBase = ""
		dueDateReason = "awaiting upstream fix, SLA paused"
		usedUpdatedAtFallback = false
	}
	return model.DesiredIssue{
		Fingerprint:                  finding.Fingerprint,
		Title:                        issueTitle(finding),
		Description:                  issueDescription(cfg.Source, managedLabels(cfg.Linear.Labels, finding), finding),
		DueDate:                      dueDate,
		DueDateBase:                  dueDateBase,
		State:                        issueState(finding.Status),
		StateReason:                  stateReason(finding.Status),
		DueDateReason:                dueDateReason,
		DueDateUsedUpdatedAtFallback: usedUpdatedAtFallback,
		ManagedLabels:                managedLabels(cfg.Linear.Labels, finding),
		CreateOnlyLabels:             cfg.Linear.Labels.CreateOnly,
		LabelReasons:                 buildLabelReasons(cfg.Linear.Labels, finding),
		Priority:                     issuePriority(finding.Severity),
	}
}

// issueTitle renders a scan-friendly Linear title. The context is the source
// repository (or reference) and the subject is the most specific source
// locator Snyk reports (file basename, package, target file or project).
// The rendered title is capped by truncateIssueTitle: the context and subject
// fields come from the Snyk API unbounded, so one long repository or package
// name would otherwise exceed Linear's title limit and make that finding's
// issue-create fail on every run, permanently.
func issueTitle(finding model.Finding) string {
	contextLabel := issueTitleContext(finding)
	severity := strings.ToLower(strings.TrimSpace(finding.Severity))
	title := strings.TrimSpace(finding.IssueTitle)
	subject := issueTitleSubject(finding)
	if contextLabel == "" {
		if subject == "" {
			return truncateIssueTitle(fmt.Sprintf("Snyk: [%s] %s", severity, title))
		}
		return truncateIssueTitle(fmt.Sprintf("Snyk: [%s] %s in %s", severity, title, subject))
	}
	if subject == "" {
		return truncateIssueTitle(fmt.Sprintf("Snyk: [%s] %s: %s", severity, contextLabel, title))
	}
	return truncateIssueTitle(fmt.Sprintf("Snyk: [%s] %s: %s in %s", severity, contextLabel, title, subject))
}

// maxIssueTitleRunes caps the rendered title at Linear's title limit (255
// characters), measured in runes so multibyte characters are not miscounted.
// The cap is applied to the fully rendered title, after every API-controlled
// field (severity, issue title, context, subject) has been concatenated.
const maxIssueTitleRunes = 255

// issueTitleTruncationMarker is appended when a rendered title is truncated,
// so readers understand why the text is cut off.
const issueTitleTruncationMarker = "..."

// truncateIssueTitle caps title at maxIssueTitleRunes runes, never splitting
// a multibyte rune and appending issueTitleTruncationMarker when truncation
// occurs (the marker replaces the last runes, so the result is never longer
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

func issueDescription(sourceCfg config.SourceConfig, managedLabels []string, finding model.Finding) string {
	return issueDescriptionWithIdentity(sourceCfg, managedLabels, finding, model.FindingIdentity(finding))
}

// storedCluster returns the cluster a ticket's description already shows
// (the "Cluster: `...`" line the sync renders above the metadata block), or
// "" when it shows none. Used only to keep that line stable in a run whose
// cluster lookup failed.
func storedCluster(description string) string {
	if start := findMetadataBlockStart(description); start >= 0 {
		description = description[:start]
	}
	for line := range strings.SplitSeq(description, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Cluster: `")
		if !ok {
			continue
		}
		if value, _, ok := strings.Cut(rest, "`"); ok {
			return markdownEscapePattern.ReplaceAllString(value, "$1")
		}
	}
	return ""
}

// issueDescriptionWithIdentity renders the managed description with an
// explicit metadata identity ("" omits the line).
func issueDescriptionWithIdentity(sourceCfg config.SourceConfig, managedLabels []string, finding model.Finding, identity string) string {
	issueURL := finding.IssueURL
	if issueURL == "" {
		issueURL = finding.IssueAPIURL
	}

	sourceFileLabel, sourceFileURL := sourceFileLink(sourceCfg, finding)
	sourceCommitURL := sourceCommitLink(sourceCfg, finding)
	repositoryURL := repositoryLink(sourceCfg, finding)
	targetFileURL := projectTargetFileLink(sourceCfg, finding)

	lines := []string{
		fmt.Sprintf("## %s [%s]", strings.TrimSpace(finding.IssueTitle), strings.ToUpper(strings.TrimSpace(finding.Severity))),
	}

	// Kubernetes deployment context leads: knowing which cluster and
	// namespace a workload runs in is the first thing a triager needs for
	// container/kubernetes findings, before the image or target file.
	if finding.ProjectCluster != "" {
		lines = append(lines, fmt.Sprintf("Cluster: `%s`", finding.ProjectCluster))
	}
	if finding.ProjectNamespace != "" {
		lines = append(lines, fmt.Sprintf("Namespace: `%s`", finding.ProjectNamespace))
	}
	if finding.Repository != "" {
		if repositoryURL != "" {
			lines = append(lines, fmt.Sprintf("Repository: [%s](%s)", finding.Repository, repositoryURL))
		} else {
			lines = append(lines, fmt.Sprintf("Repository: %s", finding.Repository))
		}
	}
	if finding.ProjectReference != "" {
		refLine := fmt.Sprintf("Ref: `%s`", finding.ProjectReference)
		if sourceCommitURL != "" {
			refLine += fmt.Sprintf(" at [`%s`](%s)", shortCommit(finding.SourceCommitID), sourceCommitURL)
		} else if finding.SourceCommitID != "" {
			refLine += fmt.Sprintf(" at `%s`", shortCommit(finding.SourceCommitID))
		}
		lines = append(lines, refLine)
	} else if finding.SourceCommitID != "" {
		if sourceCommitURL != "" {
			lines = append(lines, fmt.Sprintf("Commit: [`%s`](%s)", shortCommit(finding.SourceCommitID), sourceCommitURL))
		} else {
			lines = append(lines, fmt.Sprintf("Commit: `%s`", shortCommit(finding.SourceCommitID)))
		}
	}

	if finding.SourceFile != "" {
		if sourceFileURL != "" {
			lines = append(lines, fmt.Sprintf("File: [%s](%s)", sourceFileLabel, sourceFileURL))
		} else {
			lines = append(lines, fmt.Sprintf("File: `%s`", sourceFileLabel))
		}
	} else if finding.ProjectTargetFile != "" {
		if targetFileURL != "" {
			lines = append(lines, fmt.Sprintf("Target file: [%s](%s)", finding.ProjectTargetFile, targetFileURL))
		} else {
			lines = append(lines, fmt.Sprintf("Target file: `%s`", finding.ProjectTargetFile))
		}
	}

	lines = append(lines, "")
	if issueURL != "" {
		lines = append(lines, fmt.Sprintf("Snyk: [Open issue](%s)", issueURL))
	}
	if finding.IssueAPIURL != "" {
		lines = append(lines, fmt.Sprintf("API: [Issue details](%s)", finding.IssueAPIURL))
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Status: `%s`", statusDisplayName(finding.Status)))
	if finding.PackageName != "" {
		lines = append(lines, fmt.Sprintf("Package: `%s`", finding.PackageName))
	}
	if finding.IntroducedThrough != "" {
		lines = append(lines, fmt.Sprintf("Introduced through: `%s`", finding.IntroducedThrough))
	}
	if finding.VulnerableVersion != "" {
		lines = append(lines, fmt.Sprintf("Vulnerable version: `%s`", finding.VulnerableVersion))
	}
	if finding.FixedVersion != "" {
		lines = append(lines, fmt.Sprintf("Fix version: `%s`", finding.FixedVersion))
	}
	if summary := fixAvailabilitySummary(finding); summary != "" {
		lines = append(lines, fmt.Sprintf("Fix availability: `%s`", summary))
	}
	if finding.ExploitMaturity != "" {
		lines = append(lines, fmt.Sprintf("Exploit maturity: `%s`", finding.ExploitMaturity))
	}
	if finding.CVSS > 0 {
		lines = append(lines, fmt.Sprintf("CVSS: `%.1f`", finding.CVSS))
	}
	if ids := classIDs(finding.Classes); len(ids) > 0 {
		lines = append(lines, fmt.Sprintf("CWE: `%s`", strings.Join(ids, ", ")))
	}
	if ids := finding.CVEs; len(ids) > 0 {
		lines = append(lines, fmt.Sprintf("CVE: `%s`", strings.Join(ids, ", ")))
	}

	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("Project: `%s` (`%s`)", finding.ProjectName, finding.ProjectID))
	lines = append(lines, fmt.Sprintf("Issue ID: `%s`", finding.SnykIssueID))
	if finding.SnykIssueKey != "" {
		lines = append(lines, fmt.Sprintf("Issue key: `%s`", finding.SnykIssueKey))
	}
	if finding.ProjectOrigin != "" {
		lines = append(lines, fmt.Sprintf("Project origin: `%s`", finding.ProjectOrigin))
	}

	if finding.Description != "" {
		lines = append(lines, "", "### Description", embedSnykProse(finding.Description))
	}
	if finding.Remediation != "" {
		lines = append(lines, "", "### Remediation", embedSnykProse(finding.Remediation))
	}

	lines = append(lines, "", metadataBlock(managedMetadata{
		Fingerprint:   finding.Fingerprint,
		Identity:      identity,
		ManagedLabels: managedLabels,
		AwaitingFix:   finding.Status == model.FindingAwaitingFix,
	}))
	return strings.Join(lines, "\n")
}

// maxEmbeddedProseRunes bounds how much of Snyk's free-text Description and
// Remediation fields is embedded verbatim in the ticket description. Linear
// enforces its own description length limit; since the managed metadata
// block (containing the fingerprint used for deduplication) is always
// appended LAST, unbounded Snyk prose would risk pushing that block past
// Linear's limit and silently truncating it away. That would make the
// ticket unmanaged (extractFingerprint finds nothing) and cause the next run
// to create a duplicate. The cap is conservative relative to Linear's limit
// so the metadata block always survives.
const maxEmbeddedProseRunes = 10000

// truncationMarker is appended when embedded Snyk prose is truncated, so
// readers understand why the text is cut off.
const truncationMarker = "\n\n_[truncated by snyk-linear-sync]_"

// embedSnykProse prepares Snyk-controlled free text (finding.Description or
// finding.Remediation) for embedding in a ticket description. It:
//
//  1. Sanitizes any HTML-comment opening ("<!--") so embedded text cannot
//     look like a second "<!-- snyk-linear-sync ... -->" metadata block.
//     extractFingerprint/extractManagedLabels already defend against this by
//     always taking the LAST line-anchored marker (see
//     findMetadataBlockStart), but neutralizing the marker at the source
//     removes the ambiguity for human readers and any other tooling that
//     might scan the description.
//  2. Caps the length so the text can never grow large enough to push the
//     metadata block (appended after it) out of what Linear accepts.
//
// Both steps are pure functions of the input finding text, so calling
// embedSnykProse twice with the same Snyk data always produces byte-identical
// output — required for the sync's compare/hash pipeline
// (normalizeDescriptionForCompare, desiredIssueHash) to stay stable and not
// churn tickets between runs when nothing has actually changed.
func embedSnykProse(text string) string {
	return truncateProse(sanitizeSnykProse(text), maxEmbeddedProseRunes)
}

// sanitizeSnykProse neutralizes HTML-comment openings in Snyk-controlled
// free text by replacing "<!--" with "<!- -". The replacement never
// reintroduces the substring "<!--" (the inserted space always separates the
// two dashes), so the transformation is idempotent:
// sanitizeSnykProse(sanitizeSnykProse(x)) == sanitizeSnykProse(x).
func sanitizeSnykProse(text string) string {
	return strings.ReplaceAll(text, "<!--", "<!- -")
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

func issueTitleSubject(finding model.Finding) string {
	switch {
	case strings.TrimSpace(finding.SourceFile) != "":
		return path.Base(strings.TrimSpace(finding.SourceFile))
	case strings.TrimSpace(finding.PackageName) != "":
		return strings.TrimSpace(finding.PackageName)
	case strings.TrimSpace(finding.ProjectTargetFile) != "":
		return strings.TrimSpace(finding.ProjectTargetFile)
	case strings.TrimSpace(finding.ProjectName) != "":
		return strings.TrimSpace(finding.ProjectName)
	default:
		return ""
	}
}

func issueTitleContext(finding model.Finding) string {
	repository := strings.TrimSpace(finding.Repository)
	reference := strings.TrimSpace(finding.ProjectReference)

	switch {
	case strings.TrimSpace(finding.SourceFile) != "" && repository != "":
		return repository
	case strings.TrimSpace(finding.ProjectTargetFile) != "" && repository == "" && reference != "":
		return reference
	case repository != "":
		return repository
	case reference != "":
		return reference
	default:
		return ""
	}
}

// managedMetadata is the content of the hidden metadata block the sync keeps
// at the end of every managed ticket description. Linear is the only durable
// state the sync has, so anything a later run must know about a ticket lives
// here (the SQLite cache is only a performance hint).
type managedMetadata struct {
	// Fingerprint is the exact join key to the Snyk finding.
	Fingerprint string
	// Identity is the project-independent finding identity (see
	// model.FindingIdentity), used to rebind the ticket if Snyk recreates
	// its project under a new ID. Empty when unknown; the line is omitted.
	Identity string
	// ManagedLabels is the label set the sync owns on this ticket.
	ManagedLabels []string
	// ClosedReason records that the sync cancelled the ticket only because
	// its Snyk project went missing or was deactivated. Written by the
	// resolve loop; a normal update from a live finding never carries it, so
	// it is cleared as soon as the ticket tracks an active finding again.
	ClosedReason string
	// AwaitingFix marks the ticket as parked in Backlog for a finding Snyk
	// reports as "ignored until fix available". Unlike the awaiting-fix
	// label it is written regardless of the label configuration, so the
	// sync can recognize its own Backlog placement even when
	// LINEAR_AWAITING_FIX_LABEL is off. It is cleared as soon as the
	// finding leaves the awaiting-fix state.
	AwaitingFix bool
}

func metadataBlock(meta managedMetadata) string {
	lines := []string{
		"<!-- snyk-linear-sync",
		fmt.Sprintf("fingerprint: %s", meta.Fingerprint),
	}
	if meta.Identity != "" {
		lines = append(lines, fmt.Sprintf("identity: %s", meta.Identity))
	}
	if labels := model.NormalizeManagedLabelNames(meta.ManagedLabels); len(labels) > 0 {
		lines = append(lines, fmt.Sprintf("managed_labels: %s", strings.Join(labels, ",")))
	}
	if meta.ClosedReason != "" {
		lines = append(lines, fmt.Sprintf("closed_reason: %s", meta.ClosedReason))
	}
	if meta.AwaitingFix {
		lines = append(lines, "awaiting_fix: yes")
	}
	lines = append(lines, "-->")
	return strings.Join(lines, "\n")
}

// classIDs returns the durable identifiers (e.g. "CWE-22") for a set of
// Snyk weakness classes, preserving Snyk's ordering without deduplication.
func classIDs(classes []model.IssueClass) []string {
	out := make([]string, 0, len(classes))
	for _, class := range classes {
		id := strings.TrimSpace(class.ID)
		if id == "" {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fixAvailabilitySummary reduces the per-coordinate is_fixable_* flags into a
// concise, human-readable summary. It returns an empty string when Snyk did
// not report any coordinates for the finding, so the line is omitted rather
// than implying a fixability state the upstream data does not support.
func fixAvailabilitySummary(finding model.Finding) string {
	if !finding.HasCoordinates {
		return ""
	}
	var parts []string
	if finding.IsFixableSnyk {
		parts = append(parts, "Snyk automatic fix")
	}
	if finding.IsFixableUpstream {
		parts = append(parts, "upstream fix available")
	}
	if finding.IsUpgradeable {
		parts = append(parts, "upgrade available")
	}
	if finding.IsPinnable {
		parts = append(parts, "pin available")
	}
	if finding.IsFixableManually {
		parts = append(parts, "manual fix")
	}
	if finding.IsPatchable {
		parts = append(parts, "patch available")
	}
	if len(parts) == 0 {
		return "no fix available"
	}
	return strings.Join(parts, ", ")
}

func issueState(status model.FindingStatus) model.IssueState {
	switch status {
	case model.FindingAwaitingFix:
		return model.StateBacklog
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
		return "Snyk reports this finding as open"
	case model.FindingAwaitingFix:
		return "Snyk reports this issue as ignored until a fix is available"
	case model.FindingSnoozed:
		return "Snyk reports this issue as temporarily deferred"
	case model.FindingIgnored:
		return "Snyk reports this issue as permanently ignored"
	case model.FindingFixed:
		return "Snyk reports this finding as fixed"
	default:
		return ""
	}
}

// statusDisplayName renders the FindingStatus value for the Linear issue
// description. The raw constant values are code-internal; the description
// should show what Snyk actually reports.
func statusDisplayName(status model.FindingStatus) string {
	switch status {
	case model.FindingAwaitingFix:
		return "ignored (no fix available)"
	case model.FindingSnoozed:
		return "snoozed"
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

// issueDueDate calculates the due date for a finding. usedUpdatedAtFallback
// reports whether the updated_at re-detection fallback (below) supplied the
// base date, as opposed to the issue's original created_at or ignore expiry.
// The match loop uses this to keep the due date sticky against an existing
// Linear ticket's due date once set (see the sticky-override comment in
// Service.Run), since Snyk bumps updated_at on routine re-scans — not just
// genuine re-detections — which would otherwise advance the due date every
// run once the fallback triggers.
func issueDueDate(dueCfg config.DueDateConfig, finding model.Finding) (effective, base, reason string, usedUpdatedAtFallback bool) {
	var baseDate time.Time
	var basis string
	switch {
	case !finding.IgnoreExpiresAt.IsZero():
		expiresUTC := finding.IgnoreExpiresAt.UTC()
		baseDate = time.Date(expiresUTC.Year(), expiresUTC.Month(), expiresUTC.Day(), 0, 0, 0, 0, time.UTC)
		basis = "ignore expiry"
	case !finding.CreatedAt.IsZero():
		createdAtUTC := finding.CreatedAt.UTC()
		baseDate = time.Date(createdAtUTC.Year(), createdAtUTC.Month(), createdAtUTC.Day(), 0, 0, 0, 0, time.UTC)
		basis = "issue creation"

		// Snyk reuses issue IDs when the same vulnerability class reappears on
		// different code in the same project. The created_at reflects the
		// ORIGINAL occurrence, not the current one. If updated_at is
		// significantly newer than created_at (more than the SLA window),
		// the issue was likely reused for a new occurrence and the SLA
		// clock should restart from updated_at — otherwise a freshly-
		// detected occurrence gets a months-old due date and is immediately
		// past due despite the code being only days old.
		//
		// The SLA-window threshold avoids false positives from routine
		// re-scans that bump updated_at by a day or two, and avoids daily
		// churn: once updated_at stabilizes, the due date is stable too.
		if !finding.UpdatedAt.IsZero() {
			slaDays := severitySLADays(dueCfg, finding.Severity)
			updatedAtUTC := finding.UpdatedAt.UTC()
			if slaDays > 0 && updatedAtUTC.Sub(createdAtUTC) > time.Duration(slaDays)*24*time.Hour {
				baseDate = time.Date(updatedAtUTC.Year(), updatedAtUTC.Month(), updatedAtUTC.Day(), 0, 0, 0, 0, time.UTC)
				basis = "issue re-detection (updated_at)"
				usedUpdatedAtFallback = true
			}
		}
	default:
		return "", "", "", false
	}

	effective, base, reason = dueDateFromBase(baseDate, basis, dueCfg, finding)
	return effective, base, reason, usedUpdatedAtFallback
}

// severitySLADays returns the SLA day count for a given severity, or 0 if
// the severity is unknown. Used by issueDueDate to detect issue-ID reuse:
// if updated_at exceeds created_at by more than the SLA window, the issue
// was likely reused for a new occurrence and the SLA clock should restart.
func severitySLADays(dueCfg config.DueDateConfig, severity string) int {
	switch issuePriority(severity) {
	case 1:
		return dueCfg.CriticalDays
	case 2:
		return dueCfg.HighDays
	case 3:
		return dueCfg.MediumDays
	case 4:
		return dueCfg.LowDays
	default:
		return 0
	}
}

// dueDateFromBase calculates the due date from a given base date, severity,
// and SLA offsets. It returns the same value for both the effective due date
// and the cache base so that past-SLA dates remain stable.
func dueDateFromBase(baseDate time.Time, basis string, dueCfg config.DueDateConfig, finding model.Finding) (effective, base, reason string) {
	days := severitySLADays(dueCfg, finding.Severity)
	if days == 0 {
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
	// spurious update even when the underlying Snyk data was unchanged.

	return dueDateStr, dueDateStr, reason
}

// issueDueDateFromFixAvailability calculates a due date for an issue that has
// just become actionable after being blocked on an upstream fix. Unlike
// issueDueDate which uses the Snyk created_at as the base, this uses today
// as the base — the SLA clock starts when the fix becomes available, not
// when the issue was originally found (which would give a meaningless past
// date because the team couldn't act on it while no fix existed).
func issueDueDateFromFixAvailability(dueCfg config.DueDateConfig, finding model.Finding) (string, string, string) {
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

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dueDate := today.AddDate(0, 0, days)
	dueDateStr := dueDate.Format(time.DateOnly)

	severityName := strings.ToLower(strings.TrimSpace(finding.Severity))
	if severityName == "" {
		severityName = "unknown"
	}
	reason := fmt.Sprintf("%s severity SLA: %d days from fix availability", severityName, days)

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
	if desired.State != model.StateDone && desired.State != model.StateCancelled {
		return false
	}
	return model.NormalizeWorkflowStateName(existing.StateName) != model.NormalizeWorkflowStateName(model.StateName(desired.State))
}

func needsUpdate(existing model.ExistingIssue, desired model.DesiredIssue, states config.StateConfig) bool {
	return ComputeDiff(existing, desired, states).HasChanges()
}

// ComputeDiff returns a diff describing which managed fields changed between
// the existing and desired Linear issue. The caller is responsible for only
// displaying a change when the corresponding field is non-empty (e.g. a
// resolved issue may carry the existing issue's title and description).
// states is used for the terminal→non-terminal reopen guard.
func ComputeDiff(existing model.ExistingIssue, desired model.DesiredIssue, states config.StateConfig) *model.IssueDiff {
	d := &model.IssueDiff{}

	if existing.Title != desired.Title {
		d.TitleChanged = true
		d.TitleFrom = existing.Title
		d.TitleTo = desired.Title
	}

	existingDescription := normalizeDescriptionForCompare(existing.Description)
	desiredDescription := normalizeDescriptionForCompare(desired.Description)
	if existingDescription != desiredDescription {
		d.DescriptionChanged = true
		d.MetadataOnlyDescriptionChange = withoutMetadataBlock(existingDescription) == withoutMetadataBlock(desiredDescription)
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
		if desired.RestoreState.ID != "" {
			desiredNorm = model.NormalizeWorkflowStateName(desired.RestoreState.Name)
		}
		if existingNorm != desiredNorm {
			// Defense in depth: never report a terminal→non-terminal state
			// change as an update. The match-layer reopen guard should
			// prevent us from ever reaching here with a terminal existing
			// issue and a non-terminal desired state, but if a caller
			// bypasses that guard (or a future refactor introduces one),
			// suppress the state change rather than reopening a closed
			// ticket. The description/labels/title can still update.
			// The only exception is a reopen the match loop decided on
			// deliberately (desired.Reopen), after its own checks.
			if isTerminalLinearState(existing, states) && isNonTerminalModelState(desired.State) && !desired.Reopen {
				// Deliberately do not set d.StateChanged.
			} else {
				d.StateChanged = true
				d.StateFrom = existing.StateName
				d.StateTo = desiredNorm
				if desired.RestoreState.ID != "" {
					d.StateTo = desired.RestoreState.Name
				}
			}
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

func missingFindingState(fingerprint string, activeProjects map[string]struct{}, inactiveProjects map[string]struct{}) (model.IssueState, string) {
	projectID, ok := FingerprintProjectID(fingerprint)
	if !ok {
		return model.StateDone, "this Snyk finding is no longer present"
	}
	if _, exists := activeProjects[projectID]; exists {
		return model.StateDone, "this Snyk finding is no longer present"
	}
	if _, exists := inactiveProjects[projectID]; exists {
		return model.StateCancelled, "the Snyk project has been deactivated"
	}
	// Both deleted and inactive projects result in Cancelled: the issue is no
	// longer actionable regardless of why the project stopped producing findings.
	return model.StateCancelled, "the Snyk project no longer exists"
}

// projectClosedReason returns the metadata closed_reason for a ticket the
// resolve loop cancels because its Snyk project is gone or deactivated, or ""
// when the project is still active (the ticket is Done because the finding
// is no longer present, which is a fix, not a machine-made closure).
func projectClosedReason(fingerprint string, activeProjects map[string]struct{}, inactiveProjects map[string]struct{}) string {
	projectID, ok := FingerprintProjectID(fingerprint)
	if !ok {
		return ""
	}
	if _, exists := activeProjects[projectID]; exists {
		return ""
	}
	if _, exists := inactiveProjects[projectID]; exists {
		return model.ClosedReasonProjectDeactivated
	}
	return model.ClosedReasonProjectMissing
}

// rebindCandidatesByIdentity indexes, by identity, the existing tickets that
// a finding may take over after Snyk recreates a project under a new project
// ID. A ticket qualifies only when all of these hold:
//
//   - it carries an identity (tickets gain one on their first update);
//   - no current finding claims its fingerprint exactly;
//   - its fingerprint's project is NOT in the active project set. Two live
//     projects scanning the same target must keep separate tickets, so a
//     ticket whose project still reports findings is never taken;
//   - it is not archived (Linear does not allow updating archived issues);
//   - it is either still open (its project vanished this run and the resolve
//     loop would cancel it), or it sits in the Cancelled state with a
//     sync-recorded project-missing/project-deactivated closed_reason. A
//     ticket closed by a fix or by a person is never reused: that closure is
//     a decision, and the reopen guard keeps protecting it.
//
// Each identity's candidates are sorted deterministically: open tickets
// first, then the most recently created. takeRebindCandidate hands them out
// one at a time, so a candidate is used at most once and any surplus is left
// to the normal flow (the resolve loop cancels open ones as before).
func rebindCandidatesByIdentity(existingByFingerprint map[string]model.ExistingIssue, findingFingerprints map[string]struct{}, activeProjects map[string]struct{}, states config.StateConfig) map[string][]model.ExistingIssue {
	out := map[string][]model.ExistingIssue{}
	for fingerprint, issue := range existingByFingerprint {
		if issue.Identity == "" || issue.ArchivedAt != nil {
			continue
		}
		if _, claimed := findingFingerprints[fingerprint]; claimed {
			continue
		}
		projectID, ok := FingerprintProjectID(fingerprint)
		if !ok {
			continue
		}
		if _, active := activeProjects[projectID]; active {
			continue
		}
		if isTerminalLinearState(issue, states) && !isSyncCancelledForMissingProject(issue, states) {
			continue
		}
		out[issue.Identity] = append(out[issue.Identity], issue)
	}
	for identity := range out {
		slices.SortFunc(out[identity], func(a, b model.ExistingIssue) int {
			aOpen, bOpen := isNonTerminalLinearState(a, states), isNonTerminalLinearState(b, states)
			if aOpen != bOpen {
				if aOpen {
					return -1
				}
				return 1
			}
			if c := compareCreatedDesc(a.CreatedAt, b.CreatedAt); c != 0 {
				return c
			}
			if c := identifierNum(b.Identifier) - identifierNum(a.Identifier); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	return out
}

// rebindCandidateClusters returns the distinct clusters shown by rebind
// candidates' descriptions ("" for candidates showing none).
func rebindCandidateClusters(candidates map[string][]model.ExistingIssue) map[string]struct{} {
	out := map[string]struct{}{}
	for _, queue := range candidates {
		for _, issue := range queue {
			out[storedCluster(issue.Description)] = struct{}{}
		}
	}
	return out
}

// deferCreateWindow caps how long a finding whose cluster lookup keeps
// failing can be held back for a possible rebind: only candidates that are
// still open, or were closed less than this long ago, justify a deferral.
// A persistent lookup failure therefore never hides a finding for more than
// about a day; after that it gets a ticket without an identity as usual.
const deferCreateWindow = 24 * time.Hour

// plausibleRebindCandidate reports whether a finding whose cluster lookup
// failed would have a recent rebind candidate under the cluster one of the
// candidates records. It never binds anything: it only decides whether
// creating a fresh ticket now could pre-empt a correct rebind next run.
// Only candidates that are non-terminal, or were closed within
// deferCreateWindow of now, count; a terminal candidate with no recorded
// closed time does not, so visibility wins when the timeline is unknown.
func plausibleRebindCandidate(candidates map[string][]model.ExistingIssue, clusters map[string]struct{}, finding model.Finding, states config.StateConfig, now time.Time) bool {
	for cluster := range clusters {
		probe := finding
		probe.ProjectClusterUnknown = false
		probe.ProjectCluster = cluster
		identity := model.FindingIdentity(probe)
		if identity == "" {
			continue
		}
		for _, candidate := range candidates[identity] {
			if isNonTerminalLinearState(candidate, states) {
				return true
			}
			if candidate.ClosedAt != nil && now.Sub(*candidate.ClosedAt) < deferCreateWindow {
				return true
			}
		}
	}
	return false
}

// compareCreatedDesc orders newer creation times first; unknown times last.
func compareCreatedDesc(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	default:
		return b.Compare(*a)
	}
}

// takeRebindCandidate removes and returns the best remaining rebind
// candidate for identity, if any.
func takeRebindCandidate(candidates map[string][]model.ExistingIssue, identity string) (model.ExistingIssue, bool) {
	if identity == "" {
		return model.ExistingIssue{}, false
	}
	queue := candidates[identity]
	if len(queue) == 0 {
		return model.ExistingIssue{}, false
	}
	candidates[identity] = queue[1:]
	return queue[0], true
}

// isSyncCancelledForMissingProject reports whether a ticket is Cancelled with
// a closed_reason the sync records only when it cancels a ticket because its
// Snyk project went missing or was deactivated. A person moving such a ticket
// to Done (or anywhere else) is a decision the sync respects, so the state
// must still be the configured Cancelled state.
func isSyncCancelledForMissingProject(issue model.ExistingIssue, states config.StateConfig) bool {
	if model.NormalizeWorkflowStateName(issue.StateName) != model.NormalizeWorkflowStateName(states.Cancelled) {
		return false
	}
	switch issue.ClosedReason {
	case model.ClosedReasonProjectMissing, model.ClosedReasonProjectDeactivated:
		return true
	default:
		return false
	}
}

// continuouslyOpenSinceTicketCreated reports whether an exactly-matched
// terminal ticket may be reopened for an open finding, because Snyk shows
// the finding has stayed open for the ticket's whole life: its closure (in
// practice by an automation outside the sync) was premature, not a fix.
//
// This does not reintroduce the #28 zombie-ticket bug. #28 was Snyk reusing
// an issue ID for a different occurrence, and a reuse always starts with
// Snyk resolving the earlier occurrence, which Snyk keeps as the
// coordinate's last_resolved_at. Requiring that no resolution happened since
// the ticket was created therefore excludes every reuse the ticket could
// have tracked. The comparison is against the ticket's creation, not its
// closure: when Snyk resolves a finding the sync closes the ticket on a
// later run, so that resolution always predates the closure, and comparing
// against the closure would reopen exactly the tickets #28 protects.
//
// Every uncertainty keeps today's behavior (a fresh ticket):
//   - coarse fingerprints (no location segment) can span several code
//     occurrences, so their history says nothing about this one;
//   - archived tickets cannot be updated;
//   - a missing creation or closed time leaves the timeline unknown;
//   - an unparsable last_resolved_at leaves the resolution history unknown.
func continuouslyOpenSinceTicketCreated(existing model.ExistingIssue, finding model.Finding) bool {
	if model.FingerprintLocation(finding.Fingerprint) == "" {
		return false
	}
	if existing.ArchivedAt != nil || existing.ClosedAt == nil || existing.CreatedAt == nil {
		return false
	}
	if finding.LastResolvedAtInvalid {
		return false
	}
	if finding.LastResolvedAt.IsZero() {
		return true
	}
	return finding.LastResolvedAt.Before(*existing.CreatedAt)
}

// reopenCandidate picks the ticket the premature-closure reopen check is run
// against: the most recently created non-archived terminal ticket with the
// finding's exact fingerprint. That is the copy most likely to have tracked
// the current open period, and the only one that can pass the created-time
// check when earlier copies predate a Snyk resolution. On success it stores
// the ticket in *existing and returns true.
func reopenCandidate(terminal []model.ExistingIssue, finding model.Finding, existing *model.ExistingIssue) bool {
	if len(terminal) == 0 {
		return false
	}
	newest := terminal[0]
	for _, issue := range terminal[1:] {
		c := compareCreatedDesc(issue.CreatedAt, newest.CreatedAt)
		if c < 0 || (c == 0 && identifierNum(issue.Identifier) > identifierNum(newest.Identifier)) {
			newest = issue
		}
	}
	if !continuouslyOpenSinceTicketCreated(newest, finding) {
		return false
	}
	*existing = newest
	return true
}

// restoreReopenedStates points every deliberate reopen at the open workflow
// state its ticket was in before it was closed, instead of the configured
// open state. A sync-cancelled ticket had usually been triaged already (moved
// to Todo, or into progress) before Snyk deactivated or recreated its
// project, and sending it back to Triage throws that triage away. The state
// comes from the ticket's Linear history; when it cannot be read the reopen
// still happens, into the configured open state as before.
func (s *Service) restoreReopenedStates(ctx context.Context, desiredByFingerprint map[string]model.DesiredIssue, matchedExisting map[string]model.ExistingIssue) {
	var issueIDs []string
	for fingerprint, desired := range desiredByFingerprint {
		if desired.Reopen {
			issueIDs = append(issueIDs, matchedExisting[fingerprint].ID)
		}
	}
	slices.Sort(issueIDs)

	states, err := s.linear.LastOpenStates(ctx, issueIDs)
	if err != nil {
		s.logger.Warn("could not read Linear history for some reopened tickets; reopening them in the configured open state",
			slog.Int("reopened", len(issueIDs)),
			slog.Int("states_found", len(states)),
			slog.Any("error", err),
		)
	}
	for fingerprint, desired := range desiredByFingerprint {
		if !desired.Reopen {
			continue
		}
		existing := matchedExisting[fingerprint]
		state, ok := states[existing.ID]
		if !ok {
			continue
		}
		desired.RestoreState = state
		desired.StateReason += "; back in the state it was in before it was closed"
		desiredByFingerprint[fingerprint] = desired
		s.logger.Info("restoring reopened ticket to its state before it was closed",
			slog.String("existing", existing.Identifier),
			slog.String("state", state.Name),
		)
	}
}

// FingerprintProjectID extracts the project ID portion of a Snyk fingerprint.
func FingerprintProjectID(fingerprint string) (string, bool) {
	const prefix = "snyk:"
	if !strings.HasPrefix(fingerprint, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(fingerprint, prefix)
	projectID, _, ok := strings.Cut(rest, ":")
	if !ok || strings.TrimSpace(projectID) == "" {
		return "", false
	}
	return projectID, true
}

func normalizeDescriptionForCompare(description string) string {
	description = strings.TrimSpace(strings.ReplaceAll(description, "\r\n", "\n"))
	description = blankLineRunPattern.ReplaceAllString(description, "\n")
	description = linearAutoLinkPattern.ReplaceAllString(description, "[$1]($2)")
	description = markdownEscapePattern.ReplaceAllString(description, "$1")
	// Linear re-serializes underscore emphasis as asterisks (__x__ -> **x**,
	// _x_ -> *x*). Mapping '*' to '_' on both sides makes the written and
	// stored forms compare equal; it can only ever suppress a spurious
	// difference, never hide a real one, because it is applied to both sides.
	description = strings.ReplaceAll(description, "*", "_")
	description = strings.ReplaceAll(description, "DO NOT EDIT OR REMOVE THIS BLOCK. Used by snyk-linear-sync for deduplication.", "__SNYK_LINEAR_METADATA_WARNING__")
	description = strings.ReplaceAll(description, "DO NOT EDIT, REMOVE, OR REFORMAT THIS BLOCK. It is required by snyk-linear-sync for deduplication and safe updates.", "__SNYK_LINEAR_METADATA_WARNING__")
	return withoutScanPins(description)
}

// withoutScanPins blanks out the scanned commit and the flagged line region in
// the Ref, Commit and File lines. Snyk rescans a repository on every push to
// the monitored branch, and each rescan reports the new commit and often new
// line numbers for every Snyk Code finding in it, with nothing else changed.
// Comparing those would rewrite every Snyk Code ticket of the repository, most
// of them long closed, after every merge. The links a ticket keeps are
// permalinks to the commit they name, so they stay valid; they are refreshed
// whenever anything else about the finding changes.
func withoutScanPins(description string) string {
	return scanPinnedLinePattern.ReplaceAllStringFunc(description, func(line string) string {
		line = scannedCommitPattern.ReplaceAllString(line, "${1}`<commit>`")
		line = commitPathPattern.ReplaceAllString(line, "/$1/<commit>")
		line = sourceRegionPattern.ReplaceAllString(line, "(line <region>)")
		return lineAnchorPattern.ReplaceAllString(line, "#L<line>")
	})
}

func sourceFileLink(sourceCfg config.SourceConfig, finding model.Finding) (string, string) {
	if sourceCfg.Provider != "github" {
		return "", ""
	}
	if strings.TrimSpace(finding.Repository) == "" || strings.TrimSpace(finding.SourceFile) == "" || strings.TrimSpace(finding.SourceCommitID) == "" {
		return "", ""
	}

	link := &url.URL{
		Scheme:   "https",
		Host:     "github.com",
		Path:     fmt.Sprintf("/%s/blob/%s/%s", finding.Repository, finding.SourceCommitID, finding.SourceFile),
		Fragment: githubLineAnchor(finding),
	}

	label := finding.SourceFile
	if finding.SourceLineStart > 0 {
		label = fmt.Sprintf("%s (%s)", finding.SourceFile, sourceRegionString(finding))
	}
	return label, link.String()
}

func sourceCommitLink(sourceCfg config.SourceConfig, finding model.Finding) string {
	if sourceCfg.Provider != "github" {
		return ""
	}
	if strings.TrimSpace(finding.Repository) == "" || strings.TrimSpace(finding.SourceCommitID) == "" {
		return ""
	}

	link := &url.URL{
		Scheme: "https",
		Host:   "github.com",
		Path:   fmt.Sprintf("/%s/commit/%s", finding.Repository, finding.SourceCommitID),
	}
	return link.String()
}

func projectTargetFileLink(sourceCfg config.SourceConfig, finding model.Finding) string {
	if sourceCfg.Provider != "github" {
		return ""
	}
	if strings.TrimSpace(finding.Repository) == "" || strings.TrimSpace(finding.ProjectReference) == "" || strings.TrimSpace(finding.ProjectTargetFile) == "" {
		return ""
	}

	link := &url.URL{
		Scheme: "https",
		Host:   "github.com",
		Path:   fmt.Sprintf("/%s/blob/%s/%s", finding.Repository, finding.ProjectReference, finding.ProjectTargetFile),
	}
	return link.String()
}

func repositoryLink(sourceCfg config.SourceConfig, finding model.Finding) string {
	if sourceCfg.Provider != "github" {
		return ""
	}
	if strings.TrimSpace(finding.Repository) == "" {
		return ""
	}

	link := &url.URL{
		Scheme: "https",
		Host:   "github.com",
		Path:   fmt.Sprintf("/%s", finding.Repository),
	}
	return link.String()
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}

func githubLineAnchor(finding model.Finding) string {
	if finding.SourceLineStart <= 0 {
		return ""
	}
	if finding.SourceLineEnd > finding.SourceLineStart {
		return fmt.Sprintf("L%d-L%d", finding.SourceLineStart, finding.SourceLineEnd)
	}
	return fmt.Sprintf("L%d", finding.SourceLineStart)
}

// isConfiguredBacklogState returns true if the existing Linear issue state name
// matches the configured Backlog state (case-insensitive, with normalization
// for common variants like "Canceled" → "Cancelled").
func isConfiguredBacklogState(existingStateName, configuredBacklog string) bool {
	return model.NormalizeWorkflowStateName(existingStateName) == model.NormalizeWorkflowStateName(configuredBacklog)
}

// wasAwaitingFix reports whether the existing Linear issue was previously in
// the awaiting-fix state. It reads the awaiting_fix marker in the metadata
// block, which the sync writes whenever a finding is in the awaiting-fix
// state regardless of the label configuration, and also accepts the
// configured awaiting-fix label for tickets written before the marker
// existed. This detects issues that were blocked on an upstream fix and have
// now become actionable.
func wasAwaitingFix(existing model.ExistingIssue, awaitingFixLabel string) bool {
	if existing.AwaitingFix {
		return true
	}
	if awaitingFixLabel == "" {
		return false
	}
	normalized := model.NormalizeLabelName(awaitingFixLabel)
	return slices.Contains(model.NormalizeManagedLabelNames(existing.ManagedLabels), normalized)
}

// isNonTerminalModelState reports whether the desired model state is
// non-terminal. Todo and Backlog are non-terminal; Done and Cancelled are
// terminal. When the sync wants a terminal state the transition must always
// be allowed (handled by pendingTerminalTransition), so PreserveState only
// applies to non-terminal desired states.
func isNonTerminalModelState(state model.IssueState) bool {
	return state == model.StateTodo || state == model.StateBacklog
}

// isNonTerminalLinearState reports whether the existing Linear issue is NOT
// in a terminal state (not Done/Cancelled and not archived). Users can freely
// move issues between non-terminal states as part of triage; the sync should
// not override those manual decisions.
func isNonTerminalLinearState(existing model.ExistingIssue, states config.StateConfig) bool {
	return !isTerminalLinearState(existing, states)
}

// isTerminalLinearState reports whether the existing Linear issue is in a
// terminal state — either a configured Done/Cancelled workflow state, or
// archived (auto-archived tickets are always terminal). A terminal ticket
// must never be reopened by the sync; if Snyk re-reports an issue that maps
// to a closed or archived ticket, a fresh ticket should be created instead.
func isTerminalLinearState(existing model.ExistingIssue, states config.StateConfig) bool {
	if existing.ArchivedAt != nil {
		return true
	}
	normalized := model.NormalizeWorkflowStateName(existing.StateName)
	if normalized == model.NormalizeWorkflowStateName(states.Done) {
		return true
	}
	if normalized == model.NormalizeWorkflowStateName(states.Cancelled) {
		return true
	}
	return false
}

func upsertManagedMetadata(description string, meta managedMetadata) string {
	description = strings.TrimSpace(strings.ReplaceAll(description, "\r\n", "\n"))
	block := metadataBlock(meta)

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

// findMetadataBlockStart locates the snyk-linear-sync metadata block start
// marker in the description, anchored to the beginning of a line. This
// prevents false matches where the marker string appears mid-sentence in
// user-written text (e.g. "See <!-- snyk-linear-sync notes -->"), which
// could corrupt the description if treated as a metadata block.
//
// It returns the LAST line-anchored occurrence, not the first. Ticket
// descriptions can embed free-form Snyk-controlled prose (e.g. issue
// description/remediation text) ABOVE the real metadata block, since the
// sync always appends the managed metadata block last. If that prose
// happens to contain a line-anchored marker (e.g. quoted/copied from
// elsewhere), returning the first occurrence would hijack this function with
// a bogus block and corrupt the description, and could also break ticket
// matching via extractFingerprint/extractManagedLabels in the Linear client.
// The real, sync-managed block is always the last one in the description.
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

// withoutMetadataBlock returns the description with its trailing metadata
// block removed, so ComputeDiff can tell a metadata-only change (nothing a
// human needs a comment about) from a change to the visible body.
func withoutMetadataBlock(description string) string {
	start := findMetadataBlockStart(description)
	if start < 0 {
		return description
	}
	end := len(description)
	if relEnd := strings.Index(description[start:], "-->"); relEnd >= 0 {
		end = start + relEnd + len("-->")
	}
	return strings.TrimSpace(description[:start] + description[end:])
}

func metadataHeaderStart() string {
	return "<!-- snyk-linear-sync"
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
	// managed set, derived labels included. The awaiting-fix, tool and origin
	// fallbacks have non-empty defaults, so returning early here is what stops
	// them being asserted — and resolved, and failing the run when the label is
	// missing in Linear — after the operator disabled label management.
	if !labelCfg.Enabled() {
		return nil
	}

	labels := make([]string, 0, 4)
	labels = append(labels, strings.TrimSpace(labelCfg.Managed))

	if finding.Status == model.FindingAwaitingFix && strings.TrimSpace(labelCfg.AwaitingFix) != "" {
		labels = append(labels, labelCfg.AwaitingFix)
	}

	issueType := strings.ToLower(strings.TrimSpace(finding.IssueType))
	if issueType != "" {
		if mapped := strings.TrimSpace(labelCfg.Tool[issueType]); mapped != "" {
			labels = append(labels, mapped)
		} else if fallback := strings.TrimSpace(labelCfg.ToolDefault); fallback != "" {
			labels = append(labels, fallback)
		}
	}

	projectOrigin := strings.ToLower(strings.TrimSpace(finding.ProjectOrigin))
	if projectOrigin != "" {
		if mapped := strings.TrimSpace(labelCfg.Origin[projectOrigin]); mapped != "" {
			labels = append(labels, mapped)
		} else if fallback := strings.TrimSpace(labelCfg.OriginDefault); fallback != "" {
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

	if finding.Status == model.FindingAwaitingFix && strings.TrimSpace(labelCfg.AwaitingFix) != "" {
		reasons[model.NormalizeLabelName(labelCfg.AwaitingFix)] = "awaiting upstream fix"
	}

	issueType := strings.ToLower(strings.TrimSpace(finding.IssueType))
	if issueType != "" {
		if mapped, ok := labelCfg.Tool[issueType]; ok && strings.TrimSpace(mapped) != "" {
			reasons[model.NormalizeLabelName(mapped)] = fmt.Sprintf("Snyk issue type is %s", issueType)
		} else if strings.TrimSpace(labelCfg.ToolDefault) != "" {
			reasons[model.NormalizeLabelName(labelCfg.ToolDefault)] = fmt.Sprintf("Snyk issue type is %s", issueType)
		}
	}

	projectOrigin := strings.ToLower(strings.TrimSpace(finding.ProjectOrigin))
	if projectOrigin != "" {
		if mapped, ok := labelCfg.Origin[projectOrigin]; ok && strings.TrimSpace(mapped) != "" {
			reasons[model.NormalizeLabelName(mapped)] = fmt.Sprintf("Snyk project origin is %s", projectOrigin)
		} else if strings.TrimSpace(labelCfg.OriginDefault) != "" {
			reasons[model.NormalizeLabelName(labelCfg.OriginDefault)] = fmt.Sprintf("Snyk project origin is %s", projectOrigin)
		}
	}

	return reasons
}

// preferCanonicalDuplicate decides which of two Linear tickets sharing the
// same fingerprint should be treated as canonical. A non-terminal ticket is
// always preferred over a terminal one (archived, or a configured Done/
// Cancelled workflow state), regardless of identifier number: keeping a
// terminal ticket as canonical would make the reopen guard (see the match
// loop above) fire on every run, dropping the fingerprint from the index and
// creating a brand-new ticket each time — a self-sustaining loop that mints
// one duplicate per run forever. Among two tickets of the same class (both
// terminal or both non-terminal), the lower Linear identifier is kept, since
// it is the older ticket.
func preferCanonicalDuplicate(a, b model.ExistingIssue, states config.StateConfig) (canonical, duplicate model.ExistingIssue) {
	aTerminal := isTerminalLinearState(a, states)
	bTerminal := isTerminalLinearState(b, states)
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

// identifierNum extracts the numeric suffix from a Linear identifier (e.g. "SNYK-42" → 42).
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

func sourceRegionString(finding model.Finding) string {
	if finding.SourceLineEnd <= 0 {
		return fmt.Sprintf("line %d:%d", finding.SourceLineStart, finding.SourceColumnStart)
	}
	if finding.SourceLineStart == finding.SourceLineEnd {
		return fmt.Sprintf("line %d:%d-%d", finding.SourceLineStart, finding.SourceColumnStart, finding.SourceColumnEnd)
	}
	return fmt.Sprintf("line %d:%d to %d:%d", finding.SourceLineStart, finding.SourceColumnStart, finding.SourceLineEnd, finding.SourceColumnEnd)
}
