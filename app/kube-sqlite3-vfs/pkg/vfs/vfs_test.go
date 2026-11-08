package vfs

// Behaviour tests for the kube VFS, run entirely against the fake kubernetes
// clientset so no cluster is needed.
//
// These tests pin the optimised behaviour of issue #8: writes grow a file
// across any number of 64 KiB sectors (WriteAt fetches each sector at most
// once and writes each changed sector exactly once), reads never create
// missing sectors and treat holes as short reads/EOF, FileSize is O(1) from
// memory while a SHARED-or-better lock is held (and otherwise costs one
// revalidating LIST that also warms the sector cache), and lock state is
// tracked locally rather than re-GET per transition.
//
// Where a test wants to start from a specific multi-sector layout it seeds
// the sector ConfigMaps directly into the fake clientset; growth is pinned
// by TestWriteAtGrowIntoNewSectorsRoundTrip, and the multi-sector path is
// also exercised through real SQLite at 64 KiB pages (the cmd tools'
// configuration) by TestSQLiteMultiSector64KiBPages.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/psanford/sqlite3vfs"
	v1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	k8stesting "k8s.io/client-go/testing"
)

const testNamespace = "vfs-tests"

var (
	vfsNameCounter uint64
	dbFileCounter  uint64
	sectorSize     = int64(SectorSize)
	multiSectorLen = 70000 // spans two 64 KiB sectors
)

// newTestVFS returns a vfs backed by an in-memory fake clientset.
func newTestVFS(tb testing.TB, opts ...Option) (*vfs, *fake.Clientset) {
	tb.Helper()
	fcs := fake.NewSimpleClientset()
	return NewVFS(tb.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, opts...), fcs
}

// openTestFile opens name through the VFS itself, as sqlite would, with
// zero open flags — this helper's historical default. Zero carries no
// read-only bit, so Open takes the read-write path; tests that need a
// read-only open use openTestFileWithFlags instead.
func openTestFile(tb testing.TB, v *vfs, name string) *file {
	tb.Helper()
	return openTestFileWithFlags(tb, v, name, 0)
}

// openTestFileWithFlags is openTestFile for callers that must pick the open
// flags themselves — read-write or read-only — as sqlite does for mode=ro.
func openTestFileWithFlags(tb testing.TB, v *vfs, name string, flags sqlite3vfs.OpenFlag) *file {
	tb.Helper()
	sf, _, err := v.Open(name, flags)
	if err != nil {
		tb.Fatalf("Open(%q, flags=%d) failed: %v", name, flags, err)
	}
	f, ok := sf.(*file)
	if !ok {
		tb.Fatalf("Open(%q) returned %T, want *file", name, sf)
	}
	return f
}

// registerUniqueVFS registers the VFS under a process-unique name:
// sqlite3vfs.RegisterVFS is process-global, so every test needs its own.
// It accepts any sqlite3vfs.VFS so test wrappers (such as the
// journal-visible one in vfs_concurrent_test.go) can register too.
func registerUniqueVFS(tb testing.TB, v sqlite3vfs.VFS) string {
	tb.Helper()
	name := fmt.Sprintf("kube-vfs-test-%d", atomic.AddUint64(&vfsNameCounter, 1))
	if err := sqlite3vfs.RegisterVFS(name, v); err != nil {
		tb.Fatalf("RegisterVFS(%q) failed: %v", name, err)
	}
	return name
}

func newSQLFileName() string {
	return fmt.Sprintf("sql-test-%d.db", atomic.AddUint64(&dbFileCounter, 1))
}

// openSQLDB opens a sqlite database backed by the named registered VFS,
// mirroring the DSN the module's own main uses (in-memory journal).
func openSQLDB(tb testing.TB, vfsName, fileName string) *sql.DB {
	tb.Helper()
	dsn := fmt.Sprintf("file:%s?_journal=MEMORY&vfs=%s", fileName, vfsName)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		tb.Fatalf("sql.Open(%q) failed: %v", dsn, err)
	}
	// One connection at a time, as the module's main does: the VFS has no
	// real multi-connection locking story.
	db.SetMaxOpenConns(1)
	return db
}

// openSQLDBWithJournal is openSQLDB with the journal mode of the DSN chosen
// by the caller ("MEMORY", "DELETE", ...) instead of hardcoded. It exists so
// benchmarks and tests that need a journal stored through the VFS can have
// one without changing openSQLDB's DSN, and with it the numbers the existing
// in-memory-journal benchmarks report.
func openSQLDBWithJournal(tb testing.TB, vfsName, fileName, journalMode string) *sql.DB {
	tb.Helper()
	dsn := fmt.Sprintf("file:%s?_journal=%s&vfs=%s", fileName, journalMode, vfsName)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		tb.Fatalf("sql.Open(%q) failed: %v", dsn, err)
	}
	// One connection at a time, as the module's main does: the VFS has no
	// real multi-connection locking story.
	db.SetMaxOpenConns(1)
	return db
}

func fillPattern(p []byte) {
	for i := range p {
		p[i] = byte(i % 251)
	}
}

// fillPatternAlt fills with a different repeating pattern than fillPattern.
func fillPatternAlt(p []byte) {
	for i := range p {
		p[i] = byte((i + 17) % 251)
	}
}

// seedSector writes a sector ConfigMap into the fake clientset directly,
// with exactly the name, labels and keys the VFS's own WriteSector uses.
// This stands in for state written by another process, and lets tests build
// multi-sector files despite the write-grow panic described at the top.
func seedSector(tb testing.TB, fcs *fake.Clientset, f *file, index int64, data []byte) {
	tb.Helper()
	cm := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      f.sectorNameFromSectorIndex(index),
			Namespace: testNamespace,
			Labels:    f.SectorLabels,
		},
		BinaryData: map[string][]byte{"sector": data},
		Data:       map[string]string{"filename": f.RawName},
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Create(tb.Context(), cm, metav1.CreateOptions{}); err != nil {
		if !kerrors.IsAlreadyExists(err) {
			tb.Fatalf("seed sector %d: %v", index, err)
		}
		// Open() pre-creates an empty sector 0; overwrite it.
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Update(tb.Context(), cm, metav1.UpdateOptions{}); err != nil {
			tb.Fatalf("seed (update) sector %d: %v", index, err)
		}
	}
}

// seedMultiSectorFile seeds sectors 0 and 1 holding the first n bytes of a
// fillPattern buffer.
func seedMultiSectorFile(tb testing.TB, fcs *fake.Clientset, f *file, n int) []byte {
	tb.Helper()
	data := make([]byte, n)
	fillPattern(data)
	seedSector(tb, fcs, f, 0, data[:SectorSize])
	if n > int(SectorSize) {
		seedSector(tb, fcs, f, 1, data[SectorSize:])
	}
	return data
}

func listSectorConfigMaps(tb testing.TB, fcs *fake.Clientset, f *file) []v1.ConfigMap {
	tb.Helper()
	cms, err := fcs.CoreV1().ConfigMaps(testNamespace).List(tb.Context(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(f.SectorLabels).String(),
	})
	if err != nil {
		tb.Fatalf("list sector configmaps: %v", err)
	}
	return cms.Items
}

func getLockfileConfigMap(tb testing.TB, fcs *fake.Clientset, f *file) *v1.ConfigMap {
	tb.Helper()
	cm, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(tb.Context(), f.LockFileName(), metav1.GetOptions{})
	if err != nil {
		tb.Fatalf("get lockfile %q: %v", f.LockFileName(), err)
	}
	return cm
}

func lockState(tb testing.TB, fcs *fake.Clientset, f *file) string {
	tb.Helper()
	return getLockfileConfigMap(tb, fcs, f).Data["lock"]
}

// sharedHoldersIn parses the lockfile ConfigMap's shared-holders JSON:
// holder id -> lease expiry.
func sharedHoldersIn(tb testing.TB, fcs *fake.Clientset, f *file) map[string]time.Time {
	tb.Helper()
	raw := getLockfileConfigMap(tb, fcs, f).Data["shared-holders"]
	if strings.TrimSpace(raw) == "" {
		return map[string]time.Time{}
	}
	holders := map[string]time.Time{}
	if err := json.Unmarshal([]byte(raw), &holders); err != nil {
		tb.Fatalf("parse shared-holders %q: %v", raw, err)
	}
	return holders
}

// writerIn parses the lockfile ConfigMap's writer JSON; nil when empty.
func writerIn(tb testing.TB, fcs *fake.Clientset, f *file) *writerEntry {
	tb.Helper()
	raw := getLockfileConfigMap(tb, fcs, f).Data["writer"]
	if raw == "" {
		return nil
	}
	w := &writerEntry{}
	if err := json.Unmarshal([]byte(raw), w); err != nil {
		tb.Fatalf("parse writer %q: %v", raw, err)
	}
	return w
}

// pruneHolderFromLockCM simulates another process pruning this file's lock
// entries: it rewrites the shared-holders map and the writer slot of the
// lock ConfigMap directly, dropping this file's own entries wherever
// asked. The fake clientset's Update keeps the ResourceVersion, so this
// file's next CAS reads exactly the pruned state a real pruneExpired by
// another process would have left behind.
func pruneHolderFromLockCM(tb testing.TB, fcs *fake.Clientset, f *file, shared, writer bool) {
	tb.Helper()
	cm := getLockfileConfigMap(tb, fcs, f)
	if shared {
		holders := sharedHoldersIn(tb, fcs, f)
		delete(holders, f.holderID)
		raw, err := json.Marshal(holders)
		if err != nil {
			tb.Fatalf("marshal pruned shared-holders: %v", err)
		}
		cm.Data["shared-holders"] = string(raw)
	}
	if writer {
		cm.Data["writer"] = ""
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Update(tb.Context(), cm, metav1.UpdateOptions{}); err != nil {
		tb.Fatalf("prune this file's entries in the lock configmap: %v", err)
	}
}

func mustWriteAt(tb testing.TB, f *file, p []byte, off int64) int {
	tb.Helper()
	n, err := f.WriteAt(p, off)
	if err != nil {
		tb.Fatalf("WriteAt(len=%d, off=%d) failed: %v", len(p), off, err)
	}
	if n != len(p) {
		tb.Fatalf("WriteAt(len=%d, off=%d) wrote %d bytes", len(p), off, n)
	}
	return n
}

func mustReadAt(tb testing.TB, f *file, p []byte, off int64) int {
	tb.Helper()
	n, err := f.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		tb.Fatalf("ReadAt(len=%d, off=%d) failed: %v", len(p), off, err)
	}
	return n
}

// --- Open ---

func TestOpenFreshFileCreatesLockfileAndEmptySector(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "fresh.db")

	lf := getLockfileConfigMap(t, fcs, f)
	if got := lf.Data["lock"]; got != sqlite3vfs.LockNone.String() {
		t.Errorf("fresh lockfile lock = %q, want %q", got, sqlite3vfs.LockNone.String())
	}
	if got := lf.Data["relevant-file"]; got != string(f.b32ByteFromString("fresh.db")) {
		t.Errorf("lockfile relevant-file = %q, want the base32 file label", got)
	}
	// While the encoded name fits the 63-character label-value cap
	// (issue #351) the LABEL is the encoded name too, identical to the
	// Data key above; only an over-long name would be shortened here.
	if got := lf.Labels["relevant-file"]; got != string(f.b32ByteFromString("fresh.db")) {
		t.Errorf("lockfile label relevant-file = %q, want the base32 file label", got)
	}
	if got := lf.Data["shared-holders"]; got != "{}" {
		t.Errorf("fresh lockfile shared-holders = %q, want %q (an opened file holds nothing)", got, "{}")
	}
	if got := lf.Data["writer"]; got != "" {
		t.Errorf("fresh lockfile writer = %q, want empty (an opened file holds nothing)", got)
	}
	if got := lf.Labels["data"]; got != "lockfile" {
		t.Errorf("fresh lockfile data label = %q, want lockfile", got)
	}

	sectors := listSectorConfigMaps(t, fcs, f)
	if len(sectors) != 1 {
		t.Fatalf("fresh file has %d sector configmaps, want exactly 1", len(sectors))
	}
	if got := sectors[0].BinaryData["sector"]; len(got) != 0 {
		t.Errorf("fresh sector 0 holds %d bytes, want empty", len(got))
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize on fresh file: %v", err)
	}
	if size != 0 {
		t.Errorf("FileSize on fresh file = %d, want 0", size)
	}
}

// TestOpenLostFreshFileRaceKeepsConcurrentWritersSector0 is the regression
// test for issue #393. Open decides a file is fresh from a zero-sector LIST
// and then CREATEs an empty sector 0. A second client that created sector 0
// between that LIST and that CREATE owns the real page 1 — the 64-byte
// database header, or the journal header SQLite reads hot-or-cold from — so
// the CREATE may only fail with AlreadyExists, never be turned into an
// unconditional Update of zero bytes over their data. The reactor below
// reproduces exactly that interleaving: it lets the competing writer create
// sector 0 and then answers our CREATE with the API server's AlreadyExists.
//
// The fix is the casLock rule applied to the open: AlreadyExists means
// "re-read", so the open re-lists the sectors and sees the other writer's
// sector, whose header is untouched. Before the fix Open returned success
// while sector 0 read back as zero bytes.
//
// The re-read runs inside the open attempt rather than as another pass of
// the outer retry loop: Open loops "for range v.retries" and the shipped
// --retries default is 1 (main.go), so a re-read that cost an outer attempt
// would be unreachable in production — every lost race would end in
// "file metadata did not settle after 1 attempts". The test therefore runs
// at retries=1 (the shipped default) as well as at the test suite's usual 3.
func TestOpenLostFreshFileRaceKeepsConcurrentWritersSector0(t *testing.T) {
	name := "race.db"
	for _, retries := range []int{1, 3} {
		t.Run(fmt.Sprintf("retries=%d", retries), func(t *testing.T) {
			fcs := fake.NewSimpleClientset()
			v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), retries)
			f := NewFile(name, v)
			sector0 := f.sectorNameFromSectorIndex(0)

			// The 64-byte SQLite header another writer's page 1 holds.
			header := make([]byte, 64)
			copy(header, "SQLite format 3\x00")
			header[16], header[17] = 0x10, 0x00 // 4096-byte pages

			// Write the competitor's sector 0 as a side effect of our CREATE and
			// fail that CREATE the way the API server does.
			var creates, injected int32
			fcs.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
				ca, ok := action.(k8stesting.CreateAction)
				if !ok {
					return false, nil, nil
				}
				cm, ok := ca.GetObject().(*v1.ConfigMap)
				if !ok || cm.Name != sector0 {
					return false, nil, nil
				}
				atomic.AddInt32(&creates, 1)
				if atomic.LoadInt32(&injected) > 0 {
					return false, nil, nil
				}
				atomic.AddInt32(&injected, 1)
				competitor := f.sectorConfigMap(0, header)
				competitor.ResourceVersion = ""
				if err := fcs.Tracker().Create(configMapGVR, competitor, testNamespace); err != nil {
					t.Fatalf("seed competitor sector 0: %v", err)
				}
				return true, nil, kerrors.NewAlreadyExists(v1.Resource("configmaps"), sector0)
			})

			sf, _, err := v.Open(name, 0)
			if err != nil {
				t.Fatalf("Open(%q) under a lost fresh-file race = %v, want nil (AlreadyExists is re-read)", name, err)
			}
			if injected != 1 {
				t.Errorf("injected AlreadyExists count = %d, want exactly 1", injected)
			}
			if creates != 1 {
				t.Errorf("sector-0 CREATE attempts = %d, want 1 (the retry finds the sector in its LIST)", creates)
			}

			// The other writer's bytes must still be there: no Update may ever have
			// replaced them with zero bytes.
			for _, a := range fcs.Actions() {
				if a.GetVerb() != "update" || a.GetResource().Resource != "configmaps" {
					continue
				}
				ua, ok := a.(k8stesting.UpdateAction)
				if !ok {
					continue
				}
				if cm, ok := ua.GetObject().(*v1.ConfigMap); ok && cm.Name == sector0 {
					t.Errorf("sector 0 was UPDATEd after the lost CREATE race: %q", cm.BinaryData["sector"])
				}
			}
			sectors := listSectorConfigMaps(t, fcs, f)
			if len(sectors) != 1 {
				t.Fatalf("file has %d sector configmaps after the race, want exactly 1", len(sectors))
			}
			if got := sectors[0].BinaryData["sector"]; !bytes.Equal(got, header) {
				t.Fatalf("sector 0 after the lost race = %d bytes %x, want the other writer's %d-byte header", len(got), got, len(header))
			}

			// And the reopened file serves those bytes, so Open succeeded over the
			// database that now exists rather than over a zeroed sector.
			rf, ok := sf.(*file)
			if !ok {
				t.Fatalf("Open(%q) returned %T, want *file", name, sf)
			}
			buf := make([]byte, len(header))
			if _, err := rf.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("ReadAt(0) after the lost race: %v", err)
			}
			if !bytes.Equal(buf, header) {
				t.Errorf("ReadAt(0) after the lost race = %x, want the header %x", buf, header)
			}
		})
	}
}

