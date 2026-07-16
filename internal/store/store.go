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
	// at startup, before any concurrent steps run, so a single `steps` run is
	// never racing another to convert the same brand-new file — which is the
	// only scenario in which busy_timeout fails to cover the conversion's
	// exclusive lock.
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

func nullableString(b []byte) any {
	if len(b) == 0 {
		return nil
	}

	return string(b)
}
