# snyk-dast-linear-sync

Sync Snyk DAST (formerly Probely) findings with Linear issues since there isn't
an official integration.

Repo:

```text
github.com/tesslio/snyk-dast-linear-sync
```

## What It Does

- Authenticates to Snyk DAST with a JWT API key.
- Reads all targets accessible to the API key (optionally filtered to one team).
- Normalizes Snyk DAST findings into one Linear issue per `target + finding`.
- Stores a stable fingerprint in a hidden metadata block in the Linear issue description.
- Creates missing Linear issues.
- Updates existing Linear issues when managed fields change.
- Ensures a configurable managed label is applied to all managed issues, unless label management is explicitly turned off.
- Moves stale issues to the configured resolved state when the finding is no longer present but the Snyk DAST target still exists.
- Cancels managed Linear issues when their Snyk DAST target no longer exists, such as after target deletion.
- Uses a local SQLite cache to skip unchanged findings and unchanged Linear issues on steady-state runs.
- Sets Linear due dates from Snyk DAST finding creation time using configurable per-severity offsets.

The fingerprint format is:

```text
snyk-dast:<target-id>:<finding-id>
```

## Running

Quickstart without cloning:

Create a local `.env`, then run directly with the repo path:

```bash
go run github.com/tesslio/snyk-dast-linear-sync/cmd/snyk-dast-linear-sync@latest --env-file .env --dry-run
```

Or install the binary:

```bash
go install github.com/tesslio/snyk-dast-linear-sync/cmd/snyk-dast-linear-sync@latest
snyk-dast-linear-sync --env-file .env --dry-run
```

Default usage is to pass a dotenv file explicitly:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run
```

That avoids shell-specific `source` behavior and is the recommended way to run the tool.

Ctrl-C and SIGTERM do not kill the process mid-batch: the signal cancels the
context, in-flight Snyk DAST and Linear calls are cancelled, the run logs
`shutdown requested, cancelling in-flight calls and leaving the cache
untouched`, the cache is left untouched, and the process exits non-zero. The
next run re-examines everything against live data and retries whatever was not
written.

Dry run:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run
```

Verbose dry run (logs each planned create with its title, state, due date,
labels, and full description, and each planned update/resolve with its title,
state, due date, and labels — so you can see exactly what the ticket will look
like in Linear before anything is written):

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --dry-run --verbose
```

`--verbose` also works with a live (non-dry-run) run to log every operation
as it happens.

Normal run:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env
```

Installed binary:

```bash
snyk-dast-linear-sync --env-file .env
```

Bypass cache:

```bash
go run ./cmd/snyk-dast-linear-sync --env-file .env --bypass-cache
```

`--env-file` uses `github.com/joho/godotenv`, so the file can be a normal dotenv file and does not need to be sourced by your shell.

## Validation

Run these before every commit. `go fix ./...` may automatically rewrite code
(for example, modernising loop syntax), so run it first, review the resulting
`git diff`, and include any changes in the same commit. Repeat until
`git diff --exit-code` is clean.

```bash
go fix ./...
git diff --exit-code
go test ./...
go vet ./...
go fmt ./...
```

CI enforces this: the `go fix` check runs `go fix ./...` and then
`git diff --exit-code`, so any unfixed code will fail the build.

## Current Behavior

- Uses `github.com/guillermo/linear/linear-api` plus direct GraphQL mutations for Linear.
- Talks to the Snyk DAST (Probely) REST API directly with `Authorization: JWT <key>`.
- Stores a human-facing Snyk DAST UI link in the Linear description.
- Keeps the Snyk DAST REST API link as a secondary reference.
- Includes target, vulnerable URL, HTTP method, path, parameter, insertion point, CWE, and CVSS details when Snyk DAST provides them.
- Batches Linear create and update mutations to reduce request pressure.
- Retries and backs off on Linear rate limiting. The retryable statuses are `429` for every request, and `502`, `503` and `504` only for idempotent requests and GraphQL reads: every GraphQL request is a `POST`, so the client marks its mutations (create, update, comment, issue-unsubscribe) as non-idempotent, and an operation marked as a mutation is delivered once -- its `502`, `503` or `504` goes to the caller, which reconciles the partial write instead of re-sending it. A response that exhausts the request budget (`X-RateLimit-Requests-Remaining <= 0`), including a successful `200`, is delivered to the caller and delays the next request rather than re-sending the current one. A rate-limit error joined onto a response whose other aliases decoded (a per-alias GraphQL error) is also not re-sent, since those aliases already took effect; only an operation that decoded nothing is retried.
- Normalizes common Linear markdown rewrites when comparing descriptions so steady-state runs do not churn.
- Fails the run when a paginated Snyk DAST listing returns fewer results than the `count` the API reported for it (for example when `page_total` is missing from the response or a page comes back empty early), so the sync never plans against a partial snapshot.