// TestOpenLostRaceSector0VanishedAgain covers the tail of the lost fresh-file
// race: the CREATE answered AlreadyExists, but by the time the in-attempt
// re-read lists the sectors, the race winner's sector 0 has been deleted
// again. There is no settled state to open, so the open must fail — but the
// error must say why. The LIST that found nothing succeeded, so its nil error
// must not be stored as lastErr: a nil lastErr falls through to the
// "no open attempt was made" fallback, which blames the retry budget instead
// of the race (the medium review finding at vfs.go).
//
// At the shipped --retries 1 default the open fails with the race reported;
// with a budget left the next attempt's LIST sees no sectors and the CREATE
// succeeds, so the open goes through.
func TestOpenLostRaceSector0VanishedAgain(t *testing.T) {
	name := "vanished.db"
	for _, tc := range []struct {
		retries   int
		wantOpen  bool
		wantInErr string
		notInErr  string
	}{
		{retries: 1, wantOpen: false, wantInErr: "deleted again before the re-read", notInErr: "no open attempt was made"},
		{retries: 3, wantOpen: true},
	} {
		t.Run(fmt.Sprintf("retries=%d", tc.retries), func(t *testing.T) {
			fcs := fake.NewSimpleClientset()
			v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), tc.retries)
			f := NewFile(name, v)
			sector0 := f.sectorNameFromSectorIndex(0)

			// The first sector-0 CREATE is answered AlreadyExists with
			// nothing actually created: the winner's sector existed when the
			// API server answered and was deleted before our re-read.
			var injected int32
			fcs.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
				ca, ok := action.(k8stesting.CreateAction)
				if !ok {
					return false, nil, nil
				}
				cm, ok := ca.GetObject().(*v1.ConfigMap)
				if !ok || cm.Name != sector0 {
					return false, nil, nil
				}
				if !atomic.CompareAndSwapInt32(&injected, 0, 1) {
					return false, nil, nil
				}
				return true, nil, kerrors.NewAlreadyExists(v1.Resource("configmaps"), sector0)
			})

			sf, _, err := v.Open(name, 0)
			if tc.wantOpen {
				if err != nil {
					t.Fatalf("Open(%q) with retries=%d = %v, want success: the retry's LIST sees no sectors and the CREATE goes through", name, tc.retries, err)
				}
				rf, ok := sf.(*file)
				if !ok {
					t.Fatalf("Open(%q) returned %T, want *file", name, sf)
				}
				buf := make([]byte, 8)
				if _, err := rf.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("ReadAt(0) after the retry: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Open(%q) with retries=%d = nil error, want the vanished-sector race reported", name, tc.retries)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("Open(%q) error = %q, want it to contain %q", name, err, tc.wantInErr)
			}
			if tc.notInErr != "" && strings.Contains(err.Error(), tc.notInErr) {
				t.Errorf("Open(%q) error = %q, want it to NOT contain %q: the retry budget is not what failed", name, err, tc.notInErr)
			}
		})
	}
}

// TestOpenTransientLockfileGetErrorHonoursRetryBudget covers the lockfile
// GET failure path itself (issue #485). Every sibling failure path in the
// open loop honours the retry budget — the lockfile CAS, the sector LIST,
// the fresh-file CREATE and the re-read — but the lockfile GET used to be
// the one exception: one transient 500 or timeout failed the whole open
// with a raw Kubernetes error, no retry, and (uniquely) a non-nil file
// object alongside the non-nil error, which sqlite3vfs does not expect
// from a failed xOpen. The fix makes the GET like its siblings: retry
// within the budget, and on exhaustion return a nil file with the error
// wrapped in the usual "did not settle after N attempts" message.
//
// At retries=3 one injected error leaves budget for the second attempt,
// so the open succeeds; at the shipped --retries 1 default there is no
// second attempt, so the open fails with the wrapped error and a nil file
// (mirroring the retries=1/retries=3 structure of the lost-fresh-file-race
// tests above).
func TestOpenTransientLockfileGetErrorHonoursRetryBudget(t *testing.T) {
	name := "transient-get.db"
	for _, tc := range []struct {
		retries     int
		wantOpen    bool
		wantNErrMsg string
	}{
		{retries: 1, wantOpen: false, wantNErrMsg: "did not settle after 1 attempts"},
		{retries: 3, wantOpen: true},
	} {
		t.Run(fmt.Sprintf("retries=%d", tc.retries), func(t *testing.T) {
			fcs := fake.NewSimpleClientset()
			v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), tc.retries)
			f := NewFile(name, v)

			// The first GET of the lock ConfigMap fails transiently; every
			// later call passes through to the fake tracker untouched.
			var injected int32
			fcs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
				ga, ok := action.(k8stesting.GetAction)
				if !ok || ga.GetName() != f.LockFileName() {
					return false, nil, nil
				}
				if !atomic.CompareAndSwapInt32(&injected, 0, 1) {
					return false, nil, nil
				}
				// A plain error, so the exhaustion message can be proved to
				// wrap the raw failure with errors.Is: apimachinery status
				// errors expose their cause through Cause(), not Unwrap().
				return true, nil, errTransientLockfileGet
			})

			sf, _, err := v.Open(name, 0)
			if tc.wantOpen {
				if err != nil {
					t.Fatalf("Open(%q) with retries=%d after one transient lockfile GET error = %v, want success: the retry budget covers the GET like every sibling path", name, tc.retries, err)
				}
				if injected != 1 {
					t.Errorf("injected lockfile GET error count = %d, want exactly 1", injected)
				}
				rf, ok := sf.(*file)
				if !ok {
					t.Fatalf("Open(%q) returned %T, want *file", name, sf)
				}
				// The retry created the lockfile: the open settled as a
				// fresh file with an empty sector 0 and reads as empty.
				if _, err := rf.ReadAt(make([]byte, 8), 0); err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("ReadAt(0) after the retry: %v", err)
				}
				sectors := listSectorConfigMaps(t, fcs, rf)
				if len(sectors) != 1 {
					t.Errorf("file has %d sector configmaps after the retry, want exactly 1", len(sectors))
				}
				return
			}
			if err == nil {
				t.Fatalf("Open(%q) with retries=%d after one transient lockfile GET error = nil error and file %T, want the wrapped error with a nil file", name, tc.retries, sf)
			}
			if sf != nil {
				t.Errorf("Open(%q) on exhaustion returned a non-nil file (%T) alongside its error; sqlite3vfs does not expect a file from a failed xOpen", name, sf)
			}
			if !errors.Is(err, errTransientLockfileGet) {
				t.Errorf("Open(%q) error = %v, want it to wrap the injected transient GET error", name, err)
			}
			if !strings.Contains(err.Error(), tc.wantNErrMsg) {
				t.Errorf("Open(%q) error = %q, want it to contain %q", name, err, tc.wantNErrMsg)
			}
		})
	}
}

// errTransientLockfileGet is the injected error of
// TestOpenTransientLockfileGetErrorHonoursRetryBudget; a sentinel so the
// test can prove the exhaustion error wraps (not replaces) the raw
// Kubernetes-side failure.
var errTransientLockfileGet = errors.New("injected transient lockfile GET failure")

// --- Label limits (issue #351) ---

// longNameDB is the 40-byte database name of issue #351: its base32
// encoding is 64 characters, one past the 63-character Kubernetes
// label-value cap, so no ConfigMap labelled with it verbatim survives a
// real API server.
const longNameDB = "/data/prod/warehouse-events-2026.sqlite3"

// longNameDBEncoded and longNameDBLabel pin, independently of the VFS, the
// two values its rule must produce for longNameDB: the straight base32 of
// the name, and the 57-character stand-in "long-" + lower-case unpadded
// base32 of SHA-256(name).
const (
	longNameDBEncoded = "f7sgc7dbf72he55ef750c6tfnb11k65ffvs1m3loor3s4mrqgi5c665rnru1i3jt"
	longNameDBLabel   = "long-zikguppo7uhkbgd33ycix5uhbeirvfvsjjyxfejltjpbzxdbwr5a"
)

// TestLabelRuleShortAndLongNames pins the rule of relevantFileLabel, the
// single place the label limit is decided (issue #351): a name whose
// encoding fits the 63-character cap keeps the encoded name as the label;
// a name over 35 bytes gets the "long-" stand-in; and the label depends on
// the name alone, so two files for the same name — two processes on one
// database — always derive the same one, while a different long name never
// shares it.
func TestLabelRuleShortAndLongNames(t *testing.T) {
	v, _ := newTestVFS(t)

	// 35 bytes encode to 56 characters: the last length that fits, and the
	// label is that encoding verbatim.
	name35 := strings.Repeat("a", 35)
	f35 := NewFile(name35, v)
	if got, want := f35.relevantFileLabel(), string(f35.b32ByteFromString(name35)); got != want {
		t.Errorf("35-byte name: label = %q, want the full encoding %q", got, want)
	}

	// 36 bytes encode to 64 characters: the first that does not fit, so
	// the label switches to the stand-in, pinned verbatim.
	name36 := strings.Repeat("a", 36)
	f36 := NewFile(name36, v)
	const want36 = "long-ela5es6napu252mdf36m23ngcp6haj4tc6hf6ewjixd3m7o5vezq"
	if got := f36.relevantFileLabel(); got != want36 {
		t.Errorf("36-byte name: label = %q, want the pinned stand-in %q", got, want36)
	}
	if got := len(f36.relevantFileLabel()); got > 63 {
		t.Errorf("36-byte name: label is %d characters, want at most 63", got)
	}

	// The 40-byte name of the issue, with both values pinned verbatim.
	fL := NewFile(longNameDB, v)
	if got := string(fL.b32ByteFromString(longNameDB)); got != longNameDBEncoded {
		t.Errorf("encoding of the 40-byte name = %q, want the pinned 64-character %q", got, longNameDBEncoded)
	}
	if got := fL.relevantFileLabel(); got != longNameDBLabel {
		t.Errorf("label of the 40-byte name = %q, want the pinned %q", got, longNameDBLabel)
	}
	if got := len(fL.relevantFileLabel()); got > 63 {
		t.Errorf("label of the 40-byte name is %d characters, want at most 63", got)
	}

	// A second file over the same name agrees (the LIST selector of one
	// process must find the sectors of the other), and a different long
	// name gets a different label.
	if got := NewFile(longNameDB, v).relevantFileLabel(); got != fL.relevantFileLabel() {
		t.Errorf("two files over the same name disagree on the label: %q vs %q", got, fL.relevantFileLabel())
	}
	other := NewFile(longNameDB+"x", v) // a different, 41-byte name
	if got := other.relevantFileLabel(); got == fL.relevantFileLabel() {
		t.Errorf("a different long name shares the label %q", got)
	}
	if got := len(other.relevantFileLabel()); got > 63 {
		t.Errorf("label of the 41-byte name is %d characters, want at most 63", got)
	}
}

// TestLongNameWritesWorkThroughShortenedLabel exercises the 40-byte name of
// issue #351 exactly where it failed before: opening for writing, and every
// sector write. The lockfile's and every sector's "relevant-file" LABEL is
// the 63-character-safe stand-in, the lockfile's "relevant-file" Data key
// keeps the full encoded name, and growth, size, reads, LISTing and Delete
// all work through the selector built from that label. The fake clientset
// validates nothing, so what is pinned here is the VFS side of the fix:
// nothing it stores carries a label value over 63 characters, and the
// shortened label still selects exactly this file's sectors.
func TestLongNameWritesWorkThroughShortenedLabel(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, longNameDB)

	// The lockfile Open created: shortened label, full name in Data.
	lf := getLockfileConfigMap(t, fcs, f)
	if got := lf.Labels["relevant-file"]; got != longNameDBLabel {
		t.Errorf("lockfile relevant-file label = %q, want the shortened stand-in %q", got, longNameDBLabel)
	}
	if got := lf.Data["relevant-file"]; got != longNameDBEncoded {
		t.Errorf("lockfile relevant-file data key = %q, want the full encoded name %q", got, longNameDBEncoded)
	}

	// The fresh open seeds one empty sector, labelled the same way and
	// still NAMED with the full encoded name.
	sectors := listSectorConfigMaps(t, fcs, f)
	if len(sectors) != 1 {
		t.Fatalf("fresh long-named file has %d sector configmaps, want exactly 1", len(sectors))
	}
	if got := sectors[0].Labels["relevant-file"]; got != longNameDBLabel {
		t.Errorf("sector 0 label = %q, want %q", got, longNameDBLabel)
	}
	if got, want := sectors[0].Name, longNameDBEncoded+"-0"; got != want {
		t.Errorf("sector 0 name = %q, want %q (the ConfigMap name keeps the full encoding)", got, want)
	}

	// Grow the file across sectors and read it back: every step here LISTs
	// or writes through the shortened label.
	data := make([]byte, multiSectorLen)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize on the long-named file: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", size, len(data))
	}
	buf := make([]byte, len(data))
	mustReadAt(t, f, buf, 0)
	if !bytes.Equal(buf, data) {
		t.Errorf("read back different bytes than were written")
	}

	sectors = listSectorConfigMaps(t, fcs, f)
	if len(sectors) != 2 {
		t.Fatalf("the shortened label selected %d sector configmaps, want 2 (sectors 0 and 1)", len(sectors))
	}
	for _, s := range sectors {
		if got := s.Labels["relevant-file"]; got != longNameDBLabel {
			t.Errorf("sector %s label = %q, want %q", s.Name, got, longNameDBLabel)
		}
	}

	// A different long name in the same namespace is invisible to this
	// file's selector: only its own empty sector 0, none of the above.
	other := openTestFile(t, v, longNameDB+"x")
	if got := listSectorConfigMaps(t, fcs, other); len(got) != 1 {
		t.Errorf("a different long name selected %d sector configmaps, want just its own 1", len(got))
	}

	// Delete walks the same selector and removes everything.
	if err := v.Delete(longNameDB, false); err != nil {
		t.Fatalf("Delete of the long-named file: %v", err)
	}
	if got := listSectorConfigMaps(t, fcs, f); len(got) != 0 {
		t.Errorf("after Delete, %d sector configmaps remain, want 0", len(got))
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("after Delete, lockfile get returned err=%v, want NotFound", err)
	}
}

func TestOpenExistingFileKeepsDataAndSectors(t *testing.T) {
	v, fcs := newTestVFS(t)
	f1 := openTestFile(t, v, "existing.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f1, data, 0)
	if err := f1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	before := listSectorConfigMaps(t, fcs, f1)
	if len(before) != 1 {
		t.Fatalf("before reopen: %d sector configmaps, want 1", len(before))
	}

	f2 := openTestFile(t, v, "existing.db")

	after := listSectorConfigMaps(t, fcs, f2)
	if len(after) != len(before) {
		t.Errorf("reopen changed the sector count: %d -> %d, want unchanged", len(before), len(after))
	}
	size, err := f2.FileSize()
	if err != nil {
		t.Fatalf("FileSize after reopen: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("FileSize after reopen = %d, want %d", size, len(data))
	}
	got := make([]byte, len(data))
	if n := mustReadAt(t, f2, got, 0); n != len(data) {
		t.Fatalf("read after reopen got %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("data changed across reopen")
	}
}

// --- WriteAt ---

func TestWriteAtSingleSectorRoundTrip(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "roundtrip.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Fatalf("1000 byte write produced %d sectors, want 1", len(sectors))
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, f, got, 0); n != 1000 {
		t.Fatalf("read back %d bytes, want 1000", n)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}
}

func TestWriteAtFullSectorOverwrite(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "fullsector.db")

	data := make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, SectorSize)
	if n := mustReadAt(t, f, got, 0); n != len(data) {
		t.Fatalf("read back %d bytes, want %d", n, len(data))
	}
	if !bytes.Equal(got, data) {
		t.Error("full-sector round trip differs")
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(len(data)) {
		t.Errorf("FileSize = %d, want %d", size, len(data))
	}
}

// TestWriteAtMultiSectorOverExisting rewrites a whole two-sector file in one
// WriteAt. Both sectors already exist, so no unwritten sector is touched.
func TestWriteAtMultiSectorOverExisting(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "multisector.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	data := make([]byte, multiSectorLen)
	fillPatternAlt(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Fatalf("%d byte write left %d sectors, want 2", multiSectorLen, len(sectors))
	}
	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(multiSectorLen) {
		t.Errorf("FileSize = %d, want %d", size, multiSectorLen)
	}
}

func TestWriteAtPartialOverwriteKeepsSurroundingBytes(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "overwrite.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	replacement := []byte("REPLACED-REPLACED-REPLACED")
	mustWriteAt(t, f, replacement, 100)

	want := make([]byte, multiSectorLen)
	fillPattern(want)
	copy(want[100:100+len(replacement)], replacement)

	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, want) {
		t.Error("partial overwrite damaged bytes outside the overwritten range")
	}

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Errorf("partial overwrite left %d sectors, want 2", len(sectors))
	}
}

// TestWriteAtGrowIntoNewSectorsRoundTrip replaces
// TestWriteAtIntoUnwrittenSectorPanicsDocumentsCurrentBehaviour, which
// pinned the worst defect of the code as imported: getSector wrote an empty
// sector for anything missing and then dereferenced the nil response, so ANY
// write reaching an unwritten sector panicked — capping every file at one
// 64 KiB sector and crashing SQLite (unrecoverably, through cgo) the moment
// a database grew past its first page. getSector no longer creates sectors
// and WriteAt handles missing ones itself, so growth works.
func TestWriteAtGrowIntoNewSectorsRoundTrip(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "grow.db") // only the empty sector 0 exists

	data := make([]byte, multiSectorLen)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Fatalf("grow write left %d sector configmaps, want 2", len(sectors))
	}
	got := make([]byte, multiSectorLen)
	if n := mustReadAt(t, f, got, 0); n != multiSectorLen {
		t.Fatalf("read back %d bytes, want %d", n, multiSectorLen)
	}
	if !bytes.Equal(got, data) {
		t.Error("read back data differs from what was written")
	}
	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(multiSectorLen) {
		t.Errorf("FileSize = %d, want %d", size, multiSectorLen)
	}
}

// --- ReadAt ---

func TestReadAtAcrossSectorBoundary(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "across.db")
	data := seedMultiSectorFile(t, fcs, f, multiSectorLen)

	off := int64(SectorSize) - 36 // 36 bytes before the boundary
	got := make([]byte, 100)
	if n := mustReadAt(t, f, got, off); n != 100 {
		t.Fatalf("read got %d bytes, want 100", n)
	}
	if !bytes.Equal(got, data[off:off+100]) {
		t.Error("read across the sector boundary returned wrong bytes")
	}
}

func TestReadAtShortReadReturnsEOF(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "shortread.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, 2000) // longer than the file
	n, err := f.ReadAt(got, 0)
	if n != len(data) {
		t.Errorf("short read got %d bytes, want %d", n, len(data))
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("short read err = %v, want io.EOF", err)
	}
	if !bytes.Equal(got[:n], data) {
		t.Error("short read returned wrong bytes")
	}
}

