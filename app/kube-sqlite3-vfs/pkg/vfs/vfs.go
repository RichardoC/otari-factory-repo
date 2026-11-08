// Package vfs implements a sqlite3vfs virtual file system that stores
// database files in Kubernetes as ConfigMaps: one ConfigMap per 64 KiB
// sector plus a "lockfile" ConfigMap holding the SQLite lock level.
//
// A database file is split into fixed-size sectors (SectorSize — 64 KiB, the
// largest SQLite page size). Each sector is the BinaryData of a ConfigMap
// named "<base32 filename>-<sector index>", labelled with the file's base32
// name (shortened once that passes Kubernetes' 63-character label-value
// cap — see file.relevantFileLabel); the lock ConfigMap is named "<base32
// filename>-lockfile" and holds
// the lock level as a string. Reads and writes map onto Get/Update/Create
// calls, and the file size is computed from a single LIST of the file's
// sector ConfigMaps.
//
// The VFS talks to any kubernetes.Interface: pass a real clientset in
// production and client-go's fake clientset in tests. The context given to
// NewVFS is used for the sector read/write calls; SQLite calls into the VFS
// through cgo, so there is no per-call context to accept. The lock-path
// calls (the lock ConfigMap compare-and-swap and the CheckReservedLock
// read) instead run on a context derived from the construction one with
// context.WithoutCancel, each bounded by its own timeout: a caller
// cancelling its context after Open must not be able to cancel a
// compare-and-swap half-way, because the atomicity of that call is what
// issue #47's mutual exclusion is built on.
package vfs

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/psanford/sqlite3vfs"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// LockFileNameSuffix is appended to the base32-encoded file name to form
	// the name of the ConfigMap holding the file's SQLite lock level.
	LockFileNameSuffix = "lockfile"

	// SectorSize is the size of one stored sector: 64 KiB, the largest page
	// size SQLite supports. Each sector is stored as one ConfigMap.
	SectorSize = 64 * 1024 // Max pagesize supported by SQLITE3

	// defaultLockLease is how long a lock holder's lease lasts unless
	// WithLockLease says otherwise.
	defaultLockLease = 30 * time.Second

	// maxLockCASAttempts bounds the compare-and-swap retries on the lock
	// ConfigMap. Every attempt re-reads the state and re-decides from what
	// it read, so the bound only matters under sustained contention.
	// Exhaustion reports sqlite3vfs.BusyError (issue #479): contention is
	// not an invalid transition, and SQLITE_BUSY is the code sqlite runs
	// its busy handler for, where a plain error would surface as
	// SQLITE_ERROR and hard-fail the lock.
	maxLockCASAttempts = 10

	// lockCallTimeout bounds one lock-path Kubernetes call (the Get, Update
	// or Create of a lock compare-and-swap, and the CheckReservedLock read).
	// Those calls run on lockCtx, which no caller can cancel; this timeout
	// is what keeps a hung API server from hanging SQLite instead.
	lockCallTimeout = 30 * time.Second

	// maxLabelValue is Kubernetes' hard cap on a label VALUE: 63
	// characters. The "relevant-file" label of every ConfigMap of a file
	// carries that file's base32-encoded name, which needs 8 characters
	// per 5 name bytes, so a name over 35 bytes cannot be the label
	// verbatim and is represented by the shortened stand-in below
	// (issue #351).
	maxLabelValue = 63

	// longNameLabelPrefix marks a "relevant-file" label standing in for a
	// name too long to fit maxLabelValue: longNameLabelPrefix + the
	// unpadded lower-case base32 of SHA-256(name). The prefix contains
	// '-', which no plain base32 encoding produced by this package
	// does, so the two label forms can never be confused with — or collide
	// with — one another.
	longNameLabelPrefix = "long-"
)

// The lock ConfigMap's Data holds the lock state under these keys:
//
//   - lockLevelDataKey ("lock"): the aggregate lock level over all live
//     holders, spelled exactly as the single-writer format spelled it
//     (LockNone…LockExclusive), so a reader of the old format still sees a
//     meaningful level;
//   - sharedHoldersDataKey ("shared-holders"): a JSON object mapping holder
//     id to lease expiry (RFC3339Nano, UTC) — any number of SHARED holders;
//   - writerDataKey ("writer"): a JSON object {"id","level","lease"} for
//     the single RESERVED/PENDING/EXCLUSIVE holder, or empty when none.
const (
	lockLevelDataKey     = "lock"
	sharedHoldersDataKey = "shared-holders"
	writerDataKey        = "writer"
)

// CommonSectorLabel and LockfileLabel are the base labels every sector and
// lockfile ConfigMap, respectively, carries. They are vars only because Go
// cannot express a map as a const.
var (
	CommonSectorLabel = map[string]string{"data": "sector"}
	LockfileLabel     = map[string]string{"data": "lockfile"}
)

type vfs struct {
	kc        kubernetes.Interface
	logger    *slog.Logger
	retries   int
	namespace string

	// lockLease is how long one holder's lock entries stay valid without
	// renewal. A holder that stops renewing — a crashed process — is
	// pruned by the next CAS of any other process, so a dead holder can
	// never block the database forever.
	lockLease time.Duration

	// ctx backs the sector read/write Kubernetes calls this VFS makes. It
	// is fixed at construction because SQLite enters the VFS through cgo,
	// where there is no caller context to propagate.
	ctx context.Context

	// lockCtx backs every LOCK-PATH Kubernetes call: the lock ConfigMap
	// compare-and-swap and the CheckReservedLock read. It is the
	// construction context with cancellation removed
	// (context.WithoutCancel): that context usually belongs to the
	// caller's registration scope (a command run, a request handler), and
	// once it is cancelled every remaining compare-and-swap would fail
	// mid-flight — precisely the calls whose read-decide-write atomicity
	// issue #47's mutual exclusion stands on. Each call adds its own
	// lockCallTimeout.
	lockCtx context.Context

	// clock returns the current time for the whole lock path (lease
	// stamping, expiry pruning, renewal decisions). It is time.Now except
	// in tests, which advance an injected clock instead of sleeping.
	clock func() time.Time
}

// Option configures a vfs at construction time.
type Option func(*vfs)

// WithLockLease sets how long a lock holder's entries stay valid without
// renewal. The default is defaultLockLease (30s); a holder renews on every
// successful lock CAS and whenever any operation on the file — lock path or
// sector read/write — finds less than half the lease remaining.
//
// A non-positive value is ignored and keeps the default: Option cannot
// return an error, so an out-of-range value is documented behaviour (the
// default stands) rather than a silently broken lease.
func WithLockLease(d time.Duration) Option {
	return func(v *vfs) {
		if d > 0 {
			v.lockLease = d
		}
	}
}

