package cache

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sq "github.com/Masterminds/squirrel"
	_ "modernc.org/sqlite"
)

const schemaVersion = "v1"

// Snapshot is the persisted sync state: its save time, schema signature and
// the normalized hash of each Snyk DAST finding and each managed Linear issue.
// SavedAt is zero when loading a cache written before save times were recorded.
type Snapshot struct {
	SavedAt         time.Time
	SchemaSignature string
	SnykDASTHashes  map[string]string
	LinearHashes    map[string]string
}

type Store struct {
	db      *sql.DB
	builder sq.StatementBuilderType
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
		SnykDASTHashes: map[string]string{},
		LinearHashes:   map[string]string{},
	}

	query, args, err := s.builder.
		Select("key", "value").
		From("sync_meta").
		Where(sq.Eq{"key": []string{"schema_signature", "saved_at"}}).
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build cache metadata query: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load cache schema signature: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Snapshot{}, fmt.Errorf("scan cache metadata: %w", err)
		}
		switch key {
		case "schema_signature":
			snapshot.SchemaSignature = value
		case "saved_at":
			// A malformed saved_at only means the age is unknown; it must
			// not stop the cache from being used. The cache is only an
			// optimisation: a corrupt row must never fail the whole sync,
			// and it heals the next time Save runs.
			if savedAt, ok := parseTimestamp(value); ok {
				snapshot.SavedAt = savedAt
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate cache schema signature rows: %w", err)
	}

	query, args, err = s.builder.
		Select("fingerprint", "hash").
		From("snyk_dast_findings").
		ToSql()
	if err != nil {
		return Snapshot{}, fmt.Errorf("build Snyk DAST cache rows query: %w", err)
	}

	snykdastRows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Snapshot{}, fmt.Errorf("load Snyk DAST cache rows: %w", err)
	}
	defer snykdastRows.Close()
	for snykdastRows.Next() {
		var fingerprint string
		var hash string
		if err := snykdastRows.Scan(&fingerprint, &hash); err != nil {
			return Snapshot{}, fmt.Errorf("scan Snyk DAST cache row: %w", err)
		}
		snapshot.SnykDASTHashes[fingerprint] = hash
	}
	if err := snykdastRows.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("iterate Snyk DAST cache rows: %w", err)
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

// parseTimestamp parses a sync_meta timestamp value. Values are written as
// RFC3339Nano, but rows written by earlier versions or external tooling may
// carry plain RFC3339 or SQLite's CURRENT_TIMESTAMP format instead, so all
// three are accepted and the result is normalised to UTC. An empty value means
// genuinely absent and yields a zero time with ok=true. A non-empty value in
// none of these formats yields ok=false so the caller can treat the age as
// unknown rather than failing the load.
func parseTimestamp(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, true
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC(), true
		}
	}
	return time.Time{}, false
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

	savedAt := snapshot.SavedAt
	if savedAt.IsZero() {
		savedAt = time.Now().UTC()
	}

	query, args, err := s.builder.
		Insert("sync_meta").
		Columns("key", "value").
		Values("saved_at", savedAt.UTC().Format(time.RFC3339Nano)).
		Suffix("ON CONFLICT(key) DO UPDATE SET value = excluded.value").
		ToSql()
	if err != nil {
		return fmt.Errorf("build cache saved_at upsert: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("save cache saved_at timestamp: %w", err)
	}

	query, args, err = s.builder.
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

	query, args, err = s.builder.Delete("snyk_dast_findings").ToSql()
	if err != nil {
		return fmt.Errorf("build clear Snyk DAST cache rows query: %w", err)
	}
	if _, err = tx.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("clear Snyk DAST cache rows: %w", err)
	}
	for fingerprint, hash := range snapshot.SnykDASTHashes {
		query, args, err = s.builder.
			Insert("snyk_dast_findings").
			Columns("fingerprint", "hash").
			Values(fingerprint, hash).
			ToSql()
		if err != nil {
			return fmt.Errorf("build insert Snyk DAST cache row: %w", err)
		}
		if _, err = tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("insert Snyk DAST cache row: %w", err)
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
		`CREATE TABLE IF NOT EXISTS snyk_dast_findings (
			fingerprint TEXT PRIMARY KEY,
			hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS linear_issues (
			fingerprint TEXT PRIMARY KEY,
			hash TEXT NOT NULL
		)`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize sqlite cache: %w", err)
		}
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
