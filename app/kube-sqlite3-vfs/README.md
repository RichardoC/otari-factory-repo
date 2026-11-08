# kube-sqlite3-vfs
sqlite3 using kubernetes as the backing storage

## Requirements and building

The module requires **Go 1.27.2 or later** (`go 1.27.2` in `go.mod`; CI uses
the version from `go.mod`, so no explicit toolchain line is needed. Toolchains
older than 1.27.2 are refused: their standard library carries
vulnerabilities govulncheck reports as called, all fixed in 1.27.2).
Building `main` and the
`cmd/` tools needs a **cgo toolchain** and a **C compiler**: `mattn/go-sqlite3`
is imported for the `sqlite3_*` symbols the `sqlite3vfs` C shim links against.

```sh
cd app/kube-sqlite3-vfs
go build ./...                 # library and all four commands
go build -o kube-sqlite3-vfs . # just the demo binary
```

## Running the tests and benchmarks

```sh
cd app/kube-sqlite3-vfs
go test -race ./...                                              # unit tests
go test ./pkg/vfs -run ZZZ -bench . -benchmem                    # benchmarks
go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x               # benchmarks, fixed iteration count
```

`-run ZZZ` matches no test, so only the benchmarks run. The tests use
client-go's fake clientset and need no cluster. `api-calls/op` is stable
between runs; `ns/op` is not, so compare medians of several `-count=5` runs
rather than single numbers.

### Integration tests (issue #69)