func TestReadAtPastEndReturnsEOF(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "pastend.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	got := make([]byte, 100)
	for _, off := range []int64{1000, 5000} {
		n, err := f.ReadAt(got, off)
		if n != 0 {
			t.Errorf("ReadAt(off=%d) got %d bytes, want 0", off, n)
		}
		if !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt(off=%d) err = %v, want io.EOF", off, err)
		}
	}

	// Same for a never-written (empty) file.
	empty := openTestFile(t, v, "pastend-empty.db")
	if n, err := empty.ReadAt(got, 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt on empty file got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// --- FileSize ---

func TestFileSizeExact(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "size.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if size, err := f.FileSize(); err != nil || size != 1000 {
		t.Errorf("FileSize after 1000 byte write = (%d, %v), want (1000, nil)", size, err)
	}

	data = make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if size, err := f.FileSize(); err != nil || size != int64(SectorSize) {
		t.Errorf("FileSize after %d byte write = (%d, %v), want (%d, nil)", SectorSize, size, err, SectorSize)
	}

	// A two-sector file (seeded into the fake clientset): the size is
	// lastSectorIndex*SectorSize + len(lastSectorData).
	data = make([]byte, multiSectorLen)
	fillPattern(data)
	seedSector(t, fcs, f, 1, data[SectorSize:])
	if size, err := f.FileSize(); err != nil || size != int64(multiSectorLen) {
		t.Errorf("FileSize on two-sector file = (%d, %v), want (%d, nil)", size, err, multiSectorLen)
	}
}

// --- Truncate ---
//
// Both defects of the imported code are fixed and pinned below: a failing
// sector write fails the whole truncate instead of being swallowed while
// the delete loop carries on (TestTruncatePropagatesWriteSectorError), and
// the delete loop starts after the last surviving sector — the one holding
// the kept prefix, found via sectorForPos(size-1) — so a mid-sector shrink
// keeps its prefix (TestTruncateShrinkMidSectorKeepsPrefix) and a
// sector-aligned shrink drops exactly the sectors past the new size
// (TestTruncateShrinkAlignedToSectorEnd).

func TestTruncateNoOpGrow(t *testing.T) {
	v, _ := newTestVFS(t)
	f := openTestFile(t, v, "noopgrow.db")

	data := make([]byte, 1000)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)

	if err := f.Truncate(2000); err != nil { // grow is a no-op
		t.Fatalf("Truncate(grow) = %v, want nil", err)
	}
	if err := f.Truncate(1000); err != nil { // exact size is a no-op
		t.Fatalf("Truncate(same size) = %v, want nil", err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != 1000 {
		t.Errorf("FileSize after no-op truncate = %d, want 1000", size)
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, f, got, 0); n != 1000 || !bytes.Equal(got, data) {
		t.Error("no-op truncate damaged the data")
	}
}

func TestTruncateShrinkAlignedToSectorEnd(t *testing.T) {
	// 65537 bytes: one full sector plus a 1-byte second sector. Truncating
	// to 65536 drops the second sector and keeps the first, and succeeds.
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "shrink-aligned.db")
	data := seedMultiSectorFile(t, fcs, f, int(sectorSize)+1)

	if err := f.Truncate(sectorSize); err != nil {
		t.Fatalf("Truncate(%d) = %v, want nil", sectorSize, err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize after truncate: %v", err)
	}
	if size != sectorSize {
		t.Errorf("FileSize after truncate = %d, want %d", size, sectorSize)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Errorf("%d sector configmaps remain after truncate, want 1", len(sectors))
	}

	got := make([]byte, 100)
	if n := mustReadAt(t, f, got, 0); n != 100 || !bytes.Equal(got, data[:100]) {
		t.Error("truncate damaged the kept prefix")
	}
	if n, err := f.ReadAt(got, sectorSize); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read past the new EOF got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestTruncateShrinkMidSectorKeepsPrefix replaces
// TestTruncateShrinkMidSectorDocumentsCurrentBehaviour, which pinned the
// imported bug: a mid-sector truncate reported success while deleting EVERY
// sector, including the one holding the kept prefix, destroying the file.
// The delete loop now starts after the surviving sector, the surviving
// sector keeps the kept prefix, and sectors beyond the new size are gone.
func TestTruncateShrinkMidSectorKeepsPrefix(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "shrink-mid.db")
	data := seedMultiSectorFile(t, fcs, f, multiSectorLen)

	if err := f.Truncate(700); err != nil {
		t.Fatalf("Truncate(700) = %v, want nil", err)
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize after truncate: %v", err)
	}
	if size != 700 {
		t.Errorf("FileSize after truncate = %d, want 700", size)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Fatalf("%d sector configmaps remain after truncate, want 1", len(sectors))
	}

	got := make([]byte, 700)
	if n := mustReadAt(t, f, got, 0); n != 700 {
		t.Fatalf("read back %d bytes, want 700", n)
	}
	if !bytes.Equal(got, data[:700]) {
		t.Error("truncate damaged the kept prefix")
	}
	if n, err := f.ReadAt(got, 700); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read past the new EOF got (%d, %v), want (0, io.EOF)", n, err)
	}
}

// TestTruncatePropagatesWriteSectorError pins the fix for the imported bug
// of ignoring WriteSector's error in Truncate: a failing sector write must
// fail the truncate instead of silently continuing to delete sectors. This
// test fails without that fix (the error used to be swallowed).
func TestTruncatePropagatesWriteSectorError(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "truncerr.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	boom := errors.New("boom: sector write failed")
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})

	if err := f.Truncate(700); err == nil {
		t.Fatal("Truncate(700) with a failing sector write = nil, want the write error")
	}
	// Nothing may have been deleted either: the surviving sectors are still
	// both there.
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
		t.Errorf("%d sector configmaps remain after the failed truncate, want 2 (no deletions)", len(sectors))
	}
}

// TestTruncateSectorGetErrorIsNotShortRead pins the fix for issue #478: the
// GET that fetches the last surviving sector during Truncate is the one
// read whose error Truncate returns unchanged, so a transient (non-NotFound)
// GET failure used to surface as sqlite3vfs.IOErrorShortRead — the extended
// code SQLite expects only from xRead, meaning "read succeeded but returned
// fewer bytes" — and sent hot-journal rollback, VACUUM and TRUNCATE-journal
// recovery down the wrong path. It must be exactly sqlite3vfs.IOErrorRead,
// while a genuinely absent sector keeps reading as a hole: errSectorNotFound
// stays the sole absence signal.
func TestTruncateSectorGetErrorIsNotShortRead(t *testing.T) {
	t.Run("transient GET failure is IOErrorRead", func(t *testing.T) {
		v, fcs := newTestVFS(t)
		f := openTestFile(t, v, "trunc-geterr.db")
		seedMultiSectorFile(t, fcs, f, multiSectorLen)

		// Drop the sector cache wholesale, the way the 256-sector cap does
		// (see sectorData), so the shortening fetch of sector 0 is a real
		// GET the reactor can fail.
		f.sectors = make(map[int64][]byte)

		sector0 := f.sectorNameFromSectorIndex(0)
		fcs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.GetAction).GetName() == sector0 {
				return true, nil, errors.New("boom: transient sector GET failure")
			}
			return false, nil, nil
		})

		err := f.Truncate(700)
		if err == nil {
			t.Fatal("Truncate(700) with a failing sector GET = nil, want an error")
		}
		if err != sqlite3vfs.IOErrorRead {
			t.Fatalf("Truncate(700) with a failing sector GET = %v (%T), want exactly sqlite3vfs.IOErrorRead", err, err)
		}
		// The failure must have stopped the truncate before any deletion.
		if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 2 {
			t.Errorf("%d sector configmaps remain after the failed truncate, want 2 (no deletions)", len(sectors))
		}
	})

	t.Run("absent sector still reads as a hole", func(t *testing.T) {
		v, fcs := newTestVFS(t)
		f := openTestFile(t, v, "trunc-hole.db")
		seedMultiSectorFile(t, fcs, f, multiSectorLen)

		f.sectors = make(map[int64][]byte)

		sector0 := f.sectorNameFromSectorIndex(0)
		fcs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if action.(k8stesting.GetAction).GetName() == sector0 {
				return true, nil, kerrors.NewNotFound(v1.Resource("configmaps"), sector0)
			}
			return false, nil, nil
		})

		// The sector holding the new last byte does not exist: nothing to
		// shorten, so the truncate proceeds as over a hole (there is no
		// short-read error) and drops the sectors past the new end.
		if err := f.Truncate(700); err != nil {
			t.Fatalf("Truncate(700) with an absent sector = %v, want nil (treated as a hole)", err)
		}
		if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
			t.Errorf("%d sector configmaps remain after the hole truncate, want 1 (only sector 0)", len(sectors))
		}
	})
}

// TestReadAtDoesNotCreateSectors pins the read-side of the no-create rule:
// reading — including a read that reaches a sector which does not exist —
// must never write a sector ConfigMap to the cluster. The imported code
// created an empty sector on read (a write to the cluster, from a read!) and
// then panicked dereferencing the nil response it had just replaced.
func TestReadAtDoesNotCreateSectors(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "holeread.db")

	data := make([]byte, SectorSize)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if len(listSectorConfigMaps(t, fcs, f)) != 1 {
		t.Fatal("setup: want exactly 1 sector")
	}

	// A read that reaches past the end of the file, into a sector that does
	// not exist, must neither create a sector nor panic.
	got := make([]byte, 64)
	n, err := f.ReadAt(got, SectorSize-10)
	if n != 10 {
		t.Errorf("read reaching the missing sector got %d bytes, want 10 (short read)", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("read reaching the missing sector err = %v, want io.EOF", err)
	}
	if sectors := listSectorConfigMaps(t, fcs, f); len(sectors) != 1 {
		t.Errorf("read left %d sector configmaps, want 1 (reads must not create sectors)", len(sectors))
	}
}

// seedSectorWithHole seeds a three-sector file whose MIDDLE sector (1) has no
// ConfigMap at all — the state an interrupted, non-journal Delete leaves
// (vfs.go's deleteSectorsInSafeOrder can stop midway and report aDeleteFailed,
// which SQLite ignores), or one an externally deleted sector ConfigMap leaves.
// Sectors 0 and 2 are present and hold distinct patterns, so a read that lets
// sector 2 slip into sector 1's place is visible byte for byte.
func seedSectorWithHole(tb testing.TB, fcs *fake.Clientset, f *file) (sector0, sector2 []byte) {
	tb.Helper()
	sector0 = make([]byte, SectorSize)
	fillPattern(sector0)
	sector2 = make([]byte, SectorSize)
	fillPatternAlt(sector2)
	seedSector(tb, fcs, f, 0, sector0)
	seedSector(tb, fcs, f, 2, sector2)
	return sector0, sector2
}

// TestReadAtMiddleHoleReadsZeros pins the fix for issue #438: a read across a
// hole in the MIDDLE of a file must report zeros over the hole's own span and
// leave every later sector at its true offset. Before the fix ReadAt advanced
// its write position only by the bytes a sector actually contributed
// (n += copy(...)), so a nil-Data hole copied nothing and every byte after it
// slid earlier in the buffer — silently relocating data. It must also stay
// true that a read never creates the missing sector (no write to the cluster).
func TestReadAtMiddleHoleReadsZeros(t *testing.T) {
	newHoleFile := func(t *testing.T) (*file, *fake.Clientset, []byte, []byte) {
		t.Helper()
		v, fcs := newTestVFS(t)
		f := openTestFile(t, v, "middle-hole.db")
		sector0, sector2 := seedSectorWithHole(t, fcs, f)
		// Count Create calls: the read must issue none. A reactor is used in
		// addition to the ConfigMap diff below, so a create-then-delete would
		// still be caught.
		var creates atomic.Int64
		fcs.PrependReactor("create", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
			creates.Add(1)
			return false, nil, nil
		})
		t.Cleanup(func() {
			if got := creates.Load(); got != 0 {
				t.Errorf("ReadAt issued %d ConfigMap Create call(s), want 0 (reads must never create sectors)", got)
			}
		})
		return f, fcs, sector0, sector2
	}

	t.Run("hole-only-span", func(t *testing.T) {
		f, fcs, sector0, sector2 := newHoleFile(t)

		size, err := f.FileSize()
		if err != nil {
			t.Fatalf("FileSize: %v", err)
		}
		// sizeFromSectorList takes the largest index*SectorSize+len(data), so
		// the file reaches past the hole: the hole is INSIDE the file.
		if want := int64(3) * SectorSize; size != want {
			t.Fatalf("FileSize = %d, want %d (sectors 0 and 2 present)", size, want)
		}

		got := make([]byte, 3*SectorSize)
		for i := range got {
			got[i] = 0xff // prefill: a hole that contributes nothing is visible
		}
		n, err := f.ReadAt(got, 0)
		if n != len(got) {
			t.Fatalf("ReadAt across the middle hole got %d bytes, want %d (the read must cover the whole span)", n, len(got))
		}
		if err != nil {
			t.Fatalf("ReadAt across the middle hole = %d, %v; want %d, nil", n, err, len(got))
		}
		if !bytes.Equal(got[:SectorSize], sector0) {
			t.Error("sector 0's bytes differ from what was written")
		}
		if hole := got[SectorSize : 2*SectorSize]; !bytes.Equal(hole, make([]byte, SectorSize)) {
			t.Errorf("the missing middle sector did not read as zeros: %d non-zero byte(s), first at %d",
				nonZeroCount(hole), bytes.IndexFunc(hole, func(r rune) bool { return r != 0 }))
		}
		if !bytes.Equal(got[2*SectorSize:], sector2) {
			t.Error("sector 2's bytes are not at their true offset: they were shifted by the hole")
		}

		// The configmaps on the cluster are exactly the two seeded sectors.
		if names := sectorConfigMapNames(t, fcs, f); !slices.Equal(names, []string{
			f.sectorNameFromSectorIndex(0), f.sectorNameFromSectorIndex(2),
		}) {
			t.Errorf("sector configmaps after the read = %v, want only sectors 0 and 2 (no sector created for the hole)", names)
		}
	})

	t.Run("read-starts-mid-sector", func(t *testing.T) {
		f, _, sector0, sector2 := newHoleFile(t)

		// Start 10 bytes before the hole: the first sector's tail, the hole's
		// zeros, then sector 2's head — each at its true offset.
		const off = SectorSize - 10
		got := make([]byte, SectorSize+200)
		for i := range got {
			got[i] = 0xff
		}
		n, err := f.ReadAt(got, off)
		if n != len(got) {
			t.Fatalf("read from mid-sector got %d bytes, want %d", n, len(got))
		}
		if err != nil {
			t.Fatalf("read from mid-sector err = %v, want nil", err)
		}
		if !bytes.Equal(got[:10], sector0[SectorSize-10:]) {
			t.Error("the first 10 bytes are not sector 0's tail at the requested offset")
		}
		if hole := got[10 : SectorSize+10]; !bytes.Equal(hole, make([]byte, len(hole))) {
			t.Errorf("the hole did not read as zeros: %d non-zero byte(s)", nonZeroCount(hole))
		}
		if !bytes.Equal(got[SectorSize+10:], sector2[:190]) {
			t.Error("sector 2's bytes are not at their true offset after a mid-sector start")
		}
	})
	t.Run("read-starts-inside-hole", func(t *testing.T) {
		f, _, _, sector2 := newHoleFile(t)

		// The read begins inside the missing sector: its first bytes are the
		// hole's zeros, and sector 2 still lands at its true offset. Before
		// the fix this read returned (0, io.EOF) — a hole at the start of the
		// buffer was mistaken for the hasHotJournal case at offset 0.
		// 500 bytes of hole, then sector 2's first 500 bytes: the read starts
		// 500 bytes before the sector boundary, inside the missing sector.
		const off = 2*SectorSize - 500
		got := make([]byte, 1000)
		for i := range got {
			got[i] = 0xff
		}
		n, err := f.ReadAt(got, off)
		if n != len(got) {
			t.Fatalf("read starting inside the hole got %d bytes, want %d", n, len(got))
		}
		if err != nil {
			t.Fatalf("read starting inside the hole err = %v, want nil", err)
		}
		if !bytes.Equal(got[:500], make([]byte, 500)) {
			t.Errorf("the hole's zeros at the start of the read are wrong: %d non-zero byte(s)", nonZeroCount(got[:500]))
		}
		if want := sector2[:500]; !bytes.Equal(got[500:], want) {
			t.Error("sector 2's bytes are not at their true offset when the read starts inside the hole")
		}
	})
}

// nonZeroCount counts the non-zero bytes of b.
func nonZeroCount(b []byte) int {
	n := 0
	for _, c := range b {
		if c != 0 {
			n++
		}
	}
	return n
}

// sectorConfigMapNames lists this file's sector ConfigMap names, sorted by
// sector index.
func sectorConfigMapNames(tb testing.TB, fcs *fake.Clientset, f *file) []string {
	tb.Helper()
	var out []string
	for _, cm := range listSectorConfigMaps(tb, fcs, f) {
		out = append(out, cm.Name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	// Sector names are fixed-width base32 suffixes, so lexicographic order is
	// sector order for the small indices these tests use.
	return out
}

// --- Delete ---

func TestDeleteRemovesSectorsAndLockfile(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "doomed.db")
	seedMultiSectorFile(t, fcs, f, multiSectorLen)
	if len(listSectorConfigMaps(t, fcs, f)) != 2 {
		t.Fatal("setup: want 2 sectors before delete")
	}

	if err := v.Delete("doomed.db", false); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	all, err := fcs.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list all configmaps: %v", err)
	}
	if len(all.Items) != 0 {
		t.Errorf("%d configmaps remain after Delete, want 0 (sectors and lockfile)", len(all.Items))
	}

	// Deleting a file that is already gone must succeed, not error.
	if err := v.Delete("doomed.db", false); err != nil {
		t.Errorf("Delete of already-deleted file = %v, want nil", err)
	}

	// The lockfile is really gone.
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("lockfile get after delete: err = %v, want NotFound", err)
	}
}

