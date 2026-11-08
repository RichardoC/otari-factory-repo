package cache

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
		SnykDASTHashes: map[string]string{
			"snyk-dast:target-a:finding-1": "hash-a",
			"snyk-dast:target-b:finding-2": "hash-b",
		},
		LinearHashes: map[string]string{
			"snyk-dast:target-a:finding-1": "linear-a",
		},
	}
	if err := store.Save(ctx, snapshot); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if loaded.SavedAt.IsZero() {
		t.Fatal("SavedAt is zero, want Save to record a timestamp")
	}
	if loaded.SchemaSignature != "test-sig-v1" {
		t.Fatalf("SchemaSignature = %q, want %q", loaded.SchemaSignature, "test-sig-v1")
	}
	if got := loaded.SnykDASTHashes["snyk-dast:target-a:finding-1"]; got != "hash-a" {
		t.Fatalf("SnykDASTHashes[finding-1] = %q, want %q", got, "hash-a")
	}
	if got := loaded.SnykDASTHashes["snyk-dast:target-b:finding-2"]; got != "hash-b" {
		t.Fatalf("SnykDASTHashes[finding-2] = %q, want %q", got, "hash-b")
	}
	if got := loaded.LinearHashes["snyk-dast:target-a:finding-1"]; got != "linear-a" {
		t.Fatalf("LinearHashes[finding-1] = %q, want %q", got, "linear-a")
	}
}

func TestSaveReplacesPreviousSnapshot(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(filepath.Join(dir, "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := store.Save(ctx, Snapshot{
		SchemaSignature: "v1",
		SnykDASTHashes: map[string]string{
			"snyk-dast:target-a:finding-1": "old",
		},
		LinearHashes: map[string]string{
			"snyk-dast:target-a:finding-1": "old-linear",
		},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	if err := store.Save(ctx, Snapshot{
		SchemaSignature: "v1",
		SnykDASTHashes: map[string]string{
			"snyk-dast:target-b:finding-2": "new",
		},
		LinearHashes: map[string]string{},
	}); err != nil {
		t.Fatalf("Save() second error = %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if _, exists := loaded.SnykDASTHashes["snyk-dast:target-a:finding-1"]; exists {
		t.Fatal("stale SnykDASTHashes entry was not cleared on save")
	}
	if got := loaded.SnykDASTHashes["snyk-dast:target-b:finding-2"]; got != "new" {
		t.Fatalf("SnykDASTHashes[finding-2] = %q, want %q", got, "new")
	}
	if _, exists := loaded.LinearHashes["snyk-dast:target-a:finding-1"]; exists {
		t.Fatal("stale LinearHashes entry was not cleared on save")
	}
}

func TestSaveAndLoadRoundTripsSavedAt(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test-cache.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	want := time.Date(2026, time.January, 2, 3, 4, 5, 123456789, time.UTC)
	if err := store.Save(context.Background(), Snapshot{SavedAt: want}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !loaded.SavedAt.Equal(want) {
		t.Fatalf("SavedAt = %s, want %s", loaded.SavedAt, want)
	}
}

// TestLoadTreatsBadSavedAtAsUnknownAge covers the corrupted-metadata path: a
// missing or malformed saved_at row must not fail the load or stop the cache
// from being used. The cache is only an optimisation, and a load error would
// fail every later run (Save is never reached, so the row never heals), so
// Load keeps the snapshot and reports the age as a zero time instead.
func TestLoadTreatsBadSavedAtAsUnknownAge(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, store *Store)
	}{
		{
			name: "malformed saved_at value",
			corrupt: func(t *testing.T, store *Store) {
				t.Helper()
				if _, err := store.db.Exec(`UPDATE sync_meta SET value = 'not-a-timestamp' WHERE key = 'saved_at'`); err != nil {
					t.Fatalf("corrupt saved_at: %v", err)
				}
			},
		},
		{
			name: "missing saved_at row",
			corrupt: func(t *testing.T, store *Store) {
				t.Helper()
				if _, err := store.db.Exec(`DELETE FROM sync_meta WHERE key = 'saved_at'`); err != nil {
					t.Fatalf("delete saved_at: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "test-cache.db"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer store.Close()

			ctx := context.Background()
			snapshot := Snapshot{
				SchemaSignature: "test-sig-v1",
				SnykDASTHashes:  map[string]string{"snyk-dast:project-a:finding-1": "hash-a"},
				LinearHashes:    map[string]string{"snyk-dast:project-a:finding-1": "hash-l"},
			}
			if err := store.Save(ctx, snapshot); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			tt.corrupt(t, store)

			loaded, err := store.Load(ctx)
			if err != nil {
				t.Fatalf("Load() error = %v, want nil (bad saved_at must not fail the load)", err)
			}
			if !loaded.SavedAt.IsZero() {
				t.Fatalf("SavedAt = %s, want zero (age unknown)", loaded.SavedAt)
			}
			if loaded.SchemaSignature != "test-sig-v1" {
				t.Fatalf("SchemaSignature = %q, want %q (snapshot rows must survive)", loaded.SchemaSignature, "test-sig-v1")
			}
			if got := loaded.SnykDASTHashes["snyk-dast:project-a:finding-1"]; got != "hash-a" {
				t.Fatalf("SnykDASTHashes[finding-1] = %q, want %q", got, "hash-a")
			}
		})
	}
}

func TestLoadAcceptsLegacyTimestampFormats(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{
			name:  "RFC3339",
			value: "2026-01-02T03:04:05Z",
			want:  time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		},
		{
			name:  "SQLite CURRENT_TIMESTAMP",
			value: "2026-01-02 03:04:05",
			want:  time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "test-cache.db"))
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			defer store.Close()

			ctx := context.Background()
			if err := store.Save(ctx, Snapshot{}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if _, err := store.db.Exec(`UPDATE sync_meta SET value = ? WHERE key = 'saved_at'`, tt.value); err != nil {
				t.Fatalf("rewrite saved_at: %v", err)
			}

			loaded, err := store.Load(ctx)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !loaded.SavedAt.Equal(tt.want) {
				t.Fatalf("SavedAt = %s, want %s", loaded.SavedAt, tt.want)
			}
		})
	}
}