## Snyk DAST (Probely) API Permissions

The sync is **read-only against Snyk DAST** — it never writes back. There is
no built-in read-only role for Snyk DAST, so you must create a **custom role**
(Enterprise plan) with exactly these two permissions:

| Permission | ID | Why |
|---|---|---|
| **View Target** | `view_target` | Grants `GET /targets/`, `GET /findings/`, and `GET /definitions/`. The permission description is "List and view Target, Target Settings, Scans, Scheduled Scans, and Findings." Without it, all three endpoints silently return empty results. The `/definitions/` endpoint is used to determine whether each finding is a passive or active check (for `LINEAR_CHECK_TYPE_LABELS`). |
| **Correlation Viewer** | `correlation_viewer` | Grants `GET /targets/{id}/findings/{id}/integrations/snyk-sast/correlations/`, used to surface the Snyk Code source correlation (responsible repo/file) in each Linear ticket. |

**Scope:** Account (Global) so the sync can see all targets, or scope to specific
Teams/Targets and set `SNYK_DAST_TEAM` to match.

Permissions **not** required (the sync does not write back to Snyk DAST):
`change_finding`, `change_finding_state`, `start_scan`, `start_retest`,
`create_target`, `delete_target`, `change_target_settings`,
`correlation_admin`.

> **Symptom of missing `view_target`:** the sync logs `loaded Snyk DAST
> targets count=0` and `loaded Snyk DAST findings count=0` even though the UI
> shows findings. This is the API filtering out everything because the API key
> has no target visibility — findings are scoped under targets. A token with
> only `change_finding` + `correlation_viewer` (a common mistake) will see
> zero findings for this reason. Because an empty snapshot is not a truthful
> source, the sync now fails the run in this state — with managed tickets
> present in Linear and a snapshot listing no targets, it returns an error and
> makes no changes, instead of cancelling every managed ticket as if all
> findings had gone away (`--dry-run` still reports the plan, logging the same
> message at error level). Add `view_target` to fix it.

> **Symptom of missing `correlation_viewer`:** every target and finding
> loads, then the run fails with an error naming the missing
> `correlation_viewer` permission (the error does not include the target or
> finding URL). The sync cannot build the Snyk Code correlation block without
> it, so the whole run fails instead of silently dropping the block from every
> description. Add `correlation_viewer` to the custom role to fix it.

The per-finding correlation endpoint is also tolerated to answer `404` for a
single finding: that finding is logged at warn and synced without its
correlation block, the same as a finding that never had one. Any other error
from the endpoint (for example a `500`) still fails the run.

