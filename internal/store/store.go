// Package store is the sqlite-backed persistence layer for job-run/node
// state.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver name used by sql.Open below
)

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
    hash        TEXT PRIMARY KEY,
    parent_hash TEXT NOT NULL,
    kind        TEXT NOT NULL,
    job_name    TEXT NOT NULL,
    resource    TEXT NOT NULL,
    step_index  INTEGER NOT NULL,
    content     TEXT NOT NULL,
    result      TEXT,
    status      TEXT NOT NULL,
    error       TEXT,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_nodes_parent_hash ON nodes(parent_hash);

CREATE TABLE IF NOT EXISTS job_runs (
    job_name   TEXT NOT NULL,
    root_hash  TEXT NOT NULL,
    status     TEXT NOT NULL,
    error      TEXT,
    created_at TEXT NOT NULL,
    PRIMARY KEY (job_name, root_hash)
);

CREATE TABLE IF NOT EXISTS resource_checks (
    resource_name TEXT PRIMARY KEY,
    version_json  TEXT NOT NULL,
    checked_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS trigger_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    status      TEXT NOT NULL,
    enqueued_at TEXT NOT NULL,
    started_at  TEXT,
    finished_at TEXT,
    error       TEXT
);
-- At most one pending row per job at a time; a running row isn't covered,
-- so a version change mid-run still enqueues a fresh pending row for after.
CREATE UNIQUE INDEX IF NOT EXISTS idx_trigger_queue_pending_job
    ON trigger_queue(job_name) WHERE status = 'pending';

-- Which resource versions each job has SUCCESSFULLY run against. It is what
-- passed: reads: "has this exact version been green in that job yet".
CREATE TABLE IF NOT EXISTS job_versions (
    job_name      TEXT NOT NULL,
    resource_name TEXT NOT NULL,
    version_json  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    PRIMARY KEY (job_name, resource_name, version_json)
);