// NewVFS returns a VFS that stores database files as ConfigMaps in
// namespace. Every Kubernetes call the VFS makes uses ctx. retries is the
// number of attempts Open makes against races between concurrent clients.
func NewVFS(ctx context.Context, kc kubernetes.Interface, namespace string, logger *slog.Logger, retries int, opts ...Option) *vfs {
	v := &vfs{kc: kc, logger: logger, retries: retries, namespace: namespace, lockLease: defaultLockLease, ctx: ctx, lockCtx: context.WithoutCancel(ctx), clock: time.Now}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

func (f *file) b32ByteFromString(s string) []byte {
	sb := []byte(s)
	dst := make([]byte, f.encoding.EncodedLen(len(sb)))
	f.encoding.Encode(dst, sb)
	return dst
}

// longNameLabelEncoding spells the SHA-256 digest of an over-long name as
// its "relevant-file" label stand-in: RFC 4648 base32, lower case, without
// padding — 52 characters for a 32-byte digest, always within
// maxLabelValue.
var longNameLabelEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// relevantFileLabel returns the value stored under the "relevant-file"
// LABEL on this file's lockfile and every sector ConfigMap (issue #351).
// Kubernetes caps a label value at maxLabelValue (63 characters) and the
// base32 name needs 8 characters per 5 name bytes, so any name over 35
// bytes used to overflow the cap and make every ConfigMap Create fail on
// a real API server — while the fake clientset, which validates nothing,
// admitted it, so no test caught the limit. The label keeps the full
// encoded name whenever it fits — every such name keeps exactly the label
// it always had — and only an over-long name is represented by the
// fixed-size stand-in longNameLabelPrefix + base32(SHA-256(name)), which
// never passes the cap and cannot collide with a plain encoding (its
// prefix contains a character no plain encoding has) nor, bar a 2^-256
// coincidence, with another file's stand-in.
//
// The label is a pure function of the name, so every process — the one
// writing a sector, the one LISTing them, the one deleting them — derives
// the same selector. The ConfigMap NAMES keep the full encoded name
// whatever the label does, and the lockfile's "relevant-file" Data key
// keeps it too (ConfigMap data values have no 63-character cap).
func (f *file) relevantFileLabel() string {
	encoded := f.b32ByteFromString(f.RawName)
	if len(encoded) <= maxLabelValue {
		return string(encoded)
	}
	sum := sha256.Sum256([]byte(f.RawName))
	return longNameLabelPrefix + longNameLabelEncoding.EncodeToString(sum[:])
}

// Close implements sqlite3vfs.File: it releases the file's holdings —
// exactly Unlock(NONE), removing this file's shared-holder entry and writer
// (if any) and leaving every other process's entries alone.
func (f *file) Close() error {
	return f.Unlock(sqlite3vfs.LockNone)
}

// Truncate implements sqlite3vfs.File: it shortens the file to size bytes,
// deleting the sectors past the new end and shortening the one holding the
// kept prefix. Growing (or no-op) truncates do nothing.
func (f *file) Truncate(size int64) error {
	// Truncate writes (writeSectorOnce) and deletes sectors, so it is
	// gated on lease loss exactly like ReadAt and WriteAt: a file whose
	// lease was pruned must not shorten the database — sqlite calls
	// xTruncate during hot-journal rollback and after VACUUM — under
	// whichever process took over the lock. Before this gate it was the
	// one sector-I/O path that still mutated the cluster after a loss.
	if err := f.renewForIO(); err != nil {
		return ioFailure(err)
	}

	fileSize, err := f.FileSize()
	if err != nil {
		return err
	}

	if size >= fileSize {
		return nil
	}

	// The last surviving byte is at size-1; the sector holding it may need
	// shortening. (The imported code used sectorForPos(size), which for a
	// sector-aligned size points one sector PAST the surviving data.)
	// Truncating to 0 leaves no surviving byte at all: every sector goes.
	lastRemainingSector := int64(-1)
	if size > 0 {
		lastRemainingSector = f.sectorForPos(size - 1)
	}

	if lastRemainingSector >= 0 && size%SectorSize != 0 {
		sect, err := f.getSector(lastRemainingSector)
		if err != nil && !errors.Is(err, errSectorNotFound) {
			return err
		}
		if err == nil {
			keep := size % SectorSize
			if int64(len(sect.Data)) > keep {
				sect.Data = sect.Data[:keep]
			} else if int64(len(sect.Data)) < keep {
				// Inconsistent state (the sector holds less than the file
				// size claimed): zero-pad so the kept prefix has room.
				padded := make([]byte, keep)
				copy(padded, sect.Data)
				sect.Data = padded
			}
			// Truncate is sector work too (issue #459): re-check the lease
			// before the shortening write — free for a live lease — and
			// stop with BusyError on a loss instead of shortening the
			// database under a lock the cluster has given away.
			if err := f.renewForIO(); err != nil {
				return ioFailure(err)
			}
			// BUG FIX (issue #8): the imported code ignored this error and
			// carried on deleting sectors, silently losing the shortened
			// sector's data.
			if err := f.writeSectorOnce(sect.Index, sect.Data, true); err != nil {
				return err
			}
			if f.valid {
				f.cacheSector(sect.Index, sect.Data)
			}
		}
		// errSectorNotFound: the sector holding the new last byte does not
		// exist (a hole in a sparse file), so there is nothing to shorten.
	}

	lastSector := f.sectorForPos(fileSize)

	// BUG FIX (issue #8): the delete loop used to START at
	// lastRemainingSector, deleting the sector that holds the kept prefix
	// (and, when fileSize was sector-aligned, running past the last
	// existing sector). It must start after it.
	for sectToDelete := lastRemainingSector + 1; sectToDelete <= lastSector; sectToDelete++ {
		// Re-check the lease for every sector deletion (issue #459): the
		// delete loop runs once per sector of the file and can outlive
		// the entry gate, so a loss mid-loop must stop it with BusyError
		// before any further sector is deleted.
		if err := f.renewForIO(); err != nil {
			return ioFailure(err)
		}
		err := f.deleteSector(sectToDelete)
		if err != nil && !kerrors.IsNotFound(err) {
			return err
		}
		if f.valid {
			delete(f.sectors, sectToDelete)
		}
	}

	if f.valid {
		f.size = size
	}

	return nil
}

// FileSize returns the size of the file. While the cache is valid (a lock
// of SHARED or better is held) this is O(1): the size is served from memory,
// having been observed from the cluster — or written by this process — since
// the lock was acquired. Otherwise it costs one LIST of the file's sector
// ConfigMaps; the imported version LISTed and then GET the last sector on
// EVERY call, including once per read.
//
// When the lock is held, that same LIST response warms the sector cache:
// this is the revalidation required after (re)acquiring SHARED, because
// another process may have modified the database between our locks.
func (f *file) FileSize() (int64, error) {
	f.vfs.logger.Debug("FileSize", "f", f)
	if f.valid {
		return f.size, nil
	}
	cms, err := f.listSectorConfigMaps()
	if err != nil {
		f.vfs.logger.Error("FileSize: list sector configmaps", "err", err)

		return 0, err
	}
	size := f.sizeFromSectorList(cms)
	if f.lockKnown && f.lockLevel >= sqlite3vfs.LockShared {
		f.size = size
		f.valid = true
		f.sectors = make(map[int64][]byte, len(cms))
		for i := range cms {
			if idx, ok := f.sectorIndexFromConfigMapName(cms[i].Name); ok {
				data := make([]byte, len(cms[i].BinaryData["sector"]))
				copy(data, cms[i].BinaryData["sector"])
				f.cacheSector(idx, data)
			}
		}
	}
	f.vfs.logger.Debug("FileSize", "f", f, "size", size)

	return size, nil
}

// A file object backs one sqlite connection; sqlite serialises the calls
// made per connection, so (as before this change) no extra synchronisation
// is added around the fields below.
type file struct {
	// RawName is the unencoded name the file was opened under.
	RawName string

	vfs      *vfs
	encoding *base32.Encoding

	// SectorLabels are the labels every sector ConfigMap of this file
	// carries: CommonSectorLabel plus the file's "relevant-file" label —
	// the base32 name while it fits the 63-character label-value cap,
	// otherwise the "long-" stand-in (relevantFileLabel, issue #351).
	SectorLabels map[string]string

	// Local view of the shared state, kept to cut Kubernetes API round
	// trips. Everything cached here is trusted only while a lock of SHARED
	// or better is held: another process may hold and modify the database
	// between our locks, so the caches are dropped the moment the lock
	// drops below SHARED (adoptLockState) and revalidated with one LIST by
	// the first FileSize after SHARED is re-acquired.
	//
	// lockLevel/lockKnown: the lock level this process believes IT holds.
	// sqlite asks Lock, Unlock and CheckReservedLock around every
	// operation; the imported code GET the lock ConfigMap before each one.
	// A file opened through the VFS starts at NONE and only ever records
	// levels it wrote itself — the ConfigMap's stored level is never
	// adopted as this file's own, because it may be another process's
	// lock. A file object created with NewFile (the cmd tools) has no known
	// local state and consults the ConfigMap per transition, as before.
	//
	// size/valid/sectors: the file size and sector contents last observed
	// from the cluster, or written by this process, while the lock is held.
	// valid is only ever true while lockLevel >= LockShared.
	lockLevel sqlite3vfs.LockType
	lockKnown bool
	size      int64
	valid     bool
	sectors   map[int64][]byte

	// holderID identifies this file's entries in the lock ConfigMap: the
	// shared-holders key maps it to a lease expiry and the writer key
	// carries it, so a file can tell its own lock from another process's.
	// It is generated once, here, and every lock operation of this file
	// acts as this holder.
	holderID string

	// leaseExpires is when this file's own lease runs out locally — the
	// moment its entries were last written with an expiry, plus the VFS's
	// lease length. It is a local approximation used only to decide WHEN a
	// renewal CAS is worth an API call; the ConfigMap's expiries are the
	// truth other processes prune against.
	leaseExpires time.Time

	// leaseLost sticks once a renewal finds this file's entries pruned from
	// the lock ConfigMap: every sector read/write then fails with
	// sqlite3vfs.BusyError until the next successful Lock re-establishes
	// the holder, so a file whose lease was taken over never serves or
	// writes sectors in that state. The lock path needs no flag — its next
	// call re-reads the cluster state anyway.
	leaseLost bool
}

// ReadAt implements sqlite3vfs.File. A read that starts at or past the end
// of the file returns (0, io.EOF); one that reaches past the end returns
// the bytes read so far with io.EOF, which the sqlite3vfs layer turns into
// SQLITE_IOERR_SHORT_READ.
func (f *file) ReadAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debug("ReadAt", "f", f, "off", off, "len(buffer)", len(p))

	// Renew a due lease before serving: a lease that lapsed mid-read means
	// another process may hold the lock and be rewriting the database, and
	// this file must not serve — least of all from caches taken under the
	// lost lock — until sqlite has re-locked and revalidated.
	if err := f.renewForIO(); err != nil {
		return 0, ioFailure(err)
	}

	firstSector := f.sectorForPos(off)

	fileSize, err := f.FileSize()
	if err != nil {
		f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)

		return 0, err
	}

	lastByte := off + int64(len(p)) - 1

	lastSector := f.sectorForPos(lastByte)
	if lastByte > fileSize {
		lastSector = f.sectorForPos(fileSize)
		lastByte = fileSize
	}
	if off >= fileSize {
		// Maybe this will stop it being unhappy about the journal file not existing?
		err = io.EOF
		f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "fileSize", fileSize, "err", err)
		return 0, err
	}

	// n is the number of bytes of p this read has filled, as one contiguous
	// prefix: a hole the read cannot see past ends it, and the short read is
	// reported once, after the loop.
	var n int
	sectors, err := f.getSectorRange(firstSector, lastSector)
	if err != nil {
		return 0, sqlite3vfs.IOErrorRead
	}
	for _, sect := range sectors {
		// Re-check the lease for every sector (issue #459): getSectorRange
		// above is one Kubernetes call per unfetched sector, so the fetch
		// of a multi-sector read can itself cross the lease expiry, and a
		// sector must not be served after another process has pruned this
		// holder and possibly rewritten it. maybeRenew's local timestamp
		// guard keeps this check free for a live lease, exactly as at
		// entry, and the loss maps to BusyError so sqlite re-locks and
		// revalidates.
		if err := f.renewForIO(); err != nil {
			return n, ioFailure(err)
		}
		// Sector s holds file bytes [s*SectorSize, (s+1)*SectorSize), so its
		// bytes start at s*SectorSize-off in p. Placing every sector by its
		// own index — not by how many bytes the sectors before it happened to
		// contribute — is what stops a missing sector from shifting every
		// later sector's bytes earlier in the buffer.
		sectOff := sect.Index * SectorSize
		pos := int(sectOff - off)
		if pos >= len(p) {
			break
		}
		start := 0 // index into sect.Data of the first byte inside p
		if pos < 0 {
			start = -pos
			pos = 0
		}

		// The bytes of this sector that fall inside p, by position.
		want := int(SectorSize) - start
		if avail := len(p) - pos; want > avail {
			want = avail
		}
		if want <= 0 {
			break
		}

		// fillZeros fills p[n:end] with zeros — a hole, or the unwritten tail
		// of a short sector — and moves the contiguous prefix past it. end is
		// clipped to the end of the file and to p.
		fillZeros := func(end int) {
			if fileEnd := int(fileSize - off); end > fileEnd {
				end = fileEnd
			}
			if end > len(p) {
				end = len(p)
			}
			if end > n {
				clear(p[n:end])
				n = end
			}
		}

		if len(sect.Data)-start <= 0 {
			// A hole: the sector has no ConfigMap (an interrupted delete,
			// or one deleted outside this VFS). Nothing is created here, and
			// no later sector may move up to fill the gap.
			//
			// A hole at or past the end of the file is simply the end of the
			// file. A hole that begins at file offset 0 is the hasHotJournal
			// case: SQLite reads one byte there and treats a short read as
			// "no magic byte", so that hole stays a short read of nothing
			// rather than zeros. Every other hole lies INSIDE the file (the
			// file's size reaches past it) and reads as zeros, including one
			// the read happens to start inside.
			if sectOff >= fileSize {
				break
			}
			if sectOff == 0 && off == 0 {
				break
			}
			// A hole inside the file reads as zeros over its own span, so the
			// sectors after it keep their true offsets.
			fillZeros(pos + want)
			continue
		}

		// A present sector. Bytes before pos that this read has not filled
		// belong to a hole (a sector too short to reach this one): they read
		// as zeros, again keeping later bytes at their true offsets.
		if pos > n {
			fillZeros(pos)
		}
		have := len(sect.Data) - start
		if have > want {
			have = want
		}
		n = pos + copy(p[pos:], sect.Data[start:start+have])
		if have < want {
			// The sector is shorter than its slot (a partly written sector, or
			// the file's last sector): the rest of the slot reads as zeros, and
			// a later sector, if one exists, still lands at its own offset.
			fillZeros(pos + want)
		}
	}
	if lastByte >= fileSize {
		return n, io.EOF
	}
	f.vfs.logger.Debug("ReadAt", "off", off, "len(buffer)", len(p), "n", n)

	if n < len(p) {
		f.vfs.logger.Error("Read too little")
		return n, io.EOF
	}

	f.vfs.logger.Debug("ReadAt read all expected bytes")

	return n, nil
}

