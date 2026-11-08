package cache

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	sq "github.com/Masterminds/squirrel"
	_ "modernc.org/sqlite"
)

const schemaVersion = "v1"

// ignoreEntryTTL is how long a cached v1 ignore entry survives without being
// seen in the Snyk API response. It must be long enough to survive transient
// partial responses, but short enough that a deleted ignore does not stay
// cached forever.
const ignoreEntryTTL = 7 * 24 * time.Hour

// IgnoreMeta stores the v1 ignore fields that must survive a flaky Snyk API.
// It is kept in the cache package to avoid an import cycle with the snyk package.
type IgnoreMeta struct {
	IssueKey           string
	ExpiresAt          time.Time
	DisregardIfFixable bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Snapshot struct {
	SchemaSignature string
	// SavedAt is when the snapshot was written by Save. It is zero for a
	// cache written before saved_at was recorded, so callers must treat a
	// zero value as "age unknown" rather than as very old.
	SavedAt      time.Time
	SnykHashes   map[string]string
	LinearHashes map[string]string
}

// IgnoreEvictionCounts is the number of cached v1 ignore rows a Store's
// LoadIgnores calls discarded. TTLExpired rows were seen in the Snyk API but
// have not been confirmed for longer than ignoreEntryTTL; Unparseable rows
// carry an updated_at in no recognised format and so cannot be aged.
type IgnoreEvictionCounts struct {
	TTLExpired  int
	Unparseable int
}

type Store struct {
	db      *sql.DB
	builder sq.StatementBuilderType

	evictionMu      sync.Mutex
	ignoreEvictions IgnoreEvictionCounts
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create cache directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite cache: %w", err)
	}

	store := &Store{
		db:      db,
		builder: sq.StatementBuilder.PlaceholderFormat(sq.Question),
	}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Load(ctx context.Context) (Snapshot, error) {
	snapshot := Snapshot{
		SnykHashes:   map[string]string{},
		LinearHashes: map[string]string{},
	}

	query, args, err := s.builder.
		Select("value").
		From("sync_meta").
		Where(sq.Eq{"key": "schema_signature"}).
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build cache schema signature query: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load cache schema signature: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&snapshot.SchemaSignature); err != nil {
			return Snapshot{}, fmt.Errorf("scan cache schema signature: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate cache schema signature rows: %w", err)
	}

	query, args, err = s.builder.
		Select("value").
		From("sync_meta").
		Where(sq.Eq{"key": "saved_at"}).
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build cache saved_at query: %w", err)
	}

	savedAtRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load cache saved_at: %w", err)
	}
	defer savedAtRows.Close()
	for savedAtRows.Next() {
		var raw string
		if err := savedAtRows.Scan(&raw); err != nil {
			return Snapshot{}, fmt.Errorf("scan cache saved_at: %w", err)
		}
		// A malformed saved_at only means the age is unknown; it must not
		// stop the cache from being used.
		if savedAt, ok := parseTimestamp(raw); ok {
			snapshot.SavedAt = savedAt
		}
	}
	if err := savedAtRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate cache saved_at rows: %w", err)
	}

	query, args, err = s.builder.
		Select("fingerprint", "hash").
		From("snyk_findings").
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build Snyk cache rows query: %w", err)
	}

	snykRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load Snyk cache rows: %w", err)
	}
	defer snykRows.Close()
	for snykRows.Next() {
		var fingerprint string
		var hash string
		if err := snykRows.Scan(&fingerprint, &hash); err != nil {
			return Snapshot{}, fmt.Errorf("scan Snyk cache row: %w", err)
		}
		snapshot.SnykHashes[fingerprint] = hash
	}
	if err := snykRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate Snyk cache rows: %w", err)
	}

	query, args, err = s.builder.
		Select("fingerprint", "hash").
		From("linear_issues").
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build Linear cache rows query: %w", err)
	}

	linearRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load Linear cache rows: %w", err)
	}
	defer linearRows.Close()
	for linearRows.Next() {
		var fingerprint string
		var hash string
		if err := linearRows.Scan(&fingerprint, &hash); err != nil {
			return Snapshot{}, fmt.Errorf("scan Linear cache row: %w", err)
		}
		snapshot.LinearHashes[fingerprint] = hash
	}
	if err := linearRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate Linear cache rows: %w", err)
	}

	return snapshot, nil
}