-- One row per run invocation, with the steps it got through. It is what
-- --resume reads: not "has this content succeeded before" (that is the merkle
-- cache) but "did THIS run already do this step".
CREATE TABLE IF NOT EXISTS runs (
    id         TEXT PRIMARY KEY,
    job_name   TEXT NOT NULL,
    workspace  TEXT NOT NULL,
    status     TEXT NOT NULL,
    started_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS run_steps (
    run_id     TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    PRIMARY KEY (run_id, step_index)
);

-- The run-scoped key/value store an agent step writes with set_context. Keyed
-- by run so two runs of one job — including two concurrent ones under
-- steps watch — never read each other's facts.
--
-- Last write wins on a key, which is why written_by and written_at are kept:
-- the row says who overwrote what, so a run's record answers "where did this
-- value come from" without replaying the transcript.
CREATE TABLE IF NOT EXISTS run_context (
    run_id     TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    written_by TEXT NOT NULL,
    written_at TEXT NOT NULL,
    PRIMARY KEY (run_id, key)
);

-- Human decisions on approval: steps. The row IS the audit trail; it must not
-- depend on external chat history.
CREATE TABLE IF NOT EXISTS approvals (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    job_name     TEXT NOT NULL,
    message      TEXT NOT NULL,
    status       TEXT NOT NULL,
    requested_at TEXT NOT NULL,
    decided_at   TEXT,
    decided_by   TEXT,
    reason       TEXT
);

-- Which serial groups each job belongs to. Synced from config at startup; it
-- lives in the database so the claim can stay a single atomic statement
-- rather than a read-then-claim with a race in the middle.
CREATE TABLE IF NOT EXISTS job_serial_groups (
    job_name   TEXT NOT NULL,
    group_name TEXT NOT NULL,
    PRIMARY KEY (job_name, group_name)
);

-- The watch circuit breaker: how many times in a row a job has failed, and
-- whether that has taken it out of the rotation.
CREATE TABLE IF NOT EXISTS job_breaker (
    job_name    TEXT PRIMARY KEY,
    consecutive INTEGER NOT NULL,
    paused_at   TEXT
);

-- Everything a run did, in the order it did it: the persisted side of the
-- run-event bus (internal/events). It is what makes a finished run read back
-- exactly as it read live — a post-hoc view rebuilt from a different source
-- than the live one drifts, and the drift surfaces mid-incident.
--
-- Deliberately append-only and run-scoped, unlike content-addressed nodes:
-- two runs sharing a cached node still have their own separate stories about
-- reaching it.
CREATE TABLE IF NOT EXISTS run_events (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     TEXT NOT NULL,
    job_name   TEXT NOT NULL,
    type       TEXT NOT NULL,
    step_index INTEGER NOT NULL,
    step_name  TEXT NOT NULL,
    step_kind  TEXT NOT NULL,
    status     TEXT NOT NULL,
    hash       TEXT NOT NULL,
    text       TEXT NOT NULL,
    name       TEXT NOT NULL,
    detail     TEXT NOT NULL,
    duration_ms INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_run_events_run ON run_events(run_id, seq);

-- Full agent conversation transcripts, one row per agent node, kept OUT of
-- nodes.result deliberately: result is loaded by planners and routed-to
-- successors on every run and must stay bounded, while a transcript is read
-- on demand ("what did this step actually say and do"). Same hash key as
-- nodes; replaces on re-record like nodes does.
CREATE TABLE IF NOT EXISTS node_transcripts (
    hash       TEXT PRIMARY KEY,
    job_name   TEXT NOT NULL,
    transcript TEXT NOT NULL,
    created_at TEXT NOT NULL
);
`

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
	//     failing immediately with SQLITE_BUSY (relevant once this process
	//     runs concurrent steps against a shared pool).
	//   - journal_mode=WAL lets readers proceed concurrently with a writer,
	//     which the default rollback journal serializes.
	//
	// WAL is recorded in the database file header, so the conversion happens
	// exactly once and every later connection's pragma is a cheap no-op. No
	// retry loop guards the conversion: OpenStore is called once per process
	// at startup, before any concurrent access to this handle begins, so a
	// single `steps` process is never racing another *process* to convert
	// the same brand-new file — which is the only scenario in which
	// busy_timeout fails to cover the conversion's exclusive lock. `steps
	// watch --max-concurrent`'s worker pool does run concurrent goroutines
	// against this same handle later on, but that's safe independently of
	// this conversion concern: SetMaxOpenConns(1) below serializes every
	// query onto one connection, so there is still only ever one writer.
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("could not open state db %q: %w", path, err)
	}

	// SQLite only ever allows one writer at a time regardless of pool size,
	// so a pool of more than one just adds needless contention for no write
	// throughput benefit. (When this process starts running concurrent steps,
	// revisit: WAL permits a separate read pool alongside the single writer.)
	db.SetMaxOpenConns(1)

	ctx := context.Background()

	_, err = db.ExecContext(ctx, schema)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	err = addColumns(ctx, db)
	if err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("could not migrate state db %q: %w", path, err)
	}

	return &Store{db: db}, nil
}

// addedColumns are columns added to tables that already shipped. The schema
// above is all CREATE TABLE IF NOT EXISTS, which silently does nothing to a
// database created by an earlier version — so a new column on an OLD table
// needs its own ALTER, or the field exists only for people who deleted their
// state.db.
var addedColumns = []struct{ table, column, decl string }{
	{"runs", "finished_at", "TEXT"},
}

// addColumns applies addedColumns, treating "duplicate column name" as
// success. SQLite has no ADD COLUMN IF NOT EXISTS, and probing PRAGMA
// table_info first would be the same check with an extra round trip and a
// race between the check and the alter.
func addColumns(ctx context.Context, db *sql.DB) error {
	for _, add := range addedColumns {
		_, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", add.table, add.column, add.decl))
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("could not add %s.%s: %w", add.table, add.column, err)
		}
	}

	return nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	err := s.db.Close()
	if err != nil {
		return fmt.Errorf("could not close state db: %w", err)
	}

	return nil
}