See the Snyk docs on [roles and scopes by plan](https://docs.snyk.io/scan-fix-and-prevent/scan-with-snyk/snyk-api-web/managing-account/roles-and-scopes-by-plan) and [understanding permissions](https://docs.snyk.io/scan-fix-and-prevent/scan-with-snyk/snyk-api-web/managing-account/understanding-permissions).

## Linear Permissions

This project is designed to work with:

- `Read`
- `Create issues`
- `Update issues`

It does not require label creation permissions.
If `LINEAR_MANAGED_LABEL` is enabled, the configured label must already exist in Linear.

If `LINEAR_COMMENTS=true` is enabled, the sync also posts change-summary comments on updated issues, which requires Linear comment creation permission. This is off by default.

## Managed Linear Description

Each managed issue contains:

- a heading with vulnerability title and severity
- target name/host and target URL immediately below
- the vulnerable request context (URL, method, path, parameter, insertion point)
- CWE and CVSS context when available
- the fix description when provided
- Snyk Code source correlation block when Snyk DAST has linked a Snyk Code source vulnerability (surfaces the responsible repo/file)
- Snyk DAST UI and API links grouped together
- target and finding IDs lower in the body for debugging and API work
- hidden metadata block

The metadata block is required for deduplication and safe updates:

```text
<!-- snyk-dast-linear-sync
fingerprint: snyk-dast:target-123:456
managed_labels: snyk-dast-automation,snyk-dast-api
-->
```

Changing or removing that block can cause duplicate issues or prevent updates from matching the correct Linear issue.

Because Snyk-supplied text (the fix description and the Snyk Code correlation
block) is embedded in the body and can quote scanned-application content, any
HTML comment opening in it is neutralized (`<!--` becomes `<!- -`) before it is
embedded. Embedded Snyk-supplied prose is also capped at 10,000 runes per
block (with a stable `[truncated by snyk-dast-linear-sync]` marker), so an
oversized block can never push the metadata block past Linear's description
limit. Because a finding can be linked to any number of Snyk Code
vulnerabilities, at most three correlation blocks are embedded; any further
ones are summarised by a stable `_N more linked Snyk Code vulnerabilities not
shown._` line, so the whole correlation section stays within a fixed bound.
Metadata block detection also takes the *last* line-anchored marker, since the
managed block is always appended last. Prose therefore cannot spoof or
hide the fingerprint, and an update rewrites only the real block.

The whole description body above the metadata block is bounded too, at a limit
derived from the per-block caps plus a margin for headings and single-line
fields, so one maximal Fix and three maximal correlation blocks still render
in full while the bound stays below Linear's description limit. A line that
does not fit is dropped whole with a stable truncation marker, and a `### `
section heading is dropped together with its body, so an oversized field can
never push the metadata block away or leave an empty section heading.

## Issue Format

The synced issue body is optimized for fast developer triage:

- heading first: vulnerability title plus severity
- target and vulnerable request context immediately below
- Snyk DAST UI and API links grouped together
- fix context next
- target and finding IDs lower in the body for debugging and API work

The synced title includes the most useful DAST context:

- the scanned target host (falling back to target name)
- the vulnerable request as `METHOD path`

These fields come from the scan API unbounded, so the rendered title is capped
at Linear's title limit (255 characters, measured in runes) with a `...`
truncation marker. Without the cap, one long request path or target name would
make that finding's issue-create fail on every run.

## Managed Labels

`LINEAR_MANAGED_LABEL` controls the automation label applied to managed issues:

- default: `snyk-dast-automation`
- set to another label name to use that label instead
- set to `off` to disable label management

When label management is enabled, the sync:

- adds the configured label to newly created managed issues
- preserves unrelated existing labels, reading each ticket's current labels live
  immediately before an update so a label another actor added mid-run survives
- removes the previously managed label if the configured label changes
- removes the previously managed label if label management is turned off

Turning label management off (`off`) also turns off the derived check-type and
target-type labels, even when their mappings or defaults are still configured:
none of them are asserted, and the ones previously managed are removed.

If the configured label does not exist in Linear, the run fails with a clear message telling the operator to create the label or disable label management.

Linear label groups are mutually exclusive — an issue may carry at most one child of a group. If a ticket carries two children of the same group (for example, labels applied before the group was made exclusive), updates keep exactly one: the managed label when the sync asserts one for that group, otherwise the first existing one. Without this, Linear rejects every update for such a ticket ("labelIds not exclusive child labels") until the ticket is edited by hand.

`LINEAR_CHECK_TYPE_LABELS` optionally maps Snyk DAST check types to additional managed Linear labels. Snyk DAST has two check types — **passive** (response analysis, e.g. missing security headers) and **active** (exploit payloads, e.g. SQL injection, XSS) — which is the DAST equivalent of Snyk's multi-product tool labels (`snyk-code`, `snyk-open-source`, etc.). Passive and active findings have different triage workflows, so labeling them separately helps route issues to the right team.

- format: comma-separated `check_type:label` pairs
- example: `passive:snyk-dast-passive,active:snyk-dast-active`
- labels must already exist in Linear

`LINEAR_CHECK_TYPE_LABEL_DEFAULT` controls the fallback label for check types without an explicit mapping:

- default: `off`
- set to `off` to disable the fallback

`LINEAR_TARGET_TYPE_LABELS` optionally maps Snyk DAST target `type` values to additional managed Linear labels:

- format: comma-separated `target_type:label` pairs
- example: `api:snyk-dast-api,single:snyk-dast-web`
- labels must already exist in Linear

`LINEAR_TARGET_TYPE_LABEL_DEFAULT` controls the fallback label for target types without an explicit mapping:

- default: `off`
- set to `off` to disable the fallback

`LINEAR_PROTECTED_LABELS` lists labels this sync must never add to, or remove
from, an existing ticket. Use it for labels another actor owns as control state —
a triage bot, a dispatch harness, a human workflow — where deleting one silently
undoes that actor's decision.

- format: comma-separated label names
- example: `df:dispatch,df:dispatch-complete`
- default: empty (no labels protected)
- set to `off` to disable protection entirely, overriding any value in an env file

Linear's `issueUpdate` replaces a ticket's whole label set rather than applying
a delta, so every update restates the labels the ticket should end up with. This
sync owns only its managed labels; every other label — protected, human-applied,
or from a different automation — belongs to someone else and is carried forward
untouched.

To make that safe against edits made mid-run, each update batch re-reads the
current labels of the issues it is about to write, immediately before the write.
That live read — not the run's opening snapshot, which can be minutes old — is
the authority for which labels the ticket carries:

- a label another actor added since the snapshot is preserved; one removed since
  the snapshot is not resurrected.
- if that live read returns nothing for an issue, or returns a truncated page of
  its labels, the update is refused rather than proceeding — writing a label set
  that might drop a label the sync does not own is worse than skipping one issue
  for one run. The caller retries issues individually, so the rest of the batch
  still lands, and the refusal is counted in `failed_ops`, which fails the run.
- the run's opening snapshot pages labels the same way and refuses a truncated
  page, because the snapshot feeds the cache hash: a label read that truncates
  at Linear's default page size would stop the hash from seeing a managed
  label removed by hand past the boundary.
- protected labels are never asserted from the managed set either, so a
  misconfiguration cannot stamp another actor's control label onto a ticket that
  does not carry one.
- new issues need no special handling: a create has no pre-existing label set to
  replace.

The live read costs one extra query per update batch and always runs, whether or
not any labels are protected: a human's or another automation's label needs it
exactly as much as a protected control label does.

`LINEAR_UNSUBSCRIBE_ACTOR` controls whether the Linear API actor should be kept off the subscriber list for managed issue creates and updates:

- default: `true`
- `true`: create issues without subscribing the actor, and preserve the current subscriber list unchanged on updates
- `false`: let Linear use its default subscriber behavior

This setting only controls the issue subscriber list. Linear will still record the API user as the issue creator, and the create mutation response may briefly include that creator in `subscribers` even when the persisted issue subscriber list is empty after a refresh.

`LINEAR_COMMENTS` controls whether the sync posts a change-summary comment on each updated Linear issue, explaining which managed fields changed and why (state, due date, labels, etc.):

- default: `false`
- `true`: post a comment after each successful update batch
- `false`: no comments; the Linear activity log still shows what changed

When enabled, this requires Linear comment creation permission in addition to the base `Update issues` permission.

## State Mapping

Snyk DAST (Probely) finding states map to Linear workflow states:

- `notfixed` / `retesting` -> `Todo`
- `accepted` with a future `expiration_date` -> `Todo` (SLA from acceptance expiry)
- `accepted` with a past `expiration_date` -> `Todo` (the acceptance has lapsed; the due date is the acceptance expiry plus the configured severity offset, the same basis used while the acceptance was in force). A date-only `expiration_date` covers the whole of that day, so the acceptance lapses at the end of it, not at midnight.
- `accepted` without an `expiration_date` -> `Cancelled`
- `accepted` with an `expiration_date` that cannot be parsed -> `Cancelled` (treated the same as a permanent acceptance; the sync logs a warning naming the finding and the unparseable value, so the cancellation is visible)
- `invalid` -> `Cancelled`
- `fixed` -> `Done`
- missing finding in an existing Snyk DAST target -> `Done`
- missing finding because the Snyk DAST target no longer exists -> `Cancelled`

The configured Linear state names are resolved by name first, then by workflow type where possible.

These distinctions are intentional:

- A time-limited `accepted` finding is kept open in `Todo` because it requires attention once the acceptance expires. Its due date is calculated from the acceptance expiry so the SLA extends to the normal severity offset from when the acceptance ends.
- Once that acceptance has lapsed, the finding stays in `Todo` rather than being cancelled. A time-limited acceptance is a request to be reminded, so treating an expired one as a permanent acceptance would silently close the ticket at exactly the moment it needs attention.
- A permanently `accepted` (no expiry) or `invalid` finding is cancelled: it is no longer actionable.
- If a Snyk DAST finding disappears but the target still exists, the tool treats that as the finding being resolved and moves the Linear ticket to `Done`.
- A finding that cannot be normalized is deferred, not resolved. When a finding's embedded target summary has no id, no fingerprint can be built for it, so it is skipped with a warning naming its finding id, and every managed ticket whose fingerprint ends in that finding id is left untouched for the run (matched on the finding-id part of the fingerprint, because the target id is what is missing). The run reports the count as `deferred_tickets`.
- If the Snyk DAST target itself is gone, the tool treats the managed Linear ticket as no longer actionable and moves it to `Cancelled`.

### Manual Non-Terminal State Override

If a user manually moves a managed issue to any non-terminal Linear state that differs from what the sync would set (e.g. `Todo` or `In Progress` when the configured open state is `Triage`), subsequent syncs will preserve the user's chosen state. This prevents the automation from dragging issues back to the configured state after intentional triage decisions.

Terminal states (`Done`, `Cancelled`) are never preserved — the sync always transitions to a terminal state when the Snyk DAST finding is fixed or permanently accepted/invalidated.

### Snapshot Scope

Each run loads a snapshot of the sync's own Linear tickets. The query is scoped
by team, then by an archive window, then by identity:

```text
team = LINEAR_TEAM_ID
  AND (not auto-archived OR auto-archived within the lookback window)
  AND (title starts with "Snyk DAST:" OR carries LINEAR_MANAGED_LABEL)
```

Both identity predicates are exact — a title prefix and a label name — and either
alone identifies a managed ticket, since the sync builds every title with the
prefix and re-asserts the managed label on every update. Keeping both means a
retitled ticket, or one whose label was removed by hand, is still found rather
than being invisible and duplicated. With `LINEAR_MANAGED_LABEL=off` the label
clause is omitted rather than matching nothing.

This matters when the Linear team is shared with another sync: the scope must
exclude the other tool's tickets. The fingerprint metadata block is deliberately
**not** used as a query predicate — `description: { contains: … }` did not bound
the result set (a run against a shared team loaded 59,866 issues for 7 findings,
effectively the whole team), and Linear does not document `description` as
supporting the string comparators at all. The metadata block remains the
authority for identity once an issue is loaded; it just cannot be trusted to
select which issues to load.

The same scope bounds the ambiguous-create reconciliation query
(`ExistingFingerprints`), which runs in the retry path right after a failed
batch create: scoped this way it pages only through this sync's own tickets,
instead of every non-archived issue in the team.

### Archived Linear Issues

Linear auto-archives closed issues after the team's configured inactivity
period, and archived issues are excluded from Linear's default issue query.
Snyk DAST keeps `fixed`, `accepted`, and `invalid` findings in its API
indefinitely, so if the sync cannot see its own archived tickets it considers
them missing and creates a fresh duplicate. The replacement is itself visible
until Linear archives it in turn, at which point the finding looks ticketless
again — so the duplication repeats once per archive cycle rather than on every
run, and the copies accumulate.

The issue snapshot therefore requests archived issues explicitly, bounded to
those auto-archived within `LINEAR_ARCHIVE_LOOKBACK_DAYS` (default 3650 — ten
years, i.e. effectively the whole archive). The sync treats archived issues as terminal and
does not update, resolve, or cancel them in place. If a finding whose ticket has
been archived becomes active again, a replacement ticket is created.

Creating a replacement is a deliberate choice rather than an API limitation.
Linear exposes an `issueUnarchive` mutation with no documented precondition, so
restoring the original ticket and updating it would be possible. That path was
considered and not taken: each recurrence gets a clean ticket with its own SLA
clock, rather than one ticket reused indefinitely across recurrences.

#### Why the window is effectively unbounded

`LINEAR_ARCHIVE_LOOKBACK_DAYS` bounds how long ago a ticket was **auto-archived**
— not how long your team's auto-archive period is. Linear expresses that period
in *months* (`Team.autoArchivePeriod`), and the two values are independent: a
short window is not made safe by a short period.

The window applies only to auto-archived issues, because it filters on
`autoArchivedAt`. Issues archived manually or through the API — including
trashed (deleted) ones — carry `archivedAt` without `autoArchivedAt`, so they
match the not-auto-archived filter arms and are returned regardless of age. That
is deliberate: including them can only prevent duplicates, never cause them, and
the pinned `linear-api` binding predates `IssueFilter.archivedAt` so bounding
them is not expressible today.

Because Snyk DAST retains `fixed`, `accepted`, and `invalid` findings
indefinitely, each of them keeps a live desired issue forever. So any *finite*
window eventually drops the ticket out of the snapshot, the finding looks
ticketless, and a duplicate closed ticket is created. Worse, the copies do not
converge: the original is no longer visible, so the duplicate-cancellation pass
cannot pair them up, and one extra closed ticket accumulates per cycle.

Worked example with a one-month auto-archive period and a 35-day window: the
ticket archives ~30 days after closing, stays visible for 35 more days, and is
recreated at roughly day 65 — then that copy repeats the cycle. About one
spurious ticket every two months, per permanently-terminal finding, forever.

Hence the ten-year default, which covers the whole archive in practice. The
setting remains configurable purely as a size/latency escape hatch: lowering it
shrinks the snapshot and speeds up each run, at the cost of reintroducing the
cycle above. The snapshot query pages 100 issues at a time and costs roughly
6,300 of Linear's 10,000 per-query complexity points, so widening the window
costs extra pages rather than risking a rejected query.

The alternative fix — not creating tickets for findings that are already
terminal and have no ticket — was considered and rejected: every Snyk DAST
finding should end up with a Linear ticket, including findings closed before the
sync first ran, so the ticket history is complete.

### Due Dates

Default due date offsets:

- critical -> 15 days
- high -> 30 days
- medium -> 45 days
- low -> 90 days

The base date is normally the Snyk DAST finding `created_at` timestamp. For
time-limited `accepted` findings, the base date is the acceptance expiry date,
so the due date extends to the normal severity SLA calculated from when the
acceptance expires.

Past due dates are left as-is (Linear renders them as overdue) rather than
floored to today, both to show how long an issue has exceeded its SLA and to
avoid daily cache churn.

## Cache Behavior

The cache lives in SQLite and stores:

- the UTC time the snapshot was saved, reported as `saved_at` in cache logs
- a schema signature for the managed issue format
- a normalized hash for each Snyk DAST finding
- a normalized hash for each managed Linear issue

On a normal run:

1. Load Snyk DAST findings.
2. Load the current Linear snapshot.
3. Skip fingerprints whose source hash and Linear hash both match the last successful run.
4. Apply creates, updates, and resolves for the rest.
5. Refresh the cache from the live post-write Linear snapshot. A fingerprint
   whose Linear write failed in this run is left out of the refresh, so the next
   run retries it instead of skipping it as unchanged.

An interrupted run (Ctrl-C, SIGTERM, or any cancelled context) never refreshes
the cache: its plan was only partly written, so the next run re-examines
everything against live data.

A cache read that fails — including a read that stops part-way through a table —
aborts the run instead of comparing against a partial hash snapshot. Delete the
cache file or rerun with `--bypass-cache`; both rebuild it from live data.

The cache fast-path never suppresses a pending move into a terminal state
(`Done`/`Cancelled`). Use `--bypass-cache` to ignore the cache for a run and
rebuild it from live data.

## Configuration

Required:

- `SNYK_DAST_API_KEY`
- `LINEAR_API_KEY`
- `LINEAR_TEAM_ID`

Optional:

- `--env-file`
- `SNYK_DAST_API_BASE`
- `SNYK_DAST_APP_BASE`
- `SNYK_DAST_TEAM`
- `LINEAR_STATE_TODO`
- `LINEAR_STATE_BACKLOG`
- `LINEAR_STATE_DONE`
- `LINEAR_STATE_CANCELLED`
- `LINEAR_MANAGED_LABEL`
- `LINEAR_CHECK_TYPE_LABELS`
- `LINEAR_CHECK_TYPE_LABEL_DEFAULT`
- `LINEAR_TARGET_TYPE_LABELS`
- `LINEAR_TARGET_TYPE_LABEL_DEFAULT`
- `LINEAR_PROTECTED_LABELS`
- `LINEAR_UNSUBSCRIBE_ACTOR`
- `LINEAR_COMMENTS`
- `LINEAR_ARCHIVE_LOOKBACK_DAYS`
- `LINEAR_DUE_DAYS_CRITICAL`
- `LINEAR_DUE_DAYS_HIGH`
- `LINEAR_DUE_DAYS_MEDIUM`
- `LINEAR_DUE_DAYS_LOW`
- `SYNC_WORKERS`
- `SNYK_DAST_HTTP_CONCURRENCY`
- `LINEAR_HTTP_CONCURRENCY`
- `ERROR_LOG_FILE`
- `CACHE_DB_FILE`

Integer variables must be plain integers, and boolean variables must be
`true`/`false` (or another form `strconv.ParseBool` accepts: `1`, `0`, `t`,
`f`, `T`, `F`; spelling is case-insensitive). A malformed value stops the run
at startup with an error naming the variable and quoting the value it could
not parse; it is never silently replaced by the default.

See [.env.example](/.env.example).

## Diagnostic Tool

`cmd/investigate-duedate` is a helper for inspecting the due-date decision for a
single managed Linear issue. It loads the issue by identifier, finds the
matching Snyk DAST finding, and prints every plausible due-date scenario plus
the cache state:

```bash
go run ./cmd/investigate-duedate --env-file .env --issue SEC-12127
```

Add `--json` for machine-readable output.

The helper follows the sync's signal handling: Ctrl-C or SIGTERM cancels its
in-flight calls, and the run then finishes the way it always does -- it prints
the diagnostics it could build from what was loaded and exits non-zero. On the
way out it says so on stderr: `shutdown requested; cancelling in-flight calls
and exiting non-zero`, followed by `exiting non-zero` with the status it exited
with. If the signal lands before a call answers, that cancelled call is what the
run reports as its error instead of the diagnostics; either way the run is never
reported to a shell or a scheduler as a success, and a complete run exits 0.

## Exit Status

The sync exits non-zero when a state-changing Linear operation was left
permanently unapplied, so a scheduler or CI job notices the drift instead of
seeing a successful run that quietly dropped work.

"Permanently" is the key word. Batch mutations are retried per entry, and only
the outcome after that retry counts:

- a batch that fails and whose retry lands is a **successful** run — the
  operation applied, it just took two calls
- a create, update, resolve or duplicate-cancellation whose retry also failed, or
  which was deliberately left for the next run because reconciliation was
  impossible, is counted in `failed_ops` and **fails** the run
- `failed_comments` never fails the run: a missing change comment leaves the
  synced issue state correct, and comments are opt-in via `LINEAR_COMMENTS`

Both counts appear in the `sync complete` summary log either way, and that
summary is emitted before the exit status is decided.

An interrupt is a non-zero exit too, with a different cause. Ctrl-C or SIGTERM
cancels the context, so in-flight Snyk DAST and Linear calls are cancelled, no
further jobs start, and the run logs `shutdown requested, cancelling in-flight
calls and leaving the cache untouched`. The cache is **not** refreshed — the
plan was only partly written, so a refresh would describe the fingerprints whose
jobs never ran as reconciled (see [Cache Behavior](#cache-behavior) and
PROJECT.md `## SQLite Cache`). `Run` returns an error wrapping
`context.Canceled`/`context.DeadlineExceeded`, which becomes the process exit
status, so a scheduler or CI job notices. A signal that arrives after a run has
already finished changes nothing.

## Logs

- Console logs show startup, load progress, work progress, cache refresh, and final summary.
- Error logs are appended to `ERROR_LOG_FILE`.
- Default error log path: `logs/snyk-dast-linear-sync-errors.log`
- The console and error-log handlers are independent: every record is offered to
  each handler whose level admits it, and one handler failing (a closed console
  pipe, a full disk) never stops the record reaching the other. A handler
  failure is reported as the logger's error but does not swallow the record.

## More Detail

See [PROJECT.md](/PROJECT.md) for the project intent, architecture, sync rules, and operational model.