// --- Lock transitions ---
//
// The lock levels the sqlite3vfs shim passes through are None=0, Shared=1,
// Reserved=2, Pending=3, Exclusive=4. SQLite never requests Pending
// explicitly (it is implicit in the OS backend), and this VFS rejects it.
//
// Since issue #47 the lock ConfigMap records WHO holds what: a
// shared-holders map (holder id -> lease expiry) plus one writer entry, so
// two files — two processes — can hold SHARED at once, RESERVED excludes
// other writers only, and CheckReservedLock answers "is any writer held",
// not "is any lock held". The aggregate "lock" key keeps its old meaning
// (the highest level held) for readers of the old format.

func TestLockTransitionsValid(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "locks.db")

	if err := f.Lock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Lock(None) on an unlocked file = %v, want nil", err)
	}
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared) = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("lock after Lock(Shared) = %q, want %q", got, sqlite3vfs.LockShared.String())
	}
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 1 {
		t.Errorf("shared-holders after Lock(Shared) = %v, want exactly this file's entry", holders)
	} else if _, ok := holders[f.holderID]; !ok {
		t.Errorf("shared-holders %v lacks this file's holder id", holders)
	}
	if w := writerIn(t, fcs, f); w != nil {
		t.Errorf("writer after Lock(Shared) = %v, want none", w)
	}

	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved) from Shared = %v, want nil", err)
	}
	if err := f.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("Lock(Exclusive) from Reserved = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("lock after Lock(Exclusive) = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
	// The EXCLUSIVE write subsumes this file's shared entry: the writer is
	// the only record of the lock now.
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 0 {
		t.Errorf("shared-holders after Lock(Exclusive) = %v, want empty (subsumed by the writer)", holders)
	}
	if w := writerIn(t, fcs, f); w == nil || w.ID != f.holderID || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after Lock(Exclusive) = %v, want this file at LockExclusive", w)
	}

	// Requesting a level already held or lower is a silent no-op.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Lock(Shared) while Exclusive = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("downward Lock request changed the stored lock to %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

func TestLockTransitionsInvalid(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "badlocks.db") // lockfile exists at LockNone

	if err := f.Lock(sqlite3vfs.LockReserved); err == nil {
		t.Error("Lock(Reserved) from None = nil, want an error")
	}
	if err := f.Lock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("Lock(Exclusive) from None = nil, want an error")
	}
	if err := f.Lock(sqlite3vfs.LockPending); err == nil {
		t.Error("Lock(Pending) = nil, want an error (sqlite never requests pending)")
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockNone.String() {
		t.Errorf("failed lock requests changed the stored lock to %q, want %q", got, sqlite3vfs.LockNone.String())
	}
	if w := writerIn(t, fcs, f); w != nil {
		t.Errorf("failed lock requests wrote a writer entry %v, want none", w)
	}
}

func TestUnlockTransitions(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "unlocks.db")

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	// Unlock to the level already held is a no-op.
	if err := f.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Unlock(Shared) while Shared = %v, want nil", err)
	}
	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Errorf("Unlock(None) from Shared = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockNone.String() {
		t.Errorf("lock after Unlock(None) = %q, want %q", got, sqlite3vfs.LockNone.String())
	}
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 0 {
		t.Errorf("shared-holders after Unlock(None) = %v, want empty (this file's entry released)", holders)
	}

	// Reserved drops straight back to Shared, then to None.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	if err := f.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Errorf("Unlock(Shared) from Reserved = %v, want nil", err)
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("lock after Unlock(Shared) = %q, want %q", got, sqlite3vfs.LockShared.String())
	}
	if err := f.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Errorf("Unlock(None) = %v, want nil", err)
	}
}

// TestUnlockRejectsInvalidRequests pins the panic-to-error change: Unlock
// used to log.Panic on an unlock above SHARED or above the current lock
// level; through the sqlite3vfs shim a panic crashes the whole process, so
// both cases must now be returned as errors, leaving the stored lock level
// untouched.
func TestUnlockRejectsInvalidRequests(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "badunlock.db")

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Unlock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("Unlock(Exclusive) while holding Shared = nil, want an error (was a panic)")
	}
	if err := f.Unlock(sqlite3vfs.LockReserved); err == nil {
		t.Error("Unlock(Reserved) above the current lock level = nil, want an error (was a panic)")
	}
	if got := lockState(t, fcs, f); got != sqlite3vfs.LockShared.String() {
		t.Errorf("failed unlock requests changed the stored lock to %q, want %q", got, sqlite3vfs.LockShared.String())
	}
}

// TestCheckReservedLockSecondProcess simulates a second process by holding a
// second file object over the same (fake) API server: the lock ConfigMap is
// the shared state between them. Since the holder-aware format
// (issue #47), CheckReservedLock reports whether any WRITER (RESERVED or
// above) is held: shared holders are readers, and another process's plain
// SHARED lock must not read as "someone has reserved the database".
// TestCheckReservedLockSecondProcess pins the Go-level answer of
// CheckReservedLock across two files — WITH THE KNOWN INVERSION the upstream
// dependency forces on it. CheckReservedLock returns the NEGATION of "a
// reserved-or-greater lock is held" (false means held), because upstream
// github.com/psanford/sqlite3vfs writes *pResOut = 0 for a true answer and 1
// for false — see the comment on file.CheckReservedLock and the README's
// "Known upstream bug" section. The value SQLite finally sees is pinned end
// to end, across the cgo boundary, by
// TestCheckReservedLockCrossesCGOBoundary in vfs_concurrent_test.go.
func TestCheckReservedLockSecondProcess(t *testing.T) {
	v, _ := newTestVFS(t)
	f1 := openTestFile(t, v, "twoproc.db")
	f2 := NewFile("twoproc.db", v) // "second process", same backing store

	// Nobody holds anything: the true answer is "not held", inverted to true.
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock on unlocked file = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	// A SHARED holder is a reader, not a reserved-lock holder.
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock while another process holds Shared = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	// A reserved lock IS held: the inverted answer is false, which upstream
	// turns into *pResOut = 1 — "locked" — for SQLite.
	if reserved, err := f2.CheckReservedLock(); err != nil || reserved {
		t.Errorf("CheckReservedLock while another process holds Reserved = (%v, %v), want (false, nil)", reserved, err)
	}

	// Downgrading the writer back to SHARED clears the writer entry, and
	// the second process observes that too.
	if err := f1.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Unlock(Shared): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock after the writer downgraded = (%v, %v), want (true, nil)", reserved, err)
	}

	if err := f1.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock(None): %v", err)
	}
	if reserved, err := f2.CheckReservedLock(); err != nil || !reserved {
		t.Errorf("CheckReservedLock after the other process unlocked = (%v, %v), want (true, nil)", reserved, err)
	}

	// The second process observes the released state, so the transitions
	// None -> Shared -> Reserved are legal for it now.
	if err := f2.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second process Lock(Shared): %v", err)
	}
	if err := f2.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Errorf("second process Lock(Reserved) from Shared = %v, want nil", err)
	}
}

// TestFailedUnlockInvalidatesCaches pins the failed-unlock fix: a transient
// failure of the lock ConfigMap write on Unlock(NONE) must leave no valid
// cache and no trusted lock level, because sqlite largely ignores unlock
// errors and keeps going. The bug this replaces updated the lock state and
// the caches only on write success, so after one failed unlock the next
// Lock(SHARED) made no API call and served cached sectors and size while
// another process had rewritten the database — wrong reads, and lost writes
// once a partial-sector write built on the cached data.
func TestFailedUnlockInvalidatesCaches(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "stale.db")

	old := make([]byte, 1000)
	fillPattern(old)
	mustWriteAt(t, fa, old, 0)
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	got := make([]byte, 1000)
	if n := mustReadAt(t, fa, got, 0); n != 1000 {
		t.Fatalf("A warm-up read got %d bytes, want 1000", n)
	}
	if !fa.valid || len(fa.sectors) == 0 {
		t.Fatal("setup: A caches are not warm after a read under SHARED")
	}

	// A's next unlock fails once, at the API layer, on the lock ConfigMap
	// only (sector writes must still go through).
	var failed int32
	boom := errors.New("boom: transient API error")
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, isUpdate := action.(k8stesting.UpdateAction)
		if !isUpdate {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != fa.LockFileName() {
			return false, nil, nil
		}
		if atomic.AddInt32(&failed, 1) > 1 {
			return false, nil, nil
		}
		return true, nil, boom
	})
	if err := fa.Unlock(sqlite3vfs.LockNone); err == nil {
		t.Fatal("Unlock(None) with a failing ConfigMap write = nil, want the error")
	}
	if fa.valid || fa.sectors != nil || fa.lockKnown {
		t.Errorf("failed unlock left state known/caches valid: valid=%v, sectors=%v, lockKnown=%v; caches must be dropped and the lock state unknown whatever the write returns", fa.valid, fa.sectors, fa.lockKnown)
	}

	// B rewrites the database while A no longer trusts anything of its own:
	// new bytes, bigger file.
	fb := openTestFile(t, v, "stale.db")
	latest := make([]byte, 2000)
	fillPatternAlt(latest)
	mustWriteAt(t, fb, latest, 0)

	// A re-locks and reads: it must see B's data and size, and re-locking
	// must cost API calls — the bug served all of it from the stale cache
	// with zero calls.
	fcs.ClearActions()
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A re-Lock(Shared): %v", err)
	}
	size, err := fa.FileSize()
	if err != nil {
		t.Fatalf("A FileSize after re-lock: %v", err)
	}
	if size != int64(len(latest)) {
		t.Errorf("A FileSize after re-lock = %d, want %d (B size)", size, len(latest))
	}
	got = make([]byte, len(latest))
	if n := mustReadAt(t, fa, got, 0); n != len(latest) {
		t.Fatalf("A read %d bytes after re-lock, want %d", n, len(latest))
	}
	if !bytes.Equal(got, latest) {
		t.Error("A served stale bytes after a failed unlock: the cache survived an unlock whose write failed")
	}
	if calls := len(fcs.Actions()); calls == 0 {
		t.Error("A re-lock and read made 0 API calls: the failed unlock left the cache valid")
	}
}

// TestOpenDoesNotAdoptAnotherProcessLock pins the no-adoption fix: the lock
// level stored in the ConfigMap may be another process's, so a file opened
// through the VFS must start at NONE locally and take every one of its own
// transitions through the ConfigMap. The bug this replaces copied the
// stored level into the new file object's lockLevel, so a newly opened A
// believed it held B's EXCLUSIVE: its CheckReservedLock answered from that
// belief, its Lock(SHARED) made no call, its Unlock(NONE) wrote NONE while
// B still held EXCLUSIVE, and its later Lock(RESERVED)/Lock(EXCLUSIVE) were
// silent no-ops that let it write while the cluster showed NONE.
func TestOpenDoesNotAdoptAnotherProcessLock(t *testing.T) {
	v, fcs := newTestVFS(t)
	fb := openTestFile(t, v, "held.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive): %v", err)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Fatalf("setup: stored lock = %q, want exclusive", got)
	}

	// A opens while B holds EXCLUSIVE.
	fa := openTestFile(t, v, "held.db")
	if !fa.lockKnown || fa.lockLevel != sqlite3vfs.LockNone {
		t.Errorf("opened file lock state = (known=%v, level=%v), want (true, NONE): the stored level must never be adopted", fa.lockKnown, fa.lockLevel)
	}

	// A must still learn, from the cluster, that a lock is held. The
	// answer is the KNOWN INVERSION the upstream dependency forces on
	// CheckReservedLock (see file.CheckReservedLock): B holding EXCLUSIVE
	// means reservedHeld is true, so CheckReservedLock answers false —
	// true would mean nobody holds a writer lock.
	if reserved, err := fa.CheckReservedLock(); err != nil || reserved {
		t.Errorf("A CheckReservedLock while B holds Exclusive = (%v, %v), want (false, nil): the inverted answer for a held writer lock", reserved, err)
	}

	// A holds nothing, so unlocking to NONE is a no-op that must not write:
	// the stored level stays B's EXCLUSIVE.
	fcs.ClearActions()
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	if calls := len(fcs.Actions()); calls != 0 {
		t.Errorf("A no-op Unlock(None) made %d API calls, want 0", calls)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("A Unlock(None) changed the stored lock to %q; B still holds Exclusive", got)
	}

	// From the un-adopted NONE, climbing straight to RESERVED or EXCLUSIVE
	// is an error, not a silent no-op: A cannot believe it already holds a
	// higher lock than it ever asked for.
	if err := fa.Lock(sqlite3vfs.LockReserved); err == nil {
		t.Error("A Lock(Reserved) from the un-adopted None = nil, want an error")
	}
	if err := fa.Lock(sqlite3vfs.LockExclusive); err == nil {
		t.Error("A Lock(Exclusive) from the un-adopted None = nil, want an error")
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("rejected lock requests changed the stored lock to %q, want exclusive", got)
	}
}

// TestTwoProcessesMutualExclusion is the core issue #47 test: two vfs
// instances — two processes — over ONE fake clientset, each file with its
// own holder id. Both may hold SHARED; only one may hold RESERVED; the
// other's attempt reports sqlite3vfs.BusyError; EXCLUSIVE waits (BUSY)
// while any other holder remains and succeeds once it releases.
func TestTwoProcessesMutualExclusion(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "mutual.db")
	fb := openTestFile(t, vb, "mutual.db")
	if fa.holderID == "" || fa.holderID == fb.holderID {
		t.Fatalf("holder ids must be distinct and non-empty, got %q twice", fa.holderID)
	}

	// Both take SHARED: any number of shared holders may coexist.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared) while A holds Shared: %v, want nil (readers coexist)", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if len(holders) != 2 {
		t.Fatalf("shared-holders = %v, want entries for both holders", holders)
	}
	for _, f := range []*file{fa, fb} {
		if _, ok := holders[f.holderID]; !ok {
			t.Errorf("shared-holders %v lacks %s's entry", holders, f.holderID)
		}
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("aggregate lock with two shared holders = %q, want %q", got, sqlite3vfs.LockShared.String())
	}

	// Only one writer: A takes RESERVED, B is refused with BusyError.
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Reserved) while A holds it = %v, want sqlite3vfs.BusyError", err)
	}
	if w := writerIn(t, fcs, fa); w == nil || w.ID != fa.holderID || w.Level != sqlite3vfs.LockReserved.String() {
		t.Fatalf("writer after A takes Reserved = %v, want A at LockReserved", w)
	}
	// B keeps its SHARED through the refused attempt, and the aggregate
	// still names the highest held level.
	if holders := sharedHoldersIn(t, fcs, fb); len(holders) != 2 {
		t.Errorf("shared-holders after B's refused Reserved = %v, want both entries intact", holders)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockReserved.String() {
		t.Errorf("aggregate with A Reserved + both Shared = %q, want %q", got, sqlite3vfs.LockReserved.String())
	}

	// A downgrades to SHARED; B can take RESERVED now.
	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Unlock(Shared): %v", err)
	}
	if w := writerIn(t, fcs, fa); w != nil {
		t.Errorf("writer after A downgraded = %v, want none", w)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved) after A downgraded: %v, want nil", err)
	}

	// EXCLUSIVE waits (BusyError) while another holder still holds SHARED.
	if err := fb.Lock(sqlite3vfs.LockExclusive); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Exclusive) while A holds Shared = %v, want sqlite3vfs.BusyError", err)
	}
	// The refused attempt leaves B holding PENDING, which blocks new
	// readers from now on.
	if w := writerIn(t, fcs, fb); w == nil || w.ID != fb.holderID || w.Level != sqlite3vfs.LockPending.String() {
		t.Fatalf("writer after B's refused Exclusive = %v, want B at LockPending", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockPending.String() {
		t.Errorf("aggregate with B Pending = %q, want %q", got, sqlite3vfs.LockPending.String())
	}
	fc := openTestFile(t, va, "mutual.db")
	if err := fc.Lock(sqlite3vfs.LockShared); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("C Lock(Shared) while B holds Pending = %v, want sqlite3vfs.BusyError (pending blocks readers)", err)
	}

	// A releases; B's EXCLUSIVE now succeeds and subsumes B's own entry.
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive) after A released: %v, want nil", err)
	}
	if holders := sharedHoldersIn(t, fcs, fb); len(holders) != 0 {
		t.Errorf("shared-holders after B Exclusive = %v, want empty (B's own entry subsumed)", holders)
	}
	if w := writerIn(t, fcs, fb); w == nil || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after B Exclusive = %v, want LockExclusive", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("aggregate after B Exclusive = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

// TestCASRetriesOnConflict pins the compare-and-swap: one injected 409 on
// the lock ConfigMap Update is retried — the CAS re-reads the state
// (including the other holder's entry) and re-decides, converging without a
// lost update.
//
// The 409 has to be injected by a hand-written reactor because the bare fake
// clientset cannot arbitrate it natively: its tracker (k8s.io/client-go/
// testing, fixture.go tracker.add) replaces the stored object on every
// Update without ever comparing the submitted resourceVersion against the
// stored one, and it keeps resourceVersions per GVR rather than per object,
// so no stale write is ever refused. newArbitratingFakeClientset in
// vfs_concurrent_test.go restores the real per-object
// resourceVersion/409/AlreadyExists semantics with reactors for the
// end-to-end two-instance tests; there is no configuration of the plain fake
// that produces a Conflict on its own.
func TestCASRetriesOnConflict(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "conflict.db")
	fb := openTestFile(t, v, "conflict.db")

	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}

	// The next lock ConfigMap Update fails once with a real 409.
	var attempts, injected int32
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != fa.LockFileName() {
			return false, nil, nil
		}
		if atomic.AddInt32(&attempts, 1) > 1 {
			return false, nil, nil
		}
		atomic.AddInt32(&injected, 1)
		return true, nil, kerrors.NewConflict(v1.Resource("configmaps"), fa.LockFileName(), errors.New("injected 409"))
	})
	// Count only the updates of the transition under test.
	fcs.ClearActions()

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved) under an injected 409 = %v, want nil (the CAS retries)", err)
	}
	if injected != 1 {
		t.Errorf("injected conflict count = %d, want exactly 1", injected)
	}
	if attempts != 2 {
		t.Errorf("lock update attempts seen by the reactor = %d, want 2 (one 409 + one retry)", attempts)
	}

	// The converged state carries BOTH B's shared entry (not lost by the
	// retry) and A's writer.
	holders := sharedHoldersIn(t, fcs, fa)
	if _, ok := holders[fb.holderID]; !ok {
		t.Errorf("shared-holders %v lost B's entry: the retry overwrote instead of re-deciding", holders)
	}
	if _, ok := holders[fa.holderID]; !ok {
		t.Errorf("shared-holders %v lost A's entry", holders)
	}
	if w := writerIn(t, fcs, fa); w == nil || w.ID != fa.holderID || w.Level != sqlite3vfs.LockReserved.String() {
		t.Errorf("writer after retried Reserved = %v, want A at LockReserved", w)
	}

	// Exactly two Update attempts were made against the lock ConfigMap:
	// the conflicted one and the successful retry.
	updates := 0
	for _, a := range fcs.Actions() {
		if a.GetVerb() == "update" && a.GetResource().Resource == "configmaps" {
			updates++
		}
	}
	if updates != 2 {
		t.Errorf("lock ConfigMap updates = %d, want 2 (one 409 + one retry)", updates)
	}
}

