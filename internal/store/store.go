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
	db   *sql.DB
	path string
}

// OpenStore opens (creating if necessary) the sqlite database at path and
// applies the schema. The parent directory is created if it doesn't exist.
func OpenStore(path string) (*Store, error) {
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create state directory for %q: %w", path, err)
	}

	// The DSN sets three pragmas on every connection at open time, plus the
	// transaction mode:
	//   - busy_timeout makes a writer wait for the write lock instead of
	//     failing immediately with SQLITE_BUSY.
	//   - journal_mode=WAL lets readers proceed concurrently with a writer,
	//     which the default rollback journal serializes.
	//   - foreign_keys turns on constraint ENFORCEMENT, which SQLite leaves
	//     off per connection unless asked.
	//   - _txlock=immediate takes the write lock when a transaction BEGINS
	//     rather than when it first writes.
	//
	// foreign_keys is on so that a declared constraint means something. No
	// table declares one today, which is exactly why it is set now and
	// separately: switching enforcement on is a no-op that can be verified
	// against the whole existing schema, rather than a variable in whichever
	// change first depends on it. From here a REFERENCES clause is a rule the
	// database keeps, not documentation — and ON DELETE CASCADE is how a row
	// takes its dependents with it instead of leaving orphans for application
	// code to remember.
	//
	// That last one is not a tuning knob. A deferred transaction that reads
	// and then writes — which is what RecordVersions is, assigning
	// check_order from a MAX it just read — has to UPGRADE its lock, and
	// SQLite refuses
	// an upgrade that would have to wait, returning SQLITE_BUSY immediately
	// rather than honoring busy_timeout, because waiting there could
	// deadlock two transactions against each other. Taking the write lock up
	// front turns that into an ordinary wait. Within one process this cannot
	// arise (SetMaxOpenConns(1) below), but `steps watch` and `steps web`
	// against one state.db are two processes, and both enqueue.
	//
	// Every transaction in this package is a writer, so there is no
	// read-only transaction paying for the stricter lock.
	//
	// WAL is recorded in the database file header, so the conversion happens
	// exactly once and every later connection's pragma is a cheap no-op. No
	// retry loop guards the conversion: OpenStore is called once per process
	// at startup, so a single `steps` process is never racing another PROCESS
	// to convert the same brand-new file — the only scenario in which
	// busy_timeout fails to cover the conversion's exclusive lock.
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("could not open state db %q: %w", path, err)
	}

	// SQLite only ever allows one writer at a time regardless of pool size, so
	// a bigger pool adds contention for no write throughput. It also serializes
	// `steps watch --max-concurrent`'s worker goroutines onto one connection.
	// (Revisit if reads become hot: WAL permits a separate read pool.)
	db.SetMaxOpenConns(1)

	ctx := context.Background()

	err = dropLegacyTables(ctx, db)
	if err == nil {
		_, err = db.ExecContext(ctx, schema)
	}

	if err == nil {
		err = addColumns(ctx, db)
	}

	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	return &Store{db: db, path: path}, nil
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