// WriteAt implements sqlite3vfs.File: it writes p at off, fetching each
// touched sector at most once and writing each changed sector exactly once
// (see writeSectorOnce). Sectors past the current end are created, so a
// write grows the file across as many 64 KiB sectors as it needs.
func (f *file) WriteAt(p []byte, off int64) (int, error) {
	f.vfs.logger.Debug("WriteAt", "len(p)", len(p), "off", off)

	if len(p) == 0 {
		return 0, nil
	}

	// Renew a due lease, and refuse to write at all once it is lost: the
	// write path used to run with no reference to the lock state, so a
	// write stretch longer than the lease kept writing sectors after
	// another process had pruned this holder and taken the lock.
	if err := f.renewForIO(); err != nil {
		return 0, ioFailure(err)
	}

	firstSector := f.sectorForPos(off)

	lastByte := off + int64(len(p)) - 1

	lastSector := f.sectorForPos(lastByte)

	var nW int
	for sectorIndex := firstSector; sectorIndex <= lastSector; sectorIndex++ {
		// Re-check the lease for every sector (issue #459): a multi-sector
		// write can outlive its entry gate — each sector write is its own
		// Kubernetes call on the construction context, with no timeout, so
		// one slow UPDATE on a large write can push the loop past the lease
		// expiry — and once another process has pruned this holder and
		// taken EXCLUSIVE, no further sector may be written under a lock
		// the cluster has given away. maybeRenew's local timestamp guard
		// keeps this check free for a live lease, exactly as at entry, and
		// the loss maps to BusyError so sqlite re-locks and revalidates.
		if err := f.renewForIO(); err != nil {
			return nW, ioFailure(err)
		}
		// Fetch each sector's current bytes at most once (a cache hit while
		// the lock is held, otherwise one GET); a sector that does not exist
		// yet is created by the write below rather than by the fetch. The
		// imported code fetched every sector up front via getSector, which
		// created missing sectors on the cluster and then panicked on the
		// nil response it got back — so no write could ever grow a file.
		data, exists, err := f.sectorDataForWrite(sectorIndex)
		if err != nil {
			return nW, sqlite3vfs.IOErrorRead
		}

		lastPossibleByteForThisSector := ((sectorIndex + 1) * SectorSize) - 1
		startByteForThisSector := sectorIndex * SectorSize

		currentOffset := off + int64(nW)
		var sectorData []byte
		if lastByte > lastPossibleByteForThisSector {
			sectorData = make([]byte, SectorSize)
		} else {
			// If there's existing data, ensure the buffer is large enough to hold it
			newlyRequiredSize := (1 + lastByte) - startByteForThisSector
			newlyRequiredSize = max(newlyRequiredSize, int64(len(data)))
			sectorData = make([]byte, newlyRequiredSize)
		}
		startOffset := currentOffset - startByteForThisSector
		// copy in existing data
		_ = copy(sectorData, data)
		// copy new data
		nn := copy(sectorData[startOffset:], p[nW:])
		// Write the sector exactly once: UPDATE when it is known to exist,
		// CREATE when it is known to be new; an unexpected conflict falls
		// back to the other verb (see writeSectorOnce).
		if err := f.writeSectorOnce(sectorIndex, sectorData, exists); err != nil {
			f.vfs.logger.Error("WriteAt: write sector", "sector", sectorIndex, "err", err)
			return nW, err
		}
		if f.valid {
			f.cacheSector(sectorIndex, sectorData)
			if end := sectorIndex*SectorSize + int64(len(sectorData)); end > f.size {
				f.size = end
			}
		}
		nW += nn
	}

	return nW, nil
}

// Sync implements sqlite3vfs.File: it no-ops, because sector writes reach
// the cluster directly.
func (f *file) Sync(flag sqlite3vfs.SyncType) error {
	f.vfs.logger.Debug("Sync", "flag", flag)

	return nil
}

// lockLevelFromString maps a stored lock level string to a
// sqlite3vfs.LockType, reporting false for an unknown value.
func lockLevelFromString(currentLockString string) (sqlite3vfs.LockType, bool) {
	switch currentLockString {
	case sqlite3vfs.LockNone.String():
		return sqlite3vfs.LockNone, true
	case sqlite3vfs.LockShared.String():
		return sqlite3vfs.LockShared, true
	case sqlite3vfs.LockPending.String():
		return sqlite3vfs.LockPending, true
	case sqlite3vfs.LockExclusive.String():
		return sqlite3vfs.LockExclusive, true
	case sqlite3vfs.LockReserved.String():
		return sqlite3vfs.LockReserved, true
	}
	return sqlite3vfs.LockNone, false
}

// writerEntry is the JSON stored under the lock ConfigMap's "writer" key:
// the one holder climbing past SHARED, its level and its lease. Level is
// stored spelled as sqlite3vfs.LockType.String() ("LockReserved" and so on)
// so the entry is legible in kubectl output.
type writerEntry struct {
	ID    string    `json:"id"`
	Level string    `json:"level"`
	Lease time.Time `json:"lease"`
}

// lockType parses the writer's stored level.
func (w *writerEntry) lockType() (sqlite3vfs.LockType, bool) {
	return lockLevelFromString(w.Level)
}

// lockView is a parsed view of the lock ConfigMap's holder state. All
// expiries are UTC; a state is always pruned of expired entries before any
// decision is made from it, so a holder whose lease has passed counts as
// absent — on reads it is simply not there, and a successful CAS drops it
// from the stored state.
type lockView struct {
	sharedHolders map[string]time.Time
	writer        *writerEntry
}

func emptyLockView() lockView {
	return lockView{sharedHolders: map[string]time.Time{}}
}

// parseLockState reads the holder/writer keys out of a lock ConfigMap's
// Data. Missing keys mean an empty holder state — a lockfile written by an
// older version of this module carries only the aggregate "lock" key. Keys
// that are present but not valid JSON are an error rather than silently
// ignored state: guessing could break the mutual exclusion the keys exist
// to provide.
func parseLockState(cm *corev1.ConfigMap) (lockView, error) {
	st := emptyLockView()
	if raw := cm.Data[sharedHoldersDataKey]; strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &st.sharedHolders); err != nil {
			return st, fmt.Errorf("parse %s of %q: %w", sharedHoldersDataKey, cm.Name, err)
		}
	}
	if st.sharedHolders == nil {
		st.sharedHolders = map[string]time.Time{}
	}
	if raw := cm.Data[writerDataKey]; strings.TrimSpace(raw) != "" {
		w := &writerEntry{}
		if err := json.Unmarshal([]byte(raw), w); err != nil {
			return st, fmt.Errorf("parse %s of %q: %w", writerDataKey, cm.Name, err)
		}
		st.writer = w
	}
	return st, nil
}

// pruneExpired drops every entry — shared holders and the writer — whose
// lease has passed. A holder that died mid-lock must never block the
// database: this runs before any decision on any read of the state, and a
// successful CAS writes the pruned state back.
func (st *lockView) pruneExpired(now time.Time) {
	for id, lease := range st.sharedHolders {
		if lease.Before(now) {
			delete(st.sharedHolders, id)
		}
	}
	if st.writer != nil && st.writer.Lease.Before(now) {
		st.writer = nil
	}
}