func (s *Store) Save(ctx context.Context, snapshot Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	query, args, err := s.builder.
		Insert("sync_meta").
		Columns("key", "value").
		Values("schema_signature", snapshot.SchemaSignature).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = excluded.value").
		ToSql()
	if err != nil {
		return fmt.Errorf("build cache schema signature upsert: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("save cache schema signature: %w", err)
	}

	savedAt := time.Now().UTC()
	query, args, err = s.builder.
		Insert("sync_meta").
		Columns("key", "value").
		Values("saved_at", savedAt.Format(time.RFC3339)).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = excluded.value").
		ToSql()
	if err != nil {
		return fmt.Errorf("build cache saved_at upsert: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("save cache saved_at: %w", err)
	}

	query, args, err = s.builder.Delete("snyk_findings").ToSql()
	if err != nil {
		return fmt.Errorf("build clear Snyk cache rows query: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("clear Snyk cache rows: %w", err)
	}
	for fingerprint, hash := range snapshot.SnykHashes {
		query, args, err = s.builder.
			Insert("snyk_findings").
			Columns("fingerprint", "hash").
			Values(fingerprint, hash).
			ToSql()
		if err != nil {
			return fmt.Errorf("build insert Snyk cache row: %w", err)
		}
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert Snyk cache row: %w", err)
		}
	}

	query, args, err = s.builder.Delete("linear_issues").ToSql()
	if err != nil {
		return fmt.Errorf("build clear Linear cache rows query: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("clear Linear cache rows: %w", err)
	}
	for fingerprint, hash := range snapshot.LinearHashes {
		query, args, err = s.builder.
			Insert("linear_issues").
			Columns("fingerprint", "hash").
			Values(fingerprint, hash).
			ToSql()
		if err != nil {
			return fmt.Errorf("build insert Linear cache row: %w", err)
		}
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert Linear cache row: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit cache transaction: %w", err)
	}

	return nil
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS sync_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snyk_findings (
			fingerprint TEXT PRIMARY KEY,
			hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS linear_issues (
			fingerprint TEXT PRIMARY KEY,
			hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snyk_ignores (
			project_id TEXT NOT NULL,
			issue_key TEXT NOT NULL,
			expires_at TEXT,
			disregard_if_fixable INTEGER NOT NULL DEFAULT 0,
			created_at TEXT,
			updated_at TEXT,
			PRIMARY KEY (project_id, issue_key)
		)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite cache: %w", err)
		}
	}

	if err := s.migrateSnykIgnoresCache(ctx); err != nil {
		return fmt.Errorf("migrate snyk ignores cache: %w", err)
	}

	query, args, err := s.builder.
		Insert("sync_meta").
		Columns("key", "value").
		Values("schema_version", schemaVersion).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = excluded.value").
		ToSql()
	if err != nil {
		return fmt.Errorf("build initialize sqlite schema version upsert: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("initialize sqlite cache: %w", err)
	}

	return nil
}

func (s *Store) migrateSnykIgnoresCache(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(snyk_ignores)")
	if err != nil {
		return fmt.Errorf("check snyk_ignores columns: %w", err)
	}
	defer rows.Close()

	var hasUpdatedAt bool
	for rows.Next() {
		var cid, notNull, pk int
		var name string
		var typ, dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dfltValue, &pk); err != nil {
			return fmt.Errorf("scan table_info: %w", err)
		}
		if name == "updated_at" {
			hasUpdatedAt = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate table_info: %w", err)
	}

	if !hasUpdatedAt {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE snyk_ignores ADD COLUMN updated_at TEXT"); err != nil {
			return fmt.Errorf("add updated_at column: %w", err)
		}
		if _, err := s.db.ExecContext(ctx, "UPDATE snyk_ignores SET updated_at = CURRENT_TIMESTAMP"); err != nil {
			return fmt.Errorf("seed updated_at column: %w", err)
		}
	}
	return nil
}

// sqliteTimestampLayout is the format of SQLite's CURRENT_TIMESTAMP, which
// the updated_at migration used to seed existing rows. Unlike RFC3339 it
// carries no timezone; CURRENT_TIMESTAMP is always UTC.
const sqliteTimestampLayout = "2006-01-02 15:04:05"

// parseTimestamp parses a cache timestamp column value. Values are written as
// RFC3339, but rows seeded by the updated_at migration carry SQLite's
// CURRENT_TIMESTAMP format instead, so both are accepted and the result is
// normalised to UTC. An empty value means genuinely absent and yields a zero
// time with ok=true. A non-empty value in neither format yields ok=false so
// the caller can treat it explicitly rather than silently reading a zero time.
func parseTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts.UTC(), true
	}
	if ts, err := time.Parse(sqliteTimestampLayout, raw); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}

// LoadIgnores returns the cached v1 ignore metadata for a project.
func (s *Store) LoadIgnores(ctx context.Context, projectID string) (map[string]IgnoreMeta, error) {
	out := make(map[string]IgnoreMeta)

	cutoff := time.Now().UTC().Add(-ignoreEntryTTL)

	query, args, err := s.builder.
		Select("issue_key", "expires_at", "disregard_if_fixable", "created_at", "updated_at").
		From("snyk_ignores").
		Where(sq.Eq{"project_id": projectID}).
		ToSql()
	if err != nil {
		return nil, fmt.Errorf("build cache ignores query: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("load cache ignores: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var issueKey string
		var expiresAtRaw, createdAtRaw, updatedAtRaw string
		var disregardIfFixable int
		if err := rows.Scan(&issueKey, &expiresAtRaw, &disregardIfFixable, &createdAtRaw, &updatedAtRaw); err != nil {
			return nil, fmt.Errorf("scan cache ignore row: %w", err)
		}

		meta := IgnoreMeta{
			IssueKey:           issueKey,
			DisregardIfFixable: disregardIfFixable != 0,
		}
		// Timestamps are written as RFC3339 (SaveIgnores), but the
		// updated_at migration seeded existing rows with SQLite's
		// CURRENT_TIMESTAMP, whose format has no timezone offset. Accept both
		// on read; otherwise the migration's own rows parse as a zero time
		// and never age out. An empty value is genuinely absent and keeps its
		// current meaning (a zero time). A non-empty value in neither format
		// is corrupt: for updated_at that means evict (see below); for
		// expires_at and created_at it keeps the previous zero-time reading.
		if expiresAt, ok := parseTimestamp(expiresAtRaw); ok {
			meta.ExpiresAt = expiresAt
		}
		if createdAt, ok := parseTimestamp(createdAtRaw); ok {
			meta.CreatedAt = createdAt
		}
		updatedAt, updatedAtOK := parseTimestamp(updatedAtRaw)
		if !updatedAtOK {
			// A corrupt updated_at cannot be aged, so evict the row instead
			// of keeping it silently forever. Count it separately from TTL
			// eviction so an operator can tell corruption from benign aging.
			s.countIgnoreEviction(false)
			continue
		}
		if !updatedAt.IsZero() && updatedAt.Before(cutoff) {
			s.countIgnoreEviction(true)
			continue
		}
		meta.UpdatedAt = updatedAt
		out[issueKey] = meta
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache ignores rows: %w", err)
	}

	return out, nil
}

// countIgnoreEviction records one row discarded by LoadIgnores.
func (s *Store) countIgnoreEviction(ttlExpired bool) {
	s.evictionMu.Lock()
	defer s.evictionMu.Unlock()
	if ttlExpired {
		s.ignoreEvictions.TTLExpired++
		return
	}
	s.ignoreEvictions.Unparseable++
}

// IgnoreEvictions returns the cumulative counts of cached v1 ignore rows
// discarded by LoadIgnores over this store's lifetime. A sync run loads each
// project's ignores once, so these are the eviction counts for the run.
func (s *Store) IgnoreEvictions() IgnoreEvictionCounts {
	s.evictionMu.Lock()
	defer s.evictionMu.Unlock()
	return s.ignoreEvictions
}

// SaveIgnores stores v1 ignore metadata for a project, replacing any existing rows.
func (s *Store) SaveIgnores(ctx context.Context, projectID string, ignores map[string]IgnoreMeta) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache ignores transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	query, args, err := s.builder.
		Delete("snyk_ignores").
		Where(sq.Eq{"project_id": projectID}).
		ToSql()
	if err != nil {
		return fmt.Errorf("build clear cache ignores query: %w", err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("clear cache ignores: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for issueKey, meta := range ignores {
		expiresAtRaw := ""
		if !meta.ExpiresAt.IsZero() {
			expiresAtRaw = meta.ExpiresAt.UTC().Format(time.RFC3339)
		}
		createdAtRaw := ""
		if !meta.CreatedAt.IsZero() {
			createdAtRaw = meta.CreatedAt.UTC().Format(time.RFC3339)
		}
		updatedAtRaw := now
		if !meta.UpdatedAt.IsZero() {
			updatedAtRaw = meta.UpdatedAt.UTC().Format(time.RFC3339)
		}
		var disregard int64
		if meta.DisregardIfFixable {
			disregard = 1
		}

		query, args, err = s.builder.
			Insert("snyk_ignores").
			Columns("project_id", "issue_key", "expires_at", "disregard_if_fixable", "created_at", "updated_at").
			Values(projectID, issueKey, expiresAtRaw, disregard, createdAtRaw, updatedAtRaw).
			ToSql()
		if err != nil {
			return fmt.Errorf("build insert cache ignore row: %w", err)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert cache ignore row: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache ignores transaction: %w", err)
	}
	return nil
}