// alwaysConflictReactor injects a real 409 Conflict on every Update of the
// named lock ConfigMap — the bare fake clientset cannot produce one natively
// (see the comment on TestCASRetriesOnConflict) — and counts the attempts it
// saw.
func alwaysConflictReactor(fcs *fake.Clientset, lockCM string) *int32 {
	var attempts int32
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ua.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != lockCM {
			return false, nil, nil
		}
		atomic.AddInt32(&attempts, 1)
		return true, nil, kerrors.NewConflict(v1.Resource("configmaps"), lockCM, errors.New("injected 409"))
	})
	return &attempts
}

// TestLockCASExhaustionReturnsBusy pins the error class when the compare-
// and-swap loop runs out of attempts under sustained contention (issue
// #479): a lock Update that always returns 409 must surface as
// sqlite3vfs.BusyError — SQLITE_BUSY, which sqlite runs its busy handler
// for — not the plain error the loop used to end with, which reached the
// cgo shim's bare type assertion and degraded to a generic SQLITE_ERROR
// that hard-fails Lock. maxLockCASAttempts bounds the loop, so exactly that
// many Updates are attempted.
func TestLockCASExhaustionReturnsBusy(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "cas-exhaustion.db")

	attempts := alwaysConflictReactor(fcs, f.LockFileName())

	err := f.Lock(sqlite3vfs.LockShared)
	if err != sqlite3vfs.BusyError {
		t.Fatalf("Lock(Shared) under sustained 409s = %v (%T), want bare sqlite3vfs.BusyError", err, err)
	}
	if got := atomic.LoadInt32(attempts); got != maxLockCASAttempts {
		t.Errorf("lock update attempts = %d, want %d (the CAS bound)", got, maxLockCASAttempts)
	}
	// Nothing was written under the exhausted CAS: the file still holds
	// nothing, locally and in the store.
	if f.lockLevel != sqlite3vfs.LockNone || f.lockKnown {
		t.Errorf("local lock state after exhausted CAS = level %v known %v, want NONE and unknown", f.lockLevel, f.lockKnown)
	}
	if holders := sharedHoldersIn(t, fcs, f); len(holders) != 0 {
		t.Errorf("shared-holders %v written despite the exhausted CAS, want none", holders)
	}
}

// TestRenewCASExhaustionReturnsBusyWithoutLeaseLoss pins the maybeRenew
// path flagged in the discussion on issue #479: the lease renewal rides the
// same casLock, so its exhaustion under sustained contention also surfaces
// as SQLITE_BUSY — on Lock, and on ReadAt/WriteAt through renewForIO and
// ioFailure. That is arguably right (contention, not corruption), but it
// must NOT be mistaken for a lost lease: noteLeaseLost matches only
// errLockLeaseLost, so leaseLost stays unset and the sticky read/write gate
// must not engage.
func TestRenewCASExhaustionReturnsBusyWithoutLeaseLoss(t *testing.T) {
	const lease = time.Minute

	// The lock path: maybeRenew itself returns the bare BusyError.
	t.Run("lock-path", func(t *testing.T) {
		fcs := fake.NewSimpleClientset()
		clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
		v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
		v.clock = clock.Now

		f := openTestFile(t, v, "renew-exhaustion-lock.db")
		if err := f.Lock(sqlite3vfs.LockShared); err != nil {
			t.Fatalf("Lock(Shared): %v", err)
		}

		// Renewal only fires once less than half the lease remains; move
		// the fake clock past that point so maybeRenew actually CASes.
		clock.Advance(lease/2 + time.Second)

		attempts := alwaysConflictReactor(fcs, f.LockFileName())

		err := f.maybeRenew()
		if err != sqlite3vfs.BusyError {
			t.Fatalf("maybeRenew under sustained 409s = %v (%T), want bare sqlite3vfs.BusyError", err, err)
		}
		if got := atomic.LoadInt32(attempts); got != maxLockCASAttempts {
			t.Errorf("renewal update attempts = %d, want %d (the CAS bound)", got, maxLockCASAttempts)
		}
		if f.leaseLost {
			t.Errorf("leaseLost set by renewal CAS exhaustion: SQLITE_BUSY from contention must not engage the sticky read/write gate")
		}
		if f.lockLevel != sqlite3vfs.LockShared || !f.lockKnown {
			t.Errorf("local lock state after exhausted renewal = level %v known %v, want SHARED still held", f.lockLevel, f.lockKnown)
		}
	})

	// The sector read/write path: renewForIO reaches the same exhausted
	// renewal and ioFailure maps it to SQLITE_BUSY for sqlite.
	t.Run("io-path", func(t *testing.T) {
		fcs := fake.NewSimpleClientset()
		clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
		v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
		v.clock = clock.Now

		f := openTestFile(t, v, "renew-exhaustion-io.db")
		if err := f.Lock(sqlite3vfs.LockShared); err != nil {
			t.Fatalf("Lock(Shared): %v", err)
		}
		clock.Advance(lease/2 + time.Second)

		attempts := alwaysConflictReactor(fcs, f.LockFileName())

		// WriteAt, not ReadAt: the file is still empty, so a read returns
		// io.EOF before its sector work and never reaches renewal.
		if _, err := f.WriteAt([]byte("x"), 0); err != sqlite3vfs.BusyError {
			t.Errorf("WriteAt under renewal CAS exhaustion = %v (%T), want sqlite3vfs.BusyError", err, err)
		}
		if got := atomic.LoadInt32(attempts); got != maxLockCASAttempts {
			t.Errorf("renewal update attempts = %d, want %d (the CAS bound)", got, maxLockCASAttempts)
		}
		if f.leaseLost {
			t.Errorf("leaseLost set by renewal CAS exhaustion on the write path, want unset")
		}
		if f.lockLevel != sqlite3vfs.LockShared || !f.lockKnown {
			t.Errorf("local lock state after exhausted renewal = level %v known %v, want SHARED still held", f.lockLevel, f.lockKnown)
		}
	})
}

// TestRenewExhaustedCASLeavesLocalTimerBehind pins the maybeRenew side of
// issue #499: when a renewal CAS is exhausted — every Update answered 409 —
// the local renewal timer must stay where the last successful write left it.
// stampOwnLease once committed the expiry it stamped into f.leaseExpires
// inside the mutate path, before the ConfigMap Update was known to have
// succeeded, so an exhausted renewal recorded an extension the cluster
// never saw and maybeRenew then skipped renewal for another lockLease/2 —
// long enough for the cluster-side lease to lapse and another process to
// prune this holder while it still believed the lock held. The test also
// pins the other half of the contract: a renewal whose CAS does land still
// advances the timer.
func TestRenewExhaustedCASLeavesLocalTimerBehind(t *testing.T) {
	const lease = time.Minute

	fcs := fake.NewSimpleClientset()
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "renew-timer.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	held := f.leaseExpires // the expiry the successful Lock wrote and adopted

	conflict := true
	var attempts int32
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := action.(k8stesting.UpdateAction)
		if !ok || !conflict {
			return false, nil, nil
		}
		if cm, ok := ua.GetObject().(*v1.ConfigMap); !ok || cm.Name != f.LockFileName() {
			return false, nil, nil
		}
		atomic.AddInt32(&attempts, 1)
		return true, nil, kerrors.NewConflict(v1.Resource("configmaps"), f.LockFileName(), errors.New("injected 409"))
	})

	// Renewal only fires once less than half the lease remains; move the
	// fake clock past that point so maybeRenew actually CASes.
	clock.Advance(lease/2 + time.Second)
	if err := f.maybeRenew(); err != sqlite3vfs.BusyError {
		t.Fatalf("maybeRenew under sustained 409s = %v (%T), want bare sqlite3vfs.BusyError", err, err)
	}
	if got := atomic.LoadInt32(&attempts); got != maxLockCASAttempts {
		t.Errorf("renewal update attempts = %d, want %d (the CAS bound)", got, maxLockCASAttempts)
	}
	if !f.leaseExpires.Equal(held) {
		t.Errorf("exhausted renewal advanced f.leaseExpires to %v, want it still at the last successful write %v (issue #499)", f.leaseExpires, held)
	}

	// The local timer never moved, so the very next maybeRenew — no clock
	// advance — is still due and attempts the CAS again, instead of the old
	// behaviour of waiting out another lockLease/2 while the cluster lease
	// lapses.
	if err := f.maybeRenew(); err != sqlite3vfs.BusyError {
		t.Fatalf("second maybeRenew after the exhausted one = %v (%T), want sqlite3vfs.BusyError (the renewal must be re-attempted, not skipped)", err, err)
	}
	if got := atomic.LoadInt32(&attempts); got != 2*maxLockCASAttempts {
		t.Errorf("renewal update attempts after the second maybeRenew = %d, want %d (the next renewal must attempt its own CAS bound)", got, 2*maxLockCASAttempts)
	}
	if !f.leaseExpires.Equal(held) {
		t.Errorf("second exhausted renewal still advanced f.leaseExpires to %v, want %v", f.leaseExpires, held)
	}

	// Contention ends: the next renewal CAS lands, and only then does the
	// local timer advance past the expiry the store used to carry.
	conflict = false
	if err := f.maybeRenew(); err != nil {
		t.Fatalf("maybeRenew once the 409s stop: %v, want nil", err)
	}
	want := clock.Now().UTC().Add(lease)
	if !f.leaseExpires.Equal(want) {
		t.Errorf("successful renewal left f.leaseExpires at %v, want %v (clock + lease)", f.leaseExpires, want)
	}
	holders := sharedHoldersIn(t, fcs, f)
	if got := holders[f.holderID]; !got.Equal(want) {
		t.Errorf("stored shared-holder lease after successful renewal = %v, want %v", got, want)
	}
}

// TestOpenCASExhaustionReturnsBusy pins Open's fresh-lockfile CAS path
// flagged in the discussion on issue #479: when creating the initial lock
// state never settles, Open's final error must carry the BusyError class.
// xOpen never runs sqlite's busy handler, so nothing is gained or lost by
// the class itself, but SQLITE_BUSY names the truth (contention) where the
// wrapped plain error Open used to return degraded through the shim's bare
// type assertion to a generic SQLITE_ERROR that claims malformed state.
func TestOpenCASExhaustionReturnsBusy(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := NewFile("open-exhaustion.db", v)

	// Contention on the fresh-lockfile CAS is an always-lost Create race:
	// the Get finds nothing (nothing exists yet) and the Create is answered
	// AlreadyExists, which casLock treats as "re-read" — the loop then
	// re-reads NotFound and Creates again, until the bound runs out.
	var createAttempts int32
	fcs.PrependReactor("create", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(k8stesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		cm, ok := ca.GetObject().(*v1.ConfigMap)
		if !ok || cm.Name != f.LockFileName() {
			return false, nil, nil
		}
		atomic.AddInt32(&createAttempts, 1)
		return true, nil, kerrors.NewAlreadyExists(v1.Resource("configmaps"), f.LockFileName())
	})

	_, _, err := v.Open("open-exhaustion.db", 0)
	if err != sqlite3vfs.BusyError {
		t.Fatalf("Open under sustained AlreadyExists on the fresh lockfile = %v (%T), want bare sqlite3vfs.BusyError", err, err)
	}
	// The CAS bound is spent once per outer retry (v.retries = 3 here).
	if got := atomic.LoadInt32(&createAttempts); got != maxLockCASAttempts*3 {
		t.Errorf("lock create attempts = %d, want %d (CAS bound x retries)", got, maxLockCASAttempts*3)
	}
}

// TestStaleHolderPruned pins lease expiry: a holder that stops renewing —
// a crashed process — is pruned by the next CAS of another process, so its
// shared entry and writer stop blocking RESERVED and EXCLUSIVE even though
// it never unlocked.
func TestStaleHolderPruned(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 80 * time.Millisecond
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3) // default 30s lease

	fa := openTestFile(t, va, "stale-holder.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}

	// B (with its long default lease) joins as a reader; A's writer blocks
	// B's RESERVED.
	fb := openTestFile(t, vb, "stale-holder.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("B Lock(Reserved) while A (live) holds it = %v, want sqlite3vfs.BusyError", err)
	}

	// A dies: no more renewals. Sleep past its lease.
	time.Sleep(lease + 80*time.Millisecond)

	// B's next CAS prunes A's expired entries: the RESERVED is free again
	// even though A never unlocked, and B can climb to EXCLUSIVE.
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved) after A's lease expired: %v, want nil (stale holder pruned)", err)
	}
	if err := fb.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("B Lock(Exclusive) after A's lease expired: %v, want nil", err)
	}
	holders := sharedHoldersIn(t, fcs, fb)
	if _, ok := holders[fa.holderID]; ok {
		t.Errorf("A's expired entry survived B's CAS: %v", holders)
	}
	if w := writerIn(t, fcs, fb); w == nil || w.ID != fb.holderID || w.Level != sqlite3vfs.LockExclusive.String() {
		t.Errorf("writer after B Exclusive = %v, want B at LockExclusive (A's stale writer pruned)", w)
	}
	if got := lockState(t, fcs, fb); got != sqlite3vfs.LockExclusive.String() {
		t.Errorf("aggregate after B Exclusive = %q, want %q", got, sqlite3vfs.LockExclusive.String())
	}
}

// TestLeaseRenewedOnLockPathOps pins renewal: a no-op Lock on the level
// already held renews a lease that is under half spent, and the renewed
// entry is still live — unpruned by another process's CAS — after the
// original expiry has passed. The renewal never touches the sector read
// path, which is separately pinned by TestSectorReadPathMakesNoLockCalls.
func TestLeaseRenewedOnLockPathOps(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 120 * time.Millisecond
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "renewal.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	first := sharedHoldersIn(t, fcs, fa)[fa.holderID]

	// Past half the lease, still before expiry: the no-op Lock renews.
	time.Sleep(lease/2 + 20*time.Millisecond)
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A no-op Lock(Shared) (should renew): %v", err)
	}
	second := sharedHoldersIn(t, fcs, fa)[fa.holderID]
	if !second.After(first) {
		t.Fatalf("renewal did not move the lease forward: %v -> %v", first, second)
	}

	// The original expiry is now past. Another process's CAS must find A's
	// renewed entry live and leave it alone.
	time.Sleep(lease/2 + 20*time.Millisecond)
	fb := openTestFile(t, vb, "renewal.db")
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("B Lock(Reserved): %v", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if _, ok := holders[fa.holderID]; !ok {
		t.Error("A's renewed entry was pruned although it never lapsed")
	}
}

// TestLeaseLossDetected pins the losing side of a lapsed lease: a file that
// made no lock-path call past its own lease discovers, on its next no-op
// lock operation, that its entries are gone — the local belief is reset and
// the loss is reported instead of writing under a false belief.
func TestLeaseLossDetected(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	lease := 40 * time.Millisecond
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))

	fa := openTestFile(t, v, "lost.db")
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	time.Sleep(2 * lease) // the lease lapses with no lock-path call

	// Issue #51: the exact error, checked with errors.Is in both
	// directions. A lost lease is deliberately not a conflict — BusyError
	// (SQLITE_BUSY) would send sqlite's busy handler into retrying the
	// same request, whose holder state is already gone.
	err := fa.Lock(sqlite3vfs.LockShared)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("Lock after lapsed lease = %v, want errLockLeaseLost", err)
	}
	if errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Lock after lapsed lease = %v, want a distinct error and not sqlite3vfs.BusyError: BUSY would make sqlite retry a request whose holder state is gone", err)
	}
	if fa.lockLevel != sqlite3vfs.LockNone || !fa.lockKnown {
		t.Errorf("after lease loss local state = (level %v, known %v), want (NONE, true)", fa.lockLevel, fa.lockKnown)
	}

	// The file re-acquires cleanly, as a fresh holder.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the loss: %v", err)
	}
	if holders := sharedHoldersIn(t, fcs, fa); len(holders) != 1 {
		t.Errorf("shared-holders after re-acquire = %v, want exactly one entry", holders)
	}
}

// assertLossGatesSectorWrite checks both halves of a lease loss found by an
// upgrade mutation (issue #412): the sticky loss must gate the sector write
// path — WriteAt refused with sqlite3vfs.BusyError and no ConfigMap API
// call, so not a single sector is written — and the refused write must not
// have landed: after re-acquiring SHARED the file still reads back the
// bytes committed before the loss, not the pattern WriteAt carried.
func assertLossGatesSectorWrite(t *testing.T, fcs *fake.Clientset, f *file, kept []byte) {
	t.Helper()
	alt := make([]byte, len(kept))
	fillPatternAlt(alt)
	callsBefore := len(fcs.Actions())
	if _, err := f.WriteAt(alt, 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("WriteAt after the upgrade lease loss = %v, want sqlite3vfs.BusyError", err)
	}
	if n := len(fcs.Actions()) - callsBefore; n != 0 {
		t.Errorf("refused WriteAt caused %d ConfigMap API calls, want 0 (refused before any sector write)", n)
	}

	// Re-acquire SHARED as a fresh holder, and read back what is stored:
	// the refused write must not have landed.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the loss: %v", err)
	}
	got := make([]byte, len(kept))
	if n, err := f.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt after re-lock: %v", err)
	} else if !bytes.Equal(got[:n], kept[:n]) {
		t.Errorf("stored bytes after the refused write = %q, want the pre-loss pattern (the write must not have landed)", got[:n])
	}
}

