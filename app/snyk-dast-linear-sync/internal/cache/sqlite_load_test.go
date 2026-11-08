package cache

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	sq "github.com/Masterminds/squirrel"
)

// errStubReadFailure is the sentinel a stub Rows returns from Next once its
// fixture rows are exhausted and failure is armed, to simulate a
// mid-iteration read failure (for example an I/O error on a later page).
var errStubReadFailure = errors.New("stub read failure")

// stubRows yields its fixture rows, then either io.EOF or, when failure is
// armed, the sentinel error.
type stubRows struct {
	columns []string
	fixture [][]driver.Value
	// failWhenExhausted returns the sentinel instead of io.EOF after the
	// last fixture row, so iteration fails mid-way through the result set.
	failWhenExhausted bool
	next              int
}

func (r *stubRows) Columns() []string { return r.columns }
func (r *stubRows) Close() error      { return nil }

func (r *stubRows) Next(dest []driver.Value) error {
	if r.next >= len(r.fixture) {
		if r.failWhenExhausted {
			return errStubReadFailure
		}
		return io.EOF
	}
	copy(dest, r.fixture[r.next])
	r.next++
	return nil
}

// stubConn answers Load's three queries; failingTable arms the sentinel for
// at most one of them and is empty on the healthy path.
type stubConn struct {
	failingTable string
}

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("stub driver: statements are not supported")
}

func (c *stubConn) Close() error { return nil }

func (c *stubConn) Begin() (driver.Tx, error) {
	return nil, errors.New("stub driver: transactions are not supported")
}

func (c *stubConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	switch {
	case strings.Contains(query, "sync_meta"):
		return &stubRows{
			columns: []string{"key", "value"},
			fixture: [][]driver.Value{
				{"schema_signature", "sig-v1"},
				{"saved_at", "2026-01-02T03:04:05Z"},
			},
			failWhenExhausted: c.failingTable == "sync_meta",
		}, nil
	case strings.Contains(query, "snyk_dast_findings"):
		return &stubRows{
			columns: []string{"fingerprint", "hash"},
			fixture: [][]driver.Value{
				{"fp-1", "hash-a"},
				{"fp-2", "hash-b"},
			},
			failWhenExhausted: c.failingTable == "snyk_dast_findings",
		}, nil
	case strings.Contains(query, "linear_issues"):
		return &stubRows{
			columns: []string{"fingerprint", "hash"},
			fixture: [][]driver.Value{
				{"issue-1", "hash-l-a"},
				{"issue-2", "hash-l-b"},
			},
			failWhenExhausted: c.failingTable == "linear_issues",
		}, nil
	}
	return nil, errors.New("stub driver: unexpected query " + query)
}

type stubConnector struct{ failingTable string }

func (c *stubConnector) Connect(context.Context) (driver.Conn, error) {
	return &stubConn{failingTable: c.failingTable}, nil
}

func (c *stubConnector) Driver() driver.Driver { return stubDriver{} }

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("stub driver: Open is not supported; use sql.OpenDB")
}

// newStubStore builds a Store directly from a stub connector, without going
// through Open and a real SQLite file, so Load can be driven with rows that
// fail mid-iteration.
func newStubStore(t *testing.T, failingTable string) *Store {
	t.Helper()
	db := sql.OpenDB(&stubConnector{failingTable: failingTable})
	t.Cleanup(func() { _ = db.Close() })
	return &Store{db: db, builder: sq.StatementBuilder.PlaceholderFormat(sq.Question)}
}

func TestLoadFailsInsteadOfReturningPartialSnapshot(t *testing.T) {
	tests := []struct {
		name         string
		failingTable string
		wantTable    string
	}{
		{
			name:         "schema signature read fails mid-iteration",
			failingTable: "sync_meta",
			wantTable:    "schema signature",
		},
		{
			name:         "Snyk DAST read fails mid-iteration",
			failingTable: "snyk_dast_findings",
			wantTable:    "Snyk DAST cache",
		},
		{
			name:         "Linear read fails mid-iteration",
			failingTable: "linear_issues",
			wantTable:    "Linear cache",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStubStore(t, tt.failingTable)

			snapshot, err := store.Load(context.Background())
			if err == nil {
				t.Fatalf("Load() error = nil, want error wrapping %v; snapshot = %+v", errStubReadFailure, snapshot)
			}
			if !errors.Is(err, errStubReadFailure) {
				t.Fatalf("Load() error = %v, want it to wrap %v", err, errStubReadFailure)
			}
			if !strings.Contains(err.Error(), tt.wantTable) {
				t.Fatalf("Load() error = %q, want it to name %q", err.Error(), tt.wantTable)
			}
			if snapshot.SchemaSignature != "" || len(snapshot.SnykDASTHashes) != 0 || len(snapshot.LinearHashes) != 0 {
				t.Fatalf("Load() snapshot = %+v, want empty Snapshot on read failure", snapshot)
			}
		})
	}
}

func TestLoadHealthyIterationReturnsAllRows(t *testing.T) {
	store := newStubStore(t, "")

	snapshot, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	wantSnyk := map[string]string{"fp-1": "hash-a", "fp-2": "hash-b"}
	if len(snapshot.SnykDASTHashes) != len(wantSnyk) {
		t.Fatalf("SnykDASTHashes = %v, want %v", snapshot.SnykDASTHashes, wantSnyk)
	}
	for fingerprint, hash := range wantSnyk {
		if got := snapshot.SnykDASTHashes[fingerprint]; got != hash {
			t.Fatalf("SnykDASTHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}

	wantLinear := map[string]string{"issue-1": "hash-l-a", "issue-2": "hash-l-b"}
	if len(snapshot.LinearHashes) != len(wantLinear) {
		t.Fatalf("LinearHashes = %v, want %v", snapshot.LinearHashes, wantLinear)
	}
	for fingerprint, hash := range wantLinear {
		if got := snapshot.LinearHashes[fingerprint]; got != hash {
			t.Fatalf("LinearHashes[%q] = %q, want %q", fingerprint, got, hash)
		}
	}
}
