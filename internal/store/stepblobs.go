package store

// The artifact-store index: action key -> output digests. See the schema
// comment on step_blobs for why this lives in SQLite while the bytes live in
// S3.

import (
	"context"
	"database/sql"
	"fmt"
)

// stepBlobEntryCap bounds how many step entries this index keeps per
// pipeline, newest INSERTED first — which is not the local cache's LRU: a
// local hit refreshes the entry's mtime but records nothing here, so under
// churn past the cap the rows evicted first are the hottest keys. Known and
// accepted: re-recording on every hit would cost a digest walk per hit, the
// loss is cost-only (the other machine re-runs and re-records), and the cap
// exists for bound, not fidelity. It mirrors the on-disk step cache's own entry bound
// (workspace's defaultStepCacheMaxEntries, 200): an index entry whose local
// bytes could never still be cached buys little, since the action keys of
// evicted entries stop being computed long before they stop being stored.
// Count, never age — the standing rule for caches in this schema.
const stepBlobEntryCap = 200

// RecordStepBlobs files the content digests of a step's outputs under its
// action key, replacing whatever the key held. outputs maps a DECLARED output
// name to the digest of the artifact it produced.
func (s *Store) RecordStepBlobs(ctx context.Context, actionKey string, outputs map[string]string) error {
	if len(outputs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not record step blobs: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	err = replaceStepBlobs(ctx, tx, s.pipelineID, actionKey, outputs)
	if err != nil {
		return err
	}

	err = pruneStepBlobs(ctx, tx, s.pipelineID, stepBlobEntryCap)
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not record step blobs: %w", err)
	}

	return nil
}

func replaceStepBlobs(ctx context.Context, tx *sql.Tx, pipelineID int64, actionKey string, outputs map[string]string) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM step_blobs WHERE pipeline_id = ? AND action_key = ?`,
		pipelineID, actionKey)
	if err != nil {
		return fmt.Errorf("could not record step blobs: %w", err)
	}

	for output, digest := range outputs {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO step_blobs (pipeline_id, action_key, output, digest) VALUES (?, ?, ?, ?)`,
			pipelineID, actionKey, output, digest)
		if err != nil {
			return fmt.Errorf("could not record step blobs: %w", err)
		}
	}

	return nil
}

// pruneStepBlobs keeps the newest keep entries — whole action keys, ordered
// by each entry's newest row, so eviction takes entries rather than splitting
// them.
func pruneStepBlobs(ctx context.Context, tx *sql.Tx, pipelineID int64, keep int) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM step_blobs
		WHERE pipeline_id = ?
		  AND action_key NOT IN (
		      SELECT action_key FROM step_blobs WHERE pipeline_id = ?
		      GROUP BY action_key
		      ORDER BY MAX(rowid) DESC
		      LIMIT ?
		  )
	`, pipelineID, pipelineID, keep)
	if err != nil {
		return fmt.Errorf("could not prune step blobs: %w", err)
	}

	return nil
}

// StepBlobs returns the digests recorded under an action key, keyed by
// declared output name — empty when the key is unknown, which a caller reads
// as an ordinary miss.
func (s *Store) StepBlobs(ctx context.Context, actionKey string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT output, digest FROM step_blobs WHERE pipeline_id = ? AND action_key = ?`,
		s.pipelineID, actionKey)
	if err != nil {
		return nil, fmt.Errorf("could not read step blobs: %w", err)
	}

	defer func() { _ = rows.Close() }()

	outputs := map[string]string{}

	for rows.Next() {
		var output, digest string

		err = rows.Scan(&output, &digest)
		if err != nil {
			return nil, fmt.Errorf("could not read step blobs: %w", err)
		}

		outputs[output] = digest
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read step blobs: %w", err)
	}

	return outputs, nil
}
