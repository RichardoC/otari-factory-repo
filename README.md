# factory-repo

An experiment in running [CodeAF](https://github.com/Agent-Field/CodeAF) entirely inside
GitHub: issues go in, CodeAF does the work in GitHub Actions, and pull requests come out.

It is set up as a monorepo:
- applications live under `app/<name>/`, each its own Go module with its own README
  and licence;
- CI checks every module separately;
- the first application here is [kube-sqlite3-vfs](app/kube-sqlite3-vfs/README.md), a SQLite VFS that stores database files in Kubernetes ConfigMaps;
- the CodeAF harness and CI live in `.github/`.

## Using it

| You do | CodeAF does |
|---|---|
| Add the **`codeaf`** label to an issue | Works the issue on a new branch `codeaf/issue-N-<run id>` cut from the default branch and opens its own PR (labelled `codeaf`), so re-running never collides with an earlier run |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled PR | Continues on the PR's branch with your instructions, the review comments and the conversation |
| Submit a **Request changes** review on a `codeaf`-labelled PR | Same as above, using the review as the instructions |
| Comment `/codeaf <instructions>` on a `codeaf`-labelled issue | Re-runs the issue on a new branch and opens a new PR; earlier runs' branches and PRs are left alone |

Nothing else triggers it. Only people with triage access can add labels, and `/codeaf`
commands are obeyed only from repository owners, members and collaborators, so
outside contributors cannot spend the API budget. PRs from forks are ignored.

Every run posts a report comment with CodeAF's outcome, a per-model cost table —
including each model's reasoning share (exact from reasoning tokens, or an estimate
labeled “(chars)” from character counts when the model does not report them) — and the
run's timing figures (wall time, time with a call in flight, tokens/s, longest call).
Three workflow artifacts are uploaded, none of them encrypted:
- `codeaf-output-*` (14 days): the patch, its base commit, CodeAF's JSON result
  and the call ledger (each call's start and duration, slot wait, the wait for
  upstream's response headers (`header_ms`, issue #145), reasoning tokens,
  and the reasoning/output character counts backing the estimate);
- `codeaf-diagnostics-*` (30 days): the brief, CodeAF's stderr, its run records
  (prompts, replies, tool output), call log and usage, and the proxy log;
- `codeaf-state-*` (14 days): what the run left in CodeAF's home, and what it
  cost, for the shared state below.

### Independent review of every PR (issue #32)

Every CodeAF run that opens or updates a pull request is followed by one
**review run** — a second CodeAF run in read-only review mode. The reviewer
sits on a different model from the worker that wrote the patch (worker
**GLM-5.3-Flash**, reviewer **DeepSeek-V4.1-Flash 10/17 checkpoint**, both from
[`models.json`](.github/codeaf/models.json)), so it does not share the same
blind spots. It reviews the PR's diff against the issue for correctness,
concurrency, error handling and whether the tests actually exercise the
claims, and it re-runs the benchmarks the patch claims. The queue starts the
review run itself: `publish` no longer dispatches it directly, it labels the PR
**`codeaf-review-needed`**, and the queue picks the PR up as a `CodeAF review #N
(lane L)` run that holds no area, through the same spend threshold and lane
capacity as issue runs.

It posts one findings comment on the PR, with a severity and a file:line for
each finding. When anything is high or medium, that comment doubles as a
`/codeaf fix` command and a fix run starts on the PR's branch, counted
against the same continuation cap as the auto-continue feature — **2 per PR**.
When nothing blocks, the PR gets the **`codeaf-review-passed`** label, so a
person or a merge rule can merge with confidence.

The review run never publishes anything: it has no write token, and `publish`
applies no patch from it. Starting it goes through the same gate, spend caps
and state merge as any other run. The workflow change itself needs a
maintainer's approval, like any other change under `.github/`.

## How it is wired

- [`.github/workflows/codeaf.yml`](.github/workflows/codeaf.yml) runs three jobs,
  each checked out at the default branch:
  - `gate` checks the event against the rules above (`harness gate`).
  - `run`, which has no write token, builds CodeAF at the commit pinned in
    [`codeaf-ref`](.github/codeaf/codeaf-ref), writes a brief from the
    issue/PR, runs `codeaf do` headless and hands over a patch. The agent
    runs as its own unprivileged user -- created fresh each run with no
    sudo, handed the working tree and CodeAF's home for the run, started in
    a session of its own -- so it can neither signal nor read the runner
    (`Runner.Worker`, the pricing proxy) or use the runner's sudo; the
    workflow proves that with probes before the agent starts.
  - `publish`, on a fresh runner, checks and applies the patch, then commits,
    pushes, opens or updates the PR and dispatches [CI](.github/workflows/ci.yml).
    Pushes made with `GITHUB_TOKEN` do not trigger CI on their own.
