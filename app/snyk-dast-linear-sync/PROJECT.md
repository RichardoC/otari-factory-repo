# Project Overview

## Purpose

This project exists to keep Linear aligned with Snyk DAST.

Canonical module path:

```text
github.com/tesslio/snyk-dast-linear-sync
```

Snyk DAST is the rebrand of Probely. The tool talks to the Probely REST API
(current hostnames `api.probely.com` / `plus.probely.app`, both configurable)
and is forward-compatible with the Snyk DAST rebrand.

The intended outcome is:

- every relevant Snyk DAST finding has a corresponding Linear issue
- the Linear issue stays current as the Snyk DAST finding changes
- resolved or accepted/invalidated findings move to the correct Linear workflow state
- repeated runs become cheap by skipping unchanged records

This is an operational sync tool, not a generic library.

## Core Contract

The project treats Snyk DAST as the source of truth for security finding data
and Linear as the execution surface for tracking and triage.

It is responsible for:

- reading Snyk DAST findings (and the targets they belong to)
- deciding the desired Linear representation
- reconciling the current Linear state to that desired state

It is not responsible for:

- writing back to Snyk DAST
- preserving arbitrary manual edits inside the managed section of the Linear description
- syncing comments or custom fields beyond the currently managed issue body, title, priority, due date, workflow state, and managed automation label

## Identity Model

Each Snyk DAST finding is identified by:

```text
snyk-dast:<target-id>:<finding-id>
```

The `<finding-id>` is the bare numeric finding id returned by the Snyk DAST
API (the `id` field from `GET /findings/`). Global uniqueness comes from the
`snyk-dast:<target-id>:` prefix. That fingerprint is embedded in the Linear
description metadata block and is the durable join key between systems.

Without that fingerprint, the sync cannot safely deduplicate or update issues.

## Issue Lifecycle

### Create

Create a Linear issue when:

- a Snyk DAST finding exists
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

When a previously tracked finding no longer exists in Snyk DAST but its target
still exists, move the Linear issue to the resolved (`Done`) state.

A finding that cannot be normalized is never read as "no longer exists". If a
finding's embedded target summary carries no id, no
`snyk-dast:<target-id>:<finding-id>` fingerprint can be built for it, so it is
skipped during the load with a warning naming its finding id. Its id is
recorded on the snapshot, and the resolve loop holds back every managed ticket
whose fingerprint ends in that finding id — matched on the finding-id part of
the fingerprint, since the target id is exactly what is missing. A deferred
ticket is neither resolved nor recreated for that run, and the count is
reported as `deferred_tickets`, so API drift cannot record a false resolution
or cause a close/reopen cycle.

When a previously tracked finding no longer exists because its Snyk DAST target
no longer exists, cancel the Linear issue instead.

An empty Snyk DAST snapshot is not trusted: if the snapshot lists no targets at
all while managed issues exist in Linear, the run fails with an error instead of
resolving anything, and makes no Linear write and no cache refresh. A silently
empty target and finding list is the documented symptom of an API key without
the `view_target` permission, of a `SNYK_DAST_TEAM` filter that matches no
targets, or of an outage — acting on it would cancel every managed ticket in one
run. In `--dry-run` the plan is still reported, with the same message logged at
error level. A brand-new deployment with no managed issues yet is not affected.
The guard covers the "nothing at all" case only; a partial listing is not a
plausibility failure.

### Conflict

If multiple Linear issues share the same fingerprint, the sync treats that as a
conflict:

- it logs the conflict
- it keeps the lowest-identifier issue as canonical and cancels the duplicates

## Description Strategy

The Linear issue description is intentionally structured for fast triage first,
deep debugging second. Snyk DAST is a DAST product, so findings describe
vulnerabilities in a scanned web application or API target rather than
source-code locations or package dependencies.

It includes:

- heading with vulnerability title and severity
- target name/host and target URL near the top
- the vulnerable request context: URL, HTTP method, path, parameter, insertion point
- CWE and CVSS score when available
- human-usable Snyk DAST UI link
- Snyk DAST REST API link
- the fix description when Snyk DAST provides one
- target and finding identifiers
- the Snyk Code correlation block when the API key has the
  `correlation_viewer` permission; without it the run fails with an error
  naming the missing permission, and a `404` for a single finding's
  correlations only drops that finding's block
