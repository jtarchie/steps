package sqlite

// job_runs: the chain-level cache index. A row means "this job has already
// run this exact content", which is what lets a rerun skip work. Only green
// chains are rows; see store.Cache for why a failure deletes instead.

import (
	"context"
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

// HasSucceededBatch reports which of rootHashes have a prior succeeded run
// recorded for jobName, in one round trip instead of one query per hash.
func (s *Store) HasSucceededBatch(ctx context.Context, jobName string, rootHashes []string) (map[string]bool, error) {
	found, err := collect(ctx, s.db, "job_runs",
		`SELECT root_hash FROM job_runs
		 WHERE pipeline_id = ? AND job_name = ? AND root_hash IN (SELECT value FROM json_each(?))`,
		[]any{s.pipelineID, jobName, jsonList(rootHashes)}, scanString)
	if err != nil {
		return nil, err
	}

	result := make(map[string]bool, len(found))
	for _, hash := range found {
		result[hash] = true
	}

	return result, nil
}