// levelOf returns the level holder holds in this state: SHARED while it has
// a shared-holders entry, raised to its writer level when it is also the
// writer.
func (st *lockView) levelOf(holderID string) sqlite3vfs.LockType {
	level := sqlite3vfs.LockNone
	if _, ok := st.sharedHolders[holderID]; ok {
		level = sqlite3vfs.LockShared
	}
	if st.writer != nil && st.writer.ID == holderID {
		if wl, ok := st.writer.lockType(); ok && wl > level {
			level = wl
		}
	}
	return level
}

// hasOtherHolders reports whether any holder other than except still holds
// a shared lock.
func (st *lockView) hasOtherHolders(except string) bool {
	for id := range st.sharedHolders {
		if id != except {
			return true
		}
	}
	return false
}

// aggregate returns the highest level any live holder holds — the value
// kept under the "lock" key so a reader of the old single-writer format
// still sees a meaningful level.
func (st *lockView) aggregate() sqlite3vfs.LockType {
	level := sqlite3vfs.LockNone
	if len(st.sharedHolders) > 0 {
		level = sqlite3vfs.LockShared
	}
	if st.writer != nil {
		if wl, ok := st.writer.lockType(); ok && wl > level {
			level = wl
		}
	}
	return level
}

// serializeLockState renders st for the ConfigMap's Data keys: the aggregate
// level string, the shared-holder lease map as JSON (always present, "{}"
// when empty) and the writer entry as JSON ("" when none).
func serializeLockState(st *lockView) (lockLevel, holdersJSON, writerJSON string) {
	holders, err := json.Marshal(st.sharedHolders)
	if err != nil {
		// map[string]time.Time marshalling cannot fail; nothing below it
		// can either. Panic rather than write a half-serialised state.
		panic(fmt.Sprintf("serialize shared-holders: %v", err))
	}
	writerJSON = ""
	if st.writer != nil {
		b, err := json.Marshal(st.writer)
		if err != nil {
			panic(fmt.Sprintf("serialize writer: %v", err))
		}
		writerJSON = string(b)
	}
	return st.aggregate().String(), string(holders), writerJSON
}

// newLockConfigMap builds the lock ConfigMap with the same labels and
// "lock"/"relevant-file" keys the single-writer setLock always wrote, plus
// the holder keys. The "relevant-file" LABEL carries the short form
// (relevantFileLabel): a label value is capped at 63 characters, while the
// "relevant-file" Data key keeps the full base32 name, a Data value having
// no such cap.
func (f *file) newLockConfigMap(lockLevel, holdersJSON, writerJSON string) *corev1.ConfigMap {
	fileNameLabel := string(f.b32ByteFromString(f.RawName))
	lockfileLabels := make(map[string]string, len(LockfileLabel)+1)
	maps.Copy(lockfileLabels, LockfileLabel)
	lockfileLabels["relevant-file"] = f.relevantFileLabel()

	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: f.LockFileName(), Labels: lockfileLabels},
		Data: map[string]string{
			lockLevelDataKey:     lockLevel,
			"relevant-file":      fileNameLabel,
			sharedHoldersDataKey: holdersJSON,
			writerDataKey:        writerJSON,
		},
	}
}

// stampOwnLease computes the fresh lease for every entry this file holds
// in st and writes it into the state, returning the expiry it stamped.
// casLock runs it after every successful mutation, which is what keeps a
// live holder's lease fresh: any CAS a file performs — transition, renewal
// or no-op that turned out to need a write — re-stamps its own entries.
//
// The return value is the expiry casLock must commit to f.leaseExpires once
// the mutation is known to have reached the cluster (issue #499). Stamping
// the local timer inside the mutate path — before the ConfigMap Update is
// known to have succeeded — recorded an extension the cluster never saw: a
// renewal CAS that exhausted its 409 retries, or failed on an API error,
// left the cluster keeping the old expiry while maybeRenew skipped renewal
// for another lockLease/2 on the strength of the phantom local stamp.
func (f *file) stampOwnLease(st *lockView) time.Time {
	lease := f.vfs.clock().UTC().Add(f.vfs.lockLease)
	if _, ok := st.sharedHolders[f.holderID]; ok {
		st.sharedHolders[f.holderID] = lease
	}
	if st.writer != nil && st.writer.ID == f.holderID {
		st.writer.Lease = lease
	}
	return lease
}

// casLock applies mutate to the lock ConfigMap's state with real mutual
// exclusion (issue #47): the state is read, the new state computed from
// what was read, and the Update carries the ResourceVersion just read — so
// two processes can never overwrite each other's holders. On a 409 Conflict
// the state is re-read and re-decided, never overwritten; a missing
// ConfigMap is Created, with AlreadyExists from the Create treated as
// "re-read". The loop is bounded by maxLockCASAttempts; exhaustion
// reports sqlite3vfs.BusyError — sustained contention is not an invalid
// request, and SQLITE_BUSY is the code sqlite retries through its busy
// handler, where a plain error would surface as SQLITE_ERROR and hard-fail
// the lock (issue #479).
//
// mutate reports sqlite3vfs.BusyError when another process holds a
// conflicting lock and a plain error for an invalid request; both are
// returned unwrapped, because the sqlite3vfs shim maps errors to SQLite
// codes with a bare type assertion and a wrapped BusyError would degrade to
// a generic error instead of SQLITE_BUSY.
//
// On every attempt that ends in a write, the leases of this file's
// surviving entries are refreshed first (stampOwnLease); a mutation that
// leaves the stored keys byte-identical writes nothing at all.
func (f *file) casLock(mutate func(*lockView) error) error {
	configMaps := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace)
	for range maxLockCASAttempts {
		ctx, cancel := f.vfs.lockCallCtx()
		cm, err := configMaps.Get(ctx, f.LockFileName(), metav1.GetOptions{})
		cancel()
		var st lockView
		exists := true
		switch {
		case kerrors.IsNotFound(err):
			exists = false
			st = emptyLockView()
		case err != nil:
			return fmt.Errorf("get lock configmap: %w", err)
		default:
			st, err = parseLockState(cm)
			if err != nil {
				return err
			}
			st.pruneExpired(f.vfs.clock().UTC())
		}

		if err := mutate(&st); err != nil {
			return err
		}
		lease := f.stampOwnLease(&st)

		lockLevel, holdersJSON, writerJSON := serializeLockState(&st)
		if exists {
			if cm.Data[lockLevelDataKey] == lockLevel &&
				cm.Data[sharedHoldersDataKey] == holdersJSON &&
				cm.Data[writerDataKey] == writerJSON {
				// Nothing changed: no write, and no API call beyond the Get.
				// The store already carries exactly the fresh expiry we
				// computed, so the local timer may adopt it (issue #499).
				f.leaseExpires = lease
				return nil
			}
			cm.Data[lockLevelDataKey] = lockLevel
			cm.Data[sharedHoldersDataKey] = holdersJSON
			cm.Data[writerDataKey] = writerJSON
			// cm still carries the ResourceVersion from the Get above: if
			// another process wrote in between, this Update is a 409 and
			// the loop re-reads instead of clobbering its holders. The
			// Update must not carry a caller-cancellable context either:
			// half a compare-and-swap is worse than none.
			ctx, cancel := f.vfs.lockCallCtx()
			_, err = configMaps.Update(ctx, cm, metav1.UpdateOptions{})
			cancel()
			if kerrors.IsConflict(err) || kerrors.IsNotFound(err) {
				// Lost the race, or the ConfigMap was deleted under us:
				// re-read and re-decide.
				continue
			}
			if err != nil {
				return fmt.Errorf("update lock configmap: %w", err)
			}
			// The Update landed: the store now carries the expiry we
			// stamped, so the local renewal timer may adopt it (issue
			// #499). Committing only here keeps a failed or exhausted CAS
			// from recording an extension the cluster never saw.
			f.leaseExpires = lease
		} else {
			cm = f.newLockConfigMap(lockLevel, holdersJSON, writerJSON)
			ctx, cancel := f.vfs.lockCallCtx()
			_, err = configMaps.Create(ctx, cm, metav1.CreateOptions{})
			cancel()
			if kerrors.IsAlreadyExists(err) {
				// Another process created it first: re-read.
				continue
			}
			if err != nil {
				return fmt.Errorf("create lock configmap: %w", err)
			}
			f.leaseExpires = lease
		}
		return nil
	}
	// The detail goes to the log, not the error: sqlite only sees the
	// class, and the class must be a bare BusyError (see above) so the
	// busy handler runs instead of the lock failing with SQLITE_ERROR.
	f.vfs.logger.Error("lock configmap did not settle", "name", f.LockFileName(), "attempts", maxLockCASAttempts)
	return sqlite3vfs.BusyError
}

// lockCallCtx returns the context for ONE lock-path Kubernetes call:
// derived from lockCtx, which no caller can cancel, with a fresh
// lockCallTimeout. See vfs.lockCtx for why the construction context must not
// flow into these calls.
func (v *vfs) lockCallCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(v.lockCtx, lockCallTimeout)
}

// errLockLeaseLost reports that this file's own lease expired and its
// entries were pruned by another process: whatever the local state
// believed, the cluster no longer holds this file's lock.
var errLockLeaseLost = errors.New("lock lease expired and was pruned by another process")

// noteLeaseLost rewrites a lease-loss into the local state it implies —
// this file holds nothing, and everything cached under the lost lock may
// have been rewritten by whoever took over — and returns the error as-is.
func (f *file) noteLeaseLost(err error) error {
	if errors.Is(err, errLockLeaseLost) {
		f.lockLevel = sqlite3vfs.LockNone
		f.lockKnown = true
		f.dropCaches()
		// The loss also gates the sector read/write path: renewForIO fails
		// every read and write with BusyError until a successful Lock
		// re-establishes this holder and adoptLockState clears the flag.
		f.leaseLost = true
	}
	return err
}

