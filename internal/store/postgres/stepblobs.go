package postgres

// The artifact-store index: action key -> output digests.

import (
	"context"
	"database/sql"
	"fmt"
)

// stepBlobEntryCap bounds how many step entries this index keeps per
// pipeline, newest inserted first; see the sqlite driver's for why this cap.
const stepBlobEntryCap = 200

// RecordStepBlobs files the content digests of a step's outputs under its
// action key, replacing whatever the key held.
func (s *Store) RecordStepBlobs(ctx context.Context, actionKey string, outputs map[string]string) error {
	if len(outputs) == 0 {
		return nil
	}

	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM step_blobs WHERE pipeline_id = $1 AND action_key = $2`, s.pipelineID, actionKey)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below
		}

		for output, digest := range outputs {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO step_blobs (pipeline_id, action_key, output, digest) VALUES ($1, $2, $3, $4)`,
				s.pipelineID, actionKey, output, digest)
			if err != nil {
				return err //nolint:wrapcheck // wrapped below
			}
		}

		// Whole entries, ordered by each entry's newest row, so eviction
		// takes entries rather than splitting them.
		_, err = tx.ExecContext(ctx, `
			DELETE FROM step_blobs
			WHERE pipeline_id = $1
			  AND action_key NOT IN (
			      SELECT action_key FROM step_blobs WHERE pipeline_id = $1
			      GROUP BY action_key
			      ORDER BY MAX(seq) DESC
			      LIMIT $2
			  )
		`, s.pipelineID, stepBlobEntryCap)

		return err //nolint:wrapcheck // wrapped below
	})
	if err != nil {
		return fmt.Errorf("could not record step blobs: %w", err)
	}

	return nil
}

// StepBlobs returns the digests recorded under an action key, keyed by
// declared output name.
func (s *Store) StepBlobs(ctx context.Context, actionKey string) (map[string]string, error) {
	blobs, err := collect(ctx, s.db, "step blobs",
		`SELECT output, digest FROM step_blobs WHERE pipeline_id = $1 AND action_key = $2`,
		[]any{s.pipelineID, actionKey}, scanPair)
	if err != nil {
		return nil, err
	}

	outputs := make(map[string]string, len(blobs))
	for _, blob := range blobs {
		outputs[blob[0]] = blob[1]
	}

	return outputs, nil
}
