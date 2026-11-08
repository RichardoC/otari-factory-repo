# Project Overview

## Purpose

This project exists to keep Linear aligned with Snyk.

Canonical module path:

```text
github.com/tesslio/snyk-linear-sync
```

This repository is a detached operational copy of
github.com/RichardoC/snyk-linear-sync; fixes are contributed back upstream.

The intended outcome is:

- every relevant Snyk finding has a corresponding Linear issue
- the Linear issue stays current as the Snyk finding changes
- resolved or ignored findings move to the correct Linear workflow state
- repeated runs become cheap by skipping unchanged records

This is an operational sync tool, not a generic library.

## Core Contract

The project treats Snyk as the source of truth for security finding data and Linear as the execution surface for tracking and triage.

It is responsible for:

- reading Snyk findings
- deciding the desired Linear representation
- reconciling the current Linear state to that desired state

It is not responsible for:

- writing back to Snyk
- preserving arbitrary manual edits inside the managed section of the Linear description
- syncing comments or custom fields beyond the currently managed issue body, title, priority, due date, workflow state, and managed automation label

## Identity Model

Each Snyk finding is identified by:

```text
snyk:<project-id>:<issue-id>[:<location>]
```

That fingerprint is embedded in the Linear description metadata block and is the durable join key between systems. `<location>` (source file for code issues, `package@version` for dependencies) separates occurrences that share a Snyk issue ID.

Without that fingerprint, the sync cannot safely deduplicate or update issues.

The metadata block also carries a project-independent `identity`: a hash of the project's origin, name, target file, target reference, repository and Kubernetes cluster, the issue key, and the location. The cluster is included because Snyk's Kubernetes project names carry no cluster, so the same workload in staging and prod would otherwise share an identity. It exists because Snyk sometimes recreates a project under a new project ID, which changes every fingerprint for findings that are otherwise the same. Identity is used only to decide which existing ticket a finding is matched to; ticket content and state still derive only from current Snyk data.

Matching order for each finding:

1. exact fingerprint
2. coarse fingerprint (migration of pre-location tickets; open tickets only)
3. identity rebind: a ticket with the same identity whose project is not active, and which is either open or `Cancelled` with a sync-recorded `closed_reason` of `project-missing`/`project-deactivated`. Never a ticket of an active project, one closed by a fix or a person, or an archived one. Each candidate is used once; open tickets are preferred, then the most recently created.

A failed Kubernetes cluster lookup makes the cluster unknown (not "no cluster"): those findings get no identity for the run, keep their tickets' stored identity and Cluster line, never rebind, and defer creating a new ticket only when a rebind candidate could be theirs and is still open or was closed less than 24 hours ago (so a persistent failure hides a finding for at most about a day).

Linear is the only durable state: identity and `closed_reason` live in the metadata block, not in the SQLite cache. Tickets written before the identity line existed gain it on their next update, which on the first run after upgrading is a one-time, metadata-only update of every matched ticket.

## Issue Lifecycle

### Create

Create a Linear issue when:

- a Snyk finding exists
- no Linear issue with the same fingerprint exists

### Update

Update the Linear issue when managed fields differ:

- title
- description
- due date
- priority
- mapped state
- managed automation label

### Resolve

When a previously tracked finding no longer exists in Snyk but its Snyk project still exists, move the Linear issue to the resolved state.

When a previously tracked finding no longer exists because its Snyk project no longer exists, cancel the Linear issue instead.

When the sync itself closes an open ticket this way it records `closed_reason: project-missing` (or `project-deactivated`) in the metadata block, so a later run can tell the closure was machine-made and rebind the ticket if the project is recreated. A ticket that is already closed keeps its state and records no `closed_reason`: a `Done` ticket stays `Done`, because the project going away says nothing about how the ticket was closed.

An empty Snyk snapshot is not trusted: if the snapshot lists no projects at all — neither active nor inactive — while managed tickets exist in Linear, the run fails with an error instead of resolving anything, and makes no Linear write and no cache refresh. A silently empty project and finding list is the documented symptom of an API key that cannot read projects, of an outage, or of `SNYK_ORG_ID` naming an organization the API key cannot see — acting on it would cancel every managed ticket in one run. In `--dry-run` the plan is still reported, with the same message logged at error level. A brand-new deployment with no managed tickets yet is not affected, and an organization whose issues are all fixed still lists its projects, so those tickets still move to the resolved state. The guard covers the "nothing at all" case only; a partial listing is not a plausibility failure.