// HasSucceeded reports whether jobName has a prior succeeded run recorded
// against rootHash.
func (s *Store) HasSucceeded(ctx context.Context, jobName, rootHash string) (bool, error) {
	var status string

	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM job_runs WHERE job_name = ? AND root_hash = ?`,
		jobName, rootHash,
	).Scan(&status)
	if err == sql.ErrNoRows {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not query job_runs: %w", err)
	}

	return status == "succeeded", nil
}

// hasSucceededBatchChunkSize bounds how many root hashes go into a single
// IN (...) query, well under sqlite's compiled-in bind-variable limit
// regardless of how many chains a version: every fanout produces.
const hasSucceededBatchChunkSize = 500

// HasSucceededBatch reports which of rootHashes have a prior succeeded run
// recorded for jobName, in one (or a few chunked) round trip instead of one
// query per hash.
func (s *Store) HasSucceededBatch(ctx context.Context, jobName string, rootHashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(rootHashes))

	for start := 0; start < len(rootHashes); start += hasSucceededBatchChunkSize {
		end := min(start+hasSucceededBatchChunkSize, len(rootHashes))

		err := s.queryHasSucceededChunk(ctx, jobName, rootHashes[start:end], result)
		if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// queryHasSucceededChunk runs one IN (...) query for a chunk of root hashes,
// setting result[hash] = true for each one found with a succeeded status.
func (s *Store) queryHasSucceededChunk(ctx context.Context, jobName string, chunk []string, result map[string]bool) error {
	if len(chunk) == 0 {
		return nil
	}

	placeholders := strings.Repeat("?,", len(chunk))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(chunk)+1)
	args = append(args, jobName)

	for _, hash := range chunk {
		args = append(args, hash)
	}

	//nolint:gosec // G201: placeholders is a repeated, fixed "?," string (one per chunk element), never interpolated data — every actual value is still a bound arg below
	query := fmt.Sprintf(
		`SELECT root_hash FROM job_runs WHERE job_name = ? AND status = 'succeeded' AND root_hash IN (%s)`,
		placeholders,
	)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("could not query job_runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var hash string

		err := rows.Scan(&hash)
		if err != nil {
			return fmt.Errorf("could not scan job_runs row: %w", err)
		}

		result[hash] = true
	}

	err = rows.Err()
	if err != nil {
		return fmt.Errorf("could not read job_runs rows: %w", err)
	}

	return nil
}

// NodeRecord is the subset of a merkle plan node's fields this package
// persists. It's a plain data shape rather than an import of merkle.Node so
// this leaf package doesn't need to depend on the planner — callers convert
// their own Node type into one of these.
type NodeRecord struct {
	Hash       string
	ParentHash string
	Kind       string
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only
	Content    map[string]any
}

// RecordNode upserts a node's execution outcome, keyed by its content hash.
func (s *Store) RecordNode(ctx context.Context, node NodeRecord, jobName, status string, result map[string]any, execErr error) error {
	content, err := json.Marshal(node.Content)
	if err != nil {
		return fmt.Errorf("could not marshal node content: %w", err)
	}

	var resultJSON []byte

	if result != nil {
		resultJSON, err = json.Marshal(result)
		if err != nil {
			return fmt.Errorf("could not marshal node result: %w", err)
		}
	}

	var errMsg string

	if execErr != nil {
		errMsg = execErr.Error()
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO nodes (hash, parent_hash, kind, job_name, resource, step_index, content, result, status, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			parent_hash = excluded.parent_hash,
			kind        = excluded.kind,
			job_name    = excluded.job_name,
			resource    = excluded.resource,
			step_index  = excluded.step_index,
			content     = excluded.content,
			result      = excluded.result,
			status      = excluded.status,
			error       = excluded.error,
			created_at  = excluded.created_at
	`,
		node.Hash, node.ParentHash, node.Kind, jobName, node.Resource, node.StepIndex,
		string(content), nullableString(resultJSON), status, nullableString([]byte(errMsg)), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return nil
}