// maybeRenew renews this file's lease when less than half of it remains. It
// runs from the lock-path operations (Lock, Unlock, Close and
// CheckReservedLock) and, through renewForIO, from the sector read/write
// path; the guard below is a local timestamp check, so a lease with plenty
// left still costs those paths zero API calls. The renewal CAS is a no-op mutation: stampOwnLease
// makes the fresh expiry the only change, so a still-valid lease costs
// nothing and a due one costs one Get plus one Update. A renewal whose CAS
// finds this file's entries gone means the lease lapsed and another process
// pruned us; noteLeaseLost resets the local state, sticks the loss for the
// sector read/write path, and the loss is reported rather than pretending
// the lock still holds.
func (f *file) maybeRenew() error {
	if !f.lockKnown || f.lockLevel == sqlite3vfs.LockNone {
		return nil // this file holds nothing, so there is no lease to renew
	}
	if f.leaseExpires.Sub(f.vfs.clock()) >= f.vfs.lockLease/2 {
		return nil
	}
	return f.noteLeaseLost(f.casLock(func(st *lockView) error {
		if st.levelOf(f.holderID) == sqlite3vfs.LockNone {
			return errLockLeaseLost
		}
		return nil
	}))
}

// renewForIO renews this file's lease from the sector read/write path when
// less than half of it remains, and reports the sticky lease loss. It runs
// at entry to ReadAt/WriteAt/Truncate and again inside each of their sector
// loops (issue #459): a call whose sector work outlives the lease must not
// serve or write a single further sector. A read or
// write stretch longer than the lease used to outrun renewal entirely —
// renewal lived only on the lock path — so the lease lapsed, another process
// could prune this holder and take the lock, and WriteAt went on writing
// sectors regardless. The guard in maybeRenew is a local timestamp check, so
// the common case — a live lease — costs the read/write path zero API calls;
// only a due renewal CASes. A file that holds no lock at all (the cmd tools,
// which drive NewFile directly and never lock) stays exactly as ungated as
// it ever was.
func (f *file) renewForIO() error {
	if f.leaseLost {
		// The lease lapsed and the holder was pruned: whoever holds the lock
		// now may be rewriting everything this file cached or is about to
		// touch. Fail until sqlite re-locks (which revalidates the caches)
		// instead of serving or writing without the lock.
		return errLockLeaseLost
	}
	if !f.lockKnown || f.lockLevel == sqlite3vfs.LockNone {
		return nil // this file holds no lock, so there is no lease to renew
	}
	return f.maybeRenew()
}

// ioFailure maps a renewal failure on the sector read/write path to the
// error sqlite should see: a lost lease becomes sqlite3vfs.BusyError —
// SQLITE_BUSY, which aborts the transaction and sends sqlite back through
// Lock, where the state is re-established and every cache revalidated — and
// any other error (a failed Kubernetes call) propagates as-is.
func ioFailure(err error) error {
	if errors.Is(err, errLockLeaseLost) {
		return sqlite3vfs.BusyError
	}
	return err
}

// LockFileName returns the name of the ConfigMap holding this file's SQLite
// lock level: the base32-encoded file name plus LockFileNameSuffix.
func (f *file) LockFileName() string {
	localLockFileName := fmt.Sprintf("%s-%s", f.b32ByteFromString(f.RawName), LockFileNameSuffix)
	return localLockFileName
}

// generateSectorsLabels builds the labels every sector ConfigMap of this
// file carries: CommonSectorLabel plus this file's "relevant-file" label
// (relevantFileLabel — the base32 name while it fits a label value,
// otherwise the longNameLabelPrefix stand-in, issue #351). The selector
// built from these labels is the only thing that LISTs and deletes this
// file's sectors, so every one of them must carry exactly these.
func (f *file) generateSectorsLabels() {
	f.SectorLabels = make(map[string]string, len(CommonSectorLabel)+1)
	maps.Copy(f.SectorLabels, CommonSectorLabel)
	f.SectorLabels["relevant-file"] = f.relevantFileLabel()
}

// dropCaches discards the cached file size and sector contents. Everything
// cached is trusted only while this process holds a lock of SHARED or
// better; from the moment that stops being true the cache may not match the
// cluster, and the next FileSize revalidates it with one LIST after SHARED
// is re-acquired.
func (f *file) dropCaches() {
	f.valid = false
	f.sectors = nil
}

// adoptLockState records a lock transition this process just made, and
// drops the sector/size caches when the lock falls below SHARED: from that
// moment another process may modify the database, so everything cached
// under the lock is stale until the next FileSize revalidates it after
// SHARED is re-acquired.
func (f *file) adoptLockState(lock sqlite3vfs.LockType) {
	f.lockLevel = lock
	f.lockKnown = true
	// The entries this successful write left on the cluster expire at
	// now+lease; keep the local approximation in step so maybeRenew fires
	// at the right time.
	f.leaseExpires = f.vfs.clock().UTC().Add(f.vfs.lockLease)
	// A successful transition re-established this holder: the sector
	// read/write path may proceed again.
	f.leaseLost = false
	if lock < sqlite3vfs.LockShared {
		f.dropCaches()
	}
}

// Lock implements sqlite3vfs.File: it acquires or upgrades this file's lock
// through the lock ConfigMap, acting as this file's holder id. Only the
// transitions of the sqlite locking chart below are legal; sqlite never
// requests PENDING explicitly, and a file holding no lock cannot climb past
// SHARED in one step. A transition another process makes impossible — a
// conflicting writer, or other live holders for EXCLUSIVE — is reported as
// sqlite3vfs.BusyError (SQLITE_BUSY), never waited out.
//
// A lapsed lease is a different event, and is deliberately not a BusyError:
// when this file's own entries were pruned — the lease expired while it
// made no lock-path call — Lock returns the distinct errLockLeaseLost
// error after resetting the local state (see noteLeaseLost). SQLITE_BUSY
// would send sqlite's busy handler into retrying the very same request,
// but with the state reset that retry fails as an invalid transition, and
// a retry that did succeed would carry on over a database another process
// may already have rewritten — without the change-counter revalidation a
// fresh lock acquire performs. A plain error fails the statement instead:
// sqlite releases its locks, and the next transaction re-acquires from
// scratch, as a fresh holder, and revalidates every cache.
//
// Every decision that writes is made inside the CAS against the state just
// read (see casLock and the mutate* builders below); the local lockLevel
// only short-circuits no-ops and invalid requests that cannot require an
// API call. A file whose lease was pruned mid-transaction is therefore
// judged by the cluster, not by its stale local belief.
func (f *file) Lock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debug("Lock", "elock", elock)

	//    UNLOCKED -> SHARED
	//    SHARED -> RESERVED
	//    SHARED -> (PENDING) -> EXCLUSIVE
	//    RESERVED -> (PENDING) -> EXCLUSIVE
	//    PENDING -> EXCLUSIVE

	if f.lockKnown {
		// A request for the level held, or below it, is a silent no-op —
		// judged against this file's own level, which records only the
		// transitions this file performed itself.
		if elock <= f.lockLevel {
			return f.maybeRenew()
		}
		//  (1) We never move from unlocked to anything higher than shared lock.
		if f.lockLevel == sqlite3vfs.LockNone && elock > sqlite3vfs.LockShared {
			return errors.New("invalid lock transition requested")
		}
		//  (2) SQLite never explicitly requests a pending lock.
		if elock == sqlite3vfs.LockPending {
			return errors.New("invalid Lock() request for state pending")
		}
		//  (3) A shared lock is always held when a reserve lock is requested.
		if elock == sqlite3vfs.LockReserved && f.lockLevel != sqlite3vfs.LockShared {
			return errors.New("can only transition to Reserved lock from Shared lock")
		}
	}

	if elock == sqlite3vfs.LockExclusive {
		// Two CAS steps, exactly the chart's SHARED/RESERVED -> (PENDING) ->
		// EXCLUSIVE path: first claim (or promote) this file's writer entry
		// to PENDING — which alone blocks new SHARED acquisitions — then,
		// once no other live holder remains, subsume this file's shared
		// entry into the EXCLUSIVE write. While other holders remain the
		// file stays in PENDING and BusyError is reported; sqlite retries,
		// and each retry re-claims (renewing the lease as a side effect).
		if err := f.noteLeaseLost(f.casLock(f.mutateLockExclusiveClaim)); err != nil {
			return err
		}
		f.adoptLockState(sqlite3vfs.LockPending)
		if err := f.noteLeaseLost(f.casLock(f.mutateLockExclusiveAcquire)); err != nil {
			return err
		}
		f.adoptLockState(sqlite3vfs.LockExclusive)
		return nil
	}

	if err := f.noteLeaseLost(f.casLock(f.mutateLock(elock))); err != nil {
		return err
	}
	f.adoptLockState(elock)
	return nil
}

