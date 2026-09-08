package sqlite

// job_runs: the chain-level cache index. A row means "this job has already
// run this exact content", which is what lets a rerun skip work. Only green
// chains are rows; see store.Cache for why a failure deletes instead.

import (
	"context"
	"database/sql"
	"fmt"
)

// RecordChainSucceeded adds (jobName, rootHash) to the skip index.
func (s *Store) RecordChainSucceeded(ctx context.Context, jobName, rootHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO job_runs (pipeline_id, job_name, root_hash)
		VALUES (?, ?, ?)
	`, s.pipelineID, jobName, rootHash)
	if err != nil {
		return fmt.Errorf("could not record job run (job %q, root %q): %w", jobName, rootHash, err)
	}

	return nil
}

// ForgetChain removes (jobName, rootHash) from the skip index; a chain never
// recorded is a no-op.
func (s *Store) ForgetChain(ctx context.Context, jobName, rootHash string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM job_runs WHERE pipeline_id = ? AND job_name = ? AND root_hash = ?
	`, s.pipelineID, jobName, rootHash)
	if err != nil {
		return fmt.Errorf("could not forget job run (job %q, root %q): %w", jobName, rootHash, err)
	}

	return nil
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

		args := make([]any, 0, len(chunk)+2)
		args = append(args, s.pipelineID, jobName)

		for _, hash := range chunk {
			args = append(args, hash)
		}

		found, err := collect(ctx, s.db, "job_runs",
			`SELECT root_hash FROM job_runs WHERE pipeline_id = ? AND job_name = ? AND root_hash IN (`+
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