- metadata block

The synced title is also structured for scanability. It leads with the scanned
target (host, falling back to target name) and the vulnerable request
(`METHOD path`). The rendered title is capped at Linear's title limit (255
characters, measured in runes) with a `...` truncation marker, since these
fields come from the scan API unbounded.

The metadata block also records the managed automation label set when label
management is enabled.

Snyk DAST controls part of the body it does not own the meaning of: the fix
text and every Snyk Code correlation block are embedded free text, and scanned
application content can reach the correlation markdown. Each such block is
therefore sanitized before embedding: every HTML comment opening (`<!--`)
becomes `<!- -`. The replacement can never reintroduce `<!--`, so sanitizing is
idempotent and description comparison stays stable. As a second line of
defence, metadata block detection returns the *last* line-anchored
`snyk-dast-linear-sync` marker rather than the first, because the sync always
appends the managed block last. Together these stop embedded prose from
spoofing (or hiding) the fingerprint and managed labels, and stop
`upsertManagedMetadata` from rewriting the prose region instead of the real
block.

Because the managed block is appended last, unbounded prose would also risk
Linear's description limit truncating the block away, making the ticket
unmanaged and minting a duplicate on the next run. Each embedded block is
therefore capped at 10,000 runes (matching the sibling `snyk-linear-sync`),
via a truncation helper that never splits a multibyte rune and appends one
stable marker when it cuts. The per-block cap is not a bound on the
description by itself, because Snyk DAST returns one Snyk Code correlation
block per linked vulnerability and does not bound how many; only the first
three blocks are embedded and the rest are replaced by one stable
`_N more linked Snyk Code vulnerabilities not shown._` line, so the embedded
correlation section has a fixed upper bound however many blocks the API
returns. Capping is a pure function of the input, so description comparison and
cache hashing stay stable between runs.

The other scan-controlled fields — the heading, host, path, parameter,
insertion point, rendered labels and hrefs, and the target, finding and
definition identifiers — are single lines, and each is prepared the same way:
surrounding space is trimmed, carriage returns and newlines are flattened to
single spaces, and HTML comment openings are sanitized exactly like the prose
blocks. Flattening newlines is the defence that matters for these fields,
because metadata block detection matches a *line-anchored* marker; a newline
inside a backticked field would otherwise carry a fake marker and a fake
`fingerprint:` line onto their own lines even after the `<!--` opening itself
is rewritten. Rendered labels and identifier fields are additionally capped at
the same 80-rune limit the title uses; hrefs are never truncated — a shortened
URL is a wrong link. Instead, the whole rendered body (every line above the
metadata block) is bounded at a limit derived from the per-block caps plus a
margin for headings and single-line fields — one 10,000-rune Fix, three
10,000-rune correlation blocks, their truncation markers and the surrounding
lines all fit — and that limit stays far below Linear's description limit. A
line that no longer fits is dropped whole, one stable truncation marker is
appended, and later lines still fit, so one oversized path, parameter or URL
can push out its own line but never the fix text, the links, or the metadata
block, which is still appended last and intact. A `### ` section heading is
kept or dropped together with its body, so the bound can never leave an empty
`### Fix` or `### Snyk Code correlation` heading. Like the prose caps,
bounding is a pure function of the finding, so description comparison and
cache hashing stay stable between runs.

Linear may rewrite parts of the description body when rendering or storing
markdown. The sync therefore normalizes known Linear formatting changes during
compare and cache hashing.

## Managed Labels

`LINEAR_MANAGED_LABEL` controls the label this tool manages on synced issues.

- default: `snyk-dast-automation`
- `off`: disables label management
- any other value: the exact Linear label name to manage

Behavior:

- the configured managed label is added to new synced issues
- unrelated existing labels are preserved
- if the configured managed label changes, the old managed label is removed and the new one is applied
- if label management is disabled, no managed labels are asserted — the check-type and target-type labels included — and previously managed labels are removed