// mutateLock returns the CAS mutation for a single-step Lock transition
// (NONE->SHARED or SHARED->RESERVED). The decision is made from the state
// just read, so it holds even when this file's local belief is stale; the
// invalid-request errors match the ones Lock rejects locally. A state
// holding no entry for a file that believes it holds SHARED means its lease
// lapsed and another process pruned it: errLockLeaseLost makes Lock reset
// the local state instead of writing under a false belief.
func (f *file) mutateLock(elock sqlite3vfs.LockType) func(*lockView) error {
	return func(st *lockView) error {
		own := st.levelOf(f.holderID)
		if elock <= own {
			return nil // already held at or above elock: a renewal CAS at most
		}
		switch elock {
		case sqlite3vfs.LockPending:
			return errors.New("invalid Lock() request for state pending")
		case sqlite3vfs.LockShared:
			// NONE -> SHARED. Readers are allowed while a writer holds
			// RESERVED; PENDING and EXCLUSIVE block new readers.
			if st.writer != nil {
				if wl, ok := st.writer.lockType(); ok && wl >= sqlite3vfs.LockPending {
					return sqlite3vfs.BusyError
				}
			}
			st.sharedHolders[f.holderID] = time.Time{} // stamped by casLock
			return nil
		case sqlite3vfs.LockReserved:
			// SHARED -> RESERVED. At most one writer of any kind may exist.
			if own != sqlite3vfs.LockShared {
				if own == sqlite3vfs.LockNone && f.lockKnown && f.lockLevel == sqlite3vfs.LockShared {
					// The state just read no longer has this file's shared
					// entry although this file believes it holds SHARED: the
					// lease lapsed and another process pruned it. Report the
					// loss (mirroring mutateLockExclusiveAcquire) so
					// noteLeaseLost resets the local state and the sector
					// write path fails with BusyError, instead of a plain
					// "invalid transition" that leaves the file writing
					// under a lock the cluster has given away.
					return errLockLeaseLost
				}
				return errors.New("can only transition to Reserved lock from Shared lock")
			}
			if st.writer != nil {
				return sqlite3vfs.BusyError
			}
			st.writer = &writerEntry{ID: f.holderID, Level: sqlite3vfs.LockReserved.String()}
			return nil
		default:
			return errors.New("invalid lock transition requested")
		}
	}
}

// mutateLockExclusiveClaim is the first CAS step of Lock(EXCLUSIVE): claim
// the writer slot for this file at PENDING, promoting its own RESERVED
// writer in place. A writer held by another file is a conflict; this file
// already holding PENDING is a no-op (a retry after BusyError). A state
// holding no entry for this file, which believed it held SHARED or better,
// means the lease lapsed and another process pruned it: errLockLeaseLost
// makes Lock reset the local state instead of writing under a false belief.
func (f *file) mutateLockExclusiveClaim(st *lockView) error {
	own := st.levelOf(f.holderID)
	if sqlite3vfs.LockExclusive <= own {
		return nil // already exclusive: nothing to claim
	}
	if own == sqlite3vfs.LockNone {
		// The state just read no longer has any entry for this file. When
		// this file believed it held SHARED or better, that belief rests on
		// a lease that lapsed and was pruned by another process: report
		// errLockLeaseLost (mirroring mutateLockExclusiveAcquire, the
		// acquire step below) so noteLeaseLost resets the local state and
		// the sector write path fails with BusyError, instead of a plain
		// "invalid transition" that leaves the file writing under a lock
		// the cluster has given away. A file that never held the lock at
		// all is a genuinely invalid request and keeps the plain error.
		if f.lockKnown && f.lockLevel >= sqlite3vfs.LockShared {
			return errLockLeaseLost
		}
		return errors.New("invalid lock transition requested")
	}
	if st.writer == nil {
		st.writer = &writerEntry{ID: f.holderID, Level: sqlite3vfs.LockPending.String()}
		return nil
	}
	if st.writer.ID != f.holderID {
		return sqlite3vfs.BusyError
	}
	if wl, ok := st.writer.lockType(); ok && wl == sqlite3vfs.LockReserved {
		st.writer.Level = sqlite3vfs.LockPending.String()
	}
	return nil // ours and already PENDING
}

// mutateLockExclusiveAcquire is the second CAS step of Lock(EXCLUSIVE):
// from this file's own PENDING writer, take EXCLUSIVE once no other live
// holder remains; this file's shared entry is removed, subsumed by the
// EXCLUSIVE write. While another holder's entry exists the file stays in
// PENDING and BusyError is reported. A missing own writer means this
// file's PENDING claim was pruned (its lease lapsed): errLockLeaseLost
// makes Lock reset the local state instead of writing under a false belief.
func (f *file) mutateLockExclusiveAcquire(st *lockView) error {
	if st.writer == nil || st.writer.ID != f.holderID {
		return errLockLeaseLost
	}
	if st.hasOtherHolders(f.holderID) {
		return sqlite3vfs.BusyError // stay in PENDING
	}
	delete(st.sharedHolders, f.holderID)
	st.writer.Level = sqlite3vfs.LockExclusive.String()
	return nil
}

// Unlock implements sqlite3vfs.File: it drops or downgrades this file's
// lock through the lock ConfigMap, touching only this file's own entries.
// An unlock above SHARED (sqlite never unlocks upward) or to a level above
// the one currently held is an error, not a panic: the sqlite3vfs interface
// lets Unlock report an error, and a panic here crashed the whole process
// through cgo. Unlocking when this file holds nothing is a no-op downgrade
// — never an error, and never a write, so a file that holds nothing cannot
// clobber another process's holders.
func (f *file) Unlock(elock sqlite3vfs.LockType) error {
	f.vfs.logger.Debug("Unlock", "elock", elock)

	if elock > sqlite3vfs.LockShared {
		return fmt.Errorf("invalid unlock request to level %s", elock)
	}

	if f.lockKnown {
		if f.lockLevel < elock {
			return fmt.Errorf("cannot unlock to a level %s above the current lock level %s", elock, f.lockLevel)
		}
		if f.lockLevel == elock {
			return f.maybeRenew()
		}
		// Any downgrade invalidates the local view BEFORE the write is
		// attempted, whatever the write returns (issue #19). If the CAS
		// fails transiently, sqlite largely ignores the unlock error and
		// carries on, so the caches must already be gone by then: with the
		// lock state unknown, the next FileSize pays its revalidating LIST,
		// so writes another process made between our locks can never be
		// masked by stale cached sectors.
		f.dropCaches()
		f.lockKnown = false
	}

	if err := f.noteLeaseLost(f.casLock(f.mutateUnlock(elock))); err != nil {
		return err
	}
	f.adoptLockState(elock)
	return nil
}

// mutateUnlock returns the CAS mutation dropping this file's holdings to
// elock, judged against the holder-aware state just read: a missing own
// entry makes the request a no-op, whatever other processes hold.
func (f *file) mutateUnlock(elock sqlite3vfs.LockType) func(*lockView) error {
	return func(st *lockView) error {
		own := st.levelOf(f.holderID)
		if own < elock {
			// The state just read holds nothing for this file although this
			// file believed it held elock or more — its pre-CAS local level,
			// still in f.lockLevel because adoptLockState runs only on
			// success: the lease lapsed and another process pruned the
			// entries. Report errLockLeaseLost (mirroring the issue #412
			// guards on the Lock path) so noteLeaseLost resets the local
			// state and sticks the loss for the sector read/write path,
			// instead of the plain transition error that leaves
			// renewForIO's guards unset — lockKnown false, leaseLost false —
			// and WriteAt writing sectors under a lock the cluster has given
			// away. A file whose local level never reached elock never held
			// what it is unlocking: a genuinely invalid request keeps the
			// plain error.
			if f.lockLevel >= elock {
				return errLockLeaseLost
			}
			return fmt.Errorf("cannot unlock to a level %s above the current lock level %s", elock, own)
		}
		if own == elock {
			return nil // nothing held above elock: a renewal CAS at most
		}
		if elock == sqlite3vfs.LockShared {
			// Drop this file's writer, keep (or restore) its shared entry:
			// EXCLUSIVE had subsumed it, RESERVED and PENDING keep it.
			if st.writer != nil && st.writer.ID == f.holderID {
				st.writer = nil
			}
			if _, ok := st.sharedHolders[f.holderID]; !ok {
				st.sharedHolders[f.holderID] = time.Time{} // stamped by casLock
			}
			return nil
		}
		// To NONE: everything of this file's goes.
		delete(st.sharedHolders, f.holderID)
		if st.writer != nil && st.writer.ID == f.holderID {
			st.writer = nil
		}
		return nil
	}
}

// SectorSize implements sqlite3vfs.File: it reports the size of one stored
// sector, SectorSize (64 KiB).
func (f *file) SectorSize() int64 {
	return SectorSize
}

// DeviceCharacteristics implements sqlite3vfs.File: it advertises atomic
// 64 KiB writes, since one sector is one ConfigMap write, and safe appends.
//
// IocapSafeAppend is not cosmetic. SQLite's writeJournalHdr (pager.c) only
// writes the rollback journal's magic bytes at offset 0 when the database
// file advertises SQLITE_IOCAP_SAFE_APPEND (or runs no-sync / an in-memory
// journal); otherwise it deliberately writes ZEROS there, on the assumption
// that a crash could leave garbage appended to a partially grown file. A
// zeroed magic makes hasHotJournal (which reads that first byte and treats
// zero as "not a hot journal") skip its xCheckReservedLock call entirely, so
// a live writer's journal is never recognised as live and SQLite's whole
// hot-journal protocol is short-circuited — and the cgo workaround for the
// upstream CheckReservedLock inversion becomes unobservable through SQLite.
//
// Safe append is true here in the strongest sense: a sector never grows by
// appending to an existing object. Every sector is one ConfigMap, whole,
// written exactly once (UPDATE or CREATE), so a partial write is impossible
// and no garbage can ever follow the bytes a reader has already seen. This
// is the same guarantee the unix VFS reports for SQLITE_IOCAP_SAFE_APPEND.
func (f *file) DeviceCharacteristics() sqlite3vfs.DeviceCharacteristic {
	return sqlite3vfs.IocapAtomic64K | sqlite3vfs.IocapSafeAppend
}

// NewFile returns a file object for name backed by v. The cmd tools use it
// to read and write a database stored in the cluster without going through
// a sqlite connection. The file's holder id is generated here: every lock
// operation of this file acts as this holder, so two files — two processes
// — over the same database always hold distinguishable entries in the lock
// ConfigMap.
func NewFile(name string, v *vfs) *file {
	o := base32.NewEncoding("abcdefghijklmnopqrstuv0123456789")
	e := o.WithPadding('x')
	f := &file{RawName: name, vfs: v, encoding: e, holderID: uuid.NewString()}
	f.generateSectorsLabels()
	return f
}