// RecordJobRun upserts the outcome of a job run's chain, keyed by
// (jobName, rootHash).
func (s *Store) RecordJobRun(ctx context.Context, jobName, rootHash, status string, runErr error) error {
	var errMsg string

	if runErr != nil {
		errMsg = runErr.Error()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_runs (job_name, root_hash, status, error, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_name, root_hash) DO UPDATE SET
			status     = excluded.status,
			error      = excluded.error,
			created_at = excluded.created_at
	`,
		jobName, rootHash, status, nullableString([]byte(errMsg)), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("could not record job run (job %q, root %q): %w", jobName, rootHash, err)
	}

	return nil
}

// LastCheckedVersion returns the JSON of the most recently recorded version
// for resourceName, or found=false if it's never been checked.
func (s *Store) LastCheckedVersion(ctx context.Context, resourceName string) (string, bool, error) {
	var versionJSON string

	err := s.db.QueryRowContext(ctx,
		`SELECT version_json FROM resource_checks WHERE resource_name = ?`,
		resourceName,
	).Scan(&versionJSON)
	if err == sql.ErrNoRows {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not query resource_checks: %w", err)
	}

	return versionJSON, true, nil
}

// RecordCheckedVersion upserts the latest observed version JSON for
// resourceName, independent of whether any job triggered by the change
// succeeds — version-checking and build outcomes are tracked separately,
// mirroring how job_runs only ever records succeeded chains.
func (s *Store) RecordCheckedVersion(ctx context.Context, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO resource_checks (resource_name, version_json, checked_at)
		VALUES (?, ?, ?)
		ON CONFLICT(resource_name) DO UPDATE SET
			version_json = excluded.version_json,
			checked_at   = excluded.checked_at
	`,
		resourceName, versionJSON, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("could not record checked version for %q: %w", resourceName, err)
	}

	return nil
}

