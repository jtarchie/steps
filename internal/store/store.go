// Package store is the sqlite-backed persistence layer for job-run/node
// state.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver name used by sql.Open below
)

// Store is the sqlite-backed persistence layer for job-run/node state, scoped
// to one pipeline.
//
// pipelineID is why every method below reads like it always did: a state file
// may hold several pipelines, and rather than thread a pipeline through sixty
// signatures, the handle IS the scope. A caller cannot forget to pass it and
// cannot pass the wrong one; `steps web` serving three pipelines from one file
// holds three of these.
type Store struct {
	db         *sql.DB
	path       string
	pipeline   string
	pipelineID int64
	// revisionID is the configuration StartRun pins onto new runs, 0 before
	// anything has recorded one (see RecordRevision). Atomic because a
	// `steps web` handle is shared by the poller and the queue drain, and a
	// reload writes it while a build reads it.
	revisionID atomic.Int64
	// readOnly marks a handle obtained by RESOLVING a pipeline rather than
	// registering one (OpenExisting). It changes nothing about what the ~60
	// methods can do — Go cannot enforce that — and everything about Close,
	// which otherwise compacts and checkpoints the file. A command whose whole
	// contract is that it only asks must not change the bytes on disk, and
	// must not take the write lock a live daemon is using.
	readOnly bool
}

// OpenStore opens (creating if necessary) the sqlite database at path, applies
// the schema, and scopes the returned handle to the named pipeline — creating
// its row on first sight. The parent directory is created if it doesn't exist.
//
// pipelineName is an identity, not a path: it defaults to the YAML's base name
// and is overridden with --name. See the pipelines table.
func OpenStore(path, pipelineName string) (*Store, error) {
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
	// arise for one HANDLE (SetMaxOpenConns(1) below), but a --state file
	// holding several pipelines gets one handle per pipeline — separate pools
	// on one file, inside one process — and every one of them enqueues.
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
	//     truncated only at a checkpoint no long-lived `steps web` ever
	//     reaches on its own. An unbounded WAL is a second copy of the database
	//     growing beside it.
	//
	// auto_vacuum is recorded in the file header and can only be SET on an empty
	// database, so this takes effect for databases steps creates; an existing one
	// keeps its mode until it is vacuumed. That is why Close still checkpoints
	// explicitly rather than relying on the mode alone.
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	_, err = db.ExecContext(ctx, schema)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	err = checkSchemaVersion(ctx, db, path)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	id, err := registerPipeline(ctx, db, pipelineName, path)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	return &Store{db: db, path: path, pipeline: pipelineName, pipelineID: id}, nil
}

// openDB opens the sqlite file with the pragmas above, for a writer and a
// reader alike. A Reader takes the same connection settings deliberately: it
// reads the same file a writer may be appending to, so it wants the same WAL
// and the same busy timeout, and the only thing it does differently is never
// write.
func openDB(path string) (*sql.DB, error) {
	return openDSN(path, "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"+
		"&_pragma=auto_vacuum(incremental)&_pragma=journal_size_limit(67108864)&_txlock=immediate")
}

// openReadOnlyDB opens an existing file for a reader.
//
// Every pragma the writer sets that WRITES is dropped: journal_mode and
// auto_vacuum are recorded in the file header, journal_size_limit sizes a log
// only a writer creates, and _txlock=immediate takes the write lock at BEGIN.
// They are set on connect, so a reader carrying them modifies the database
// before it has read anything — which is both a broken promise and, against a
// file the operator has made read-only, an outright failure to open.
//
// busy_timeout stays: a reader still waits behind a daemon's writer rather
// than failing instantly. foreign_keys stays because it costs nothing and
// keeps every handle in this package answering the same way.
func openReadOnlyDB(path string) (*sql.DB, error) {
	return openDSN(path, "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
}

func openDSN(path, dsn string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+dsn)
	if err != nil {
		return nil, fmt.Errorf("could not open state db %q: %w", path, err)
	}

	// SQLite only ever allows one writer at a time regardless of pool size, so
	// a bigger pool adds contention for no write throughput. It also serializes
	// `steps web --max-concurrent`'s worker goroutines onto one connection.
	// (Revisit if reads become hot: WAL permits a separate read pool.)
	db.SetMaxOpenConns(1)

	return db, nil
}