// Open implements sqlite3vfs.VFS: it backs name with the cluster, creating
// the file's lock ConfigMap at LockNone and an empty sector 0 if the file
// has no sectors yet — unless sqlite3vfs.OpenReadOnly is set. A read-only
// open must leave no trace: for a name with no lock ConfigMap or no sectors
// it performs no Create and answers CantOpenError, the SQLITE_CANTOPEN a
// filesystem VFS gives for a missing file, instead of fabricating the
// lockfile plus empty sector 0 a read-write open would create. Every write
// path for a read-write open is unchanged. The lock level stored in the
// ConfigMap may belong to another process, so a file opened here starts at
// NONE locally and records only the transitions it performs itself (see
// file.lockLevel). Transient races between clients are retried up to the
// VFS's retry budget.
func (v *vfs) Open(name string, flags sqlite3vfs.OpenFlag) (sqlite3vfs.File, sqlite3vfs.OpenFlag, error) {
	v.logger.Debug("Open", "name", name, "flags", flags)

	if _, err := v.kc.Discovery().ServerVersionWithContext(v.ctx); err != nil {
		v.logger.Error("Open: server version", "err", err)
		return nil, flags, sqlite3vfs.IOError
	}

	// A read-only open never creates anything: the lock ConfigMap and the
	// fresh sector 0 are writes, and a reader must not mutate the store.
	readOnly := flags&sqlite3vfs.OpenReadOnly != 0

	var lastErr error
	for range v.retries {
		f := NewFile(name, v)

		// Now check for lock file
		_, err := v.kc.CoreV1().ConfigMaps(v.namespace).Get(v.ctx, f.LockFileName(), metav1.GetOptions{})
		switch {
		case kerrors.IsNotFound(err):
			if readOnly {
				// No lock ConfigMap means no file was ever created
				// here. Creating the lockfile (and the sector 0 the
				// read-write path adds below) would write to the
				// cluster on an open that promised not to; fail the
				// way sqlite expects for a missing file instead.
				v.logger.Debug("Open: read-only name has no lockfile", "name", name)
				return nil, flags, sqlite3vfs.CantOpenError
			}
			// Fresh lockfile: create it through the CAS so a lost race
			// against another opener's Create is re-read, not an error.
			// The no-op mutation leaves the state empty — this file starts
			// at NONE, holding nothing.
			if err := f.casLock(func(*lockView) error { return nil }); err != nil {
				v.logger.Error("Open: set initial lock state", "err", err)
				lastErr = err
				continue
			}
		case err != nil:
			// A transient failure of the lockfile GET itself (one 500 or
			// timeout) must not fail the whole open with a raw Kubernetes
			// error: every sibling failure path in this loop honours the
			// retry budget, and returning the file object alongside the
			// error is not something sqlite3vfs expects from a failed
			// xOpen. Retry, and fail with a nil file if the attempts run
			// out.
			v.logger.Error("Open: get lockfile", "err", err)
			lastErr = err
			continue
		default:
			// Every opened file starts at NONE locally. The ConfigMap's
			// stored level may be another process's lock, and adopting it
			// made this file believe it held that lock: its Lock calls
			// became no-ops and its Unlock wrote the level down under the
			// other process. This object records only the transitions it
			// performs itself, each one through the ConfigMap.
			f.lockLevel = sqlite3vfs.LockNone
			f.lockKnown = true
		}

		cms, err := f.listSectorConfigMaps()
		if err != nil {
			// Without the LIST we cannot tell a fresh file from an existing
			// one: treating it as fresh would write an empty sector 0 over
			// the first sector of an existing database. Retry, and fail if
			// the attempts run out.
			v.logger.Error("Open: list sector configmaps", "err", err)
			lastErr = err
			continue
		}
		names := make([]string, 0, len(cms))
		for _, cm := range cms {
			names = append(names, cm.Name)
		}

		v.logger.Debug("Checked for existing sectors", "sectors", names, "len(cms)", len(cms))

		if len(cms) == 0 {
			if readOnly {
				// The lock ConfigMap exists but there are no sectors:
				// no readable file. Fabricating the empty sector 0 is
				// a write the read-only open must not do — a read of
				// this name could not be served — so fail the same way
				// as for a fully missing name.
				v.logger.Debug("Open: read-only open of an empty file", "name", name)
				return nil, flags, sqlite3vfs.CantOpenError
			}
			// Fresh (or externally emptied) file: create the empty sector 0
			// that the size computation and reads expect. Known to be new,
			// so this is one CREATE — but with no overwrite fallback, unlike
			// the lock-gated write path: this file holds no lock here, and a
			// second client may have created sector 0 between our LIST and
			// our CREATE. Its bytes are then that client's page 1, so an
			// AlreadyExists answer means "re-read", the casLock rule (issue
			// #393): the re-LIST below must see their sector instead of this
			// open force-updating zero bytes over the first sector of a
			// database that now exists.
			if err := f.createSector0IfAbsent(); err != nil {
				if !kerrors.IsAlreadyExists(err) {
					v.logger.Error("Open: create initial empty sector", "err", err)
					lastErr = err
					continue
				}
				// Lost the fresh-file race. Re-read within this attempt, the
				// way casLock re-reads within its own bound: the continue
				// below spends one of v.retries outer attempts, and at the
				// shipped --retries 1 default there is no second attempt, so
				// the re-read must not depend on the retry budget.
				cms, err = f.listSectorConfigMaps()
				if err != nil {
					v.logger.Error("Open: list sector configmaps after lost fresh-file race", "err", err)
					lastErr = err
					continue
				}
				if len(cms) == 0 {
					// The race winner's sector 0 is gone again (deleted
					// between our CREATE and this LIST). There is no settled
					// state to open; report the race and let the retry budget
					// (if any) start over. lastErr must be set to a real error
					// here: err is the successful re-read LIST's nil, and a nil
					// lastErr falls through to the "no open attempt was made"
					// fallback below, which blames the retry budget instead of
					// the race that actually failed the open.
					lastErr = errors.New("sector 0 of the fresh-file race winner was deleted again before the re-read")
					continue
				}
				names = names[:0]
				for _, cm := range cms {
					names = append(names, cm.Name)
				}
				v.logger.Debug("Re-read sectors after lost fresh-file race", "sectors", names, "len(cms)", len(cms))
			}
		}
		v.logger.Debug("Opened file successfully", "name", name, "flags", flags)

		return f, flags, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no open attempt was made (retries must be positive)")
	}
	v.logger.Debug("Failed to open file")

	// A deliberate sqlite3vfs error class recorded by an attempt — the CAS
	// exhaustion's BusyError — is returned bare: wrapping it would degrade
	// SQLITE_BUSY to a generic SQLITE_ERROR under the cgo shim's bare type
	// assertion, and SQLITE_BUSY names the truth (the lock could not be
	// settled) where SQLITE_ERROR claims malformed state. xOpen never runs
	// sqlite's busy handler either way, so surfacing the class loses
	// nothing (issue #479).
	if errors.Is(lastErr, sqlite3vfs.BusyError) {
		return nil, flags, sqlite3vfs.BusyError
	}
	return nil, flags, fmt.Errorf("open %q: file metadata did not settle after %d attempts: %w", name, v.retries, lastErr)
}

// Delete implements sqlite3vfs.VFS: it removes every sector ConfigMap of
// the named file and then its lock ConfigMap, retrying transient races up
// to the VFS's retry budget. A Delete that reports NotFound is not an
// error: the object is in the state this call wanted, so a half-deleted
// file left by an earlier crash converges to fully deleted on the next
// attempt, and SQLite's ordinary "delete the journal I never created" (and
// the "I already deleted it" race between two connections) succeed.
//
// Deleting a file is several Kubernetes calls, so a crash at any point
// leaves a half-state. For a rollback journal the half-states matter,
// because SQLite decides whether a journal must be played back from two
// observable facts: xAccess (here: does the name have at least one sector
// ConfigMap?) and the first byte read at offset 0 (here: the first byte of
// sector 0; a missing sector reads as zeros). SQLite's hasHotJournal skips
// a journal whose first byte is zero, and treats a journal it can access
// but not read a non-zero first byte from as cold. So the order below is
// chosen to make every prefix of it a state SQLite reads as "not hot":
//
//  1. sector 0 (the journal header) FIRST. Writing a header is what made
//     the journal hot in the first place, and the header is the last sector
//     a hot journal ever needs, so removing it first means no prefix of
//     this delete can leave a journal that is still hot. Before the call
//     the journal is intact — a complete, replayable journal is a safe
//     state (that is what crash recovery is for). After it, the first byte
//     at offset 0 reads as zero: SQLite sees a cold journal.
//  2. the remaining sectors, ascending. Each one is NotFound-tolerant, so
//     leftovers from an earlier crashed delete are collected too. While any
//     of them remains, Access still answers true — the name "exists" — so
//     SQLite may still open the journal, but with no header it cannot
//     replay anything: it sees a cold journal and (for a read-only or
//     recovery open) ignores it until some later delete or truncate clears
//     the rest. Leaving re-playable bytes behind is impossible; leaving
//     unreachable sectors behind is merely untidy.
//  3. the lockfile LAST. Access answers from the sector count, so the name
//     stops existing exactly when its last sector goes — after the header
//     is already gone. Deleting the lockfile first would instead let
//     another opener create a fresh journal and lockfile for this name
//     (a fresh transaction) only for this delete to delete its sectors.
//
// A non-journal name keeps the historical order — every sector, then the
// lockfile. Header-first is not safer there: no SQLite hot/cold decision
// reads sector 0 of a database file, so the order only changes which
// partial state a crash exposes, and deleting the database's own sector 0
// first would expose a header-less database for longer. None of the states
// this delete passes through is a valid database, so recovery from a crash
// mid-delete is the caller's job, not an ordering this loop can fix.
func (v *vfs) Delete(name string, dirSync bool) error {
	v.logger.Debug("Delete", "name", name, "dirSync", dirSync)
	// in case we're racing another client
	f := NewFile(name, v)
	isJournal := strings.HasSuffix(name, "-journal")
	var lastErr error
	for range f.vfs.retries + 1 {
		v.logger.Debug("Deleting configmaps representing this filename", "name", name)
		cms, err := f.listSectorConfigMaps()
		if err != nil {
			// Without the LIST the sectors cannot be found; deleting only
			// the lockfile would leave them orphaned on the cluster.
			v.logger.Error("Delete: list sector configmaps", "err", err)
			lastErr = err
			continue
		}

		if !deleteSectorsInSafeOrder(v, f, cms, isJournal) {
			// A sector DELETE failed for a reason other than NotFound:
			// another round re-LISTs and picks up whatever survived, the
			// header first again.
			continue
		}

		v.logger.Debug("Deleting lockfile for this filename", "name", name)
		err = v.kc.CoreV1().ConfigMaps(v.namespace).Delete(v.ctx, f.LockFileName(), metav1.DeleteOptions{})
		if err == nil || kerrors.IsNotFound(err) {
			return nil
		}
		v.logger.Error("Delete: delete lockfile", "err", err)
		lastErr = err
	}
	v.logger.Error("Failed to delete file", "filename", name, "dirSync", dirSync, "err", lastErr)
	return sqlite3vfs.IOError
}