// EnqueueJob inserts a pending trigger_queue row for jobName, unless one
// already exists — idx_trigger_queue_pending_job makes that a no-op dedup
// rather than an error, so a resource going dirty twice before a worker
// claims the row doesn't queue jobName twice.
func (s *Store) EnqueueJob(ctx context.Context, jobName, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trigger_queue (job_name, reason, status, enqueued_at)
		VALUES (?, ?, 'pending', ?)
		ON CONFLICT (job_name) WHERE status = 'pending' DO NOTHING
	`,
		jobName, reason, time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

// ClaimNextJob atomically transitions the oldest claimable pending row to
// running and returns its id/jobName; found=false when nothing is claimable.
//
// Two rows are not claimable. One whose job already has a running row — this
// serializes builds of the same job (a version change enqueued mid-run runs
// only after the in-flight build finishes, never concurrently with it), even
// under a worker pool. And one whose job shares a serial_groups: entry with a
// job that is currently running, which is what stops two different jobs
// mutating the same deploy target at once.
//
// Both conditions are inside the single UPDATE...RETURNING rather than checked
// beforehand, so two workers can never claim conflicting rows — a
// read-then-claim would have a race exactly where the lock is supposed to be.
func (s *Store) ClaimNextJob(ctx context.Context) (int64, string, bool, error) {
	var (
		id      int64
		jobName string
	)

	err := s.db.QueryRowContext(ctx, `
		UPDATE trigger_queue
		SET status = 'running', started_at = ?
		WHERE id = (
			SELECT id FROM trigger_queue AS tq
			WHERE tq.status = 'pending'
			  AND NOT EXISTS (
			      SELECT 1 FROM trigger_queue AS r
			      WHERE r.job_name = tq.job_name AND r.status = 'running'
			  )
			  AND NOT EXISTS (
			      SELECT 1
			      FROM job_serial_groups AS mine
			      JOIN job_serial_groups AS theirs ON theirs.group_name = mine.group_name
			      JOIN trigger_queue AS busy
			        ON busy.job_name = theirs.job_name AND busy.status = 'running'
			      WHERE mine.job_name = tq.job_name
			  )
			ORDER BY tq.id LIMIT 1
		)
		RETURNING id, job_name
	`,
		time.Now().UTC().Format(time.RFC3339),
	).Scan(&id, &jobName)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}

	if err != nil {
		return 0, "", false, fmt.Errorf("could not claim next job: %w", err)
	}

	return id, jobName, true, nil
}

// CompleteJob marks a claimed row done or failed. Callers must not call this
// for a run that stopped because ctx was canceled (SIGINT/SIGTERM) — that
// row should stay running so ResetStaleRunning re-queues it on the next
// watch startup, since a graceful shutdown isn't a real failure and nothing
// else will re-trigger the job.
func (s *Store) CompleteJob(ctx context.Context, id int64, status string, runErr error) error {
	var errMsg string

	if runErr != nil {
		errMsg = runErr.Error()
	}

	_, err := s.db.ExecContext(ctx, `
		UPDATE trigger_queue
		SET status = ?, finished_at = ?, error = ?
		WHERE id = ?
	`,
		status, time.Now().UTC().Format(time.RFC3339), nullableString([]byte(errMsg)), id,
	)
	if err != nil {
		return fmt.Errorf("could not complete job (id %d): %w", id, err)
	}

	return nil
}

// ResetStaleRunning flips every running row back to pending — called once
// at Watch startup so a killed (or gracefully but incompletely shut down)
// watch process doesn't strand claimed work forever.
//
// A running row may coexist with a pending row for the same job (a version
// change enqueued while that job was mid-run). Flipping the running row to
// pending would then create two pending rows for one job, violating
// idx_trigger_queue_pending_job. So any running row whose job already has a
// pending successor is deleted instead of flipped — that later pending row
// already covers re-running the job, and the interrupted build's own outputs
// were never captured, so there is nothing to preserve. Called once at
// startup before any worker/poller goroutine exists, so the two statements
// run without concurrent writers.
func (s *Store) ResetStaleRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE status = 'running'
		  AND EXISTS (
		      SELECT 1 FROM trigger_queue AS p
		      WHERE p.job_name = trigger_queue.job_name AND p.status = 'pending'
		  )
	`)
	if err != nil {
		return fmt.Errorf("could not clear superseded running jobs: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE trigger_queue SET status = 'pending', started_at = NULL WHERE status = 'running'
	`)
	if err != nil {
		return fmt.Errorf("could not reset stale running jobs: %w", err)
	}

	return nil
}

func nullableString(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return string(b)
}

// RecordJobOutcome updates a job's consecutive-failure count and reports
// whether the job is now paused.
//
// It counts triggered RUNS, not the retries inside one — conflating the two
// would trip the breaker on ordinary flakiness that a retry would have
// absorbed, which is the opposite of the intent. maxFailures of 0 means the
// job has no breaker: the count is still kept (so turning one on later starts
// from a real number rather than zero), but it never pauses.
func (s *Store) RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (paused bool, consecutive int, err error) {
	if succeeded {
		return false, 0, s.ResetJobFailures(ctx, jobName)
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO job_breaker (job_name, consecutive) VALUES (?, 1)
		ON CONFLICT (job_name) DO UPDATE SET consecutive = job_breaker.consecutive + 1
		RETURNING consecutive
	`, jobName).Scan(&consecutive)
	if err != nil {
		return false, 0, fmt.Errorf("could not record failure for job %q: %w", jobName, err)
	}

	if maxFailures <= 0 || consecutive < maxFailures {
		return false, consecutive, nil
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE job_breaker SET paused_at = ? WHERE job_name = ? AND paused_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), jobName)
	if err != nil {
		return false, consecutive, fmt.Errorf("could not pause job %q: %w", jobName, err)
	}

	return true, consecutive, nil
}

