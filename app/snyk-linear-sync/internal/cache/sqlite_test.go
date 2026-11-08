package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSnykIgnoresTTLEvictsStaleEntries(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	projectID := "project-a"
	issueKey := "SNYK-1"

	// Save an entry that is already older than the TTL.
	staleUpdatedAt := time.Now().UTC().Add(-ignoreEntryTTL - time.Hour)
	if err := store.SaveIgnores(ctx, projectID, map[string]IgnoreMeta{
		issueKey: {
			IssueKey:           issueKey,
			ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
			DisregardIfFixable: false,
			CreatedAt:          time.Now().UTC().Add(-48 * time.Hour),
			UpdatedAt:          staleUpdatedAt,
		},
	}); err != nil {
		t.Fatalf("SaveIgnores() error = %v", err)
	}

	loaded, err := store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}
	if _, ok := loaded[issueKey]; ok {
		t.Fatalf("stale ignore entry %q was not evicted", issueKey)
	}

	// Save a fresh entry and verify it survives.
	if err := store.SaveIgnores(ctx, projectID, map[string]IgnoreMeta{
		issueKey: {
			IssueKey:           issueKey,
			ExpiresAt:          time.Now().UTC().Add(24 * time.Hour),
			DisregardIfFixable: false,
			CreatedAt:          time.Now().UTC(),
			UpdatedAt:          time.Now().UTC(),
		},
	}); err != nil {
		t.Fatalf("SaveIgnores() fresh error = %v", err)
	}

	loaded, err = store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() fresh error = %v", err)
	}
	if _, ok := loaded[issueKey]; !ok {
		t.Fatalf("fresh ignore entry %q was evicted", issueKey)
	}
}