// deleteSectorsInSafeOrder deletes the sector ConfigMaps of one file and
// reports whether every one of them is gone.
//
// The order is the heart of crash-safe journal deletion (see Delete): for a
// rollback journal, sector 0 — the header SQLite reads to decide whether the
// journal is hot — goes FIRST, and the rest follow in ascending index order.
// For any other file the order is the order the LIST returned, which is the
// historical behaviour.
//
// It tolerates NotFound: that sector is already in the wanted state, so the
// delete continues rather than reporting an error. Once the header sector is
// gone, no crash anywhere in this loop can leave a journal SQLite would try
// to replay: at every intermediate step the first byte at offset 0 reads as
// zero (see Access and file.ReadAt), so hasHotJournal calls the journal cold.
// The leftover sectors are garbage that the next delete (this one's retry,
// or SQLite's next xDelete of the name) or a truncate collects.
func deleteSectorsInSafeOrder(v *vfs, f *file, cms []corev1.ConfigMap, journalHeaderFirst bool) bool {
	ordered := make([]corev1.ConfigMap, 0, len(cms))
	if journalHeaderFirst {
		// Header first, then everything else in ascending sector order, so
		// a resumable retry makes monotonic progress through the leftovers
		// of a crashed attempt.
		ordered = append(ordered, cms...)
		sort.SliceStable(ordered, func(i, j int) bool {
			return sectorIndexLess(f, ordered[i].Name, ordered[j].Name)
		})
		if len(ordered) > 0 {
			if idx, ok := f.sectorIndexFromConfigMapName(ordered[0].Name); ok && idx != 0 {
				// The header sector is not in the LIST: it is already gone,
				// so this journal is already cold and the rest is junk.
				v.logger.Debug("Delete: journal header sector already gone", "name", f.RawName)
			}
		}
	} else {
		ordered = append(ordered, cms...)
	}

	aDeleteFailed := false
	for i := range ordered {
		if err := v.kc.CoreV1().ConfigMaps(v.namespace).Delete(v.ctx, ordered[i].Name, metav1.DeleteOptions{}); err != nil {
			if kerrors.IsNotFound(err) {
				// Already deleted (an earlier crash, or the retry after one):
				// the wanted state holds for this sector.
				v.logger.Debug("Delete: sector configmap already gone", "configmap", ordered[i].Name)
				continue
			}
			v.logger.Error("Delete: delete sector configmap", "configmap", ordered[i].Name, "err", err)
			aDeleteFailed = true
			continue
		}
		v.logger.Debug("Deleted configmap", "configmap", ordered[i].Name)
	}

	return !aDeleteFailed
}

// sectorIndexLess orders two sector ConfigMap names by sector index, with an
// unparsable name last. Sector indices are unbounded in principle and the
// base32 file name contains no '-', so the index is parsed out of the name
// rather than compared as a string (which would order "10" before "2").
func sectorIndexLess(f *file, a, b string) bool {
	ia, oka := f.sectorIndexFromConfigMapName(a)
	ib, okb := f.sectorIndexFromConfigMapName(b)
	switch {
	case oka && okb:
		return ia < ib
	case oka:
		return true
	default:
		return false
	}
}

// Access implements sqlite3vfs.VFS. It reports whether the named file
// exists in the cluster, so SQLite can tell a rollback journal it must
// recover from a journal it has yet to create.
//
// This used to be a blanket "sqlite side files never exist": every name
// ending in "-wal" or "-journal" answered false. That was a lie for the
// rollback journal, and it broke crash recovery: on the first read after a
// crash SQLite calls xAccess on <db>-journal to decide whether a hot
// journal is present. A false answer tells it there is nothing to roll
// back, so the half-written transaction is accepted as committed — data
// corruption — even though the journal's ConfigMaps are sitting in the
// cluster. A real filesystem answers from the directory, and so does this
// one now: the "-journal" name exists exactly when it has at least one
// sector ConfigMap.
//
// The "-wal" answer stays false: this VFS has no WAL support, so a WAL never
// exists here and saying otherwise would send SQLite down a path this VFS
// cannot serve. Every other name keeps answering true, as before.
func (v *vfs) Access(name string, flags sqlite3vfs.AccessFlag) (bool, error) {
	v.logger.Debug("Access", "name", name, "flags", flags)
	if strings.HasSuffix(name, "-wal") {
		return false, nil
	}
	if strings.HasSuffix(name, "-journal") {
		// Cluster truth: LIST this exact journal name's sector ConfigMaps
		// and report existence from the count, exactly as a filesystem
		// would report from its directory entry.
		f := NewFile(name, v)
		cms, err := f.listSectorConfigMaps()
		if err != nil {
			v.logger.Error("Access: list journal sector configmaps", "name", name, "err", err)

			return false, err
		}

		return len(cms) > 0, nil
	}

	return true, nil
}

// FullPathname implements sqlite3vfs.VFS. Although file names are already
// cluster-wide — there is no on-disk hierarchy to resolve them against —
// SQLite reaches xFullPathname on every open and hands through whatever
// spelling the caller used, so an uncleaned name (./app.db, dir/../app.db)
// would otherwise reach the VFS verbatim and map to a different ConfigMap
// set than the cleaned spelling, splitting one database in two on the
// cluster. The rule implemented here: clean the name and normalise
// separators, while absolute names pass through unchanged (SQLite appends
// -journal itself after canonicalisation, so cleaning the base name
// suffices).
func (v *vfs) FullPathname(name string) string {
	return filepath.ToSlash(filepath.Clean(name))
}

// CheckReservedLock implements sqlite3vfs.File — with a DELIBERATELY
// INVERTED bool, working around a bug in the dependency at the cgo boundary.
//
// The bug: upstream github.com/psanford/sqlite3vfs (sqlite3vfscgo.go,
// goVFSCheckReservedLock, at the pinned version
// v0.0.0-20260519004904-f9180fa2acc9) writes *pResOut = 0 when the File
// answers true and 1 when it answers false — the opposite of SQLite's
// xCheckReservedLock contract, which is "set *pResOut to a non-zero value if
// a reserved lock exists". SQLite consumes that value in hasHotJournal to
// decide whether an existing rollback journal belongs to a live writer
// (reserved lock held → live → leave it alone) or is a stale crash remnant
// (nobody holds reserved → hot → play it back); with the uncorrected
// inversion SQLite reads or deletes a live writer's journal mid-write.
//
// The workaround: negate here, at the last Go code the answer passes
// through, so the value that reaches SQLite is correct. Remove this
// negation — and the mirrored expectations in the tests — once upstream maps
// the File's answer straight through. TestCheckReservedLockCrossesCGOBoundary
// drives the answer through real SQLite across the cgo boundary, and the
// README's "Known upstream bug" section documents it for users.
func (f *file) CheckReservedLock() (bool, error) {
	locked, err := f.reservedHeld()
	if err != nil {
		return false, err
	}
	// Upstream maps a true answer to *pResOut = 0 (see the bug above), so
	// it is the NEGATED answer that must reach the shim: a held reserved
	// lock must leave this method as false, a free one as true.
	return !locked, nil
}

// reservedHeld reports the true answer to "does anyone — this file included
// — hold RESERVED or above?", the question the inverted CheckReservedLock
// above forwards. When this file itself holds a writer-level lock the answer
// is known locally and costs no API call (beyond a due lease renewal);
// otherwise the lock ConfigMap is consulted and the answer reports whether
// the writer entry is non-empty after pruning: expired entries count as
// absent on this read and are not rewritten.
func (f *file) reservedHeld() (bool, error) {
	if f.lockKnown && f.lockLevel >= sqlite3vfs.LockReserved {
		if err := f.maybeRenew(); err != nil {
			return false, err
		}
		return true, nil
	}

	ctx, cancel := f.vfs.lockCallCtx()
	cm, err := f.vfs.kc.CoreV1().ConfigMaps(f.vfs.namespace).Get(ctx, f.LockFileName(), metav1.GetOptions{})
	cancel()
	if kerrors.IsNotFound(err) {
		// No lockfile: nobody holds anything.
		return false, nil
	}
	if err != nil {
		f.vfs.logger.Error("CheckReservedLock", "err", err)
		return false, fmt.Errorf("get lock configmap: %w", err)
	}
	st, err := parseLockState(cm)
	if err != nil {
		return false, err
	}
	st.pruneExpired(f.vfs.clock().UTC())
	return st.writer != nil, nil
}