// ResetJobFailures clears a job's consecutive-failure count and un-pauses it.
// Any success resets the breaker — including a manual `steps run`, which is
// the natural way to confirm a fix.
func (s *Store) ResetJobFailures(ctx context.Context, jobName string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_breaker (job_name, consecutive, paused_at) VALUES (?, 0, NULL)
		 ON CONFLICT (job_name) DO UPDATE SET consecutive = 0, paused_at = NULL`, jobName)
	if err != nil {
		return fmt.Errorf("could not reset the failure count for job %q: %w", jobName, err)
	}

	return nil
}

// IsJobPaused reports whether the breaker has taken a job out of the rotation.
func (s *Store) IsJobPaused(ctx context.Context, jobName string) (bool, error) {
	var pausedAt sql.NullString

	err := s.db.QueryRowContext(ctx,
		`SELECT paused_at FROM job_breaker WHERE job_name = ?`, jobName).Scan(&pausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not read the breaker for job %q: %w", jobName, err)
	}

	return pausedAt.Valid, nil
}

// PausedJob is one job the breaker has stopped, with the count that stopped it.
type PausedJob struct {
	Name        string
	Consecutive int
	PausedAt    string
}

// PausedJobs lists every job currently out of the rotation, oldest pause
// first. It backs `steps jobs`, which is how an operator finds out a nightly
// job stopped without reading a weekend of logs.
func (s *Store) PausedJobs(ctx context.Context) ([]PausedJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT job_name, consecutive, paused_at FROM job_breaker
		 WHERE paused_at IS NOT NULL ORDER BY paused_at`)
	if err != nil {
		return nil, fmt.Errorf("could not list paused jobs: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var paused []PausedJob

	for rows.Next() {
		var job PausedJob

		scanErr := rows.Scan(&job.Name, &job.Consecutive, &job.PausedAt)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read a paused job: %w", scanErr)
		}

		paused = append(paused, job)
	}

	if rows.Err() != nil {
		return nil, fmt.Errorf("could not list paused jobs: %w", rows.Err())
	}

	return paused, nil
}

// HasNodeSucceeded reports whether a node with this exact hash has already
// been recorded as succeeded for this job.
//
// It is per-NODE memoization, distinct from HasSucceeded's per-CHAIN check.
// The chain form asks "did this whole path succeed", which is right for a
// sequence: a changed step invalidates everything after it. An across: cell
// has no such sequence — cells are siblings, and one cell changing says
// nothing about another — so a cell asks about itself alone.
func (s *Store) HasNodeSucceeded(ctx context.Context, jobName, hash string) (bool, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM nodes WHERE hash = ? AND job_name = ? AND status = 'succeeded'`,
		hash, jobName).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("could not read node %q: %w", hash, err)
	}

	return count > 0, nil
}

// SaveNodeTranscript stores (or replaces) an agent node's full conversation
// transcript, a JSON array of events. Kept in its own table so nodes.result —
// which planners and routed-to successors load on every run — stays bounded;
// a transcript is read on demand instead.
func (s *Store) SaveNodeTranscript(ctx context.Context, hash, jobName, transcript string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO node_transcripts (hash, job_name, transcript, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (hash) DO UPDATE SET
			job_name = excluded.job_name,
			transcript = excluded.transcript,
			created_at = excluded.created_at
	`, hash, jobName, transcript, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("could not save transcript for node %q: %w", hash, err)
	}

	return nil
}

// NodeTranscript returns the stored transcript JSON for a node hash, with ok
// reporting whether one exists — mirroring LastCheckedVersion's shape rather
// than inventing a sentinel error for the common "never recorded" case.
func (s *Store) NodeTranscript(ctx context.Context, hash string) (string, bool, error) {
	var transcript string

	err := s.db.QueryRowContext(ctx,
		`SELECT transcript FROM node_transcripts WHERE hash = ?`, hash).Scan(&transcript)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("could not read transcript for node %q: %w", hash, err)
	}

	return transcript, true, nil
}

