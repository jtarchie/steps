// Package store is the sqlite-backed persistence layer for job-run/node
// state.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver name used by sql.Open below
)

// Store is the sqlite-backed persistence layer for job-run/node state.
type Store struct {
	db *sql.DB
}

// OpenStore opens (creating if necessary) the sqlite database at path and
// applies the schema. The parent directory is created if it doesn't exist.
func OpenStore(path string) (*Store, error) {
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create state directory for %q: %w", path, err)
	}

	// The DSN sets two pragmas on every connection at open time:
	//   - busy_timeout makes a writer wait for the write lock instead of
	//     failing immediately with SQLITE_BUSY.
	//   - journal_mode=WAL lets readers proceed concurrently with a writer,
	//     which the default rollback journal serializes.
	//
	// WAL is recorded in the database file header, so the conversion happens
	// exactly once and every later connection's pragma is a cheap no-op. No
	// retry loop guards the conversion: OpenStore is called once per process
	// at startup, so a single `steps` process is never racing another PROCESS
	// to convert the same brand-new file — the only scenario in which
	// busy_timeout fails to cover the conversion's exclusive lock.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("could not open state db %q: %w", path, err)
	}

	// SQLite only ever allows one writer at a time regardless of pool size, so
	// a bigger pool adds contention for no write throughput. It also serializes
	// `steps watch --max-concurrent`'s worker goroutines onto one connection.
	// (Revisit if reads become hot: WAL permits a separate read pool.)
	db.SetMaxOpenConns(1)

	ctx := context.Background()

	_, err = db.ExecContext(ctx, schema)
	if err == nil {
		err = addColumns(ctx, db)
	}

	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	err := s.db.Close()
	if err != nil {
		return fmt.Errorf("could not close state db: %w", err)
	}

	return nil
}

// now is the timestamp format every table but runs/run_events uses.
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// nowNano is the sub-second form, for tables whose rows are ordered or
// subtracted: at whole-second resolution every run faster than a second
// reports a duration of zero, which is most task-only runs. RFC3339 parsing
// accepts the fractional form, so reading stays uniform.
func nowNano() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func nullableString(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return string(b)
}

// errText renders an error for a nullable error column: NULL when there was
// none.
func errText(err error) any {
	if err == nil {
		return nil
	}

	return err.Error()
}

// parseTimestamp turns a stored RFC3339 string into a Time, yielding the zero
// Time for an empty or unparseable value. Reading history is a diagnostic, so
// a malformed row is better rendered blank than fatal.
func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

// collect runs a query and decodes every row through scan.
//
// It exists because the alternative — query, defer Close, loop, scan, check
// rows.Err — is fifteen lines of identical bookkeeping at every one of the
// sixteen list queries in this package, and the last of those five steps is
// the one that is quietly forgotten. what names the thing being read, for the
// error message.
func collect[T any](ctx context.Context, db *sql.DB, what, query string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", what, err)
	}

	defer func() { _ = rows.Close() }()

	var out []T

	for rows.Next() {
		item, scanErr := scan(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read %s: %w", what, scanErr)
		}

		out = append(out, item)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", what, err)
	}

	return out, nil
}
