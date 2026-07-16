// Package store is the sqlite-backed persistence layer for job-run/node
// state.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// A pending row whose job already has a running row is *not* claimable — this
// serializes builds of the same job (a version change enqueued mid-run runs
// only after the in-flight build finishes, never concurrently with it), even
// under a worker pool. The UPDATE...RETURNING is a single statement, so two
// workers can never claim the same row — combined with this Store's single
// (SetMaxOpenConns(1)) connection, no additional locking is needed.
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