### Reopen Guard

A terminal ticket is not reopened when Snyk reports its finding as open again; a fresh ticket is created instead, because Snyk reuses issue IDs across different code (#28). Two exceptions:

- a rebind of a sync-cancelled ticket after project recreation (see Identity Model);
- an exact, location-bearing fingerprint match whose ticket has known creation and closed times in Linear, where Snyk's `last_resolved_at` (the latest across the issue's coordinates) is empty or earlier than the ticket's creation. The finding was then open for the ticket's whole life, so the closure was premature (for example an automation) and the ticket is reopened (among several closed copies with that fingerprint, the most recently created one is checked and becomes canonical) with the reason "Snyk still reports this finding as open; reopened instead of creating a duplicate". This cannot reintroduce #28: an issue-ID reuse starts with Snyk resolving the old occurrence, which shows as a `last_resolved_at` after the ticket's creation. The comparison is deliberately against creation rather than closure, because the sync closes a ticket on a run after Snyk's resolution, so the resolution always predates the closure. An unparsable `last_resolved_at` or a missing timestamp keeps the default behavior.

A reopened ticket (by either exception) moves to the last open workflow state its Linear history shows it in before it was closed, so triage done before the closure (moving it from Triage to Todo, or into progress) survives. Only states of the team qualify. When the history yields none or cannot be read, the reopen still happens, into the configured open state.

### Conflict

If multiple Linear issues share the same fingerprint, the sync treats that as a conflict:

- it logs the conflict
- it skips automatic mutation for that fingerprint

## Description Strategy

The Linear issue description is intentionally structured for fast triage first, deep debugging second.

It includes:

- heading with vulnerability title and severity
- Kubernetes cluster and namespace near the top for findings whose project
  origin is `kubernetes` (cluster from the Snyk v1 project detail's
  `imageCluster` field — the REST project resource does not expose it and
  the v1 projects list endpoint is deprecated — namespace parsed from the
  project name, which the Snyk Kubernetes integration builds as
  `<namespace>/<kind>.<group>/<workload>:<target>`)
- repository context near the top
- branch/reference and commit context when available
- source file or project target file context near the top
- human-usable Snyk UI link
- Snyk REST API link
- package/version details
- fix availability, CVSS, CWE class(es), and CVE identifier(s) when Snyk reports them
- vulnerability description and remediation guidance sections when Snyk reports them
- project identifiers
- issue identifiers
- GitHub repository links when `SOURCE_PROVIDER=github`
- GitHub source file and commit links when `SOURCE_PROVIDER=github`
- GitHub project target file links when `SOURCE_PROVIDER=github` and no precise source location is available
- metadata block

The synced title is also structured for scanability. It includes the best available source context:

- repository for code and repository-backed findings
- branch/image/reference for non-GitHub target-file findings such as Kubernetes or container scans

These fields come from the Snyk API unbounded, so the rendered title is capped
at Linear's title limit (255 characters, measured in runes) with a `...`
truncation marker.

The subject portion then uses this preference:

1. source file basename
2. package name
3. project target file
4. project name

The metadata block also records the managed automation label name when label management is enabled.

Linear may rewrite parts of the description body when rendering or storing markdown. The sync therefore normalizes known Linear formatting changes during compare and cache hashing, including the blank lines Linear inserts after headings.

The compare and the cache hash also ignore the scanned commit and the flagged line region in the `Ref`, `Commit` and `File` lines. Snyk reports a new commit, and often new line numbers, for every Snyk Code finding of a repository on each rescan of its branch, with nothing else changed. A ticket therefore keeps the commit and line links it was last written with; they are permalinks to that commit, and are refreshed whenever anything else about the finding changes.

## Source Hosting

`SOURCE_PROVIDER` controls how source references are rendered.

- `unknown` leaves source references as plain text
- `github` renders public GitHub links for:
  - repositories
  - source files, pinned to the reported commit
  - source commits
  - project target files, pinned to the reported branch/reference when no source commit/file is available

If repository, file, or commit data is missing, the sync falls back to plain text.

## Managed Labels

`LINEAR_MANAGED_LABEL` controls the label this tool manages on synced issues.