The configured label must already exist in Linear. If it does not, the run
fails with a clear operator-facing error.

`LINEAR_TARGET_TYPE_LABELS` optionally maps Snyk DAST target `type` values
(`single` for web applications, `api` for APIs) to additional managed Linear
labels.

- format: comma-separated `target_type:label` pairs
- example: `api:snyk-dast-api,single:snyk-dast-web`

`LINEAR_TARGET_TYPE_LABEL_DEFAULT` controls the fallback label for unmapped
target types.

- default: `off`
- `off`: disables the fallback

The metadata block stores the full managed label set so the sync can remove
stale target-type-derived labels while preserving unrelated manual labels.

`LINEAR_PROTECTED_LABELS` names labels this sync must never add to, or remove
from, an existing ticket, for labels another actor owns as control state.

- format: comma-separated label names
- default: empty
- `off`: disables protection entirely

Linear's `issueUpdate` replaces a ticket's whole label set rather than applying a
delta, so every update restates the label set the ticket should end up with. The
sync owns only its managed labels; every other label — protected, human-applied,
or from a different automation — belongs to someone else and is carried forward
untouched. Restating the opening snapshot would delete any label another actor
added since that snapshot, so labels the sync does not own are never taken from
the snapshot:

- each update batch re-reads the live labels of the issues it is about to write,
  immediately before the write, and every unmanaged label is taken from that
  read and from nowhere else. This preserves one added mid-run and equally
  respects one removed mid-run (an absent label is never re-added).
- an issue missing from that read is indistinguishable from one carrying no
  unmanaged label, so the batch is failed rather than guessed at. The caller
  already retries issues individually, so only the unreadable issue is reported
  failed, and it lands in `FailedOps`, which fails the run.
- the labels connection is checked for truncation (`pageInfo.hasNextPage`) and a
  truncated page fails the issue closed too: a label past the page boundary —
  including a protected control label — would be absent from the read and then
  deleted by the update, which is the exact failure the read exists to prevent.
  Refusing beats paginating — a nested connection cannot be paged within the one
  query, and an issue with more than 250 labels is pathological rather than a
  case worth serving.
- the run's opening snapshot pages labels the same way (`labels(first: 250)`
  with a `pageInfo` check): a bare label read truncates at Linear's default
  page size of 50, so a managed label past the boundary would be missing from
  the snapshot's label set — hiding a manually removed managed label from the
  cache hash, or manufacturing a spurious update and change comment. A
  truncated snapshot page fails the snapshot, and with it the run, rather than
  emitting a label set a label may be missing from.
- protected labels are never asserted from the managed set, bounding the blast
  radius of a misconfiguration.
- creates need no handling: there is no pre-existing label set to replace.