The tests in `pkg/vfs/vfs_integration_test.go` live behind the
`//go:build integration` build tag (issue #69). They run against a **real
kube-apiserver** via controller-runtime's envtest, so the resourceVersion
compare-and-swap (409 Conflict), `AlreadyExists` on Create and label-selector
LISTs are exercised by the API server itself rather than by hand-written fake
reactors. Because of the build tag they are excluded from the default
`go test ./...` — the unit tests need no cluster and no downloaded binaries.

To run them, install the envtest binaries first. `setup-envtest` downloads the
kube-apiserver and etcd binaries from the envtest-tools release storage and
prints the directory it put them in:

```sh
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.3-0.20261012174806-32759fab3119
KUBEBUILDER_ASSETS="$(setup-envtest use 1.37.0 -p path)"
```

The `1.37.0` binary set matches the module's `k8s.io/*` v0.37.x dependencies.
Then run the tagged tests; with `KUBEBUILDER_ASSETS` set (it also works to set
it inline for one command):

```sh
go test -race -count=1 -tags integration ./pkg/vfs
```

## How it works

Creates a set of configmaps per file requested, which is named a base32 encoding of the filename
the namespace contains
a configmap called `<base32 filename>-lockfile` which holds the locking state
(issue #47): the `lock` data key still carries the highest lock level any live
holder has — the aggregate old readers saw; a `shared-holders` data key maps every
SHARED holder's id (a UUID generated per `Open`) to its lease expiry; a `writer`
data key carries the one writer's id, level and lease expiry, or is empty when no
writer holds
a series of configmaps named which contain up to 64kB of data each

namespaces all labelled with "kube-sqlite3-vfs": "used" to ease cleanup

Configmap names can be 253 characters long after encoding, but until
issue #351 the full encoded name was also copied into the `relevant-file`
**label** on every lockfile and sector ConfigMap, and Kubernetes caps a
label value at **63 characters**. The base32 encoding needs 8 characters
per 5 name bytes, so that capped usable database names at 35 bytes: a
36-byte name encodes to 64 characters, and any real API server rejected
its lockfile and sectors outright — such a database could not be opened
for writing (the fake clientset used by the tests validates nothing,
which is why no test caught it).

The label is now the full encoded name only while it fits — every raw
name up to 35 bytes keeps exactly the label it always had — and longer
names are labelled `long-` + the unpadded lower-case base32 of SHA-256
of the name (57 characters, a pure function of the name, so the
label-selector LIST that finds and deletes a file's sectors still
matches). The lockfile's `relevant-file` **data key** always carries the
full encoded name. With the label no longer the binding limit, the
253-character ConfigMap **name** is: after the `-lockfile` (9 characters)
and `-<sector index>` suffixes, raw names up to about 145 bytes stay
within it.

## Command-line flags

All four commands (`main`, `db-upload`, `db-download`, `file-create`) share:

* `--kubeconfig PATH` — absolute path to the kubeconfig file (defaults to
  `$HOME/.kube/config`);
* `-v`, `--verbosity` — log at **debug** level as text instead of the default
  info-level JSON;
* `--retries N` — number of attempts `Open` makes against races between
  concurrent clients (default `1`).

`db-download` additionally takes `--filename PATH`, the local SQLite file to
store in the cluster, defaulting to `file2.db`. (`db-upload` and
`db-download` are demo tools with fixed remote name `file2.db`.)

`db-download` also takes `--journal-mode MODE` (issue #318): the mode of the
read-only connection it opens to read the row count back, validated by the
same shared rules as the writer's flag, so an operator running the writer
with, say, `TRUNCATE` can give the reader the same mode and keep the two
aligned.

`main` (the writer) additionally takes `--journal-mode MODE` (issue #120),
the SQLite rollback journal mode it opens its database with — `DELETE` (the
default), `TRUNCATE` or `PERSIST`, matched case-insensitively. The default is
the measured trade-off documented with the benchmarks below. `MEMORY` and
`OFF` are refused before any cluster work, with the crash-safety reason from
issue #68: their rollback journal never reaches the VFS, so a crash
mid-transaction leaves a half-written database that cannot be rolled back.
`WAL` is refused too: it needs SQLite's shared-memory `-shm` file, which this
ConfigMap-backed VFS cannot store. Any other value is refused as unknown,
naming the accepted modes.

Every command prints an error to stderr and exits non-zero; none of them
panics on an ordinary error.

## WARNINGS

**Crash safety now comes from an on-cluster rollback journal (issue #68).** An in-memory journal (`_journal=MEMORY`) leaves no on-cluster record of an uncommitted transaction, so a crash mid-transaction could not be rolled back and very likely corrupted the database; that was the old warning, and it is gone. The databases this project opens — `main.go` (read-write) and `cmd/db-download` (read-only, `mode=ro`) — take `_journal` from `--journal-mode`, defaulting to `DELETE` so a clean default deployment leaves no journal state behind, and the journal is stored through the same VFS as the database: when SQLite opens `<db>-journal` it reaches this VFS's `xOpen`, answers `xAccess` from cluster truth (the name exists exactly when it has at least one sector ConfigMap), and removes the whole journal with `xDelete`. (`cmd/db-upload` and `cmd/file-create` open no database at all — they drive `vfs.NewFile` and sector I/O directly — so the journal mode is not theirs to set.) So a crash mid-transaction is now recoverable:

* **Committed data survives a crash.** Rows committed before the crash are intact; `PRAGMA integrity_check` reports `ok` after the next opener recovers.
* **A crash mid-transaction leaves a hot journal the next opener rolls back.** The uncommitted transaction's journal sectors and modified database sectors are both on the cluster, the journal's magic byte at offset 0 is non-zero, so `hasHotJournal` treats it as hot and SQLite plays it back, restoring the pre-transaction image and unlinking the journal. This is proven by `TestCrashAfterCompleteJournalRollsBackOnReopen` (crash after the journal is complete) and `TestCrashBeforeCompleteJournalKeepsDataIntact` (crash before any journal sector exists — nothing to roll back, and nothing is lost) in `pkg/vfs/vfs_journal_test.go`, both through a fresh VFS instance over the same fake clientset after the dead holder's lease lapses. `TestCrashMidTransactionWithoutStoredJournalLosesData` is the control that fails without the change: with `_journal=MEMORY`, or with a VFS whose `Access` pretends a `-journal` name never exists, the same crash loses or corrupts data.

### Crash part-way through deleting the rollback journal (issue #121)

Deleting a rollback journal is not one Kubernetes call: `xDelete` on `<db>-journal` is one LIST of the journal's sector ConfigMaps plus one Delete per sector and one for its own lockfile. A crash at any point in that sequence therefore leaves a **half-deleted journal** on the cluster, and the two observers that matter read different things:

* `xAccess` answers from **cluster truth**: the `-journal` name "exists" exactly when at least one of its sector ConfigMaps does. A leftover lane of sectors keeps the name answering *exists* even though SQLite has already committed and finished with it.
* SQLite's `hasHotJournal` asks whether the file exists, reads the **byte at offset 0** of it — that byte is the first byte of sector 0, and a missing sector 0 reads as empty bytes, i.e. as zero — and treats a first byte of zero as "**not hot**". It also treats a journal it can access but cannot read a non-zero first byte from as cold.

So a half-state is safe exactly when the header is gone, or when the header is still there *and* the rest of the journal is still intact enough to be replayed. The order below makes every prefix of the delete safe:

| # | State left by a crash (all with the database's own sectors untouched) | `xAccess` answers | `hasHotJournal` decides | Next opener |
| - | --------------------------------------------------------------------- | ----------------- | ----------------------- | ----------- |
| 1 | Some sectors gone — header sector 0 **still present**, later sectors deleted, lockfile still present | *exists* (sectors remain) | reads a non-zero first byte, so the journal is **hot** | **Replays** the journal. The journal is intact as far as the header's declared page count reaches, so this is the ordinary #68 recovery path; the leftover lockfile is irrelevant to it. This state cannot be produced by the current order (sector 0 goes first), but it is the state a crash during the *writing* of a journal leaves, and it is what recovery exists for. |
| 2 | Header sector 0 gone, later sectors left, lockfile still present | *exists* — the later sectors keep the name alive | offset 0 reads as zero → **cold** | **Ignores** it. SQLite sees a journal that is not hot, so it neither replays nor refuses: it opens the database and the leftover sectors are inert. This is the state the header-first order makes reachable by design, and it is the one a crash in the middle of this delete lands in. |
| 3 | Lockfile gone **first**, sector 0 still present, possibly some later sectors gone | *exists* | offset 0 reads non-zero → **hot** | **Replays** a journal that is missing its middle or end, which is the dangerous state of the three: `hasHotJournal` says hot on the strength of sector 0 alone, and the playback then needs sectors the delete already removed. This is precisely what deleting the lockfile first (or deleting sectors out of order) would expose, and it is why the lockfile is deleted **last**. |
| 4 | Lockfile gone last, after every sector | **does not exist** — no sector ConfigMaps left | offset 0 reads as zero (nothing to read) → **cold** | **Ignores** it: the name does not exist, so `hasHotJournal` is not even reached, and the database opens normally. This is the state the order below produces on success. |
| 5 | Complete deletion (the normal success state) | **does not exist** | n/a — nothing to read → **cold** | **Ignores** it, the same as #4, and the next write starts a fresh transaction. |

**Half-state 3 is the one that must never happen.** A journal whose sector 0 is still present but whose later sectors are gone would be read past its end during replay: SQLite declares it hot from the header byte alone and then asks for pages the journal no longer holds, which is how a replay turns into corruption rather than recovery. The implementation therefore never deletes the lockfile first, and never deletes a later sector while sector 0 is still there.

The order `vfs.Delete` now uses for a name ending in `-journal` (see the `deleteSectorsInSafeOrder` and `Delete` comments in `pkg/vfs/vfs.go`, which are the source of truth for this list) is:

0. one **LIST** of the file's sector ConfigMaps — the only way to find out which sectors and lockfile exist, and the step the retry loop repeats;
1. **Delete sector 0**, the journal header, **first**;
2. **Delete the remaining sectors**, ascending, with `NotFound` tolerated on each so the leftovers of an earlier crashed attempt are collected in the same pass;
3. **Delete the lockfile LAST**, only once every sector is gone.

Deleting the header first is what makes the sequence safe: writing a header is what made the journal hot in the first place, and the header is the last sector a hot journal ever needs, so no prefix of this delete can leave a journal SQLite still considers hot. Everything after step 1 is only cleanup of a journal that already reads as cold. (`NotFound` on either a sector or the lockfile is not an error anywhere in this sequence — a `Delete` for an object that is already gone leaves the world in the state the call wanted — so the sequence is idempotent and a retry makes monotonic progress.) A name that is *not* a journal keeps the historical order — every sector in LIST order, then the lockfile — because no SQLite hot/cold decision reads sector 0 of a database file, and deleting a database's own header sector first would only expose a header-less database for longer.

The consequence is that **the file always completes its delete on a retry after a crash**: re-running `xDelete` re-LISTs, and whatever survived is deleted in the same order, so a half-deleted journal converges to fully deleted with no state SQLite will replay. Any sectors a crash left behind are **inert garbage** — cold, because the header is gone — and they are collected by

* the next `xDelete` of that name, which is what SQLite issues at the end of the next successful transaction, and
* an `xTruncate` that reaches past them: `Truncate` deletes every sector after the new end of the file, so truncating the journal to 0 removes the leftovers wholesale.

One residual remains, and it is a cost rather than a hazard: until something collects them, leftover sectors keep a stale `-journal` **name that still answers "exists" to `xAccess`**, and each such `xAccess` (and each `Open` that follows it) pays one LIST to find that out. The invariant this order buys is **safety, not zero leftovers** — the database is always intact and never replayed as garbage, but an abandoned half-delete can go on costing one LIST per open until the next write collects it.

The tests that pin this are the issue #121 additions in `pkg/vfs/vfs_delete_crash_test.go` (`TestCrashPartWayThroughJournalDelete`, which injects the failure after each Delete; `TestJournalDeleteTransientFailureIsRecovered`, `TestJournalDeleteHeaderFirstIsLoadBearing`, `TestJournalDeleteLeavesNoReplayableGarbage` and `TestJournalDeleteFixtureIsNotVacuous`) and in `pkg/vfs/vfs_delete_halfstate_test.go` (`TestHalfDeletedJournalReaderSide`, the reader-side half-states beside them): they inject a fake-clientset failure after **each** Delete in the sequence above, then reopen the database through a **fresh VFS** over the same clientset — a new VFS instance and a new connection, the way a later process would — and assert `PRAGMA integrity_check` is `ok` with the pre-transaction row count and no half-applied transaction — never a mix of the two images — and then run one more committed write and assert that it collects the leftover sectors. (The three #68 tests in that file — `TestCrashAfterCompleteJournalRollsBackOnReopen`, `TestCrashBeforeCompleteJournalKeepsDataIntact` and `TestCrashMidTransactionWithoutStoredJournalLosesData` — cover the journal's *creation*, not its deletion.)

What is still **not** crash-safe:

* **No WAL.** `_journal=MEMORY` is no longer the default anywhere in this project, but `-wal` is still reported inaccessible, so WAL mode cannot be selected; the rollback journal above is the only durability mechanism.
* **The limits below still stand** — a paused process can lose its lease, clocks must be roughly synchronised, every process touching the database must run this version, and sector writes are gated only by lease loss, not by lock level.

The default journal mode is **`_journal=DELETE`**, and the writer takes it as a flag (`--journal-mode`, issue #120). All crash-safe modes (`DELETE`, `TRUNCATE`, `PERSIST`) store the journal through this VFS; `MEMORY` and `OFF` are refused (issue #68, above). Measured against each other (issue #120, full table below), `TRUNCATE` and `PERSIST` are actually the **cheaper** modes — the earlier argument here, that they would pay a header rewrite plus extra `xAccess`/`xOpen` on every later transaction where `DELETE` pays one deletion at commit, is not what the API-call counter shows. What a commit costs is not the whole story, though: `DELETE` is the default because of what it leaves behind. After clean commits and close (measured over the fake clientset), `DELETE` leaves **no journal state at all**, `TRUNCATE` leaves the journal's lockfile ConfigMap behind permanently (one per database, never reclaimed), and `PERSIST` leaves the journal's sector ConfigMaps too — cluster storage nothing ever cleans up. The clean post-commit layout is what issue #68's recovery story is built on ("no journal sectors" is exactly what `Access` reports for a recovered file), so the ~4 API calls per transaction that `TRUNCATE` would save are traded for a cluster that holds no journal artifacts between transactions. Deployments that prefer the saving can set `--journal-mode=TRUNCATE`; the trade-off is measured and written down rather than argued.

**Concurrent access between processes is now guarded by real mutual exclusion (issue #47) — under the limits listed below.** The lock ConfigMap no longer stores one bare level written blindly. Every transition is a compare-and-swap: the holder state is read, the new state is decided from what was read, and the Update carries the `resourceVersion` just read, so two processes can never overwrite each other's holdings; on a 409 Conflict the state is re-read and the decision re-made, never forced. A missing lockfile is created with Create, and `AlreadyExists` from that Create is treated as "re-read". Each file — generated once per `Open`, and per `NewFile` — carries a holder id (a UUID) and every lock operation acts as that holder, so a process can tell its own locks from another process's; `Close` releases them. The stored state expresses sqlite's model: any number of SHARED holders, at most one writer at a time (RESERVED, PENDING or EXCLUSIVE), and EXCLUSIVE only once no other holder remains. A transition another process has made impossible — a second RESERVED, EXCLUSIVE while other SHARED holders remain — returns SQLITE_BUSY (`sqlite3vfs.BusyError`) instead of blocking; sqlite retries on its own schedule. The aggregate `lock` key still holds the highest level any live holder has, so a reader of the old single-level format still sees a meaningful value. The #19 cache rules are unchanged: caches are dropped before any downgrade is written, and a file never adopts the ConfigMap's stored state as its own lock.

The guarantees hold only under these limits:

* **Roughly synchronised clocks.** Leases compare client-generated UTC timestamps (default 30 s, configurable with `WithLockLease`): every holder entry carries an expiry, entries past their expiry are pruned before any lock decision, and a successful CAS re-stamps the holder's own entries. The default lease equals one lock-path call timeout — `defaultLockLease == lockCallTimeout == 30s` in `pkg/vfs/vfs.go`. The coupling means a single lock-path Kubernetes call (one Get or Update of the compare-and-swap) can, at worst, span exactly the whole lease: a renewal due at half-lease finishes inside the remaining lease on a healthy API server, while a call wedged for the full timeout outlives it — the CAS then finds this holder's own entries expired, prunes them like any other process would, and reports the loss instead of renewing a lock it no longer holds (the same liveness rule as the paused-process bullet). It also bounds what `WithLockLease` can safely do: a lease shorter than `lockCallTimeout` makes losses routine, because one slow API call then outlives the lease even while the process is healthy.
* **A paused process can lose its lock.** A process paused or suspended for longer than its lease — a laptop asleep, a VM frozen, a SIGSTOP — has its entries pruned by the next holder's CAS and its lock can be taken by another process. On resuming, its next lock-path operation reports the loss and resets its local state instead of writing under a false belief. The loss is reported as the distinct `errLockLeaseLost` error ("lock lease expired and was pruned by another process") — deliberately not as `SQLITE_BUSY`/`sqlite3vfs.BusyError`: BUSY invites sqlite's busy handler to retry the same lock request, but after a lapse this file's state is gone, a retried upgrade fails as an invalid transition, and a retry that did succeed would continue a transaction over a database another process may already have rewritten, without the change-counter revalidation a fresh lock acquire performs. The plain error fails the statement; sqlite releases its locks, and the next transaction re-acquires from scratch as a fresh holder, revalidating every cache. The loss is detected on every lock-path operation, `Unlock` included: a downgrade whose CAS reads no entry for this holder reports `errLockLeaseLost` like the upgrade paths, so `noteLeaseLost` resets the local state — the plain "cannot unlock to a level above the current lock level" transition error would leave `leaseLost` unset with `lockKnown` already cleared, and the sector write gate open (`TestLeaseLossOnUnlockDowngrade`). (The sector read/write path keeps reporting SQLITE_BUSY for a lost lease — see the lease-renewal bullet: mid-write, BUSY is the only way to make sqlite stop writing and come back through Lock.)
* **Every process must run this version.** A process still running the pre-#47 code updates the lock ConfigMap unconditionally, carrying no resourceVersion, and overwrites whatever holders are stored there. The guarantee holds only while every process touching the database runs the new code.
* **Lease renewal needs the file to be driven.** `Lock`, `Unlock`, `Close` and `CheckReservedLock` re-stamp the lease when less than half of it remains, and so do `ReadAt` and `WriteAt` — the read/write path used to gain no renewal at all, so one I/O stretch longer than the lease could lapse the lease mid-transaction while `WriteAt` kept writing. Now a due renewal rides the next sector read or write at zero extra API calls while the lease is fresh, and once the lease is lost the file refuses to serve or write sectors (`SQLITE_BUSY`) until sqlite re-locks and revalidates. The check is per sector, not per call (issue #459): every sector loop in `ReadAt`, `WriteAt` and `Truncate` re-checks the lease, so a multi-sector call that outlives its own lease — each sector is its own Kubernetes call with no timeout — stops with `SQLITE_BUSY` at the first sector past the lapse instead of writing or serving on under a lock the cluster has given away. A transaction still lapses if its I/O stops entirely — a paused process loses its lock as described above — but a merely long transaction keeps its lease as long as its I/O keeps flowing.
* **The file advertises `SQLITE_IOCAP_SAFE_APPEND` (and atomic 64 KiB writes).** SQLite only writes a rollback journal's magic bytes at offset 0 when the database file claims safe appends; otherwise it writes zeros there on purpose, and `hasHotJournal` — which reads that byte to decide whether a journal is hot — then never asks whether a live writer holds a reserved lock. One sector is one whole ConfigMap, written exactly once, so an append can never leave garbage after bytes a reader has seen: the guarantee the unix VFS reports for the same flag. Without it SQLite's hot-journal protocol is short-circuited and a reader can play back a live writer's journal.
* **Sector writes are gated only by lease loss, not by lock level.** The exclusion is enforced on the lock transitions sqlite performs; `WriteAt` is refused (`SQLITE_BUSY`) only once this file's own lease has been pruned by another process — until a successful `Lock` re-establishes the holder — so a file never keeps writing sectors under a lock it no longer holds, not even mid-loop: the per-sector re-check (issue #459) stops a `WriteAt` whose sector loop has outlived the lease. A file that never took a lock at all (the cmd tools, which drive `NewFile` directly) stays ungated, as before: it still must not be run against a database other processes are using.

### Known upstream bug: `CheckReservedLock` is inverted by the dependency

At the pinned version of [`github.com/psanford/sqlite3vfs`](https://github.com/psanford/sqlite3vfs) (`v0.0.0-20260519004904-f9180fa2acc9`), the cgo shim's `goVFSCheckReservedLock` writes `*pResOut = 0` when the Go `File.CheckReservedLock` answers `true` and `1` when it answers `false` (`sqlite3vfscgo.go`, around lines 399–403) — the opposite of SQLite's `xCheckReservedLock` contract, which is "set `*pResOut` to a non-zero value if a reserved lock exists". SQLite consumes that value in `hasHotJournal` to decide whether an existing rollback journal belongs to a live writer (reserved lock held → leave it alone) or is a stale crash remnant (nobody holds reserved → play it back), so with the uncorrected inversion SQLite can read or delete another writer's journal mid-write.

Worked around in this VFS: `file.CheckReservedLock` deliberately returns the negation of the true answer — the last Go code the answer passes through — so the value that reaches SQLite is correct. The cost is that the Go-level answer is inverted for direct callers too (the tests pin the inverted expectations). Remove the negation — and flip those expectations — once upstream maps the File's answer straight through.

`TestCheckReservedLockCrossesCGOBoundary` pins the crossing itself, through real SQLite across two VFS instances: writer A holds RESERVED with its rollback journal on the disk, reader B's first read reaches `hasHotJournal`, and B must leave A's journal alone. That test only exercises the answer if the file advertises `SQLITE_IOCAP_SAFE_APPEND`; without it SQLite zeroes the journal's magic at offset 0 and `hasHotJournal` discards the value before `xCheckReservedLock` can matter — see the device-characteristics bullet above. The test therefore fails if the negation is removed AND the characteristic is advertised, and passing in-process assertions alone cannot see this: the value SQLite finally consumes is what counts.

The VFS is no longer "really very slow" at the Kubernetes API, though: reads and writes no longer LIST every sector ConfigMap of the file, `FileSize` is O(1) from memory while a SHARED-or-better lock is held (and otherwise costs one LIST, which also revalidates and warms the sector cache), each changed sector is written exactly once per write (UPDATE or CREATE chosen from what is known), reads never create missing sectors, and a lock transition for a level already held still makes no lock API call at all. See the benchmarks below.

### API changes and migration notes (issue #47)

* **`NewVFS` takes variadic options.** The signature is now `NewVFS(ctx, kc, namespace, logger, retries, opts ...Option)`. Existing calls compile unchanged (the variadic parameter may be omitted); to pass options, append them: `NewVFS(ctx, kc, ns, logger, retries, vfs.WithLockLease(10*time.Second))`. The only option today is `WithLockLease`.
* **`WithLockLease(0)` (or any non-positive value) keeps the default 30 s lease.** `Option` cannot return an error, so an out-of-range value is documented behaviour — the default stands — rather than a silently broken lease. Pass a positive duration to shorten or lengthen the lease.

### FullPathname canonicalises file names (this PR)

SQLite reaches `xFullPathname` on every open, and before this change the VFS returned the name exactly as the caller spelt it. A database created under a name that was not clean — for example `./x.db` or `a/../x.db` — was therefore stored on the cluster under that literal name (`base32("./x.db")…`), while the same database opened as `x.db` now resolves to the cleaned name and looks for `base32("x.db")…`. **The ConfigMaps of a database first created under an unclean spelling are no longer found**, and opening the database anew creates a second, empty one under the clean name.

To migrate such a database:

* open it by its old literal name with an earlier version of this VFS and copy the data out (for example `ATTACH` both databases and `INSERT INTO clean.db SELECT * FROM old-literal.db`, or `sqlite3`'s `.backup`), then open it under the clean name going forward; or
* rename the ConfigMaps in place: names, the `relevant-file` label and the `filename` data key are all pure functions of the file name, so every ConfigMap whose name, label or `filename` key encodes the old literal name can be renamed (or edited) to the base32 encoding of the cleaned name — the lockfile suffix `-lockfile` and the `-<index>` sector suffixes stay as they are.

Two boundaries of the canonicalisation worth knowing:

* absolute paths are cleaned but stay absolute: `/data/sub/../app.db` resolves to `/data/app.db`;
* a leading slash is never folded away: **`/app.db` and `app.db` remain two different databases**, as before.

## TODO

* ~~have filesize be O(1) rather than O(n)~~ — done: `FileSize` is O(1) while a SHARED-or-better lock is held, and otherwise costs one LIST that revalidates and warms the sector cache.

## On-cluster data layout (issue #47 added lockfile keys; issue #68 added journal files)

The on-cluster data layout is backward and forward compatible — databases written by the old code remain readable by the new code and vice versa. Issue #47 added two data keys to the lockfile ConfigMap and issue #68 added journal files, both described in the bullets below:

* sector ConfigMaps are still named `<base32 filename>-<index>`, labelled `data: sector` and `relevant-file: <base32 filename>` (shortened to the `long-` stand-in once the encoding passes the 63-character label-value cap — see "How it works"; issue #351), and hold the sector bytes in the `sector` binary key plus the `filename` data key;
* the lockfile ConfigMap is still named `<base32 filename>-lockfile`, labelled `data: lockfile` and `relevant-file`, and holds the locking state: the lock level — the highest level any live holder has — in the `lock` data key as before, plus the issue #47 keys `shared-holders` (holder id → lease expiry) and `writer` (the one writer's id, level and lease); a lockfile written by the old code carries only `lock`, which the new code reads as "nobody holds anything" until its next CAS rewrites it in the new format;
* a **rollback journal** (issue #68) is stored exactly like the database it belongs to: its sector ConfigMaps are named `<base32 "<db>-journal">-<index>` with the same `data: sector` and `relevant-file` labels and the same `sector` and `filename` data keys, and it has its own lockfile ConfigMap named `<base32 "<db>-journal">-lockfile`. `xOpen`, `xAccess`, `xTruncate`, `xFileSize` and `xDelete` on that name go through the VFS. Deleting the journal removes every one of its sectors: one LIST of them, then the header sector 0 first, then the remaining sectors, and its lockfile last — the order that makes every half-state of a crash mid-delete safe, described in "Crash part-way through deleting the rollback journal (issue #121)" under WARNINGS above. A database written with `_journal=MEMORY` has no such ConfigMaps and still opens unchanged — its journal mode is a per-connection decision, and "no journal sectors" is exactly what `Access` reports for a name with no sector ConfigMaps;
* namespaces are still labelled `kube-sqlite3-vfs: used`.

## Benchmarks (issue #8: fewer API calls per operation)

Measured on the fake clientset with `go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x`, two runs each. ns/op varies from run to run; api-calls/op is the number that matters and is stable, counted from the fake clientset's recorded actions (`fake.ClearActions()` around the operation, `len(fake.Actions())` after).

| Benchmark                                                               | ns/op before (run 1 / run 2) | ns/op after (run 1 / run 2) | API calls/op before | API calls/op after |
| ----------------------------------------------------------------------- | ---------------------------- | --------------------------- | ------------------- | ------------------ |
| BenchmarkSequentialRead                                                 | 320759 / 298050              | 287236 / 211234             | 3.00                | 2.00               |
| BenchmarkSequentialReadLocked (held SHARED lock)                        | —                            | 150826 / 91810              | —                   | 0.00               |
| BenchmarkRandomPageRead                                                 | 765906 / 705638              | 582095 / 647806             | 3.00                | 2.00               |
| BenchmarkSequentialWrite                                                | 334660 / 410730              | 214296 / 240557             | 3.00                | 2.00               |
| BenchmarkSQLiteInsert1000Rows (one 64 KiB sector, 4 KiB pages — superseded workload) | 3026310 / 2981848 | 2509512 / 2829036         | 39.00               | 14.00              |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, transaction spans 7 sectors) | —                          | 2453079 / 2389578           | —                   | 17.00              |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, `_journal=MEMORY` — the same workload, for comparison) | —                          | —                           | —                   | 25.00              |
| BenchmarkSQLiteInsert1000RowsJournalModes/journal=DELETE/transactions=01 (64 KiB pages, `_journal=DELETE`, journal stored through the VFS; the standalone DELETE variant this row was first measured with was folded into this sub-benchmark) | —                          | 2628566 / 2588233           | —                   | 65.00              |

What changed per operation: an unlocked read used to pay a LIST + a GET of the last sector (in `FileSize`) plus a GET for the sector read, and now pays one LIST + one GET (nothing at all while a lock is held — sectors come from the cache); a sector overwrite used to try CREATE first and fall back to UPDATE (two calls) and now writes the sector once, choosing UPDATE or CREATE from what is known; `Lock`, `Unlock` and `CheckReservedLock` used to GET the lock ConfigMap before every transition and now use the locally tracked state.

BenchmarkSequentialReadLocked is new: it measures the under-lock steady state directly. After one warm-up read pays the revalidating LIST, a read under a held SHARED lock costs zero API calls — `FileSize` is served from memory and every sector comes from the cache.

The insert benchmark now runs at 64 KiB pages (the `main.go`/cmd-tools configuration: one VFS sector per page) with ~200-byte rows, so its transaction spans seven 64 KiB sectors and exercises the multi-sector write path. Its previous one-sector numbers are kept above for the record. There is no before number for the multi-sector workload: the pre-optimisation code panicked the moment a database outgrew its first sector, so it cannot run this workload at all.

## Locking (issue #47): mutual exclusion and what it costs

The WARNINGS section above describes the new locking guarantees: compare-and-swap on the lock ConfigMap via `resourceVersion`, holder ids, leases. This table records what that costs at the Kubernetes API, measured as in the issue #8 table above — fake clientset, `go test ./pkg/vfs -run ZZZ -bench . -benchtime 10x -count 2`; BEFORE was recorded on the pre-#47 tree with the same flags, AFTER on this tree. `BenchmarkLockUnlockShared` was additionally run with `-count 5` for a stable median. ns/op varies between runs; api-calls/op is exact and is the number that matters.

| Benchmark                                                                | api-calls/op before | api-calls/op after | ns/op after (run 1 / run 2) |
| ------------------------------------------------------------------------ | ------------------- | ------------------ | --------------------------- |
| BenchmarkSequentialRead                                                  | 2.000               | 2.000              | 197822 / 212903             |
| BenchmarkSequentialReadLocked (held SHARED lock)                         | 0                   | 0                  | 80374 / 87765               |
| BenchmarkRandomPageRead                                                  | 2.000               | 2.000              | 468399 / 535553             |
| BenchmarkSequentialWrite                                                 | 2.000               | 2.000              | 200935 / 216833             |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, transaction spans 7 sectors) | 17.00               | 24.00              | 2456432 / 2197694           |
| BenchmarkSQLiteInsert1000Rows (64 KiB pages, `_journal=MEMORY` — the same workload, for comparison) | —                   | 25.00              | —                           |
| BenchmarkSQLiteInsert1000RowsJournalModes/journal=DELETE/transactions=01 (64 KiB pages, `_journal=DELETE`) | —                   | 65.00              | 2628566 / 2588233           |
| BenchmarkLockUnlockShared                                                | 2.000               | 4.000              | 24686 / 19554               |
| BenchmarkLockUnlockExclusive (new)                                       | —                   | 6.000              | 30912 / 31336               |

The read and write paths are untouched — their api-calls/op is identical, because the sector path still makes no lock calls at all (`BenchmarkSequentialReadLocked` stays at 0). Everything that moved is the CAS itself: a transition that writes now GETs the lock ConfigMap first, where the old code wrote blindly. `BenchmarkLockUnlockShared`'s acquire + release cycle goes 2 → 4 (one GET + one UPDATE per transition). The insert transaction performs five lock transitions — SHARED, RESERVED, EXCLUSIVE, back to SHARED, NONE — costing +5 GETs, and its EXCLUSIVE climb is now two CAS steps (claim PENDING, then acquire EXCLUSIVE once no other holder remains) where the old code wrote EXCLUSIVE in a single blind UPDATE, another +1 GET +1 UPDATE: 17 → 24. `BenchmarkLockUnlockExclusive` is new and has no before number; its three transitions (PENDING claim, EXCLUSIVE acquire, downgrade to SHARED) cost 6. The extra GET and the JSON round-trip of the holder state also show in CPU terms: `BenchmarkLockUnlockShared` went from ~12.9k–14.7k ns/op and 127 allocs/op before to ~20k–41k ns/op across seven runs (median of the five `-count 5` runs ~30k) and 170 allocs/op. The on-cluster rollback journal costs 40 extra API calls per committed transaction. The `_journal=DELETE` figure was first measured by a standalone DELETE variant of the insert benchmark, which run #309 (issue #120) folded into `BenchmarkSQLiteInsert1000RowsJournalModes` — the 1000-row insert is run verbatim — same 64 KiB pages, same CREATE TABLE before ClearActions, same 1000-row prepared-insert transaction, same `api-calls/op` accounting and same multi-sector assertion — opening the database with `_journal=DELETE` instead of `_journal=MEMORY`. Its journal is therefore a real file through the VFS: SQLite writes and later deletes the `<db>-journal` sector ConfigMaps, and those calls land in the count. Measured with `go test ./pkg/vfs -run ZZZ -bench BenchmarkSQLiteInsert1000Rows -benchtime 10x`: `_journal=MEMORY` 25.00 api-calls/op, `_journal=DELETE` 65.00 api-calls/op (exact, identical across `-count 3` runs; the issue-#120 table below re-measures the same 65.00 as `BenchmarkSQLiteInsert1000RowsJournalModes/journal=DELETE/transactions=01`). The delta is the journal header and page writes plus the journal's deletion — the price of a rollback that can actually be rolled back after a crash.

What did not change: `CheckReservedLock` costs the same as before (zero calls while this file holds RESERVED or better locally, one GET otherwise), and a lock transition for a level already held still makes no lock API call.

## Benchmarks (issue #120): every journal mode, one and ten transactions

`BenchmarkSQLiteInsert1000RowsJournalModes` runs the module's committed
1000-row insert workload (64 KiB pages, the transaction spanning several
sectors) under each journal mode — `DELETE`, `TRUNCATE`, `PERSIST` and
`MEMORY` — once for a single committed transaction and once for ten
consecutive committed transactions. One benchmark iteration is one unit of
`api-calls/op`: one transaction for the `transactions=01` sub-benchmarks, the
whole ten-transaction sequence for `transactions=10`; `api-calls/tx`
restates the ten-transaction numbers per single transaction so they compare
directly with the one-transaction rows. The sub-benchmark also asserts
`PRAGMA journal_mode` reports the mode it was asked to measure, so a mode
SQLite silently ignored could not masquerade as a number.

```
go test ./pkg/vfs -run ZZZ -bench 'BenchmarkSQLiteInsert1000Rows' -benchtime 10x -count 3
```

api-calls/op and api-calls/tx were identical across all three runs (the
metric is exact); ns/op is the median of the three.

| Sub-benchmark (`BenchmarkSQLiteInsert1000RowsJournalModes/...`) | api-calls/op (1 tx) | api-calls/op (10 tx) | api-calls/tx (10 tx) | ns/op median |
| --- | --- | --- | --- | --- |
| `journal=DELETE/transactions=01`   | 65.00 | —     | —     | 3,662,953 |
| `journal=DELETE/transactions=10`   | —     | 832.0 | 83.20 | 45,912,698 |
| `journal=TRUNCATE/transactions=01` | 61.00 | —     | —     | 3,409,148 |
| `journal=TRUNCATE/transactions=10` | —     | 792.0 | 79.20 | 49,047,410 |
| `journal=PERSIST/transactions=01`  | 62.00 | —     | —     | 3,757,908 |
| `journal=PERSIST/transactions=10`  | —     | 783.0 | 78.30 | 59,008,417 |
| `journal=MEMORY/transactions=01`   | 25.00 | —     | —     | 2,988,410 |
| `journal=MEMORY/transactions=10`   | —     | 261.0 | 26.10 | 39,598,961 |
| `BenchmarkSQLiteInsert1000Rows` (the pre-existing MEMORY baseline) | 25.00 | — | — | 3,233,790 |

What the table says, in before/after terms for the issue #68-era numbers:

* `MEMORY` costs 25.00 API calls per committed transaction against the
  crash-safe modes' 61.00–65.00 — the delta is the rollback journal that can
  actually be rolled back after a crash, and it is the price #68 chose to
  pay.
* The earlier claim that `TRUNCATE`/`PERSIST` would cost **more** per
  transaction than `DELETE` is refuted: they measure ~4 calls/transaction
  **cheaper** (61.00/62.00 vs 65.00 for one transaction; 79.20/78.30 vs
  83.20 over ten). Where DELETE pays one LIST plus one Delete per journal
  sector plus the journal lockfile's Delete at every commit, `TRUNCATE`'s
  `xTruncate` deletes the journal's sectors directly — no LIST, no lockfile
  call. `PERSIST` skips the per-transaction journal writes almost entirely
  once the journal is warm (its journal sectors survive commit), which is
  why it edges `TRUNCATE` at ten transactions.
* The ten-transaction rows are where the per-transaction costs show up, and
  they confirm the ranking holds across consecutive transactions. The
  per-transaction cost itself is not constant, though: from
  `transactions=01` to `transactions=10` it rises ~16–18 calls/tx for the
  crash-safe modes (DELETE 65.00 → 83.20, TRUNCATE 61.00 → 79.20, PERSIST
  62.00 → 78.30 api-calls/tx) and ~1 for MEMORY (25.00 → 26.10). The
  measured source of that growth is in the next section.
* `DELETE` stays the default anyway (see the journal-mode paragraph in the
  WARNINGS section): it is the only mode that leaves no journal state on the
  cluster after a clean commit — `TRUNCATE` leaves the journal's lockfile
  ConfigMap, `PERSIST` leaves its journal sectors. `MEMORY` remains
  refusable-only: 25.00 calls buys no crash safety at all.

### Where the ten-transaction growth comes from (issue #338)

The `transactions=01` row is the cheapest transaction of the sequence, not a
steady-state rate. Each 1000-row transaction adds ~200 KiB — about three
64 KiB sectors — to the database, and as the b-tree deepens the same insert
workload overwrites more sectors. A crash-safe mode pays for every
overwritten sector twice: once to write the database sector, and once to
record that sector's original content in the rollback journal (which DELETE
deletes again at commit, TRUNCATE truncates away and PERSIST rewrites in
place). `MEMORY` keeps no journal on the cluster, so it pays only the
database-sector share — which is why the crash-safe modes' averages climb
~16–18 api-calls/tx above their `transactions=01` row over ten transactions
while `MEMORY` climbs ~1.

`BenchmarkSQLiteInsert1000RowsFirstVsLater` measures the sequence one
transaction at a time — the journal-mode matrix above conflates the first
transaction with the nine later ones, so it cannot show this shape:

```
go test ./pkg/vfs -run ZZZ -bench BenchmarkSQLiteInsert1000RowsFirstVsLater -benchtime 1x
```

api-calls per transaction (the metric is exact; identical across repeated
runs):

| journal  | tx01 | tx02 | tx03 | tx04 | tx05 | tx06 | tx07 | tx08 | tx09 | tx10 | 10-tx avg |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `DELETE`   | 65 | 73 | 73 | 79 | 73 | 93 | 97 | 73 | 103 | 103 | 83.20 |
| `TRUNCATE` | 61 | 69 | 69 | 75 | 69 | 89 | 93 | 69 | 99  | 99  | 79.20 |
| `PERSIST`  | 62 | 69 | 69 | 75 | 69 | 87 | 91 | 69 | 96  | 96  | 78.30 |
| `MEMORY`   | 25 | 24 | 24 | 30 | 24 | 26 | 30 | 24 | 27  | 27  | 26.10 |

The cost is flat while consecutive transactions overwrite the same number of
sectors (DELETE's runs of 73s) and steps up when a transaction's writes reach
further into the file — the jumps land as extra sector creates, gets,
updates and journal-sector deletes, not as a smooth per-transaction tax, and
the shape is stepwise rather than monotonic (DELETE tx04 costs more than
tx05) because it tracks which sectors a transaction's writes happen to
reach, not the transaction count. The 10-transaction averages of these
sequences reproduce the matrix above exactly (83.20 / 79.20 / 78.30 /
26.10), and each tx01 row reproduces the matching `transactions=01` row
(65.00 / 61.00 / 62.00 / 25.00). What this table does not do is attribute
the jumps call by call; the totals are exact, the mechanism above accounts
for their direction and size, and the individual calls inside a jump were
not traced further.

## Go 1.27 and dependencies (issue #9)

The module moved from `go 1.19` to `go 1.27` and its dependencies from their
2023 versions, among them several `golang.org/x/*` releases with published
vulnerabilities. On go1.27.0 govulncheck still reported eight called
standard-library vulnerabilities (net/http, http2, crypto/tls,
mime/multipart), all fixed in go1.27.2, so the issue #28 follow-up raised
the module's floor to `go 1.27.2`. `govulncheck ./...` now reports no
vulnerabilities in this module.

| Module | Old | New |
| ------ | --- | --- |
| Go (language/module version) | 1.19 | 1.27.2 |
| `k8s.io/api` | v0.26.3 | v0.37.1 |
| `k8s.io/apimachinery` | v0.26.3 | v0.37.1 |
| `k8s.io/client-go` | v0.26.3 | v0.37.1 |
| `github.com/mattn/go-sqlite3` | v1.14.16 | v1.14.52 |
| `github.com/google/uuid` | v1.3.0 | v1.6.0 |
| `github.com/thought-machine/go-flags` | v1.6.2 | v1.7.0 |
| `github.com/psanford/sqlite3vfs` | v0.0.0-20230408… | v0.0.0-20260519004904-f9180fa2acc9 |
| `go.uber.org/zap` | v1.24.0 | **removed** — replaced by `log/slog` |
| `golang.org/x/net`, `.../sys`, `.../text`, `.../time`, `.../term`, `.../oauth2` and the rest of the indirect set | 2023 releases | current releases (`golang.org/x/net` v0.61.0, `golang.org/x/sys` v0.49.0, …) |

`k8s.io/{api,apimachinery,client-go}` move together at the same version, as
they must. Everything else is at `latest`; `go mod tidy` is clean. Nothing
in the direct set had to be held back — the only dependency that went away is
zap.

### Follow-up (issue #28)

* **Go 1.27.2 floor.** `go.mod` requires `go 1.27.2` (see above:
  eight called standard-library vulnerabilities on 1.27.0, all fixed in
  1.27.2). `govulncheck ./...` reports nothing reachable.
* **The tools exit non-zero on failure.** `db-download` used to log failed
  reads, writes, size checks and byte comparisons and still exit 0 on a
  half-failed transfer (and ignored the `Seek` error between them), and
  `main` logged a failed insert and committed the rest of the transaction
  anyway. Every failure now returns up to `main`, which prints it to stderr
  and exits 1 — and because nothing calls `os.Exit` mid-run any more, the
  deferred `db.Close`/`f.Close` calls actually run on the error paths.
* **`file-create` opened the wrong file.** It opened the literal path `fn`
  instead of the variable `fn` holding the file's name, so the local half of
  its differential comparison always went to a stray file called `fn`.
* **Tests.** `main`, `db-upload` and `db-download` are covered by tests
  driven with the fake clientset (`go test -race ./...`): db-upload's stored
  sector bytes, db-download's transfer and its size/byte verification, and
  the failure paths — missing file, failed sector write, failed insert,
  failed commit, size mismatch, byte mismatch.

### Modernisation (behaviour unchanged except where noted)

* **`context.Context` through the VFS.** `NewVFS(ctx, kc, namespace, logger, retries)`
  holds the context and every Kubernetes call the VFS makes uses it, so callers
  can cancel or time out API calls. There is no `context.TODO()` left in the
  library. SQLite calls into the VFS through cgo, so there is no per-call
  context to accept; the context is per VFS.
* **Errors, not panics.** The library returns errors: `Unlock` used to call
  `logger.Panicf`/`Panic` on an invalid request and now returns a wrapped error,
  pinned by `TestUnlockRejectsInvalidRequests`. The commands print the error to
  stderr and exit non-zero. *Deliberate behaviour change* (see below).
* **`errors.Is`/`errors.As` and `%w`.** Sentinels such as `errSectorNotFound`
  are compared with `errors.Is`, and errors that gain context are wrapped with
  `%w`.
* **Modern idioms** where they simplify: `maps.Copy`, builtin `min`/`max`,
  range-over-int, `math/rand/v2`.
* **Dead code removed.** `cmd/db-create` (whose body after flag/logger setup was
  entirely commented out) is deleted, along with the large commented-out block
  in `cmd/db-upload` and the write-never-read `Sector.Labels` field.
* **Docs.** `pkg/vfs` has a package comment and every exported identifier has a
  doc comment.
* **zap → `log/slog`.** zap is gone from `go.mod` and `go.sum`. The diff stays
  small because every zap call maps onto an equivalent `slog` call
  (`logger.Info`/`Error`/`Debug`/`With`), the standard library removes two
  direct dependencies (`go.uber.org/zap`, `go.uber.org/multierr`) and their
  indirect tail, and `-v` keeps exactly its old meaning: debug-level logging
  (as `zap.NewDevelopment()` was) instead of info-level JSON.

### Deliberate behaviour changes

1. **`Unlock` returns an error instead of panicking** on an invalid request. A
   panic unwound through the cgo sqlite boundary and killed the process without
   a usable message; the caller now gets a wrapped error.
2. **The commands exit non-zero with a stderr message instead of panicking.**
3. **`db-download --filename` defaults to `file2.db`** instead of the author's
   hardcoded `/home/richardf/gitclones/kube-sqlite3-vfs/file2.db`. The old
   default could not work on any other machine.
4. **`-v` selects slog debug text logging** instead of zap's development logger.
   The effective level (debug) is unchanged.

## Benchmark results after the Go 1.27 / dependency work (issue #9)

Medians of a `-count=5` run of `go test ./pkg/vfs -run ZZZ -bench . -benchmem`,
before (the tree as it stood after issue #8, recorded on the same machine) and
after the modernisation. `api-calls/op` is stable; `ns/op` is not.

| Benchmark | ns/op before | ns/op after | change | API calls/op |
| --------- | ------------ | ----------- | ------ | ------------ |
| BenchmarkSequentialRead | 162694 | ~154000–156000 | −4% to −5% | 2 (unchanged) |
| BenchmarkSequentialReadLocked | 1694 | ~1685–1706 | ±1% | 0 (unchanged) |
| BenchmarkRandomPageRead | 327324 | ~325000–336000 | −1% to +3% | 2 (unchanged) |
| BenchmarkSequentialWrite | 158834 | ~161000–167000 | +1% to +5% | 2 (unchanged) |
| BenchmarkSQLiteInsert1000Rows | 2933441 | ~2730000–2820000 | −4% to −7% | 17 (unchanged) |

No benchmark regressed by more than 10%; the worst movement is a ~5% `ns/op`
increase on `BenchmarkSequentialWrite`, which is within run-to-run noise on this
shared machine and less than the already-recorded variation of the issue #8
baseline. `api-calls/op` is identical for every benchmark, so the on-cluster
call pattern did not change.