// registerPipeline resolves the name to its row id, inserting it the first
// time.
//
// It does NOT fill in the path: what belongs in that column is where the
// pipeline's YAML lives, and this function is given the state file — the same
// string for every pipeline sharing one, which is no answer at all to "which
// checkout is `infra`". Only a command that LOADED the YAML knows, and
// SetSourcePath is how it says so.
func registerPipeline(ctx context.Context, db *sql.DB, name, path string) (int64, error) {
	if name == "" {
		return 0, errors.New("a state store needs a pipeline name; pass --name or let it default to the pipeline file's base name")
	}

	_, err := db.ExecContext(ctx, `
		INSERT INTO pipelines (name, path) VALUES (?, '')
		ON CONFLICT(name) DO NOTHING
	`, name)
	if err != nil {
		return 0, fmt.Errorf("could not register pipeline %q in %q: %w", name, path, err)
	}

	var id int64

	err = db.QueryRowContext(ctx, `SELECT id FROM pipelines WHERE name = ?`, name).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("could not read pipeline %q from %q: %w", name, path, err)
	}

	return id, nil
}

// SetSourcePath records where this pipeline's YAML lives, for whoever reads
// the file back — `steps runs --state shared.db` lists it, and a name alone
// does not say which of two checkouts `pipeline` is.
//
// Written on every open by the commands that load a pipeline, so a checkout
// that moves stops reporting where it used to live. A read-only command has
// nothing to correct and does not call this: it would be recording a path it
// never resolved.
func (s *Store) SetSourcePath(ctx context.Context, source string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET path = ? WHERE id = ?`, source, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not record the source path of pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Pipeline is the name this handle is scoped to.
func (s *Store) Pipeline() string { return s.pipeline }

// Path is the state file behind this handle. Two handles reporting the same
// path share a file — which is what --state makes possible, and what lets a
// caller ask one Reader about several pipelines at once instead of querying
// each and merging.
func (s *Store) Path() string { return s.path }

// ErrSchemaVersion is a database some other build of steps wrote.
var ErrSchemaVersion = errors.New("the state database was written by a different version of steps")

// checkSchemaVersion refuses a database this build cannot write to, and stamps
// one it just created.
//
// This is NOT migration machinery, and deliberately not the first step of any:
// there is no upgrade path here, by design, and the answer to a stale database
// is still to delete it. What it adds is that somebody is TOLD.
//
// Without it the failure was silent and looked like success. The schema is
// CREATE TABLE IF NOT EXISTS, so an older database opens fine and simply lacks
// a column; every INSERT naming that column then fails, and the run-event sink
// only warns — a build went green having recorded nothing, and whoever ran it
// found out when they opened the web UI to an empty run. A green build that
// quietly lost its history is worse than a red one that says why.
//
// PRAGMA user_version rather than a table: it lives in the file header, costs
// no row, and cannot itself be the thing that is missing.
func checkSchemaVersion(ctx context.Context, db *sql.DB, path string) error {
	found, err := readSchemaVersion(ctx, db, path)
	if err != nil {
		return err
	}

	// Zero is a database created before this check existed, which is every
	// database older than it — and every one of those predates at least this
	// column. Stamping instead of refusing would bless exactly the stale files
	// the check is for, so the only zero that is accepted is one this open
	// just created, which the DDL above has already filled in.
	if found == schemaVersion {
		return nil
	}

	if found == 0 && freshlyCreated(ctx, db) {
		_, err = db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion))
		if err != nil {
			return fmt.Errorf("could not stamp the schema version of %q: %w", path, err)
		}

		return nil
	}

	return schemaVersionError(path, found)
}

// readSchemaVersion reports what schema the file at path was written by.
func readSchemaVersion(ctx context.Context, db *sql.DB, path string) (int, error) {
	var found int

	err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&found)
	if err != nil {
		return 0, fmt.Errorf("could not read the schema version of %q: %w", path, err)
	}

	return found, nil
}

// schemaVersionError is the one message a file this build cannot use gets,
// whether the caller meant to write to it or only to read it — a reader of a
// stale file is told the same thing, because the answer is the same and
// because a read that silently returned nothing is the failure mode this
// whole check exists for.
func schemaVersionError(path string, found int) error {
	return fmt.Errorf("%w: %s is at schema %d and this steps writes %d — there is no upgrade path (see the schema's own comment); delete the file and run again, which loses that pipeline's run history and cache but nothing else",
		ErrSchemaVersion, path, found, schemaVersion)
}

// freshlyCreated reports that this open made the database, rather than
// inheriting one an older build left. A database with no run and no node has
// nothing to lose by being stamped.
func freshlyCreated(ctx context.Context, db *sql.DB) bool {
	var rows int

	err := db.QueryRowContext(ctx,
		"SELECT (SELECT COUNT(*) FROM runs) + (SELECT COUNT(*) FROM nodes)").Scan(&rows)

	return err == nil && rows == 0
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

	// A reader compacts nothing. Both statements below are writes — one moves
	// pages, the other rewrites the log — so a `steps runs` that ran them
	// changed the file it was only asked to read, took the write lock a live
	// daemon holds, and failed outright on a file with the write bit off.
	if s.readOnly {
		//nolint:wrapcheck // the caller names the store it was closing
		return s.db.Close()
	}

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