- default: `snyk-automation`
- `off`: disables label management, and with it the whole managed label set —
  the derived awaiting-fix, tool and origin labels are not applied or looked up
  either, whatever their configured values. `LINEAR_CREATE_ONLY_LABELS` is a
  separate set rather than part of label management, so its labels are still
  resolved and stamped on create
- any other value: the exact Linear label name to manage

Behavior:

- the configured managed label is added to new synced issues
- unrelated existing labels are preserved
- if the configured managed label changes, the old managed label is removed and the new one is applied
- if label management is disabled, the previously managed label is removed

The configured label must already exist in Linear. If it does not, the run fails with a clear operator-facing error.

`LINEAR_TOOL_LABELS` optionally maps Snyk issue `type` values to additional managed Linear labels.

- format: comma-separated `issue_type:label` pairs
- example: `code:snyk-code,package_vulnerability:snyk-open-source`

`LINEAR_TOOL_LABEL_DEFAULT` controls the fallback label for unmapped issue types.

- default: `snyk-automation`
- `off`: disables the fallback

`LINEAR_ORIGIN_LABELS` optionally maps Snyk project `origin` values to additional managed Linear labels.

- format: comma-separated `origin:label` pairs
- example: `github:snyk-github,kubernetes:snyk-kubernetes`

`LINEAR_ORIGIN_LABEL_DEFAULT` controls the fallback label for unmapped project origins.

- default: `off`
- `off`: disables the fallback

`LINEAR_AWAITING_FIX_LABEL` controls the label applied to issues where Snyk reports the ignore as "until fix is available" (disregardIfFixable).

- default: `triage-dependency`
- `off`: disables the awaiting-fix label
- any other value: the exact Linear label name to apply
- the label must already exist in Linear

When an issue is in the awaiting-fix state, the sync adds the configured label. When the fix becomes available and the issue moves to `Todo`, the label is removed. The label is tracked in the metadata block alongside tool and origin labels. The metadata block also carries an `awaiting_fix: yes` marker, written whenever the finding is in the awaiting-fix state regardless of the label configuration, and cleared when the state ends.

The metadata block stores the full managed label set so the sync can remove stale tool-derived, origin-derived, and awaiting-fix labels while preserving unrelated manual labels.

Because `issueUpdate` replaces the ticket's whole label set, every update batch re-reads the current labels of the issues it is about to write, immediately before the write, and carries labels the sync does not own over from that live read rather than from the run's opening snapshot. An issue whose labels that read cannot deliver is refused instead of guessed at: this covers an issue absent from the response and one returned with a truncated labels page (`pageInfo.hasNextPage`), where a label past the page boundary — including a `LINEAR_PROTECTED_LABELS` control label — would be absent from the read and then deleted by the update. The refusal is not paginated away: a nested connection cannot be paged from within this one query, and the sync's own cap of 250 labels per issue makes a truncating ticket pathological rather than a case worth serving. The caller retries the batch item by item, so only the refused issue fails; its update is counted in `failed_ops`.

`LINEAR_UNSUBSCRIBE_ACTOR` is an optional operator control for notification behavior.

- default: `true`
- when enabled, the sync creates issues without subscribing the Linear API actor
- update operations preserve the current subscriber list exactly as it already exists

This only affects the persisted subscriber list. Linear still records the API user as the issue creator, which is separate from `subscribers` and can make the UI look like the creator is still "following" the issue even when the stored subscriber list is empty.

## Snapshot Scope

Each run loads a snapshot of the sync's own Linear tickets. The query is scoped
by team, then by an archive window, then by identity:

```text
team = LINEAR_TEAM_ID
  AND (not auto-archived OR auto-archived within the lookback window)
  AND (title starts with "Snyk:" OR carries LINEAR_MANAGED_LABEL)
```

Both identity predicates are exact -- a title prefix and a label name -- and
either alone identifies a managed ticket, since the sync builds every title with
the prefix and re-asserts the managed label on every update. Keeping both means a
retitled ticket, or one whose label was removed by hand, is still found rather
than being invisible and duplicated. With `LINEAR_MANAGED_LABEL=off` the label
clause is omitted rather than matching nothing.

Each ticket is loaded with its labels (up to 250 per ticket). A ticket whose
labels do not fit one page returns a truncated set, and a truncated set is not
trusted as complete: the run fails with an error naming the ticket rather than
hashing a partial label set into the cache or rebuilding ticket labels from it.
The same refusal applies to the single-issue lookup used by diagnostic and
reconciliation paths, and mirrors the live label read, which refuses the update
batch for the same reason (see "Label Preservation").