// RecordPassedVersion records that jobName completed successfully against this
// exact version of a resource. It is what a downstream job's passed: reads.
func (s *Store) RecordPassedVersion(ctx context.Context, jobName, resourceName, versionJSON string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_versions (job_name, resource_name, version_json, recorded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (job_name, resource_name, version_json) DO NOTHING
	`, jobName, resourceName, versionJSON, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("could not record a passed version for job %q: %w", jobName, err)
	}

	return nil
}

// HasPassedVersion reports whether jobName has already succeeded against this
// exact version of a resource.
func (s *Store) HasPassedVersion(ctx context.Context, jobName, resourceName, versionJSON string) (bool, error) {
	var count int

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_versions WHERE job_name = ? AND resource_name = ? AND version_json = ?`,
		jobName, resourceName, versionJSON).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("could not read passed versions for job %q: %w", jobName, err)
	}

	return count > 0, nil
}

// SyncSerialGroups replaces the recorded job/serial-group membership with
// what the pipeline currently declares.
//
// Replaced wholesale rather than merged: a group removed from the YAML must
// stop holding a lock, and a stale row would keep two jobs apart forever with
// nothing in the pipeline to explain why.
func (s *Store) SyncSerialGroups(ctx context.Context, groups map[string][]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not sync serial groups: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `DELETE FROM job_serial_groups`)
	if err != nil {
		return fmt.Errorf("could not clear serial groups: %w", err)
	}

	for jobName, names := range groups {
		for _, group := range names {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO job_serial_groups (job_name, group_name) VALUES (?, ?)
				 ON CONFLICT (job_name, group_name) DO NOTHING`, jobName, group)
			if err != nil {
				return fmt.Errorf("could not record serial group %q for job %q: %w", group, jobName, err)
			}
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not sync serial groups: %w", err)
	}

	return nil
}

// SerialGroupHolder names a running job that shares a serial group with
// jobName, or "" when nothing is holding the lock. It exists so a waiting job
// can say WHO it is waiting for — "queued" and "blocked on a lock" are
// different states, and a reader who cannot tell them apart cannot tell a
// stuck pipeline from a busy one.
func (s *Store) SerialGroupHolder(ctx context.Context, jobName string) (string, error) {
	var holder string

	err := s.db.QueryRowContext(ctx, `
		SELECT busy.job_name
		FROM job_serial_groups AS mine
		JOIN job_serial_groups AS theirs ON theirs.group_name = mine.group_name
		JOIN trigger_queue AS busy ON busy.job_name = theirs.job_name AND busy.status = 'running'
		WHERE mine.job_name = ?
		LIMIT 1
	`, jobName).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("could not read the serial-group holder for job %q: %w", jobName, err)
	}

	return holder, nil
}

// Approval is one recorded request for a human decision, and what became of
// it. The row IS the audit trail: who approved a deploy, when, and why a
// rejection was a rejection, are exactly the facts someone needs to
// reconstruct later — and they must not depend on external chat history.
type Approval struct {
	ID          int64
	JobName     string
	Message     string
	Status      string // pending, approved, rejected, expired
	RequestedAt string
	DecidedAt   string
	DecidedBy   string
	Reason      string
}

// RequestApproval records a pending approval and returns its id.
func (s *Store) RequestApproval(ctx context.Context, jobName, message string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (job_name, message, status, requested_at)
		VALUES (?, ?, 'pending', ?)
	`, jobName, message, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	return id, nil
}

// DecideApproval records a decision, refusing to overwrite one already made.
func (s *Store) DecideApproval(ctx context.Context, id int64, status, by, reason string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = ?, decided_at = ?, decided_by = ?, reason = ?
		WHERE id = ? AND status = 'pending'
	`, status, time.Now().UTC().Format(time.RFC3339), by, reason, id)
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("approval %d is not pending (already decided, expired, or never existed)", id)
	}

	return nil
}

// ApprovalStatus reads one approval's current state.
func (s *Store) ApprovalStatus(ctx context.Context, id int64) (Approval, error) {
	var (
		approval                     Approval
		decidedAt, decidedBy, reason sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_name, message, status, requested_at, decided_at, decided_by, reason
		FROM approvals WHERE id = ?
	`, id).Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
		&approval.RequestedAt, &decidedAt, &decidedBy, &reason)
	if err != nil {
		return Approval{}, fmt.Errorf("could not read approval %d: %w", id, err)
	}

	approval.DecidedAt, approval.DecidedBy, approval.Reason = decidedAt.String, decidedBy.String, reason.String

	return approval, nil
}

