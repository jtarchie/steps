package store

// job_runs: the chain-level cache index. A row means "this job has already
// run this exact content", which is what lets a rerun skip work.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// JobRunRow is one recorded job run.
type JobRunRow struct {
	JobName   string
	RootHash  string
	Status    string
	Error     string
	CreatedAt time.Time
}

// RecordJobRun upserts the outcome of a job run's chain, keyed by
// (jobName, rootHash).
func (s *Store) RecordJobRun(ctx context.Context, jobName, rootHash, status string, runErr error) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_runs (job_name, root_hash, status, error, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_name, root_hash) DO UPDATE SET
			status     = excluded.status,
			error      = excluded.error,
			created_at = excluded.created_at
	`, jobName, rootHash, status, errText(runErr), now())
	if err != nil {
		return fmt.Errorf("could not record job run (job %q, root %q): %w", jobName, rootHash, err)
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

		chunk := rootHashes[start:end]

		args := make([]any, 0, len(chunk)+1)
		args = append(args, jobName)

		for _, hash := range chunk {
			args = append(args, hash)
		}

		found, err := collect(ctx, s.db, "job_runs",
			`SELECT root_hash FROM job_runs WHERE job_name = ? AND status = 'succeeded' AND root_hash IN (`+
				placeholders(len(chunk))+`)`,
			args, func(rows *sql.Rows) (string, error) {
				var hash string

				return hash, rows.Scan(&hash)
			})
		if err != nil {
			return nil, err
		}

		for _, hash := range found {
			result[hash] = true
		}
	}

	return result, nil
}

// ListJobRuns returns the most recent job runs, newest first. An empty
// jobName covers every job.
func (s *Store) ListJobRuns(ctx context.Context, jobName string, limit int) ([]JobRunRow, error) {
	return collect(ctx, s.db, "job_runs", `
		SELECT job_name, root_hash, status, error, created_at
		FROM job_runs
		WHERE (? = '' OR job_name = ?)
		ORDER BY created_at DESC, rowid DESC
		LIMIT ?
	`, []any{jobName, jobName, limit}, func(rows *sql.Rows) (JobRunRow, error) {
		var (
			row       JobRunRow
			errCol    sql.NullString
			createdAt string
		)

		err := rows.Scan(&row.JobName, &row.RootHash, &row.Status, &errCol, &createdAt)

		row.Error = errCol.String
		row.CreatedAt = parseTimestamp(createdAt)

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}