Linear label groups are mutually exclusive: an issue may carry at most one child
of a group, and `issueUpdate` rejects a label set that names two ("labelIds not
exclusive child labels"). The echoed set is therefore collapsed to at most one
child per group: a managed label the sync asserts evicts the carried-forward
sibling of its group, and between two carried-forward siblings the first read
wins. Without this, a ticket that carries a second child of a managed label's
group fails every update until a person edits the ticket by hand.

The live read always runs, whether or not the protected set is empty: a human's
or another automation's label needs it exactly as much as a protected control
label does, and a mid-run edit is just as lost when no labels are protected.

`LINEAR_UNSUBSCRIBE_ACTOR` is an optional operator control for notification
behavior.

- default: `true`
- when enabled, the sync creates issues without subscribing the Linear API actor
- update operations preserve the current subscriber list exactly as it already exists

## State Mapping

Snyk DAST (Probely) finding states map to Linear workflow states as follows:

- `notfixed` / `retesting` -> `Todo`
- `accepted` with a future `expiration_date` (time-limited risk acceptance) -> `Todo` (SLA clock restarts from the acceptance expiry)
- `accepted` with a past `expiration_date` (lapsed risk acceptance) -> `Todo` (due date is the acceptance expiry plus the configured severity offset)
- `accepted` without an `expiration_date` (permanent risk acceptance) -> `Cancelled`
- `accepted` with an `expiration_date` that cannot be parsed -> `Cancelled` (treated as a permanent acceptance; a warning naming the finding and the unparseable value is logged)
- `invalid` (false positive) -> `Cancelled`
- `fixed` -> `Done`
- missing finding in an existing Snyk DAST target -> `Done`
- missing finding because the Snyk DAST target no longer exists -> `Cancelled`

The sync also normalizes workflow naming differences such as `Canceled` vs
`Cancelled`.

A time-limited `accepted` finding is kept open in `Todo` rather than cancelled,
because it requires attention once the acceptance expires. Its due date is
calculated from the acceptance expiry date so the SLA extends to the normal
severity offset from when the acceptance ends.

A lapsed time-limited acceptance stays in `Todo` for the same reason: the
acceptance was a request to be reminded at the expiry date, so mapping the
expired state onto a permanent acceptance would cancel the ticket at exactly
the moment it becomes actionable again. Only an acceptance with no expiry at
all is treated as permanent.

Archived Linear issues are a special case. Linear auto-archives closed issues
and excludes them from the default issue query, while Snyk DAST retains
terminal findings indefinitely, so the sync must ask for archived issues
explicitly (bounded by `LINEAR_ARCHIVE_LOOKBACK_DAYS`) or it recreates a
duplicate each time a ticket leaves the snapshot — once per archive cycle rather
than on every run, since each replacement is visible until Linear archives it in
turn. Archived issues are treated as
terminal and are not mutated in place; a finding that becomes active again gets
a replacement ticket.

The replacement is a product choice, not an API constraint: Linear's
`issueUnarchive` mutation carries no documented precondition, so restoring and
updating the original ticket is a supported alternative. It was considered and
rejected — each recurrence is intended to get a clean ticket with its own SLA
clock rather than one ticket reused across recurrences.

The lookback window bounds elapsed time since **auto-**archiving, not the team's
auto-archive period (which Linear expresses in months). The two are
independent, so a short window is not made safe by a short period. It also does
not bound every archived issue: manually or API-archived issues (trashed ones
included) have `archivedAt` set with `autoArchivedAt` null, so they are returned
regardless of age. Including them can only prevent duplicates, and the pinned
`linear-api` binding has no `IssueFilter.archivedAt` to bound them with.

Since Snyk DAST retains terminal findings indefinitely, each keeps a live
desired issue forever, and any finite window eventually drops its ticket from
the snapshot — minting a duplicate that cannot be reconciled, because the
original is no longer visible to the duplicate-cancellation pass. The copies
therefore accumulate, one per cycle. `LINEAR_ARCHIVE_LOOKBACK_DAYS` defaults to
ten years for that reason: it covers the whole archive in practice, and remains
configurable only as a size/latency escape hatch.

Suppressing creates for already-terminal findings would also remove the cycle,
but was rejected: the project wants every finding to end up with a ticket,
including findings closed before the sync first ran.

### Manual Non-Terminal State Override

When a user manually moves a managed issue to any non-terminal Linear workflow
state that differs from what the sync would set, subsequent syncs preserve the
user's chosen state. This prevents the automation from fighting intentional
triage decisions.

This covers:

- a user moving an open issue from the configured open state (e.g. `Triage`) to `Todo` or `In Progress`
- a user moving a snoozed issue from `Backlog` to `Todo`
- any other non-terminal state that differs from the configured state for the desired model state

Terminal states (`Done`, `Cancelled`) are never preserved: the sync always
transitions to a terminal state when the Snyk DAST finding is fixed or
permanently accepted/invalidated, even if a user manually moved the issue to a
terminal state.

### Due Dates

Due dates are derived from the Snyk DAST finding `created_at` timestamp, not
from when the issue first appears in Linear. For time-limited `accepted`
findings (snoozed), the due date is instead calculated from the acceptance
expiry date so the SLA extends to the normal severity offset from when the
acceptance expires.

Default offsets:

- critical: 15 days
- high: 30 days
- medium: 45 days
- low: 90 days

Past due dates are left as-is. Linear renders past due dates as "overdue", and
the actual past date is more informative than flooring to today: it tells the
triager how long the issue has been past its SLA. Flooring to today would also
cause daily cache churn.

## Performance Model

The project is designed for thousands of issues.

It uses:

- concurrent Snyk DAST and Linear snapshot loading
- worker-based reconciliation
- batched Linear mutations
- rate-limit backoff -- the HTTP transport retries `429` for every request, and
  `502`, `503` and `504` only for idempotent requests and GraphQL reads. Every
  GraphQL request is a `POST`, so the Linear client marks its mutation requests
  (create, update, comment, issue-unsubscribe) as non-idempotent; an operation
  marked as a mutation is delivered once, and a `502`/`503`/`504` on it goes to
  the caller, which reconciles the partial write rather than re-sending it (see
  Batch Failure Handling). A response that exhausts the request budget
  (`X-RateLimit-Requests-Remaining <= 0`) -- including a successful `200` -- is
  delivered to the caller and is not re-sent either; its only effect is to delay
  the next request until the reset instant. A rate-limit error that arrives as a
  per-alias GraphQL error joined onto a response whose other aliases decoded is
  likewise not re-sent: those aliases already took effect, and re-sending the
  batch would duplicate them. Only an operation that decoded nothing is retried.
- SQLite caching

The cache is critical for steady-state performance. A healthy steady-state run
should do little or no work when nothing has changed.

### Batch Failure Handling

Linear mutations are batched, one GraphQL alias per entry, so a batch can fail
three different ways and each needs different handling to avoid duplicating
tickets:

- **Per-alias failure.** The response carries `success: false`, or omits the
  alias, while the other aliases succeeded. Note that `gqlclient` decodes the
  response body before returning any GraphQL error, so the successful aliases are
  still available even when an error is returned. Only the reported entries are
  retried; retrying the batch would duplicate the ones that already exist.
- **Ambiguous failure.** No aliases were decoded at all, so nothing is known per
  entry. This covers a transport-level failure and equally a GraphQL failure that
  returned no data, such as a rejected query. A `502`, `503` or `504` on a
  mutation also lands here: the transport does not re-send a marked mutation,
  because a gateway error can arrive after Linear committed it, so the response
  reaches the caller and the reconciliation below decides what to retry. Crucially it does *not* prove Linear
  rejected the mutation, which may have been committed before the response was
  lost. Creates are therefore reconciled first: `ExistingFingerprints` asks Linear
  which of the batch's fingerprints now have a live issue, and only the rest are
  re-created. That reconciliation query is scoped the same way as the snapshot
  (team plus the identity arms), so a shared team cannot be paged through
  during the retry path. If that lookup also fails, nothing is retried and the
  entries are counted as failed. The finding remains in the next Snyk DAST snapshot either
  way; if its issue was never created it is absent from the next Linear snapshot
  and gets created on the next run. Missing a ticket for one cycle is recoverable;
  a duplicate is only undone later by the duplicate-cancellation pass.
- **Ambiguous comment failure.** A comment carries no fingerprint to reconcile
  against, and a duplicated change comment is more disruptive than a missing one
  (the issue state itself is already correct), so these are counted in
  `FailedComments` and not retried.

A residual race remains by construction: Linear's `issueCreate` exposes no
idempotency key, so a write that lands between the reconciliation query and the
retry still duplicates. The duplicate-cancellation pass converges that case on a
later run.

### Exit Status

`RunResult.Err` decides the process exit status, and it is deliberately narrow:
the run fails only when state-changing work was left permanently unapplied.

- `FailedOps` is incremented *after* the per-entry retry has itself failed, or
  after an ambiguous batch create whose reconciliation query failed and whose
  entries were deliberately left for the next run. So a batch failure recovered
  by retry leaves it at zero and the run succeeds — the operation applied, it
  just took two calls. A non-zero count fails the run, because a run that logs a
  dropped ticket and then reports success means nothing downstream notices, and
  the next scheduled run reports success again.
- `FailedComments` is excluded. A missing change comment leaves the synced issue
  state correct, and the feature is opt-in via `LINEAR_COMMENTS`, so it is counted
  and logged but does not fail the run.

The check runs after the `sync complete` summary log, so the counts are reported
regardless of the exit status.

## SQLite Cache

The SQLite cache stores:

- Snyk DAST-side normalized hashes keyed by fingerprint
- Linear-side normalized hashes keyed by fingerprint
- the UTC time the snapshot was saved, reported as `saved_at` in cache logs
- a schema signature for the managed issue format

The cache is used only as an optimization. It should never be the only source
used to infer real current state.

Normal behavior:

1. Read live Snyk DAST data.
2. Read live Linear data.
3. Compare both against the last successful cached hashes.
4. Skip unchanged fingerprints.
5. After writes, refresh the cache from the live post-write Linear snapshot.

The post-write refresh matters because Linear may rewrite markdown bodies after
mutation. A fingerprint whose Linear write failed in this run is left out of the
refreshed cache, so the next run compares it against live data again and retries
the write instead of treating the failed one as reconciled.

Reading the cache is fail-closed. If a read fails at any point — including a
table that stops returning rows part-way, which would otherwise load a partial
hash snapshot without reporting anything — the run fails before the comparison
instead of quietly re-hashing the missing fingerprints every run. Delete the
cache file or rerun with `--bypass-cache` to rebuild it from live data.

The cache fast-path never suppresses a pending move into a terminal state
(`Done`/`Cancelled`): a finding that became fixed or permanently
accepted/invalidated while its ticket sat in an open column must still be
closed, even if its hashes are unchanged since the last run.

An interrupted run (SIGINT/SIGTERM, or any cancelled context) never refreshes
the cache. The run's plan was only partly written, so a refresh would describe
the fingerprints whose jobs never ran as reconciled; the run returns an error
instead, and the next run re-examines everything against live data.

## Safety Assumptions

- The metadata block must remain intact. It is the last line-anchored
  `<!-- snyk-dast-linear-sync` marker in the description; embedded Snyk DAST
  prose has its comment openings neutralized and cannot displace it.
- The managed description body is owned by this tool.
- The Linear team may be shared with another sync, so the snapshot query must
  scope to this tool's own tickets by exact predicates (title prefix, managed
  label) rather than by matching the fingerprint metadata block, which does not
  bound the result set.