// PendingApprovals lists every approval still waiting, oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]Approval, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, message, requested_at FROM approvals
		WHERE status = 'pending' ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("could not list pending approvals: %w", err)
	}

	defer func() { _ = rows.Close() }()

	var pending []Approval

	for rows.Next() {
		approval := Approval{Status: "pending"}

		scanErr := rows.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.RequestedAt)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read a pending approval: %w", scanErr)
		}

		pending = append(pending, approval)
	}

	if rows.Err() != nil {
		return nil, fmt.Errorf("could not list pending approvals: %w", rows.Err())
	}

	return pending, nil
}

// Run is one `steps run` invocation that can be resumed.
type Run struct {
	ID        string
	JobName   string
	Workspace string
	Status    string
	StartedAt string
}

// StartRun records a run and the workspace its steps will share.
//
// The timestamps on this table are RFC3339Nano, not RFC3339 like the rest of
// the schema: a run's duration is derived from them, and at whole-second
// resolution every run faster than a second reports a duration of zero —
// which is most task-only runs. RFC3339 parses the fractional form, so
// reading stays uniform.
func (s *Store) StartRun(ctx context.Context, id, jobName, workspaceDir string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, job_name, workspace, status, started_at) VALUES (?, ?, ?, 'running', ?)
		ON CONFLICT (id) DO UPDATE SET status = 'running', workspace = excluded.workspace
	`, id, jobName, workspaceDir, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("could not record run %q: %w", id, err)
	}

	return nil
}

// FinishRun records how a run ended, and when. The timestamp is what makes a
// run's duration answerable at all: started_at alone leaves every finished
// run looking like it is still going.
func (s *Store) FinishRun(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
		status, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("could not finish run %q: %w", id, err)
	}

	return nil
}

// RecordRunStep marks one step of a run as done, so a resume skips it.
func (s *Store) RecordRunStep(ctx context.Context, runID string, index int, name string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_steps (run_id, step_index, step_name) VALUES (?, ?, ?)
		ON CONFLICT (run_id, step_index) DO NOTHING
	`, runID, index, name)
	if err != nil {
		return fmt.Errorf("could not record step %d of run %q: %w", index, runID, err)
	}

	return nil
}

// FindRun reads a run by id.
func (s *Store) FindRun(ctx context.Context, id string) (Run, error) {
	var run Run

	err := s.db.QueryRowContext(ctx,
		`SELECT id, job_name, workspace, status, started_at FROM runs WHERE id = ?`, id).
		Scan(&run.ID, &run.JobName, &run.Workspace, &run.Status, &run.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("no run %q was recorded", id)
	}

	if err != nil {
		return Run{}, fmt.Errorf("could not read run %q: %w", id, err)
	}

	return run, nil
}

// CompletedRunSteps returns the step indexes a run already finished.
func (s *Store) CompletedRunSteps(ctx context.Context, runID string) (map[int]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT step_index, step_name FROM run_steps WHERE run_id = ?`, runID)
	if err != nil {
		return nil, fmt.Errorf("could not read the steps of run %q: %w", runID, err)
	}

	defer func() { _ = rows.Close() }()

	done := map[int]string{}

	for rows.Next() {
		var (
			index int
			name  string
		)

		scanErr := rows.Scan(&index, &name)
		if scanErr != nil {
			return nil, fmt.Errorf("could not read a step of run %q: %w", runID, scanErr)
		}

		done[index] = name
	}

	if rows.Err() != nil {
		return nil, fmt.Errorf("could not read the steps of run %q: %w", runID, rows.Err())
	}

	return done, nil
}