This matters because the Linear team is shared with `snyk-dast-linear-sync`: the
scope must exclude the other tool's tickets. The fingerprint metadata block is
deliberately **not** used as a query predicate -- `description: { contains: ... }`
does not bound the result set (the DAST sync, using this same filter shape
against this same team, loaded 59,866 issues for 7 findings), and Linear does not
document `description` as supporting the string comparators at all. The metadata
block remains the authority for identity once an issue is loaded; it just cannot
be trusted to select which issues to load.

## State Mapping

The current workflow mapping is:

- `open` -> `Todo`
- `ignored until fix available` (disregardIfFixable) -> `Backlog` (no due date, `triage-dependency` label)
- `temporarily ignored` (snoozed with future expiry) -> `Todo`
- `permanently ignored` -> `Cancelled`
- `fixed` (or `resolved`/disappeared) -> `Done`
- missing finding in an existing active Snyk project -> `Done`
- missing finding because the Snyk project no longer exists -> `Cancelled`
- Snyk project is inactive (de-activated) -> `Cancelled`

The sync also normalizes workflow naming differences such as `Canceled` vs `Cancelled`.

Resolution takes precedence over ignore state: if Snyk reports a finding as resolved (status `resolved`/`fixed`, resolution `fixed`, or all coordinates resolved), it maps to `Done` even when the finding still carries an ignore. The ignore branches (`until fix available`, `temporarily ignored`, `permanently ignored`) apply only to findings that still exist in Snyk. Without this precedence, a resolved finding that still has an active or expired ignore would map to a non-terminal state and resurface as a permanently-open, birth-overdue ticket for a vulnerability that no longer exists.

The `ignored until fix available` state is distinct from other ignored states because the ignore is conditional on an upstream fix. These issues are placed in `Backlog` with no due date and the `triage-dependency` label (configurable via `LINEAR_AWAITING_FIX_LABEL`). The description renders `Status: ignored (no fix available)` so the Snyk-side reality is visible in Linear. When a fix becomes available, Snyk flips `ignored=false` and the next run moves the issue to `Todo` with a recalculated due date.

### Manual Backlog Override

When a user manually moves a managed open ticket from `Todo` to `Backlog` in Linear, subsequent syncs preserve the `Backlog` state instead of overriding it back to `Todo`. This prevents the automation from fighting intentional user triage decisions. The override matches the configured `LINEAR_STATE_BACKLOG` value with case-insensitive normalization.

The override does not apply to a ticket the sync itself parked in `Backlog` for an `ignored until fix available` finding: that placement is the sync's own, not a manual move. When the fix becomes available Snyk flips `ignored=false`, the next run moves the ticket to `Todo` with a recalculated due date, and the change comment says why. Only the `Backlog` state the sync chose is overridden this way — if a person moved the awaiting-fix ticket to some other non-terminal state (for example `In Progress`), the general manual non-terminal state override below still preserves their choice. The detection reads the `awaiting_fix` marker in the metadata block, which the sync writes regardless of the label configuration, so it works with `LINEAR_AWAITING_FIX_LABEL=off` too. Tickets written before the marker existed carry neither marker nor label and keep the old behavior: the sync cannot tell its own `Backlog` placement from a manual one, and the ticket stays in `Backlog` when the fix arrives, until the sync rewrites its description in a later awaiting-fix run.

### Manual Non-Terminal State Override

When a user manually moves a managed issue to any non-terminal Linear workflow state that differs from what the sync would set, subsequent syncs preserve the user's chosen state. This generalises the previous Backlog-only and hardcoded "Todo" overrides.

This covers:

- a user moving an open issue from the configured open state (e.g. `Triage`) to `Todo` or `In Progress`
- a user moving an awaiting-fix issue from `Backlog` to `Todo`
- any other non-terminal state that differs from the configured state for the desired model state

Terminal states (`Done`, `Cancelled`) are never preserved: the sync always transitions to a terminal state when the Snyk finding is fixed or ignored, even if a user manually moved the issue to a terminal state.

### Due Dates

Due dates are normally derived from the Snyk issue creation timestamp, not from when the issue first appears in Linear. For temporarily ignored issues, the due date is instead calculated from the ignore expiry date so the SLA extends to the normal severity offset from when the ignore expires.