// TestLeaseLossOnReservedUpgrade pins the lapsed-lease behaviour of the
// SHARED->RESERVED upgrade (issue #412): with this file's shared entry
// pruned from the lock ConfigMap — the lease lapsed and another process
// took the holder away — Lock(RESERVED) must report errLockLeaseLost, not
// a plain "invalid lock transition" that leaves the file writing sectors
// under a lock the cluster has given away. noteLeaseLost resets the local
// state, so the sticky loss gates the sector write path next.
func TestLeaseLossOnReservedUpgrade(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	f := openTestFile(t, v, "lost-upgrade.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	kept := make([]byte, 64)
	fillPattern(kept)
	mustWriteAt(t, f, kept, 0)

	// Another process prunes this file's shared entry while the local
	// state still believes SHARED.
	pruneHolderFromLockCM(t, fcs, f, true, false)
	err := f.Lock(sqlite3vfs.LockReserved)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("Lock(Reserved) after the shared entry was pruned = %v, want errLockLeaseLost", err)
	}
	if errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Lock(Reserved) after the prune = %v, want the distinct lease-loss error and not sqlite3vfs.BusyError: BUSY would make sqlite retry a request whose holder state is gone", err)
	}
	if f.lockLevel != sqlite3vfs.LockNone || !f.lockKnown || !f.leaseLost {
		t.Errorf("after the upgrade lease loss local state = (level %v, known %v, lost %v), want (NONE, true, true)", f.lockLevel, f.lockKnown, f.leaseLost)
	}

	assertLossGatesSectorWrite(t, fcs, f, kept)
}

// TestLeaseLossOnExclusiveClaim pins the lapsed-lease behaviour of the
// first CAS step of Lock(EXCLUSIVE) (issue #412): with this file's writer
// and shared entries pruned from the lock ConfigMap while the local state
// still believes RESERVED, the PENDING claim must report errLockLeaseLost,
// not a plain "invalid lock transition request" that leaves the file
// writing sectors under a lock the cluster has given away. (A prune of the
// writer entry alone leaves a live shared entry, which is a re-claim, not
// a lease loss; pruneExpired drops all of a lapsed holder's entries, whose
// leases are stamped together, so both go here.)
func TestLeaseLossOnExclusiveClaim(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	f := openTestFile(t, v, "lost-claim.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	kept := make([]byte, 64)
	fillPattern(kept)
	mustWriteAt(t, f, kept, 0)

	// Another process prunes this file's writer entry — with its shared
	// entry, whose lease is the same — while the local state still
	// believes RESERVED.
	pruneHolderFromLockCM(t, fcs, f, true, true)
	err := f.Lock(sqlite3vfs.LockExclusive)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("Lock(Exclusive) after the writer entry was pruned = %v, want errLockLeaseLost", err)
	}
	if errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Lock(Exclusive) after the prune = %v, want the distinct lease-loss error and not sqlite3vfs.BusyError: BUSY would make sqlite retry a request whose holder state is gone", err)
	}
	if f.lockLevel != sqlite3vfs.LockNone || !f.lockKnown || !f.leaseLost {
		t.Errorf("after the claim lease loss local state = (level %v, known %v, lost %v), want (NONE, true, true)", f.lockLevel, f.lockKnown, f.leaseLost)
	}

	assertLossGatesSectorWrite(t, fcs, f, kept)
}

// TestLeaseLossOnUnlockDowngrade pins the lapsed-lease behaviour of the
// downgrade step of Unlock (issue #437): with this file's writer and shared
// entries pruned from the lock ConfigMap while the local state still
// believes RESERVED, Unlock(SHARED) must report errLockLeaseLost, not the
// plain "cannot unlock to a level above the current lock level" transition
// error. The plain error leaves leaseLost unset while Unlock has already
// cleared lockKnown, so renewForIO finds both guards down and WriteAt goes
// on writing sectors under a lock the cluster has given away. (A file that
// never held the lock keeps the plain transition error: Unlock's early
// guard rejects it before the CAS, and mutateUnlock only reports the loss
// when the pre-CAS local level reached elock.)
func TestLeaseLossOnUnlockDowngrade(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	f := openTestFile(t, v, "lost-unlock.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if err := f.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved): %v", err)
	}
	kept := make([]byte, 64)
	fillPattern(kept)
	mustWriteAt(t, f, kept, 0)

	// Another process prunes this file's writer entry — with its shared
	// entry, whose lease is the same — while the local state still
	// believes RESERVED.
	pruneHolderFromLockCM(t, fcs, f, true, true)
	err := f.Unlock(sqlite3vfs.LockShared)
	if !errors.Is(err, errLockLeaseLost) {
		t.Fatalf("Unlock(Shared) after the entries were pruned = %v, want errLockLeaseLost", err)
	}
	if errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Unlock(Shared) after the prune = %v, want the distinct lease-loss error and not sqlite3vfs.BusyError: BUSY would make sqlite retry a request whose holder state is gone", err)
	}
	if f.lockLevel != sqlite3vfs.LockNone || !f.lockKnown || !f.leaseLost {
		t.Errorf("after the unlock lease loss local state = (level %v, known %v, lost %v), want (NONE, true, true)", f.lockLevel, f.lockKnown, f.leaseLost)
	}

	assertLossGatesSectorWrite(t, fcs, f, kept)
}

// fakeClock is the injectable clock the lease tests drive: the test advances
// it instead of sleeping, and the VFS's whole lock path stamps, prunes and
// renews against it through vfs.clock. It is called from the test goroutine
// only.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// TestLeaseRenewedFromIOPathWhenNearExpiry pins lease renewal from the
// sector read/write path: a read or write stretch longer than half the lease
// must not outrun renewal, or the lease lapses while WriteAt keeps writing
// (the review finding on the #47 locking). The injected clock moves time
// without sleeps; the API-call counts pin that a live lease still costs the
// read/write path nothing at all and a due renewal exactly one CAS.
func TestLeaseRenewedFromIOPathWhenNearExpiry(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "io-renewal.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	first := sharedHoldersIn(t, fcs, f)[f.holderID]
	buf := make([]byte, 64)
	fillPattern(buf)

	// Warm the sector cache outside the counted windows: the counts below
	// measure the lease machinery, not the cold-cache sector fetch a first
	// write legitimately pays (one GET of sector 0). FileSize is the
	// revalidating call that warms it — one LIST, happening with 60s of
	// lease left, so it renews nothing itself. (A ReadAt would not: the
	// file is still empty, so it returns io.EOF before touching sectors.)
	if _, err := f.FileSize(); err != nil {
		t.Fatalf("cache-warming FileSize: %v", err)
	}

	// A write well inside the lease renews nothing: exactly one API call
	// (the sector write), and the stored lease does not move.
	clock.Advance(lease / 4) // 15s in: 45s of the 60s lease remain
	before := len(fcs.Actions())
	if n, err := f.WriteAt(buf, 0); err != nil || n != len(buf) {
		t.Fatalf("WriteAt at quarter lease = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	if got := len(fcs.Actions()) - before; got != 1 {
		t.Errorf("write at quarter lease caused %d API calls, want exactly 1 (the sector write; no lock calls)", got)
	}
	if got := sharedHoldersIn(t, fcs, f)[f.holderID]; !got.Equal(first) {
		t.Errorf("stored lease moved at quarter lease: %v -> %v, want unchanged", first, got)
	}

	// Past half the lease the same write renews first: the renewal CAS is
	// one Get plus one Update on top of the sector write, and the stored
	// expiry is now+lease as of the renewal.
	clock.Advance(lease / 3) // 35s in: 25s remain, under half
	before = len(fcs.Actions())
	if n, err := f.WriteAt(buf, 128); err != nil || n != len(buf) {
		t.Fatalf("WriteAt past half lease = (%d, %v), want (%d, nil)", n, err, len(buf))
	}
	// Count before the verification read below: sharedHoldersIn is itself
	// a fake-clientset GET, and it must not be counted as the VFS's.
	calls := len(fcs.Actions()) - before
	second := sharedHoldersIn(t, fcs, f)[f.holderID]
	if want := first.Add(35 * time.Second); !second.Equal(want) {
		t.Errorf("stored lease after write past half = %v, want %v (renewed to now+lease at the renewal)", second, want)
	}
	if got := calls; got != 3 {
		t.Errorf("write past half the lease caused %d API calls, want 3 (renewal Get + Update, then the sector write)", got)
	}

	// The read path renews too, on the same half-spent rule.
	clock.Advance(lease/2 + time.Second) // 66s after the renewal: 29s remain
	if _, err := f.ReadAt(make([]byte, 8), 0); err != nil {
		t.Fatalf("ReadAt past half lease: %v", err)
	}
	third := sharedHoldersIn(t, fcs, f)[f.holderID]
	if want := second.Add(31 * time.Second); !third.Equal(want) {
		t.Errorf("stored lease after read past half = %v, want %v", third, want)
	}
}

// TestLeaseLossFailsSectorIOUntilRelock pins the losing side of the gate:
// once the lease has lapsed, WriteAt is refused instead of writing without
// the lock, the loss sticks so ReadAt is refused too (instead of serving
// sectors cached under the lost lock), and a successful Lock clears it —
// after which the file reads exactly the bytes committed before the loss,
// proving the refused write never landed.
func TestLeaseLossFailsSectorIOUntilRelock(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "io-loss.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	kept := make([]byte, 64)
	fillPattern(kept)
	if n, err := f.WriteAt(kept, 0); err != nil || n != len(kept) {
		t.Fatalf("WriteAt while live = (%d, %v), want (%d, nil)", n, err, len(kept))
	}

	clock.Advance(2 * lease) // the lease lapses with no renewal
	if _, err := f.WriteAt(make([]byte, 64), 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("WriteAt with a lapsed lease = %v, want sqlite3vfs.BusyError", err)
	}
	if f.lockLevel != sqlite3vfs.LockNone || !f.lockKnown || !f.leaseLost {
		t.Errorf("after lease loss local state = (level %v, known %v, lost %v), want (NONE, true, true)", f.lockLevel, f.lockKnown, f.leaseLost)
	}

	if _, err := f.ReadAt(make([]byte, 64), 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("ReadAt with a lost lease = %v, want sqlite3vfs.BusyError (sticky)", err)
	}

	// Truncate writes and deletes sectors too, so the loss gates it as
	// well: refused with BusyError, and with zero ConfigMap writes or
	// deletes recorded — refused before any cluster mutation, not after.
	callsBeforeTruncate := len(fcs.Actions())
	if err := f.Truncate(32); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Truncate with a lost lease = %v, want sqlite3vfs.BusyError", err)
	}
	if n := len(fcs.Actions()) - callsBeforeTruncate; n != 0 {
		t.Errorf("Truncate with a lost lease caused %d ConfigMap API calls, want 0", n)
	}

	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the loss: %v", err)
	}
	if f.leaseLost {
		t.Error("leaseLost survived a successful Lock; the read/write path would stay gated forever")
	}
	got := make([]byte, len(kept))
	n, err := f.ReadAt(got, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt after re-lock: %v", err)
	}
	if n != len(kept) || !bytes.Equal(got, kept) {
		t.Errorf("bytes after re-lock = (%d, %v), want the pre-loss %d bytes (the refused write must not have landed)", n, got, len(kept))
	}

	// The gate clears for Truncate exactly as for ReadAt/WriteAt: the
	// successful re-Lock re-enabled it, and the truncate lands.
	if err := f.Truncate(32); err != nil {
		t.Fatalf("Truncate after re-lock: %v", err)
	}
	got = make([]byte, len(kept))
	n, err = f.ReadAt(got, 0)
	if err != io.EOF || n != 32 || !bytes.Equal(got[:32], kept[:32]) {
		t.Errorf("after re-locked Truncate(32) read = (%d, %v, %v), want (32, io.EOF, the kept pattern)", n, err, got)
	}
}

// TestLeaseLossMidWriteStopsSectorLoop pins the mid-call side of the write
// gate (issue #459): a single WriteAt that spans several sectors re-checks
// the lease for every sector, not only at entry. Each sector write is its
// own Kubernetes call on the construction context, with no timeout, so one
// slow UPDATE on a large write can push the loop past the lease expiry — at
// which point another process prunes this holder and takes EXCLUSIVE, and
// this loop must stop with sqlite3vfs.BusyError instead of writing sectors
// under a lock the cluster has given away. The injected fake clock is
// advanced by a ConfigMap reactor running during sector 0's write — after
// the first sector write and before the second — so this one WriteAt call
// outlives its own lease mid-loop.
func TestLeaseLossMidWriteStopsSectorLoop(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "mid-write-loss.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	// Warm the sector cache outside the interesting call, as the renew-on-
	// I/O test does: FileSize's LIST runs while the lease is fresh, so the
	// only call the WriteAt below makes before the lapse is sector 0's write.
	if _, err := f.FileSize(); err != nil {
		t.Fatalf("cache-warming FileSize: %v", err)
	}

	// Advance the clock past the lease exactly once, from a reactor that
	// runs during sector 0's UPDATE — between the two sector writes of the
	// single WriteAt below. It hands the action back to the fake clientset
	// (false), so sector 0's write itself still lands.
	sector0 := f.sectorNameFromSectorIndex(0)
	advanced := false
	fcs.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !advanced && action.(k8stesting.UpdateAction).GetObject().(*v1.ConfigMap).Name == sector0 {
			advanced = true
			clock.Advance(2 * lease)
		}
		return false, nil, nil
	})

	// One write straddling the sector 0/1 boundary: sector 0 is written
	// under the live lease, and the loop must never reach sector 1.
	buf := make([]byte, 64)
	fillPattern(buf)
	n, err := f.WriteAt(buf, SectorSize-32)
	if !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("WriteAt whose sector loop outlived the lease = %v, want sqlite3vfs.BusyError", err)
	}
	if n != 32 {
		t.Errorf("WriteAt reported %d bytes written, want the 32 written under the live lease", n)
	}
	if !f.leaseLost {
		t.Error("mid-write lease loss did not stick: leaseLost = false, want true")
	}

	// No sector was written after the lease lapsed: sector 1 — the only
	// sector that would have followed — never got a ConfigMap...
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.sectorNameFromSectorIndex(1), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("sector 1 after the mid-write lease loss: get err %v, want NotFound (no sector written past the lapse)", err)
	}
	// ...and sector 0 holds exactly what was written under the live lease.
	cm, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), sector0, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get sector 0: %v", err)
	}
	got := cm.BinaryData["sector"]
	if len(got) != SectorSize || !bytes.Equal(got[SectorSize-32:], buf[:32]) {
		t.Errorf("sector 0 after the mid-write loss = (%d bytes, tail %v), want (%d, the 32 bytes written under the live lease)", len(got), got[SectorSize-32:], SectorSize)
	}

	// The mid-loop loss is sticky exactly like one found at entry: the next
	// WriteAt is refused before any API call at all.
	alt := make([]byte, 64)
	fillPatternAlt(alt)
	callsBefore := len(fcs.Actions())
	if _, err := f.WriteAt(alt, 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("WriteAt after the mid-write lease loss = %v, want sqlite3vfs.BusyError (sticky)", err)
	}
	if n := len(fcs.Actions()) - callsBefore; n != 0 {
		t.Errorf("sticky refusal caused %d ConfigMap API calls, want 0", n)
	}

	// A successful re-Lock re-establishes the holder and clears the loss,
	// and the refused sectors never landed: only sector 0, pre-lapse, is
	// on the cluster.
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the mid-write loss: %v", err)
	}
	out := make([]byte, 2*SectorSize)
	if n, err := f.ReadAt(out, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt after re-lock: %v", err)
	} else if n != SectorSize || !bytes.Equal(out[SectorSize-32:SectorSize], buf[:32]) {
		t.Errorf("stored bytes after the re-lock = (%d, %v), want (%d, the 32 pre-lapse pattern bytes at the tail of sector 0 and nothing more)", n, out[SectorSize-32:SectorSize], SectorSize)
	}
}

// TestLeaseLossMidReadStopsSectorLoop pins the read side of the mid-call
// gate (issue #459): ReadAt fetches its sectors up front (one GET each for
// a sector not in the cache) and then serves them one by one, so a
// multi-sector read can cross the lease expiry while fetching — and once
// another process has pruned this holder, the sectors it is rewriting must
// not be served. The fake clock is advanced by a reactor running between
// the GETs of sector 0 and sector 1 within the one ReadAt call; the
// re-check at the top of the serving loop must fail the whole read with
// sqlite3vfs.BusyError and serve nothing.
func TestLeaseLossMidReadStopsSectorLoop(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "mid-read-loss.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	bufA := make([]byte, 64)
	fillPattern(bufA)
	mustWriteAt(t, f, bufA, 0)
	bufB := make([]byte, 64)
	fillPatternAlt(bufB)
	mustWriteAt(t, f, bufB, SectorSize)

	// FileSize warms the cache (one LIST, fresh lease), then the cache is
	// reset the way cacheSector's cap does: when the 256-sector cap is
	// exceeded it drops the WHOLE map, so a locked file over the cap reads
	// every sector with a real GET. Simulating that reset is what lets the
	// reactor below observe the ReadAt between two of its sector GETs.
	if _, err := f.FileSize(); err != nil {
		t.Fatalf("cache-warming FileSize: %v", err)
	}
	f.sectors = make(map[int64][]byte)

	sector0 := f.sectorNameFromSectorIndex(0)
	advanced := false
	fcs.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !advanced && action.(k8stesting.GetAction).GetName() == sector0 {
			advanced = true
			clock.Advance(2 * lease)
		}
		return false, nil, nil
	})

	out := make([]byte, 2*SectorSize)
	n, err := f.ReadAt(out, 0)
	if !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("ReadAt whose sector fetch outlived the lease = %v, want sqlite3vfs.BusyError", err)
	}
	if n != 0 {
		t.Errorf("ReadAt after the mid-read lease loss served %d bytes, want 0", n)
	}
	if !bytes.Equal(out, make([]byte, len(out))) {
		t.Error("ReadAt after the mid-read lease loss filled the buffer, want every byte unserved")
	}
	if !f.leaseLost {
		t.Error("mid-read lease loss did not stick: leaseLost = false, want true")
	}

	// The loss is sticky: the next ReadAt is refused at entry with no API
	// call, and a successful re-Lock re-enables the path — after which the
	// stored sectors read back exactly as written, proving the refused read
	// served nothing it should not have.
	callsBefore := len(fcs.Actions())
	if _, err := f.ReadAt(out, 0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("ReadAt after the mid-read lease loss = %v, want sqlite3vfs.BusyError (sticky)", err)
	}
	if n := len(fcs.Actions()) - callsBefore; n != 0 {
		t.Errorf("sticky refusal caused %d ConfigMap API calls, want 0", n)
	}
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("re-Lock(Shared) after the mid-read loss: %v", err)
	}
	if n, err := f.ReadAt(out, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt after re-lock: %v", err)
	} else if n != SectorSize+64 || !bytes.Equal(out[:64], bufA) || !bytes.Equal(out[SectorSize:SectorSize+64], bufB) {
		t.Errorf("stored bytes after re-lock = (%d, %v), want (%d, the two committed patterns)", n, out[:128], SectorSize+64)
	}
}