- [`.github/codeaf/main.go`](.github/codeaf/main.go) is the harness: a standalone,
  standard-library-only Go program, with tests, that the workflow builds on every run.
  It decides whether an event may run (`gate`), writes the brief (`brief`), and renders
  the cost report (`report`). As `harness proxy` it runs on `127.0.0.1` for the
  length of the job and sits between CodeAF and hosted
  [Otari](https://docs.otari.ai) (`https://eu.api.otari.ai/api/v1`), which
  fronts the same synthetic.new models. It:
  - serves CodeAF an OpenRouter-style `/models` catalog containing only the models in
    [`models.json`](.github/codeaf/models.json), at synthetic.new's **API list
    ("overage") prices** (`api_*`), the prices every figure and cap in the factory
    uses; the same file also keeps each model's subscription rates, for reference only
    (both checked 2026-10-17, [synthetic.new/pricing](https://synthetic.new/pricing));
  - adds `usage.cost` to every response, which is where CodeAF's spend accounting
    reads cost from, and records Otari's own `cost_usd` in the ledger row
    (`otari_cost_usd`, reference only — caps still price from the `api_*` list
    rates);
  - refuses any other model, and returns HTTP 402 once the run reaches its per-run
    credit cap (**$5**, `limits.run_credit_usd`) or the week's credit is gone
    (`limits.weekly_credit_usd`, from the shared state); with `overage_usd` 0 (the
    default) it stops there rather than pay API prices;
  - makes no `/v2/quotas` call: chat goes through hosted Otari now, and Otari
    enforces the organisation's own budget and balance, so the old
    `min_credit_percent` quota floor is gone. The factory's own
    `run_credit_usd`/`weekly_credit_usd` caps stay as runaway guards;

  One key is held only by the proxy, passed in on stdin (a single line),
  never through the environment: the Otari key (`OTARI_API_KEY` secret),
  sent only to hosted Otari — chat and web search (Otari's
  `otari_web_search` tool, run inside the model call) both go there. The
  factory no longer holds a synthetic.new API key at all, and the key is never
  logged.
- Model choice: the seats are spread across models. synthetic.new used to
  allow only 2 calls to each model at once; since the October 2026 pricing
  change concurrency is by plan priority instead, and the factory sets its
  own per-model cap (`max_concurrent`, 8):
  - the worker runs on **GLM-5.3-Flash** ($0.15 in / $0.50 out per M tokens at
    the API list prices the factory quotes);
  - the planner and checker run on **DeepSeek-V4.1-Flash 10/17 checkpoint** ($0.60 / $1.20).
  - CodeAF's `task.parallel` is set per run to that run's slice of the
    worker model's `max_concurrent` (below).
  - The proxy enforces the limit per model (`max_concurrent` in
    `models.json`): a call past it waits for a slot instead of reaching
    synthetic.new. Each wait is written to the proxy log and the call
    ledger, so the diagnostics show whether a run was held up by it.
  - Escalation is switched off for now. CodeAF's cascade
    ([`panel.json`](.github/codeaf/panel.json)) would escalate to
    **DeepSeek-V4.1-Flash 10/17 checkpoint** ($0.06 / $0.30) and then **GLM-5.3** ($0.40 / $2.20),
    but it has two bugs:
    - it treats a reply made only of tool calls as empty, and fails every run;
    - its pinned rungs all call the seat model, so it never escalates.
  - [`codeaf.yml`](.github/workflows/codeaf.yml) explains how to turn it back on
    once CodeAF is fixed.

- Tests: `go test -race ./...` in [`.github/codeaf`](.github/codeaf) (what
  [ci.yml](.github/workflows/ci.yml) runs) covers the harness two ways. The
  unit tests call its Go functions directly; the contract tests
  ([`contract_test.go`](.github/codeaf/contract_test.go), issue #135) build the
  binary and run it the way the workflows do — the same `go build -trimpath`,
  with `models.json`, `codeaf-config.json`, `experiments.json`, `panel.json`
  and the instruction files copied beside it, and each subcommand invoked with
  only the environment the workflow would set, against fixture events in
  `testdata/contract/` and a stub GitHub API — so a subcommand that reads the
  wrong variable or prints the wrong output lines fails in CI, not in a live
  run. Run them with `go -C .github/codeaf test -race -run TestContract .`
  (under 60 s; `-short` skips the build). A shellcheck test
  (`TestWorkflowShellcheck`) lints the workflows' `run:` blocks when `yq` and
  `shellcheck` are installed.

### Web search through Otari (issue #2)

Web search now runs inside the model call through hosted Otari: the proxy
declares Otari's `otari_web_search` tool on every chat request it forwards,
Otari offers it to the model, and when the model searches, Otari runs the
search and feeds the results back — all within the one HTTP request. There
is no separate search endpoint any more: the old separate search proxy route
and the separate search command are gone, and CodeAF sees only plain
OpenAI-shaped responses (the proxy strips Otari's extra `otari_tool_calls`
field from what it relays, logging and recording counts only, never the
queries or sources).

Caps: `limits.search_max_uses_per_call` (3 in
[`models.json`](.github/codeaf/models.json)) limits the searches in one
model call, and `limits.searches_per_run` (50) caps the searches one run
may make in total — past that the proxy stops declaring the tool. The
price is `limits.search_price_usd`, about 0.01 USD per search: a call's
recorded cost is the larger of its list-price token cost plus
`searches × search_price_usd` and Otari's own `usage.cost_usd`. Each call's
ledger row carries a `searches` count, and the report's "web searches" row
reads it. The queries do leave the runner inside the model call (to the
search backend Otari is configured with); the Otari key stays inside the
proxy — CodeAF never sees it. Search results are untrusted text from the
internet — data, not instructions (see
[`AGENTS.md`](AGENTS.md#web-search)).

### Billing model (Big plan, issue #59)

The synthetic.new account is on the **Big** plan ($90/mo). Usage draws down a
**weekly credit** at the subscription rates, and past the quota usage is billed
at the **API list prices**, up to `limits.overage_usd`; at 0 (the default) the
factory stops rather than pay beyond the $90. Chat calls now reach synthetic.new
through hosted Otari, and Otari enforces the organisation's own budget and
balance — the proxy makes no `/v2/quotas` call and has no quota floor.

**Every figure and cap is at the API list ("overage") prices** (the owner's
decision, 2026-10-20): a call is priced at `api_input`/`api_cached`/`api_output`,
and the subscription rates in `models.json` are kept for reference only. On the
factory's model mix that is about 4.5× the subscription credit (DeepSeek ~7.9×),
so `weekly_credit_usd` is 100 (maxCredits × 4.5, used only to pace the queue)
and `run_credit_usd` (5) caps one run, about 2.5× the most expensive run seen.
The hard stop is the factory's own caps: the proxy refuses at
`run_credit_usd`/`weekly_credit_usd` (below), and the budget itself is enforced
by Otari and by a spending limit on the synthetic.new account.

What the harness does with it:
- the catalog prices CodeAF sees are the API list prices, the same ones the
  proxy caps on;
- the shared state carries the rolling 7-day credit spend; `harness state load`
  outputs the day's and the week's figure, and the queue starts a run only while the
  rolling week is under 75% of the weekly credit (`CODEAF_WEEK_SPENT_USD` — an
  unknown spend starts nothing);
- the proxy refuses at the weekly credit unless `overage_usd` > 0, and prices
  past-quota calls at the API rates against the overage cap.

CodeAF's own config maps onto this as closely as its settings allow: CodeAF has no
weekly-budget key, and `daily_budget_usd`'s window is a single run's lifetime, so
it is set to 20 (the weekly credit, the closest match) with `models.crew.cap` 20;
`task_cap` 5 matches `run_credit_usd`.

**Fast and Infinite modes: not used.** Neither is enabled on the account or
configured in `models.json`, and no experiment profile names them.

## Adding an app

More applications live side by side under `app/<name>/`, each its own Go
module. To onboard one:

- the app directory must have four files: `go.mod`, `README.md`, `LICENSE`
  and `AGENTS.md` (an app-specific rules file; `TestAppLayout` in the
  harness checks all four);
- an **`area:app/<name>`** label must exist (the queue job creates it
  automatically from the directories under `app/`, colour `cccccc`) and must
  be on **every issue** for the app — an issue without an area label is held
  by the queue until it has one;
- the worker, reviewer and planner briefs embed the app's `AGENTS.md` (the
  worker and reviewer read it from the default branch, never the PR head)
  automatically, so app-specific rules (build flags, build tags, benchmark
  conventions, invariants) belong there, not in the issue. See
  [app/kube-sqlite3-vfs/AGENTS.md](app/kube-sqlite3-vfs/AGENTS.md) for an
  example.

## The queue

Issues labelled **`codeaf-queued`** are the backlog. Every 10 minutes (and on
demand, via *Run workflow*)
[`codeaf-queue.yml`](.github/workflows/codeaf-queue.yml) asks the harness
(`harness next`) to start the next run: the next candidate — a pull request
labelled **`codeaf`** and **`codeaf-review-needed`** (a review run), a pull
request labelled **`codeaf`** and **`codeaf-continue-needed`** (a continuation
of that PR), or the oldest queued issue, with **`codeaf-priority`** jumping the
queue — for which all of these hold:

- fewer than `queue.max_parallel_runs` (in
  [`models.json`](.github/codeaf/models.json), default 2) `codeaf.yml` runs
  are queued or in progress — issue runs, PR continuations and review runs
  alike;
- no area conflict with an in-flight run: areas are `app/<name>/` per app
  and the harness `.github/`, taken from `area:` labels or paths in the
  issue body; an issue with no area conflicts with everything;
- every issue named in a "Depends on #N" line in its body is closed;
- the issue is ready (issue #136): its body has a `## Done when` heading with
  at least one `- ` bullet under it and a `## End-to-end check` heading with
  some text under it, and it carries an `area:` label — issues labelled
  `codeaf-ref-update` or `codeaf-experiment`, whose bodies are generated, are
  exempt from the two body sections, not from the area label. An issue held
  for any of these gets a one-time comment naming what is missing. Copy
  [`.github/codeaf/issue-template.md`](.github/codeaf/issue-template.md) to
  `.github/ISSUE_TEMPLATE/codeaf-task.md` to give new issues the sections;
- the rolling week's credit spend in the shared state is under 75% of
  `limits.weekly_credit_usd` in [`models.json`](.github/codeaf/models.json) — an
  unknown spend starts nothing;
- the candidate is queued — or is a PR labelled for review or continuation —
  and not already the subject of an in-flight run.

Review and continuation runs go through the same spend threshold, lane
capacity and area rules as issue runs, with one difference in what they hold:
a review run holds no area at all and never conflicts with anything, while a
continuation holds the PR's areas, resolved the same way as any PR run's.

A run is started by adding the **`codeaf`** label — the same trust gate as a
person adding it — and then by dispatching
[`codeaf.yml`](.github/workflows/codeaf.yml) with the issue number, or with the
PR number and the run's kind for a review or continuation. `publish` no longer
dispatches those runs itself with `gh workflow run`; it only adds the
`codeaf-review-needed` or `codeaf-continue-needed` label, and the queue starts
the run. The
dispatch step exists because events raised with `GITHUB_TOKEN` (labelling
included) do not trigger other workflows; `workflow_dispatch` is the one event
it can start. The queue starts runs only up to the parallel-run limit, and
never two on the same area, so queued work overlaps only where those rules
allow ([running several tickets at once](#running-several-tickets-at-once-issue-60)).
Runs started by hand (a label or a `/codeaf` comment) can still overlap with
them; they are not held in a single
concurrency group, because GitHub would cancel all but the newest pending run
in it and silently drop a command.

Manual controls: *Run workflow* on either workflow — `codeaf-queue.yml` runs
the queue's checks now, `codeaf.yml` starts an issue when given its number.
Known failure modes:
- a "Depends on #N" line naming an open pull request blocks too (the issues
  API lists PRs);
- if the dispatch step fails after labelling, the issue keeps `codeaf` but
  nothing runs — a maintainer runs `codeaf.yml` by hand with the issue number;
- a self-dependency or a dependency cycle blocks the issue until its body is
  edited;
- two queue ticks in quick succession can miss a just-dispatched run in the
  runs list, and the runs then share lane 0 — the per-model per-run slots
  keep the account limit safe regardless.

### Running several tickets at once (issue #60)

The queue starts up to **`queue.max_parallel_runs`** runs at once (in
[`models.json`](.github/codeaf/models.json), default 2). Everything
`codeaf.yml` runs counts toward that limit while it is queued or in
progress: issue runs, continuations of runs that stopped early, and review
runs alike.

**Lanes.** Each run the queue starts gets the lowest free lane, 0 through
`max_parallel_runs - 1`. The lane picks the run's worker seat model from the
non-experiment models in `models.json`, in file order, as
`models[lane mod count]`, so two concurrent runs rarely compete for one
model's call slots. Today that is GLM-5.3-Flash on lanes 0 and 3,
DeepSeek-V4.1-Flash 10/17 on lane 1, and Kimi-K3.1-Pro (role `top`) on
lane 2. The lane travels to the run as a
digits-only `workflow_dispatch` input, validated by the gate and validated
again by `seat-config` inside the run job — a missing or malformed lane
means lane 0. The queue reads lanes back from run titles, which read
`CodeAF #N (lane L)`; titles are untrusted data, and a title that does not
parse counts as lane 0.

**The per-model cap is account-wide.** `max_concurrent` is meant for the
whole account, and every run has its own proxy, so without slicing four runs
could each put the full cap on one model. Instead each run gets a
slice of every model's `max_concurrent`: per-run slots =
`max_concurrent / max_parallel_runs`, at least 1, with CodeAF's
`task.parallel` set to match. The proxy enforces the slice, and `loadConfig`
refuses a `max_parallel_runs` that would overflow a model, so runs sharing
one model never sum past `max_concurrent` (8 per model, at 4 runs: 2 each). Manual runs (a label or a
`/codeaf` comment) carry no lane and still overlap without coordination; the
slice caps them at 2 calls per model per run.

**Area conflicts.** The queue does not start a run on an area another run is
already working. Areas are `app/<name>/` for each app and the harness
(`.github/`), taken from `area:harness` and `area:app/<name>` labels or from
paths in the issue body. A run on a pull request — a review, a continuation
or a fix run, titled `CodeAF #<pr>` — takes its areas from three sources
united: the issue the PR closes (`Closes #N`), the PR's changed files
(fetched from the API, up to 100 paths) and the PR's own `area:` labels, if
any. An issue with no area label and no path in its body is not started and
blocks no one: it is held with reason `no area label`, and the queue comments
on it once to ask for an area label. A PR run that names no area at all, and
any in-flight run whose area is unknown, still conflicts with everything.

**The budget.** Unchanged: the queue holds new starts once the shared
rolling-week spend reaches 75% of `limits.weekly_credit_usd`, and all
concurrent runs draw that budget down together.

**Pricing.** DeepSeek-V4.1-Flash (the 10/17 checkpoint the lanes run)
has the cheapest cached-input price
(0.03 USD/M tokens at API prices), so a non-zero lane can spend less
per ticket — but it is a weaker model (`role: mid`), so the rotation trades
a little quality on busy days for no model contention.

**Where to see it.** The queue job's log lists the runs in progress, the
lanes in use, and why each candidate started or was held.

### Model experiments

To tell what is down to the model from what is down to the harness, the
same task can be run with different seat models. An experiment issue carries
the `codeaf-experiment` label and runs only when dispatched with a `profile`
input naming one of the profiles in `.github/codeaf/experiments.json`:

```sh
gh workflow run codeaf.yml -f issue=N -f lane=0 -f profile=kimi
```

A profile pins the worker, planner and checker seats to models from
`models.json`; the gate and `seat-config` both validate the name. Labels and
`/codeaf` comments never start an experiment issue, and a profile on any
other issue is refused. A profile run gets no independent review and no
continuation, so each result is that profile's unaided work. Models with
role `experiment` (GLM-5.3, Kimi-K3, GPT-OSS-120B, GLM-4.7-Flash) are
allowed by the proxy but never picked as a lane's worker.

### Runs that stop early (issue #31)

A run can stop before it is finished: CodeAF reports `stop: deadline` (it used
its 100 minutes), `incomplete`, or a budget stop — the per-run credit cap, or the
weekly credit with `overage_usd` 0, in [`models.json`](.github/codeaf/models.json).
The patch is still published, as a
**draft** pull request whose body says the run stopped early, why, and that the
work is unverified: after the push, `publish` asks the harness
([`main.go`](.github/codeaf/main.go), `harness pr-mode`) how the run ended.

For a stop a continuation can help (`deadline`, `incomplete` and the budget
family; other stops, an `error` say, leave the draft to a person), the workflow
then posts `/codeaf continue: <reason>` on that PR and dispatches
[`codeaf.yml`](.github/workflows/codeaf.yml) with its new `pr` input, which
starts a run on the PR's branch. The continuation's brief carries the previous
run's report and what the issue the PR closes still requires. The chain is
capped at **2 continuations per issue**, counted from the PR's own CodeAF
report comments — the first report is the run that opened the PR, not a
continuation. At the cap the PR gets a comment and stays a draft for a person
(a maintainer can still continue it by hand with a `/codeaf` comment). A run
that ends with `stop: done` marks the PR ready for review.

A run can also finish through its task store: CodeAF then leaves the envelope
saying `stop: incomplete` with an empty answer, while the run record's final
`end` line carries reason "finished in the store" and the real answer. The
harness ships that record beside the result in every mode and reads the ending
from it (`closing.go`), so a store-finished run counts as done — the PR is
marked ready and no continuation starts — and the report gains a `closing` row
saying the stop was promoted. The record is untrusted data: the promotion only
changes the draft/ready decision, and the independent review and `publish`'s
checks still run on the work.

### Bringing the base branch in (issue #57)

A pull request that conflicts with the default branch used to stay dirty
forever. The run job hands `publish` a patch against the PR branch's head,
`publish` applies it and pushes a plain commit, and nothing ever made a
merge commit -- so the branch never contained `main`, and a person had to
resolve the conflict by hand. In PR mode the run job now merges the PR base
branch's tip into the PR head **before** CodeAF works. A clean merge is
committed as an ordinary merge commit. A conflicting merge is committed
too, with the conflict markers left in the files: the run's base commit
becomes that merge commit, and the brief names the conflicted files and
tells CodeAF to remove every conflict marker (the lines starting with
`<<<<<<<`, `=======` and `>>>>>>>`). The merge commit exists only in the
run job's checkout -- the run has no write token and never pushes.

A new pull request can be born conflicted: when `publish` must start the
PR's branch from a base that moved and the patch conflicts with the new base,
the branch starts from the old base and the PR opens already in conflict with
`main`. The queue then continues it once: `publish` labels the PR
**`codeaf-continue-needed`**, and the continuation run takes the base-merge
path above, which merges the base in and has CodeAF resolve the conflict
markers -- so the PR is not left dirty until a person resolves it by hand.

`publish`, which treats everything from `run` as untrusted data, recreates
the merge commit itself from the same two parents (the PR head and the
base tip) and refuses unless the tree hash it computes matches the tree
recorded in the run's `merge-info.json` artifact (its fields: a kind of
`clean` or `conflicted`, commit, base, onto, tree, files). It then applies
the patch -- the patch is what removes the markers -- and refuses the
whole publish if any conflict marker is left in the previously conflicted
files. When there is no `merge-info.json` (the run died before merging,
say), `publish` keeps a fallback: it merges the base tip first, then
applies the patch. The invariants do not move: `git apply` is still the
only thing applied from `run`, the `.github/` approval check still applies
to the staged patch diff only, and every new commit still descends from
the PR head -- no force-push.

One limit: if the base branch changed anything under `.github/workflows/`
since the PR branched, bringing the base in takes a maintainer's approval.
The run job still computes and commits the merge — clean or conflicted,
exactly as for any other merge — and records that it carries workflow
changes as `workflows: true` in `merge-info.json`. When that field is set,
the workflow routes `publish` into the `codeaf-harness` environment, so a
maintainer must approve. With approval, `publish` recreates the merge (still
from the same two parents, still verified by tree hash) and pushes it with
the GitHub App token, which can carry workflow changes where `GITHUB_TOKEN`
cannot; it refuses unless the merge's `.github/workflows/` files equal the
recorded base tip's byte-for-byte, so the merge brings in only what the base
already has. Without approval, `publish` refuses with an error naming the
maintainer approval rather than silently pushing the patch alone. When there
is no `merge-info.json` (the behind and diverged fallbacks), the patch is
still pushed alone without approval, and the base is merged in as above once
approved.

The `workflows` field is untrusted input from the `run` job, so `publish`
never trusts it: it recomputes whether the recreated merge carries workflow
changes itself, requires the approval whenever the recomputed list is
non-empty or the field is set, and refuses -- even with an approval -- a run
that recorded `workflows: false` for a merge that does carry them. The field,
like `touches_harness`, only routes the job. The byte check compares the
merge with the base tip the run actually merged in, not the base branch's
current tip.

A consequence: a PR branch with its own edits under `.github/workflows/`
cannot catch up with its base through this path, because its workflow files
will never equal the base's -- a merge would keep the branch's edits
where the approval check and the workflow policy cannot see them. Such a PR
needs a person to merge the base by hand.

### Keeping CodeAF current (issue #35)

The CodeAF version is pinned by a full commit SHA in
[`codeaf-ref`](.github/codeaf/codeaf-ref), a config file under `.github/codeaf/`
rather than a workflow variable: [`codeaf.yml`](.github/workflows/codeaf.yml)
reads it from the default-branch checkout and refuses to run if the file is
missing or not exactly 40 hex characters. A bump is now a one-line change to
that file, which publishes like app code and needs no maintainer approval —
unlike the old hardcoded `CODEAF_REF` env in the workflow, which made every
bump a workflow edit that needs one.

Every Monday morning [`codeaf-ref-check.yml`](.github/workflows/codeaf-ref-check.yml)
(scheduled, and on demand via *Run workflow*) compares the pin with upstream
`Agent-Field/codeaf` `main` (`harness ref-check`). When upstream has moved, it
opens — or updates, if one exists — **one** issue, labelled
**`codeaf-ref-update`** and **`codeaf-queued`** for the queue, listing the
upstream commits since the pin and asking CodeAF to say which of the
workarounds documented in this README they make unnecessary, and to bump the
pin and remove those workarounds only if a test run on a small task succeeds.
When the pin has caught up, the check closes the issue again.

A bump can also be done by hand: edit the pin file to the new full commit SHA.
Removing workflow-level workarounds is a workflow change and still needs a
maintainer's approval (the `codeaf-harness` environment, see
*Limits worth knowing* below).

## The planner

When the backlog runs low the factory now files its own next work. Every queue
tick, after the start-issues loop,
[`codeaf-queue.yml`](.github/workflows/codeaf-queue.yml) asks the harness
(`harness plan-check`, issue #61) whether a **plan run** should start. It
starts one only when all of these hold:

- fewer than **`planner.min_backlog`** (in
  [`models.json`](.github/codeaf/models.json), default 3) open issues carry
  **`codeaf-queued`** and are not already running;
- no plan run has started within **`planner.min_hours`** (default 12) — the
  cooldown is read off each run's `createdAt` stamp, and a plan-titled run
  whose stamp is missing or unparseable counts as inside the cooldown, never
  outside it;
- the same guards as any run: the rolling week's spend under the queue's
  threshold, and a free lane under `queue.max_parallel_runs`.

A plan run is **read-only**, like a review run: it writes no patch, opens no
PR, and nothing it says is applied. It reads the repository (the README and
AGENTS.md included), the open issues, the recently closed issues and PRs,
recent CodeAF run reports, and the planner instructions in
[`plan-instructions.md`](.github/codeaf/plan-instructions.md). It writes up to
**`planner.max_proposals`** (default 5) proposed issues in a fixed format:
title, problem, what to do, done when, area, and dependencies on other issues.
After the run, `publish` runs `harness plan-report` over the answer, dedupes
each proposal against the open issues' titles, and opens the survivors as
issues labelled **`codeaf-proposed`** plus their area labels — **never**
`codeaf-queued`. Proposal text comes from the run job, so it is treated as
untrusted data: parsed as JSON with every field validated, capped and deduped,
never executed.

**How to promote a proposal.** A proposal is only a suggestion. A person with
triage access — or Claude acting for the owner — reviews the issue, and if it
is worth doing adds the **`codeaf-queued`** label (and may drop or replace the
area labels). The queue then runs it like any other queued issue: same trust
gate, same area and dependency rules, same budget. Nothing in the pipeline
adds `codeaf-queued` to a proposal by itself.

The planner aims its proposals, in order, at: bugs and gaps found in recent
runs and reviews; the "Out of scope" or follow-up notes in closed issues; the
feature-requests list's workarounds the harness could improve; and app work
following each app's README TODO list. Proposals that would change
`.github/workflows/` always say in their body that such a change needs a
maintainer's approval (the `codeaf-harness` environment, see *Limits worth
knowing*).

## Routines

Recurring factory activities — checking synthetic.new's prices and models,
deciding whether to take a CodeAF upgrade, recalibrating the weekly credit —
are **routines**: one mechanism, configured in one trusted file,
[`routines.json`](.github/codeaf/routines.json) (issue #117). The file sits
next to `models.json`, is validated on load, and unknown keys or bad values
fail with a message naming the field. Each routine carries:

- **`name`** — lowercase, digits and dashes, at most 32 characters. It names
  the run title (`CodeAF routine <name> (lane N)`), the dispatch input and
  the proposal label (`routine:<name>`).
- **`every_hours`** — the cooldown: a routine is due when no run of that
  name has started within this many hours. The cooldown is read off each
  run's `createdAt` stamp; a run whose stamp is missing or unparseable
  counts as inside the cooldown, never outside it (fail closed).
- **`kind`** — either:
  - **`check`**: a deterministic harness command (`command`), run by the
    routines workflow. No model runs, no secret beyond `GITHUB_TOKEN`, and
    it reads the network only for public data. It renders its report
    itself; a check routine may update one tracking issue per routine with
    its latest result, built from validated data only — never model text.
  - **`agent`**: a read-only CodeAF run whose brief is a template under
    [`routines/`](.github/codeaf/routines/) (`brief`), capped per run by
    **`max_usd`** like `run_credit_usd`. Like a plan run it writes no
    patch: its proposals go through the planner's path and open as issues
    labelled `codeaf-proposed` (plus `routine:<name>`), deduped against
    the open issues — **never** `codeaf-queued`. A person, or Claude acting
    for the owner, promotes them.

A routine is due only when the same guards as any run hold: the rolling
week's spend under the queue's threshold, and a free lane under
`queue.max_parallel_runs`. `harness routines-due issues.json runs.json`
prints one `routine=<name> lane=<n>` line per due routine. **Not wired yet:**
no workflow calls it. A dedicated read-only routines workflow, on an hourly
schedule, and `codeaf.yml`'s routine mode are a maintainer's hand edit,
because the workflow policy freezes the CodeAF workflows. A routine dispatch is gated like every
other dispatch: `routine=<name>` must name a routine in `routines.json`,
and refuses any issue, pr, review, plan or profile input beside it.

**How to add a routine.** Add one object to the `routines` array in
`routines.json`:

- for a **check** routine, add `"kind": "check"` and `"command": "<name>"`,
  then implement a Go function in `.github/codeaf/` that renders the report
  and register it with `registerCheck("<name>", fn)` in an `init` — the
  same pattern `check_modelprices.go` uses. `TestRoutineCatalog` fails if a
  committed command is not registered.
- for an **agent** routine, add `"kind": "agent"`, `"brief":
  "routines/<file>.md"` (a template under `.github/codeaf/routines/`,
  read-only instructions ending in the output contract) and `"max_usd"` —
  the per-run credit cap, enforced by the proxy. `TestRoutineCatalog`
  fails if the template file is missing.

Defined in `routines.json` (they run once the workflow is wired): `model-prices` (daily check of the synthetic.new catalog
against `models.json`), `codeaf-upgrade` (daily agent run that classifies
upstream CodeAF changes as take/hold/skip and proposes the bump),
`quota-calibration` (6h check that re-estimates `weekly_credit_usd` from
the ledger and the quota drops), `pr-sweep` (6h check on open `codeaf/*`
PRs that are red, conflicted or carry an unaddressed review finding),
`flaky-tests` (daily `go test -race -count=10` over every module) and
`factory-report` (weekly report on the tracking issue).

## Limits worth knowing

- The agent has a shell on the runner, so treat it as having the same access as any
  CI job. It gets no GitHub token: the checkout does not keep credentials, and the
  workflow pushes after CodeAF exits.
- CodeAF may change `.github/` (the harness, CI and workflows) when a task asks
  for it.
  - A change only to `.github/codeaf/` (the harness program, `models.json`,
    `codeaf-config.json`) is published like app code. Every job that holds a
    secret or a write token builds the harness from `main`, so the change
    takes effect only once merged.
  - Any other change under `.github/` (workflows, `CODEOWNERS`) controls what
    CI runs with secrets. For those, `publish` runs in the
    **`codeaf-harness`** environment and waits for a maintainer
  to approve it in the run's page (GitHub emails the reviewers) before anything
  is pushed. It also checks the approval through the API, so a misconfigured
  environment refuses the patch rather than publishing it.
  - **Workflow changes without a maintainer.** A patch whose only such
    changes are top-level workflow files is published anyway if it passes
    the deterministic workflow policy (`harness workflow-policy`). The
    policy lets the patched workflows do nothing the default branch's
    can't:
    - no new triggers, permissions or environments;
    - no new or edited steps that reference secrets;
    - no change at all to a privileged job (one holding a secret, the
      token, an environment or a write permission), since its unprivileged
      steps feed its privileged ones; in practice only read-only jobs
      such as CI's can change;
    - no new or unpinned actions;
    - no new `github.event` interpolation;
    - no new or deleted workflow files.

    The PR is labelled `codeaf-workflow-change`. The independent review run,
    on another model with a workflow-security checklist, must pass it before
    it is merged, and it is never merged automatically. Anything the policy
    refuses, such as an edit to a step that holds a secret, still waits for a
    maintainer.

    `harness workflow-policy BEFORE AFTER BASE` (a third directory, not yet
    passed by the publish job) compares the patched workflows against the
    **patch's own base commit**, not current `main`: when a branch has fallen behind and
    `main`'s workflows moved (a trigger change, a new step), the policy
    reports `<workflow>: changed on the default branch since the patch's
    base; rebase the branch before it can publish` instead of blaming the patch
    for a change it did not make. Rebase the branch and re-run.

    New or changed `run:` scripts must not **install from moving refs**. The
    policy refuses `go install`/`go run` of `@main`, `@master`, `@latest` or
    a `@release-*` ref, `curl | sh` (or `wget` piped into `sh`, `bash`,
    `zsh`), a `pip install` whose specifier names no version, a global
    `npm`/`yarn` install of a package that is unpinned or pinned to a
    moving ref (`npm install -g pkg`, `npm install -g pkg@latest`,
    `npm i -g @scope/pkg`, `yarn global add pkg`), and `git clone -b
    main`/`-b master`. A version pin of any kind passes — `==`, `>=`,
    `~=`, or a tag via `@` — and so does a commented-out line. These are
    heuristics over the script text, not a parser: they catch the common
    spellings and deliberate spellings may still slip through, so pin the
    version or SHA anyway.

    **Splitting a refused patch** (`harness patch-split`, not yet called by
    the publish job, which still refuses the whole patch) keeps the rest: every file section outside
    `.github/` (except `.github/codeaf/`, which a maintainer never has to
    approve) is applied and published as before, and the refused workflow
    diff is written out for a PR comment (`harness refused-comment`)
    instead of the whole patch being dropped. A refused section that renames or copies a file,
    a patch over 50 MB, or a header that does not parse refuses the whole
    patch (exit 2, nothing written) rather than publishing a partial one.
    To move forward, read the violated rule — the run report quotes it
    under "Workflow policy" — then either
    approve the `codeaf-harness` environment deployment for that run, or
    fix the patch (pin the install, drop the refused workflow file, rebase
    onto the current default branch) and re-run.

  One-off setup:
  - *Settings → Environments → New environment* `codeaf-harness`:
    - **Required reviewers**: yourself. Leave *Prevent self-review* off, since
      the run is started by you.
    - **Deployment branches**: `main` only.
  - Create a GitHub App to push approved harness patches, so they don't
    go out as you. `GITHUB_TOKEN` can't push changes to
    `.github/workflows/`, so without the app only changes elsewhere under
    `.github/` can be published.
    1. *Settings → Developer settings → GitHub Apps → New GitHub App*, for
       example "factory-repo CodeAF harness".
       - Untick *Webhook → Active*.
       - Repository permissions: *Contents* read and write, *Workflows* read
         and write. Nothing else.
       - *Only on this account*.
    2. Install it on `RichardoC/factory-repo` only.
    3. Generate a private key, then add two secrets to the `codeaf-harness`
       environment:
       - `CODEAF_APP_ID`: the app's ID;
       - `CODEAF_APP_PRIVATE_KEY`: the whole `.pem` file.
    4. Delete the downloaded key file.

    After approval, `publish` uses the key to mint a token that lasts an hour,
    for this repository only, with just those two permissions. It pushes with
    that token, so the push shows as `<app name>[bot]`.
- CodeAF runs in a job with no write access (`run`). A separate job on a fresh
  runner (`publish`) checks and applies the resulting patch, then commits,
  pushes, opens the PR and posts the report. Nothing CodeAF could have tampered
  with is executed with a write token.
- **Shared state.** Every run starts from one shared bundle:
  - CodeAF's memory db, router events and model ratings;
  - the rolling 7-day spend — at API list prices, from the runs
    merged so far. The weekly credit regenerates continuously (2% of the total every
    ~202 minutes), and this rolling sum is what stands in for the refill; `state
    load` outputs both the day's and the week's figure.

  [`codeaf-state.yml`](.github/workflows/codeaf-state.yml) is the only writer:
  - It merges each run's `codeaf-state-*` artifact into a new generation of
    the bundle, saved in the Actions cache as `codeaf-shared-v1-g<N>-…`. The
    last five generations are kept.
  - The CodeAF workflow's `handoff` job starts it as soon as CodeAF has
    finished. It runs one merge at a time, and each merge picks up every run
    not merged yet.
  - When runs overlap, spend is added and ratings and events are merged. The
    memory db comes only from a run that started from the latest generation.
  - Every packed run also carries one **run record** (`run-record.json`,
    issue #140): the run's mode, number, lane, stop reason, wall time, call
    and 429 counts, cost and tokens, and whether the patch touches the
    harness. Every field is bounded, and a record that cannot be believed is
    dropped — never the run it belongs to. The merge appends valid records as
    one JSON line each to `records.jsonl` in the bundle and keeps the newest
    2000; every line is re-validated on every load and merge, and nothing in
    the file is ever executed.
  - `harness metrics BUNDLE [DAYS]` summarises those records as Markdown:
    runs by mode and stop, store-finished runs, mean and p50 wall time and
    mean cost by mode, cost by day, upstream 429s, total slot wait, and PRs
    that needed more than one `pr`-mode run.

  Everything is checked before it is merged, and again before CodeAF sees it
  (see [`AGENTS.md`](AGENTS.md#caches-and-carried-state)). Saved memories reach
  CodeAF's prompts. So to reset the state, delete the `codeaf-shared-v1-*`
  caches (*Actions → Caches*); to roll back, delete only the newest ones.
- CodeAF itself is rebuilt from source on every run; nothing executable is
  carried between runs.
- **Diagnostics are public.** Artifacts on a public repo can be downloaded by
  any signed-in GitHub user, and the diagnostics are not encrypted. They hold
  the brief, prompts, model replies and tool output, so never put anything
  secret in an issue or in the `run` job. Token-shaped strings are redacted
  before upload, as a backstop. The `CODEAF_ARTIFACT_PUBLIC_KEY` and
  `CODEAF_ARTIFACT_PRIVATE_KEY` secrets are no longer used and can be deleted.
- The rules for future changes are in
  [`AGENTS.md`](AGENTS.md#security-rules-for-changing-the-harness).
- Protecting `main`: [`CODEOWNERS`](.github/CODEOWNERS) makes @RichardoC the owner of
  everything. Pair it with a ruleset on `main` that requires a PR, a Code Owner review,
  approval of the most recent push, and the `CI / go` check. A `github-actions[bot]`
  approval then never counts, even with the setting below enabled. The workflow itself
  never approves or merges anything.
- Repository setting needed: *Settings → Actions → General → Allow GitHub Actions to
  create and approve pull requests* (for `gh pr create` with `GITHUB_TOKEN`).
- Configuration lives in files, not workflow variables:
  - [`models.json`](.github/codeaf/models.json): the spend caps (`limits`: the plan,
    the weekly credit, the per-run credit and the overage cap) and the allowed models
    with their prices (USD per million tokens: `api_input`/`api_cached`/`api_output`
    are the API list rates every figure and cap uses; the `input`/`output`/`cache_read`
    subscription rates are reference only), how many calls
    each may have in flight (`max_concurrent`), and the reasoning efforts each
    model accepts (`efforts`; a request for an unlisted effort goes to the next
    one the model lists, `enabled: false` to its lowest, so nothing is sent raw
    that the model does not list — see *GLM reasoning effort* below);
  - [`panel.json`](.github/codeaf/panel.json): the escalation order (not used for
    now, see above);
  - [`codeaf-config.json`](.github/codeaf/codeaf-config.json): CodeAF's own settings
    (seat models, how many tasks run at once, budgets, telemetry and Model Pool
    off), installed as its profile config.

  Keep the three consistent when changing models. Put seats that run at the
  same time on different models, and keep `task.parallel` no higher than the
  worker model's `max_concurrent`. The only CodeAF setting left in
  the environment is the endpoint (`CODEAF_BASE_URL`), because CodeAF reads it from
  nowhere else. The panel path (`CODEAF_MODELS`) would be the second one.

### GLM reasoning effort (issue #24)

GLM returns empty answers at `low`, so a request for an unlisted effort goes to the next
one the model lists; `low` is never sent to GLM. Measured 2026-10-17 on a real multi-step Go edit-and-test task through the
live proxy, 3 repeats per case (results.md, issue #24): `medium` matched `high` on correctness
for both models, saving ~19.3% median completion tokens and ~17.8% median wall s on
glm-5.3-flash, ~35.9% and ~32.1% on glm-5.3. Both now send `low`/`minimal`/`medium` as
`medium`: glm-5.3 cleared the 20% threshold, and the owner chose `medium` for glm-5.3-flash
too, since about 72% of its output is reasoning (the report's reasoning-share column).

| model | effort | median completion tokens | median wall s | success |
|---|---|---|---|---|
| glm-5.3-flash | medium | 993 | 19.8 | 3/3 |
| glm-5.3-flash | high | 1231 | 24.1 | 3/3 |
| glm-5.3 | medium | 854 | 17.8 | 3/3 |
| glm-5.3 | high | 1332 | 26.3 | 3/3 |

### GLM reasoning share (issue #25)

synthetic.new never sends `completion_tokens_details.reasoning_tokens` for GLM, so its
reasoning-token column reads `—` and the exact reasoning share is unavailable. GLM does
stream its reasoning as `reasoning_content` (and returns it in the non-streamed `message`),
so the proxy counts those characters — plus the visible `content` and tool-call argument
characters — out of the chunk/response decode it already performs, records them in the
ledger as `reasoning_chars`/`output_chars`, and the report renders such models' reasoning
share as an estimate labeled “(chars)” (`reasoning ÷ (reasoning + visible output)`).
Models that do report reasoning tokens keep the exact token-based share. Character counts
are bytes, not runes; the proportion — not the absolute figure — is what the estimate uses.