Issues ignored "until fix is available" have no due date. The SLA clock is paused while the issue is blocked on an upstream fix. When a fix becomes available, Snyk flips `ignored=false` and the next run recalculates the due date from fix availability (today + severity offset), moving the issue to `Todo`.

Default offsets:

- critical: 15 days
- high: 30 days
- medium: 45 days
- low: 90 days

## Performance Model

The project is designed for thousands of issues.

It uses:

- concurrent Snyk and Linear snapshot loading
- worker-based reconciliation
- batched Linear mutations
- rate-limit backoff -- a rate-limit error that arrives as a per-alias GraphQL
  error joined onto a response whose other aliases decoded is not re-sent:
  those aliases already took effect, and re-sending the batch would duplicate
  them. Only an operation that decoded nothing is retried, and reads (which
  decode into structs and cannot duplicate anything) keep the full retry.
- transport-level retry of HTTP statuses -- `AdaptiveTransport`
  (`internal/httpx`) retries 429 for every request, and 502, 503 and 504 only
  for idempotent requests: GET and HEAD, and GraphQL reads, which are POSTs
  but carry no mark. Every Linear mutation is sent with a non-idempotent
  context mark (`httpx.WithNonIdempotent`, applied by
  `Client.executeMutation`), so an operation marked as a mutation is delivered
  once and its 502, 503 or 504 goes to the caller, which reconciles partial
  writes (see "Batch Failure Handling"). The reason is that a gateway error
  can arrive after Linear committed the mutation, and Linear exposes no
  idempotency key: re-sending would duplicate the creates or comments the
  batch already made. A 429 is different -- the request was rejected rather
  than possibly applied -- so even a mutation is re-sent, with its body
  rewound from `GetBody`.
- SQLite caching

The cache is critical for steady-state performance. A healthy steady-state run should do little or no work when nothing has changed.

### Batch Failure Handling

Linear mutations are batched, one GraphQL alias per entry, so a create batch can
fail in two ways that need different handling to avoid duplicating tickets:

- **Per-alias failure.** The response carries `success: false`, or omits the
  alias, while the other aliases succeeded. Note that `gqlclient` decodes the
  response body before returning any GraphQL error, so the successful aliases
  are still available even when an error is returned. Only the reported entries
  are retried; retrying the batch would duplicate the ones that already exist.
  A retried entry that is reported failed again counts in `FailedOps`, fails the
  run and stays out of the refreshed cache. A reported failed index outside the
  batch is degenerate — the real client derives indices from the slice it was
  handed — but `LinearClient` is an interface, so a faulty implementation can
  produce one; such an index identifies no entry, so nothing in the batch is
  proven to have landed and every fingerprint of it stays out of the refreshed
  cache for the same reason an ambiguous create does.
- **Ambiguous failure.** No aliases were decoded at all, so nothing is known per
  entry. This covers a transport-level failure and equally a GraphQL failure
  that returned no data, such as a 502 arriving after Linear had already
  committed the mutation (the transport does not retry a marked mutation on
  502/503/504 for exactly this reason, so that response reaches the caller
  instead of becoming a second batch). Crucially it does *not* prove Linear rejected the
  mutation, and Linear exposes no idempotency key to make the retry safe: the
  sync therefore reconciles first. `ExistingFingerprints` asks Linear which of
  the batch's fingerprints now have a live issue, and only the rest are
  re-created. That reconciliation query is scoped exactly like the snapshot
  (team plus the identity arms: the title prefix and the managed label), so a
  shared team cannot be paged through during the retry path. The metadata block
  that carries the fingerprint is deliberately *not* used as a query predicate:
  Linear does not apply `description` comparators, and filtering on them
  silently widens the query to every issue in the team (the same trap the
  sibling snyk-dast-linear-sync hit). The fingerprint check still runs on each
  decoded description, so the scope governs which issues are read, not what
  counts as a match.

  If the reconciliation query itself fails, nothing is retried: the batch is
  counted in `FailedOps` and left for the next run, which creates whatever is
  genuinely missing. The finding is still reported by Snyk either way; if its
  issue was never created it is absent from the next Linear snapshot and gets
  created then. Missing a ticket for one cycle is recoverable; a duplicate is
  only undone later by the duplicate-cancellation pass, after duplicate
  notifications have gone out.