// TestSaveAndLoadRoundTripsHashes pins that Load reads back exactly what Save
// wrote against a real SQLite store, so the fast-path comparison never starts
// from a wrong view of the cache (the stub-based Load tests cannot see this).
func TestSaveAndLoadRoundTripsHashes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	snapshot := Snapshot{
		SchemaSignature: "test-sig-v1",
		SnykHashes: map[string]string{
			"snyk:project-a:issue-1": "hash-a",
			"snyk:project-a:issue-2": "hash-b",
		},
		LinearHashes: map[string]string{
			"snyk:project-a:issue-1": "linear-a",
		},
	}
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SchemaSignature != "test-sig-v1" {
		t.Fatalf("SchemaSignature = %q, want %q", loaded.SchemaSignature, "test-sig-v1")
	}
	if len(loaded.SnykHashes) != len(snapshot.SnykHashes) {
		t.Fatalf("SnykHashes = %v, want %v", loaded.SnykHashes, snapshot.SnykHashes)
	}
	for fingerprint, hash := range snapshot.SnykHashes {
		if got := loaded.SnykHashes[fingerprint]; got != hash {
			t.Fatalf("SnykHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}
	if len(loaded.LinearHashes) != len(snapshot.LinearHashes) {
		t.Fatalf("LinearHashes = %v, want %v", loaded.LinearHashes, snapshot.LinearHashes)
	}
	for fingerprint, hash := range snapshot.LinearHashes {
		if got := loaded.LinearHashes[fingerprint]; got != hash {
			t.Fatalf("LinearHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}
}

// TestSnykIgnoresMigrationTimestampFormatIsParsedAndAges replays the
// updated_at migration against a pre-migration table. The migration seeds the
// new column with CURRENT_TIMESTAMP, whose "YYYY-MM-DD HH:MM:SS" format is
// not RFC3339; a reader that only parses RFC3339 turns those rows into zero
// times and the TTL eviction never fires for them.
func TestSnykIgnoresMigrationTimestampFormatIsParsedAndAges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-cache.db")
	ctx := context.Background()
	projectID := "project-a"

	// Create the cache, seed a row the way the sync would have written it
	// before the column existed, then drop the column so the next Open has
	// to run the migration that seeds it with CURRENT_TIMESTAMP.
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := store.db.Exec(
		`INSERT INTO snyk_ignores (project_id, issue_key, expires_at, disregard_if_fixable, created_at) ` +
			`VALUES ('project-a', 'SNYK-2', '', 0, '')`); err != nil {
		t.Fatalf("seed pre-migration row: %v", err)
	}
	if _, err := store.db.Exec(`ALTER TABLE snyk_ignores DROP COLUMN updated_at`); err != nil {
		t.Fatalf("drop updated_at column: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("re-open for migration error = %v", err)
	}
	defer store.Close()

	// The migration just stamped CURRENT_TIMESTAMP (UTC, SQLite's layout).
	// The entry is fresh, so it must survive, and UpdatedAt must be parsed
	// rather than silently discarded into a zero time.
	loaded, err := store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}
	meta, ok := loaded["SNYK-2"]
	if !ok {
		t.Fatalf("migration-seeded ignore entry was evicted")
	}
	if meta.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt = zero time, want the CURRENT_TIMESTAMP value parsed")
	}

	// Age the seeded value past the TTL, written in the very format
	// CURRENT_TIMESTAMP produces (datetime() returns the same layout; it
	// cannot itself produce a past timestamp).
	if _, err := store.db.Exec(`UPDATE snyk_ignores SET updated_at = datetime('now', '-8 days')`); err != nil {
		t.Fatalf("age seeded updated_at: %v", err)
	}
	loaded, err = store.LoadIgnores(ctx, projectID)
	if err != nil {
		t.Fatalf("LoadIgnores() stale error = %v", err)
	}
	if _, ok := loaded["SNYK-2"]; ok {
		t.Fatalf("stale migration-format ignore entry was not evicted")
	}
}

// TestSnykIgnoresTTLEvictsUnparseableUpdatedAt pins that a corrupt
// updated_at is evicted rather than kept forever, while well-formed entries
// in the same load are unaffected.
func TestSnykIgnoresTTLEvictsUnparseableUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	// SaveIgnores replaces every row of the project, so both entries go
	// through it first and only then is one row's updated_at corrupted in
	// place. A well-formed fresh entry sits alongside the corrupt one and
	// must survive the same load.
	now := time.Now().UTC()
	if err := store.SaveIgnores(ctx, "project-a", map[string]IgnoreMeta{
		"SNYK-3": {IssueKey: "SNYK-3", UpdatedAt: now, CreatedAt: now},
		"SNYK-4": {IssueKey: "SNYK-4", UpdatedAt: now},
	}); err != nil {
		t.Fatalf("SaveIgnores() error = %v", err)
	}
	if _, err := store.db.Exec(
		`UPDATE snyk_ignores SET updated_at = 'not-a-timestamp' WHERE issue_key = 'SNYK-3'`); err != nil {
		t.Fatalf("corrupt updated_at: %v", err)
	}

	loaded, err := store.LoadIgnores(ctx, "project-a")
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}
	if _, ok := loaded["SNYK-3"]; ok {
		t.Fatalf("ignore entry with unparseable updated_at was not evicted")
	}
	if _, ok := loaded["SNYK-4"]; !ok {
		t.Fatalf("well-formed ignore entry was evicted")
	}

	// A genuinely absent updated_at keeps its previous meaning: a zero time
	// that does not age the entry out.
	if _, err := store.db.Exec(
		`UPDATE snyk_ignores SET updated_at = '' WHERE issue_key = 'SNYK-4'`); err != nil {
		t.Fatalf("clear updated_at: %v", err)
	}
	loaded, err = store.LoadIgnores(ctx, "project-a")
	if err != nil {
		t.Fatalf("LoadIgnores() absent updated_at error = %v", err)
	}
	absent, ok := loaded["SNYK-4"]
	if !ok {
		t.Fatalf("ignore entry with absent updated_at was evicted")
	}
	if !absent.UpdatedAt.IsZero() {
		t.Fatalf("UpdatedAt = %v, want the zero time for an absent value", absent.UpdatedAt)
	}
}

// TestSaveAndLoadRoundTripsSavedAt pins that a snapshot's saved_at survives a
// Save/Load round trip, so a later run can report how stale the hashes it
// fast-paths against are.
func TestSaveAndLoadRoundTripsSavedAt(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	before := time.Now().UTC().Add(-time.Second)
	if err := store.Save(ctx, Snapshot{
		SchemaSignature: "test-sig-v1",
		SnykHashes:      map[string]string{},
		LinearHashes:    map[string]string{},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SavedAt.IsZero() {
		t.Fatal("SavedAt = zero time, want the save timestamp")
	}
	if loaded.SavedAt.Before(before) || loaded.SavedAt.After(after) {
		t.Fatalf("SavedAt = %v, want between %v and %v", loaded.SavedAt, before, after)
	}
}

// TestLoadIgnoresReportsEvictionCounts pins that LoadIgnores reports the rows
// it drops: a stale updated_at (TTL eviction) and an unparseable one are
// counted separately, while a fresh row survives and is not counted.
func TestLoadIgnoresReportsEvictionCounts(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	if err := store.SaveIgnores(ctx, "project-a", map[string]IgnoreMeta{
		"SNYK-FRESH":   {IssueKey: "SNYK-FRESH", UpdatedAt: now},
		"SNYK-STALE":   {IssueKey: "SNYK-STALE", UpdatedAt: now.Add(-ignoreEntryTTL - time.Hour)},
		"SNYK-CORRUPT": {IssueKey: "SNYK-CORRUPT", UpdatedAt: now},
	}); err != nil {
		t.Fatalf("SaveIgnores() error = %v", err)
	}
	if _, err := store.db.Exec(
		`UPDATE snyk_ignores SET updated_at = 'not-a-timestamp' WHERE issue_key = 'SNYK-CORRUPT'`); err != nil {
		t.Fatalf("corrupt updated_at: %v", err)
	}

	loaded, err := store.LoadIgnores(ctx, "project-a")
	if err != nil {
		t.Fatalf("LoadIgnores() error = %v", err)
	}
	if _, ok := loaded["SNYK-FRESH"]; !ok {
		t.Fatal("fresh ignore entry was evicted")
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded = %v, want only the fresh entry", loaded)
	}

	counts := store.IgnoreEvictions()
	if counts.TTLExpired != 1 {
		t.Fatalf("TTLExpired = %d, want 1", counts.TTLExpired)
	}
	if counts.Unparseable != 1 {
		t.Fatalf("Unparseable = %d, want 1", counts.Unparseable)
	}

	// Counts accumulate across projects and loads: a run loads every
	// project's ignores once, and the refreshed-cache line reports the run's
	// total rather than the last project's.
	if err := store.SaveIgnores(ctx, "project-b", map[string]IgnoreMeta{
		"SNYK-STALE-B": {IssueKey: "SNYK-STALE-B", UpdatedAt: now.Add(-ignoreEntryTTL - time.Hour)},
	}); err != nil {
		t.Fatalf("SaveIgnores() project-b error = %v", err)
	}
	if _, err := store.LoadIgnores(ctx, "project-b"); err != nil {
		t.Fatalf("LoadIgnores() project-b error = %v", err)
	}
	counts = store.IgnoreEvictions()
	if counts.TTLExpired != 2 {
		t.Fatalf("TTLExpired after second load = %d, want 2", counts.TTLExpired)
	}
	if counts.Unparseable != 1 {
		t.Fatalf("Unparseable after second load = %d, want 1", counts.Unparseable)
	}
}