// TestLeaseLossMidTruncateStopsSectorLoop pins the Truncate half of the
// mid-call gate (issue #459): the delete loop runs once per sector of the
// file and can outlive its entry gate the same way WriteAt can, so a lease
// that lapses after the first sector delete must stop the loop with
// sqlite3vfs.BusyError and leave the later sectors un-deleted. The fake
// clock is advanced by a reactor running between the deletes of sector 0
// and sector 1 of the one Truncate call.
func TestLeaseLossMidTruncateStopsSectorLoop(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	const lease = time.Minute
	clock := &fakeClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	v := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3, WithLockLease(lease))
	v.clock = clock.Now

	f := openTestFile(t, v, "mid-truncate-loss.db")
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	sect := make([]byte, SectorSize)
	fillPattern(sect)
	for i := range 3 {
		mustWriteAt(t, f, sect, int64(i)*SectorSize)
	}

	sector0 := f.sectorNameFromSectorIndex(0)
	advanced := false
	fcs.PrependReactor("delete", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if !advanced && action.(k8stesting.DeleteAction).GetName() == sector0 {
			advanced = true
			clock.Advance(2 * lease)
		}
		return false, nil, nil
	})

	if err := f.Truncate(0); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("Truncate whose delete loop outlived the lease = %v, want sqlite3vfs.BusyError", err)
	}
	if !f.leaseLost {
		t.Error("mid-truncate lease loss did not stick: leaseLost = false, want true")
	}

	// Sector 0 — deleted under the live lease — is gone, and the loop
	// stopped there: sectors 1 and 2, which would have followed, remain.
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), sector0, metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("sector 0 after the mid-truncate loss: get err %v, want NotFound (deleted pre-lapse)", err)
	}
	for i := 1; i <= 2; i++ {
		if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), f.sectorNameFromSectorIndex(int64(i)), metav1.GetOptions{}); err != nil {
			t.Errorf("sector %d after the mid-truncate loss: get err %v, want it un-deleted (the loop must stop at the lapse)", i, err)
		}
	}
}

// ctxSensitiveProbe wraps a kubernetes.Interface and fails every ConfigMap
// Get/Update/Create whose CALL context is already done, the way a real
// clientset honours per-call context cancellation. The bare fake ignores the
// context entirely — its methods never look at ctx — so without this probe a
// test cannot tell which context a lock-path call was made with, and a
// cancelled-context bug would pass unnoticed.
type ctxSensitiveProbe struct {
	kubernetes.Interface
}

func (p *ctxSensitiveProbe) CoreV1() corev1client.CoreV1Interface {
	return &ctxSensitiveCoreV1{CoreV1Interface: p.Interface.CoreV1()}
}

type ctxSensitiveCoreV1 struct {
	corev1client.CoreV1Interface
}

func (c *ctxSensitiveCoreV1) ConfigMaps(namespace string) corev1client.ConfigMapInterface {
	return &ctxSensitiveConfigMaps{ConfigMapInterface: c.CoreV1Interface.ConfigMaps(namespace)}
}

type ctxSensitiveConfigMaps struct {
	corev1client.ConfigMapInterface
}

func requireLiveCtx(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("call made with a done context: %w", err)
	}
	return nil
}

func (m *ctxSensitiveConfigMaps) Get(ctx context.Context, name string, opts metav1.GetOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Get(ctx, name, opts)
}

func (m *ctxSensitiveConfigMaps) Update(ctx context.Context, cm *v1.ConfigMap, opts metav1.UpdateOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Update(ctx, cm, opts)
}

func (m *ctxSensitiveConfigMaps) Create(ctx context.Context, cm *v1.ConfigMap, opts metav1.CreateOptions) (*v1.ConfigMap, error) {
	if err := requireLiveCtx(ctx); err != nil {
		return nil, err
	}
	return m.ConfigMapInterface.Create(ctx, cm, opts)
}

// TestLockPathSurvivesCancelledContext pins the context finding on the #47
// locking: the construction context belongs to the caller's registration
// scope, and cancelling it after Open must not break a single lock-path
// call. Every lock ConfigMap compare-and-swap — and the CheckReservedLock
// read — runs on a cancellation-detached context (context.WithoutCancel)
// with its own timeout, so the whole locking protocol still works here even
// though every call made with the registration context itself would fail.
// The probe above makes the fake reject done contexts like a real clientset,
// and the negative control at the end proves it would have caught a leaked
// cancelled context.
func TestLockPathSurvivesCancelledContext(t *testing.T) {
	fcs := fake.NewSimpleClientset()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	probe := &ctxSensitiveProbe{Interface: fcs}
	v := NewVFS(ctx, probe, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, v, "cancel-ctx.db") // opened while the ctx is live
	fb := openTestFile(t, v, "cancel-ctx.db")

	cancel() // the registration context dies; the lock path must not care

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared) after the registration context was cancelled: %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("second holder Lock(Shared) after cancellation: %v", err)
	}
	if reserved, err := fa.CheckReservedLock(); err != nil || !reserved {
		t.Fatalf("CheckReservedLock after cancellation = (%v, %v), want (true, nil)", reserved, err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("Lock(Reserved) after cancellation: %v", err)
	}
	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Unlock(Shared) after cancellation: %v", err)
	}
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("Unlock(None) after cancellation: %v", err)
	}
	if err := fb.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("second holder Unlock(None) after cancellation: %v", err)
	}
	if holders := sharedHoldersIn(t, fcs, fa); len(holders) != 0 {
		t.Errorf("shared-holders after all unlocks = %v, want empty (every CAS landed)", holders)
	}

	// Negative control: the probe really does reject a call made with the
	// cancelled context, so the passes above mean the lock path avoided it.
	if _, err := probe.CoreV1().ConfigMaps(testNamespace).Get(ctx, fa.LockFileName(), metav1.GetOptions{}); err == nil {
		t.Fatal("probe accepted a call made with the cancelled context; this test proves nothing")
	}
}

// TestCloseReleasesHolder pins Close (= Unlock(NONE)): the closing file's
// shared entry and writer are removed and every other holder's entries
// survive.
func TestCloseReleasesHolder(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "closing.db")
	fb := openTestFile(t, v, "closing.db")

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}

	if err := fa.Close(); err != nil {
		t.Fatalf("A Close: %v", err)
	}
	holders := sharedHoldersIn(t, fcs, fa)
	if len(holders) != 1 {
		t.Fatalf("shared-holders after A Close = %v, want only B's entry", holders)
	}
	if _, ok := holders[fb.holderID]; !ok {
		t.Errorf("B's entry did not survive A's Close: %v", holders)
	}
	if w := writerIn(t, fcs, fa); w != nil {
		t.Errorf("writer after A Close = %v, want none", w)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("aggregate after A Close = %q, want %q (B still holds Shared)", got, sqlite3vfs.LockShared.String())
	}

	// Closing an already-closed file is a no-op.
	if err := fa.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
	if got := lockState(t, fcs, fa); got != sqlite3vfs.LockShared.String() {
		t.Errorf("second Close changed the aggregate to %q, want %q", got, sqlite3vfs.LockShared.String())
	}
}

// TestAggregateLockKeyStaysMaxHeldLevel walks mixed states over two holders
// and asserts the aggregate "lock" key — the only key readers of the old
// format see — always names the highest level actually held.
func TestAggregateLockKeyStaysMaxHeldLevel(t *testing.T) {
	v, fcs := newTestVFS(t)
	fa := openTestFile(t, v, "aggregate.db")
	fb := openTestFile(t, v, "aggregate.db")

	assertAggregate := func(want sqlite3vfs.LockType, label string) {
		t.Helper()
		if got := lockState(t, fcs, fa); got != want.String() {
			t.Errorf("%s: aggregate = %q, want %q", label, got, want.String())
		}
	}

	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	if err := fb.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("B Lock(Shared): %v", err)
	}
	assertAggregate(sqlite3vfs.LockShared, "two shared holders")

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved): %v", err)
	}
	assertAggregate(sqlite3vfs.LockReserved, "A reserved over two shared")

	if err := fa.Unlock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Unlock(Shared): %v", err)
	}
	assertAggregate(sqlite3vfs.LockShared, "A back to shared")

	if err := fa.Lock(sqlite3vfs.LockReserved); err != nil {
		t.Fatalf("A Lock(Reserved) again: %v", err)
	}
	if err := fa.Lock(sqlite3vfs.LockExclusive); !errors.Is(err, sqlite3vfs.BusyError) {
		t.Fatalf("A Lock(Exclusive) while B holds Shared = %v, want sqlite3vfs.BusyError", err)
	}
	assertAggregate(sqlite3vfs.LockPending, "A stuck in Pending")

	if err := fb.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("B Unlock(None): %v", err)
	}
	assertAggregate(sqlite3vfs.LockPending, "B released, A still Pending")

	if err := fa.Lock(sqlite3vfs.LockExclusive); err != nil {
		t.Fatalf("A Lock(Exclusive) after B released: %v", err)
	}
	assertAggregate(sqlite3vfs.LockExclusive, "A exclusive")

	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}
	assertAggregate(sqlite3vfs.LockNone, "all released")
}

// TestSectorReadPathMakesNoLockCalls pins the zero-API-call rule for the
// hot sector-read path: once the cache is warm under a held lock, reads
// make no Kubernetes calls at all — the lease machinery must never sneak a
// renewal into ReadAt.
func TestSectorReadPathMakesNoLockCalls(t *testing.T) {
	v, fcs := newTestVFS(t)
	f := openTestFile(t, v, "hotread.db")

	data := make([]byte, 128)
	fillPattern(data)
	mustWriteAt(t, f, data, 0)
	if err := f.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("Lock(Shared): %v", err)
	}
	if _, err := f.FileSize(); err != nil {
		t.Fatalf("FileSize: %v", err)
	}

	fcs.ClearActions()
	if n := mustReadAt(t, f, make([]byte, 128), 0); n != 128 {
		t.Fatalf("read %d bytes, want 128", n)
	}
	for _, a := range fcs.Actions() {
		t.Errorf("read under a held SHARED lock made a %s %s call, want none (the hot read path must be cache-only)", a.GetVerb(), a.GetResource().Resource)
	}
}

// TestTwoVFSInstancesSeeEachOthersWritesAndTruncate covers the cross-
// instance handoff the sector cache exists to serve: two VFS instances over
// one fake clientset — two processes. A reads under SHARED and lets the
// lock go; B, seeing none of the state A warmed, rewrites the file larger
// and then truncates it; A re-locks and must see B's bytes and B's size,
// revalidated by FileSize's LIST rather than anything cached under A's
// previous lock. (B takes no lock here: this test pins data and size
// visibility across a lock handoff, not lock arbitration — the lock
// ConfigMap's compare-and-swap mutual exclusion has its own tests, see
// TestTwoProcessesMutualExclusion and the README.)
func TestTwoVFSInstancesSeeEachOthersWritesAndTruncate(t *testing.T) {
	_, fcs := newTestVFS(t)
	va := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)
	vb := NewVFS(t.Context(), fcs, testNamespace, slog.New(slog.DiscardHandler), 3)

	fa := openTestFile(t, va, "shared-file.db")
	first := make([]byte, 3000)
	fillPattern(first)
	mustWriteAt(t, fa, first, 0)

	// A reads under a held SHARED lock — warming every cache it has — then
	// lets the lock go.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A Lock(Shared): %v", err)
	}
	got := make([]byte, len(first))
	if n := mustReadAt(t, fa, got, 0); n != len(first) {
		t.Fatalf("A read %d bytes, want %d", n, len(first))
	}
	if err := fa.Unlock(sqlite3vfs.LockNone); err != nil {
		t.Fatalf("A Unlock(None): %v", err)
	}

	// B writes new bytes — the file grows across two sectors — and then
	// truncates the file back down.
	fb := openTestFile(t, vb, "shared-file.db")
	second := make([]byte, 2*int(sectorSize)+100)
	fillPatternAlt(second)
	mustWriteAt(t, fb, second, 0)
	if err := fb.Truncate(45000); err != nil {
		t.Fatalf("B Truncate(45000): %v", err)
	}

	// A re-locks: its caches were dropped at Unlock, so it must revalidate
	// and see exactly the file B left behind.
	if err := fa.Lock(sqlite3vfs.LockShared); err != nil {
		t.Fatalf("A re-Lock(Shared): %v", err)
	}
	size, err := fa.FileSize()
	if err != nil {
		t.Fatalf("A FileSize after B rewrite: %v", err)
	}
	if size != 45000 {
		t.Errorf("A FileSize = %d, want 45000 (B truncated size)", size)
	}
	got = make([]byte, 45000)
	if n := mustReadAt(t, fa, got, 0); n != 45000 {
		t.Fatalf("A read %d bytes, want 45000", n)
	}
	if !bytes.Equal(got, second[:45000]) {
		t.Error("A sees bytes other than B truncated data")
	}
	if sectors := listSectorConfigMaps(t, fcs, fa); len(sectors) != 1 {
		t.Errorf("%d sector configmaps remain after B write+truncate, want 1", len(sectors))
	}
}

// --- Through database/sql ---
//
// TestSQLiteThroughDatabaseSQL keeps the default 4 KiB page size and a small
// row count, so the whole database stays inside one 64 KiB sector.
// TestSQLiteMultiSector64KiBPages then runs the cmd tools' configuration —
// 64 KiB SQLite pages — and inserts enough rows to span several 64 KiB
// sectors, closing and reopening the database, so the multi-sector path is
// exercised end to end through real SQLite (cgo), which no other test does.

func TestSQLiteThroughDatabaseSQL(t *testing.T) {
	v, _ := newTestVFS(t)
	vfsName := registerUniqueVFS(t, v)
	fileName := newSQLFileName()

	db := openSQLDB(t, vfsName, fileName)
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	const rows = 25
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("id-%03d", i), fmt.Sprintf("title %d", i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
		t.Fatalf("COUNT(*): %v", err)
	}
	if count != rows {
		t.Fatalf("COUNT(*) = %d, want %d", count, rows)
	}

	r, err := db.Query("SELECT id, title FROM books ORDER BY id LIMIT 5")
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	for i := 0; r.Next(); i++ {
		var id, title string
		if err := r.Scan(&id, &title); err != nil {
			t.Fatalf("scan row %d: %v", i, err)
		}
		wantID, wantTitle := fmt.Sprintf("id-%03d", i), fmt.Sprintf("title %d", i)
		if id != wantID || title != wantTitle {
			t.Errorf("row %d = (%q, %q), want (%q, %q)", i, id, title, wantID, wantTitle)
		}
	}
	if err := r.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("rows close: %v", err)
	}

	// This small database stays within one 64 KiB sector; the multi-sector
	// path through database/sql is covered by TestSQLiteMultiSector64KiBPages.
	if size, err := NewFile(fileName, v).FileSize(); err != nil {
		t.Fatalf("FileSize of the sqlite file: %v", err)
	} else if size > SectorSize {
		t.Errorf("sqlite database grew to %d bytes, past the one-sector limit", size)
	}

	// The database survives closing and reopening through the same VFS.
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2 := openSQLDB(t, vfsName, fileName)
	defer db2.Close()
	var count2 int
	if err := db2.QueryRow("SELECT COUNT(*) FROM books").Scan(&count2); err != nil {
		t.Fatalf("COUNT(*) after reopen: %v", err)
	}
	if count2 != rows {
		t.Errorf("COUNT(*) after reopen = %d, want %d", count2, rows)
	}
}