- Labels are *not* owned by this tool. Unrelated labels are preserved, and any
  label named in `LINEAR_PROTECTED_LABELS` is treated as another actor's state:
  never written, never deleted. Every label the sync does not own is read live
  immediately before an update rather than taken from the run's opening snapshot.
- Linear issue history matters, so deleting and recreating all issues is a last resort, not a normal repair path.
- Cache bypass is the correct operator action when the rendering schema or compare logic changes.
- The Snyk DAST snapshot is verified complete against the API reported `count` before any resolve or cancel is planned: a paginated listing shorter than its reported `count` fails the run instead of being treated as the full target/finding set.
- An empty Snyk DAST snapshot alongside existing managed issues is treated as an
  implausible source, not as "every finding went away": the run fails rather
  than cancelling all managed tickets (see Resolve). Only the "nothing at all"
  case is guarded here; a partial listing is caught by the completeness check
  above.

## Operator Guidance

After any code change, run:

```bash
go fix ./...
go test ./...
go vet ./...
```

Use a normal run for day-to-day sync:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env
```

Use a dry run to inspect planned changes:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run
```

A malformed numeric or boolean configuration value fails the run at startup
with an error naming the variable; it never falls back to the default.

Use cache bypass when you intentionally changed the managed rendering or need a
full live reconciliation:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --bypass-cache
```

## Design Boundaries

If this project grows further, the next reasonable extensions would be:

- richer incremental Snyk DAST fetching using server timestamps where available
- more selective Linear snapshot loading if the API surface allows it safely
- richer cache statistics and observability
- explicit conflict reporting output

The current implementation is intentionally optimized for correctness first,
then steady-state efficiency.
