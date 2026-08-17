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
	// Two more pragmas exist for retention rather than for speed, and they are
	// what turns a delete into free disk:
	//   - auto_vacuum=incremental marks freed pages for reuse and lets
	//     PRAGMA incremental_vacuum hand them back to the filesystem. Without
	//     it a pruned database keeps every page it ever allocated — the file
	//     never shrinks, it only stops growing, which is not what anyone means
	//     by bounding a footprint. incremental rather than full: full vacuums on
	//     every commit, paying for compaction in the middle of a build.
	//   - journal_size_limit caps the write-ahead log, which is otherwise
	//     truncated only at a checkpoint no long-lived `steps watch` ever
	//     reaches on its own. An unbounded WAL is a second copy of the database
	//     growing beside it.
	//
	// auto_vacuum is recorded in the file header and can only be SET on an empty
	// database, so this takes effect for databases steps creates; an existing one
	// keeps its mode until it is vacuumed. That is why Close still checkpoints
	// explicitly rather than relying on the mode alone.
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
			"&_pragma=auto_vacuum(incremental)&_pragma=journal_size_limit(67108864)&_txlock=immediate")
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
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	return &Store{db: db, path: path}, nil
}

// Close reclaims what retention freed and closes the underlying connection.
//
// Both reclaims are best-effort and deliberately ignored: a database that could
// not be compacted is still a valid database, and failing Close over it would
// turn a housekeeping problem into a failed command. Deleting rows only marks
// pages free, so without this a pruned file never shrinks — it just stops
// growing.
func (s *Store) Close() error {
	ctx := context.Background()

	// Hands freed pages back to the filesystem. Unbounded on purpose: this runs
	// once, at exit, with no build waiting on it.
	_, _ = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)

	// Folds the write-ahead log back into the database and truncates it, so the
	// footprint on disk is the database rather than the database plus however
	// large the log grew during a long watch.
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)

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

// sortableNano is RFC3339 with a FIXED nine-digit fraction.
//
// time.RFC3339Nano is the obvious choice and is wrong for a stored column: its
// layout uses "9"s, which TRIM trailing zeros, so the strings it produces are
// not comparable in time order. '…:00.5Z' sorts AFTER '…:00.51Z' (because 'Z'
// 0x5A > '1' 0x31), and a timestamp landing exactly on a second has no fraction
// at all and sorts after every fractional timestamp within it.
//
// That matters because sqlite orders these as TEXT: ListRuns, LatestRunByJob
// and FirstRunSince all sort on them, and retention DELETES by that order. With
// the trimming layout, two runs in the same second could be reaped in the wrong
// order — throwing away the newer one, which is the one somebody wants to look
// at. Zero-padding costs a few bytes a row and makes string order time order.
const sortableNano = "2006-01-02T15:04:05.000000000Z07:00"

// nowNano is the sub-second form, for tables whose rows are ordered or
// subtracted: at whole-second resolution every run faster than a second
// reports a duration of zero, which is most task-only runs. RFC3339 parsing
// accepts the fractional form, so reading stays uniform.
func nowNano() string {
	return time.Now().UTC().Format(sortableNano)
}

func nullableString(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return string(b)
}

// MaxStoredErrorBytes bounds the error text any column here keeps.
//
// A stored error is unbounded by default, and one of them is routinely enormous:
// a failing check or task reports "command %q failed", where the command is the
// whole generated shell script — about 1.3KB for the built-in git check, with
// its comments. That message is exactly right in a terminal and far too big to
// keep a copy of per failure, forever. A remote that is down at a one-minute
// poll wrote that same 1.3KB every minute.
//
// 2KB keeps the head of any real message — the part naming what failed — while
// making the worst case a rounding error rather than the largest thing a failing
// watch produces.
const MaxStoredErrorBytes = 2 * 1024

// errText renders an error for a nullable error column: NULL when there was
// none, and never more than MaxStoredErrorBytes of text.
//
// Truncated at the head for the reason a transcript is: the message opens with
// what failed and closes with the detail, and "could not record node X:
// constraint failed" is worth more than the tail of a shell script.
func errText(err error) any {
	if err == nil {
		return nil
	}

	text := err.Error()
	if len(text) > MaxStoredErrorBytes {
		const notice = "… [truncated]"

		text = text[:MaxStoredErrorBytes-len(notice)] + notice
	}

	return text
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