// TestSQLiteMultiSector64KiBPages runs the cmd tools' SQLite configuration —
// 64 KiB pages, SQLite's largest page size and exactly one VFS sector — and
// inserts enough rows to push the database across several 64 KiB sectors.
// The imported code panicked (through cgo, unrecoverably) the moment a
// database outgrew its first sector, so every database/sql test was capped
// at one sector; this pins the multi-sector write/read path end to end
// through database/sql: create, fill, close, reopen, verify.
func TestSQLiteMultiSector64KiBPages(t *testing.T) {
	v, _ := newTestVFS(t)
	vfsName := registerUniqueVFS(t, v)
	fileName := newSQLFileName()

	db := openSQLDB(t, vfsName, fileName)
	defer db.Close()

	// 64 KiB pages, as main.go and the cmd tools use. page_size is only
	// honoured while the database is still empty, so it must come before
	// the first CREATE TABLE.
	if _, err := db.Exec("PRAGMA page_size=65536"); err != nil {
		t.Fatalf("PRAGMA page_size=65536: %v", err)
	}
	var pageSize int
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatalf("read back PRAGMA page_size: %v", err)
	}
	if pageSize != int(SectorSize) {
		t.Fatalf("PRAGMA page_size read back = %d, want %d; the pragma did not take effect on the connection that builds the database", pageSize, SectorSize)
	}

	if _, err := db.Exec(`CREATE TABLE books (
		id text NOT NULL PRIMARY KEY,
		title text
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// ~200 bytes per row: 1000 rows land well past three 64 KiB sectors.
	const rows = 1000
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	stmt, err := tx.Prepare("INSERT INTO books (id, title) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := 0; i < rows; i++ {
		if _, err := stmt.Exec(fmt.Sprintf("id-%06d", i), fmt.Sprintf("title %06d %0180d", i, i)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("stmt.Close: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The database must now span multiple 64 KiB sectors on the cluster.
	size, err := NewFile(fileName, v).FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	t.Logf("database size with 64 KiB pages: %d bytes (%d sectors)", size, (size+SectorSize-1)/SectorSize)
	if size <= SectorSize {
		t.Fatalf("database stayed at %d bytes: it never left the first 64 KiB sector, so this test would not exercise the multi-sector path", size)
	}

	// Rows must read back correctly before and after the file is closed:
	// the reopen forces every page through the VFS's multi-sector read path.
	verifyRows := func(db *sql.DB, label string) {
		t.Helper()
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM books").Scan(&count); err != nil {
			t.Fatalf("%s: COUNT(*): %v", label, err)
		}
		if count != rows {
			t.Fatalf("%s: COUNT(*) = %d, want %d", label, count, rows)
		}
		for _, i := range []int{0, 1, rows / 2, rows - 2, rows - 1} {
			var id, title string
			wantID := fmt.Sprintf("id-%06d", i)
			wantTitle := fmt.Sprintf("title %06d %0180d", i, i)
			if err := db.QueryRow("SELECT id, title FROM books WHERE id = ?", wantID).Scan(&id, &title); err != nil {
				t.Fatalf("%s: row %d: %v", label, i, err)
			}
			if id != wantID || title != wantTitle {
				t.Errorf("%s: row %d = (%q, %q), want (%q, %q)", label, i, id, title, wantID, wantTitle)
			}
		}
	}
	verifyRows(db, "before close")

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2 := openSQLDB(t, vfsName, fileName)
	defer db2.Close()
	verifyRows(db2, "after reopen")
}

// --- Rollback journal files through the VFS (issue #68) ---
//
// SQLite opens "<db>-journal" through the same VFS it opens the database
// with, so a rollback journal must be an ordinary file here: Access answers
// from cluster truth, and Open/Delete/Truncate/FileSize key off
// base32(RawName) like any other name. These tests pin that on a "-journal"
// name; the crash-recovery tests that drive real SQLite live in
// vfs_journal_test.go.

func TestAccessJournalReflectsClusterTruth(t *testing.T) {
	v, fcs := newTestVFS(t)
	const journal = "truth.db-journal"

	// No sector ConfigMaps for this journal: it does not exist, so SQLite
	// must be told so (this is what sends it looking for a hot journal).
	got, err := v.Access(journal, sqlite3vfs.AccessExists)
	if err != nil {
		t.Fatalf("Access(%q): %v", journal, err)
	}
	if got {
		t.Errorf("Access(%q) with no sectors = true, want false", journal)
	}

	// Seed two sectors of journal data: the journal now exists on the
	// cluster and Access must say so.
	jf := NewFile(journal, v)
	seedMultiSectorFile(t, fcs, jf, multiSectorLen)
	if n := len(listSectorConfigMaps(t, fcs, jf)); n != 2 {
		t.Fatalf("setup: want 2 journal sectors, got %d", n)
	}

	got, err = v.Access(journal, sqlite3vfs.AccessExists)
	if err != nil {
		t.Fatalf("Access(%q): %v", journal, err)
	}
	if !got {
		t.Errorf("Access(%q) with 2 sectors = false, want true", journal)
	}

	// Only the journal's own sectors count: a database of the same stem
	// must not make the journal appear to exist.
	other, err := v.Access("truth.db", sqlite3vfs.AccessExists)
	if err != nil {
		t.Fatalf("Access(%q): %v", "truth.db", err)
	}
	if !other {
		t.Errorf("Access(%q) = false, want true (non-side files always exist)", "truth.db")
	}
}

func TestAccessWalAlwaysFalse(t *testing.T) {
	v, fcs := newTestVFS(t)
	const wal = "truth.db-wal"

	// A WAL is never supported, so it never exists — even with sectors
	// seeded under that name, which this VFS has no way to serve.
	if got, err := v.Access(wal, sqlite3vfs.AccessExists); err != nil || got {
		t.Errorf("Access(%q) with no sectors = (%v, %v), want (false, nil)", wal, got, err)
	}
	wf := NewFile(wal, v)
	seedMultiSectorFile(t, fcs, wf, multiSectorLen)
	if got, err := v.Access(wal, sqlite3vfs.AccessExists); err != nil || got {
		t.Errorf("Access(%q) with sectors seeded = (%v, %v), want (false, nil)", wal, got, err)
	}
}

func TestAccessOtherNamesAlwaysTrue(t *testing.T) {
	v, _ := newTestVFS(t)
	for _, name := range []string{"plain.db", "plain.db-journal-backup", "journal", "x.db-shm", "x.db-journall"} {
		got, err := v.Access(name, sqlite3vfs.AccessExists)
		if err != nil {
			t.Fatalf("Access(%q): %v", name, err)
		}
		if !got {
			t.Errorf("Access(%q) = false, want true (only -wal/-journal are special)", name)
		}
	}
}

func TestOpenNonexistentJournalIsZeroLength(t *testing.T) {
	v, fcs := newTestVFS(t)
	const journal = "fresh.db-journal"

	// Opening a journal that has no sectors must succeed and look like a
	// zero-length file, exactly as opening a fresh database does: a
	// READ-WRITE open pre-creates the empty sector 0 the size computation
	// expects. This test is deliberately explicit about the flags now that
	// Open honours its read-only bit: only a read-write open may create.
	f := openTestFileWithFlags(t, v, journal, sqlite3vfs.OpenReadWrite)

	// The lockfile was created and sector 0 exists, empty.
	getLockfileConfigMap(t, fcs, f)
	cms := listSectorConfigMaps(t, fcs, f)
	if len(cms) != 1 {
		t.Fatalf("after Open(%q): %d sector configmaps, want 1", journal, len(cms))
	}
	if idx, ok := f.sectorIndexFromConfigMapName(cms[0].Name); !ok || idx != 0 {
		t.Errorf("sector configmap name %q does not parse as index 0", cms[0].Name)
	}
	if got := cms[0].BinaryData["sector"]; len(got) != 0 {
		t.Errorf("fresh journal sector 0 holds %d bytes, want 0", len(got))
	}

	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize on fresh journal: %v", err)
	}
	if size != 0 {
		t.Errorf("FileSize on fresh journal = %d, want 0", size)
	}
}

func TestOpenReadOnlyNonexistentCreatesNothing(t *testing.T) {
	v, fcs := newTestVFS(t)
	const db = "missing.db"

	// A READ-ONLY open of a name that does not exist must not fabricate the
	// file: no lock ConfigMap via casLock, no empty sector 0. The only calls
	// the fake clientset may see are the reads that establish the file is
	// missing (the lock ConfigMap Get), and the answer is SQLITE_CANTOPEN —
	// what a filesystem VFS gives for a missing file. Before the flags were
	// honoured this open created the lockfile and sector 0 and reported
	// success, so a read-only reader mutated the store (issue #226).
	fcs.ClearActions()
	sf, _, err := v.Open(db, sqlite3vfs.OpenReadOnly)

	if err != sqlite3vfs.CantOpenError {
		t.Fatalf("Open(%q, OpenReadOnly) error = %v, want %v", db, err, sqlite3vfs.CantOpenError)
	}
	if sf != nil {
		t.Errorf("Open(%q, OpenReadOnly) returned file %T, want nil", db, sf)
	}

	writes := 0
	for _, a := range fcs.Actions() {
		switch verb := a.GetVerb(); verb {
		case "create", "update", "patch", "replace", "delete", "delete-collection":
			t.Errorf("read-only Open recorded %q on %q, want no writes", verb, a.GetResource())
			writes++
		}
	}
	if writes == 0 && len(fcs.Actions()) == 0 {
		t.Errorf("read-only Open of %q recorded zero actions: it no longer probed for the file", db)
	}
}

func TestOpenReadOnlyExistingFileCreatesNothing(t *testing.T) {
	v, fcs := newTestVFS(t)
	const db = "there.db"

	// A read-write open creates the file; a second, READ-ONLY open of the
	// now-existing name must succeed (hot-journal recovery and reads rely
	// on readonly opens working on existing names, see cmd/db-download) and
	// record only reads: no lock ConfigMap write, no sector write.
	f := openTestFile(t, v, db)
	if _, err := f.WriteAt([]byte("krew"), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	fcs.ClearActions()
	ro := openTestFileWithFlags(t, v, db, sqlite3vfs.OpenReadOnly)
	for _, a := range fcs.Actions() {
		switch verb := a.GetVerb(); verb {
		case "create", "update", "patch", "replace", "delete", "delete-collection":
			t.Errorf("read-only Open of existing %q recorded %q on %q, want no writes", db, verb, a.GetResource())
		}
	}

	// What the read-only opener reads is what the writer wrote.
	buf := make([]byte, 4)
	if _, err := ro.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt through read-only open: %v", err)
	}
	if string(buf) != "krew" {
		t.Errorf("read-only open ReadAt = %q, want %q", buf, "krew")
	}
}

func TestJournalFileSizeOpenTruncateRoundTrip(t *testing.T) {
	v, fcs := newTestVFS(t)
	const journal = "work.db-journal"

	f := openTestFile(t, v, journal)
	seedMultiSectorFile(t, fcs, f, multiSectorLen)

	// FileSize keys off base32(RawName) like any file: two sectors seeded
	// means multiSectorLen bytes, seen through a second VFS instance too.
	size, err := f.FileSize()
	if err != nil {
		t.Fatalf("FileSize: %v", err)
	}
	if size != int64(multiSectorLen) {
		t.Fatalf("FileSize on journal = %d, want %d", size, multiSectorLen)
	}

	v2, _ := newTestVFS(t)
	_ = v2 // a second instance would see the same sectors; the name is cluster-wide

	// Truncate to a mid-sector-1 length shortens that sector in place: both
	// sectors remain (sector 0 whole, sector 1 holding the kept prefix).
	const keep = int64(SectorSize) + 100
	if err := f.Truncate(keep); err != nil {
		t.Fatalf("Truncate(%d): %v", keep, err)
	}
	cms := listSectorConfigMaps(t, fcs, f)
	if len(cms) != 2 {
		t.Fatalf("after Truncate(%d): %d sector configmaps, want 2", keep, len(cms))
	}
	size, err = f.FileSize()
	if err != nil {
		t.Fatalf("FileSize after truncate: %v", err)
	}
	if size != keep {
		t.Errorf("FileSize after Truncate = %d, want %d", size, keep)
	}

	// Truncate to a sector-aligned length drops the sectors past it.
	if err := f.Truncate(int64(SectorSize)); err != nil {
		t.Fatalf("Truncate(%d): %v", SectorSize, err)
	}
	if n := len(listSectorConfigMaps(t, fcs, f)); n != 1 {
		t.Errorf("after Truncate to one sector: %d journal sector configmaps, want 1", n)
	}

	// Truncate to 0 leaves no sector ConfigMaps of the journal at all.
	if err := f.Truncate(0); err != nil {
		t.Fatalf("Truncate(0): %v", err)
	}
	if n := len(listSectorConfigMaps(t, fcs, f)); n != 0 {
		t.Errorf("after Truncate(0): %d journal sector configmaps, want 0", n)
	}
}

func TestDeleteJournalRemovesAllSectorsAndLockfile(t *testing.T) {
	v, fcs := newTestVFS(t)
	const journal = "hot.db-journal"

	jf := openTestFile(t, v, journal)
	seedMultiSectorFile(t, fcs, jf, multiSectorLen)
	if n := len(listSectorConfigMaps(t, fcs, jf)); n != 2 {
		t.Fatalf("setup: want 2 journal sectors before delete, got %d", n)
	}

	// A database of the same stem keeps its own state: Delete of the
	// journal must not touch it.
	df := openTestFile(t, v, "hot.db")
	seedMultiSectorFile(t, fcs, df, multiSectorLen)

	if err := v.Delete(journal, false); err != nil {
		t.Fatalf("Delete(%q): %v", journal, err)
	}

	if n := len(listSectorConfigMaps(t, fcs, jf)); n != 0 {
		t.Errorf("after Delete(%q): %d journal sector configmaps, want 0", journal, n)
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), jf.LockFileName(), metav1.GetOptions{}); !kerrors.IsNotFound(err) {
		t.Errorf("journal lockfile get after delete: err = %v, want NotFound", err)
	}
	// The journal is now gone by cluster truth, so Access says so.
	if got, err := v.Access(journal, sqlite3vfs.AccessExists); err != nil || got {
		t.Errorf("Access(%q) after Delete = (%v, %v), want (false, nil)", journal, got, err)
	}

	// The same-stem database is untouched.
	if n := len(listSectorConfigMaps(t, fcs, df)); n != 2 {
		t.Errorf("same-stem database lost sectors: %d, want 2", n)
	}
	if _, err := fcs.CoreV1().ConfigMaps(testNamespace).Get(t.Context(), df.LockFileName(), metav1.GetOptions{}); err != nil {
		t.Errorf("same-stem database lockfile after journal Delete: %v", err)
	}
}

func TestDeleteNonexistentJournalSucceeds(t *testing.T) {
	v, _ := newTestVFS(t)
	// SQLite deletes the journal at the end of every transaction; the
	// common case is a journal name with nothing behind it.
	if err := v.Delete("never.db-journal", false); err != nil {
		t.Errorf("Delete of nonexistent journal = %v, want nil", err)
	}
}

// TestNewVFSNonPositiveLockLeaseKeepsDefault pins the documented behaviour of
// WithLockLease (README: "WithLockLease(0) (or any non-positive value) keeps
// the default 30 s lease"): only a positive duration changes the lease, so a
// non-positive option must leave NewVFS's defaultLockLease initialisation in
// place instead of zeroing the lease or panicking. It reads v.lockLease
// directly, which is why it lives in this package.
func TestNewVFSNonPositiveLockLeaseKeepsDefault(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
		want time.Duration
	}{
		{name: "no option", want: defaultLockLease},
		{name: "zero", opts: []Option{WithLockLease(0)}, want: defaultLockLease},
		{name: "negative", opts: []Option{WithLockLease(-5 * time.Second)}, want: defaultLockLease},
		{name: "positive", opts: []Option{WithLockLease(10 * time.Second)}, want: 10 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := newTestVFS(t, tc.opts...)
			if got := v.lockLease; got != tc.want {
				t.Errorf("lockLease = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestFullPathname(t *testing.T) {
	v, _ := newTestVFS(t)

	// Different spellings of the same relative name must canonicalise to
	// one name, so one database cannot split into two ConfigMap sets
	// depending on how it was opened. Absolute names are cleaned too, but
	// stay absolute.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "dot slash", in: "./app.db", want: "app.db"},
		{name: "dotdot traversal", in: "dir/../app.db", want: "app.db"},
		{name: "already clean", in: "app.db", want: "app.db"},
		{name: "absolute", in: "/data/app.db", want: "/data/app.db"},
		{name: "absolute clean", in: "/data/sub/../app.db", want: "/data/app.db"},
		{name: "trailing slash removed", in: "app.db/", want: "app.db"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := v.FullPathname(tc.in); got != tc.want {
				t.Errorf("FullPathname(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFullPathnameEndToEnd proves the canonicalisation through SQLite
// itself, not only through the VFS method: a database written as rev.db
// reads back when reopened under unclean spellings of the same name via
// file: URIs, and the rollback journal a write creates lands under the
// canonical name too — SQLite appends -journal to the canonicalised name,
// so the journal of ./rev.db is stored as rev.db-journal, never as
// ./rev.db-journal.
func TestFullPathnameEndToEnd(t *testing.T) {
	v, fcs := newTestVFS(t)
	vfsName := registerUniqueVFS(t, v)

	db := openSQLDBWithJournal(t, vfsName, "rev.db", "DELETE")
	if _, err := db.Exec(`CREATE TABLE rev (k TEXT NOT NULL PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rev (k, v) VALUES ('answer', '42')`); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, spelling := range []string{"./rev.db", "sub/../rev.db"} {
		t.Run(spelling, func(t *testing.T) {
			ddb := openSQLDBWithJournal(t, vfsName, spelling, "DELETE")
			defer ddb.Close()
			var got string
			if err := ddb.QueryRow(`SELECT v FROM rev WHERE k = 'answer'`).Scan(&got); err != nil {
				t.Fatalf("read back through %q: %v", spelling, err)
			}
			if got != "42" {
				t.Fatalf("read back through %q = %q, want %q", spelling, got, "42")
			}

			// While this connection holds a write transaction open, its
			// rollback journal exists on the cluster. Every sector
			// ConfigMap of that journal must carry the canonical journal
			// name's relevant-file label — and no ConfigMap may carry the
			// label of the unclean spelling.
			tx, err := ddb.Begin()
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := tx.Exec(`INSERT INTO rev (k, v) VALUES ('tx', 'open')`); err != nil {
				t.Fatalf("INSERT in tx: %v", err)
			}
			wantLabel := NewFile("rev.db-journal", v).relevantFileLabel()
			uncleanLabel := NewFile("./rev.db-journal", v).relevantFileLabel()
			cms, err := fcs.CoreV1().ConfigMaps(testNamespace).List(t.Context(), metav1.ListOptions{})
			if err != nil {
				t.Fatalf("List ConfigMaps: %v", err)
			}
			journalSectors := 0
			for _, cm := range cms.Items {
				switch cm.Labels["relevant-file"] {
				case wantLabel:
					if cm.Labels["data"] == "sector" {
						journalSectors++
					}
				case uncleanLabel:
					t.Errorf("ConfigMap %s carries the label of the unclean journal name %q; the journal must follow the canonical name rev.db-journal", cm.Name, "./rev.db-journal")
				}
			}
			if journalSectors == 0 {
				t.Fatalf("no journal sector ConfigMaps found under the canonical journal label %q", wantLabel)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatalf("Rollback: %v", err)
			}
		})
	}
}