A residual race remains by construction: a write that lands between the
reconciliation query and the retry still duplicates. The
duplicate-cancellation pass converges that case on a later run.

Update, resolve and duplicate-cancel batches keep their item-by-item fallback:
those writes set state, so re-applying one that already landed converges instead
of duplicating. Change comments also have no fingerprint to reconcile against;
an ambiguous comment-batch failure retries each comment individually, accepting
a possible duplicate notification comment over a missing one. A comment whose
individual retry still fails (an error, or its index reported failed) counts a
failed comment in `failed_comments`. A failed-index entry that names no member
of the batch is never used to index it: it counts a failed comment too, instead
of panicking the worker.

## SQLite Cache

The SQLite cache stores:

- Snyk-side normalized hashes keyed by fingerprint
- Linear-side normalized hashes keyed by fingerprint
- a schema signature for the managed issue format
- a `saved_at` timestamp for the last successful write, so the next run logs
  the cache age; a cache written before this key existed reports its age as
  unknown
- cached v1 ignore metadata; `LoadIgnores` drops entries past
  `ignoreEntryTTL` and entries whose `updated_at` cannot be parsed, counting
  both so the refreshed-cache line reports them

The cache is used only as an optimization. It should never be the only source used to infer real current state.

Normal behavior:

1. Read live Snyk data.
2. Read live Linear data.
3. Compare both against the last successful cached hashes.
4. Skip unchanged fingerprints.
5. After successful writes, refresh the cache from the live post-write Linear snapshot. When several tickets share a fingerprint, the cached Linear hash is that of the ticket the finding was matched to, so the next run's comparison against the same ticket can hit. A fingerprint whose Linear write failed in this run is left out of the cache, so the next run compares it against live data again.

The post-write refresh matters because Linear may rewrite markdown bodies after mutation.

Reading the cache is fail-closed. If a read fails at any point — including a table that stops returning rows part-way, which would otherwise load a partial hash snapshot without reporting anything — the run fails before the comparison instead of quietly re-hashing the missing fingerprints every run. Delete the cache file or rerun with `--bypass-cache` to rebuild it from live data.

An interrupted run (signal, cancelled context) does not refresh the cache. It
has made no promise about the findings it never reached, and caching "still as
it was" hashes for them would make the next run skip them.

## Safety Assumptions

- The metadata block must remain intact.
- The managed description body is owned by this tool.
- Linear issue history matters, so deleting and recreating all issues is a last resort, not a normal repair path.
- Cache bypass is the correct operator action when the rendering schema or compare logic changes.

## Operator Guidance

After any code change, run the full verification suite (see README for the
rationale behind the ordering):

```bash
go mod tidy && git diff --exit-code go.mod go.sum
go fix ./... && git diff --exit-code
go fmt ./...
go build ./...
go vet ./...
go test ./...
```

Use a normal run for day-to-day sync:

```bash
go run ./cmd/snyk-linear-sync --env-file .env
```

Use a dry run to inspect planned changes:

```bash
go run ./cmd/snyk-linear-sync --env-file .env --dry-run
```

Use cache bypass when you intentionally changed the managed rendering or need a full live reconciliation:

```bash
go run ./cmd/snyk-linear-sync --env-file .env --bypass-cache
```

### Exit Status

`RunResult.Err` decides the process exit status, and it is deliberately narrow:
the run fails only when state-changing work was left permanently unapplied.

`FailedOps` is incremented *after* the per-item retry has itself failed, so a
batch failure recovered by retry leaves it at zero and the run succeeds — the
operation applied, it just took two calls. The exception is an ambiguous batch
create whose reconciliation lookup also fails: the whole batch is counted
without a retry, because there is no safe way to tell which entries landed. A
non-zero count fails the run,
because a run that logs a dropped ticket and then reports success means nothing
downstream notices, and the next scheduled run reports success again.

The check runs after the `sync complete` summary log, so the counts are
reported regardless of the exit status.

## Design Boundaries

If this project grows further, the next reasonable extensions would be:

- stronger incremental Snyk fetching using server timestamps where available
- more selective Linear snapshot loading if the API surface allows it safely
- richer cache statistics than age and ignore-eviction counts
- explicit conflict reporting output

The current implementation is intentionally optimized for correctness first, then steady-state efficiency.
