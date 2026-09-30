package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// Pause stamps paused_at; a second Pause keeps the first stamp.
func (s *Store) Pause(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET paused_at = COALESCE(paused_at, $1) WHERE id = $2`, now(), s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not pause pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Unpause clears the stamp.
func (s *Store) Unpause(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pipelines SET paused_at = NULL WHERE id = $1`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not unpause pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Paused reports whether the pipeline-level breaker is set.
func (s *Store) Paused(ctx context.Context) (bool, error) {
	var paused bool

	err := s.db.QueryRowContext(ctx, `SELECT paused_at IS NOT NULL FROM pipelines WHERE id = $1`, s.pipelineID).Scan(&paused)
	if err != nil {
		return false, fmt.Errorf("could not read whether pipeline %q is paused: %w", s.pipeline, err)
	}

	return paused, nil
}

// Rename is one UPDATE: every scoped row reaches the pipeline by id.
func (s *Store) Rename(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("could not rename pipeline %q: the new name is empty", s.pipeline)
	}

	_, err := s.db.ExecContext(ctx, `UPDATE pipelines SET name = $1 WHERE id = $2`, name, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not rename pipeline %q to %q: %w", s.pipeline, name, err)
	}

	return nil
}

// Delete clears the RESTRICTed current revision first, deletes the pipeline
// and everything cascading off it, and sweeps the shared content it left
// unreferenced.
func (s *Store) Delete(ctx context.Context) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE pipelines SET current_revision_id = NULL WHERE id = $1`, s.pipelineID)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the pipeline
		}

		_, err = tx.ExecContext(ctx, `DELETE FROM pipelines WHERE id = $1`, s.pipelineID)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the pipeline
		}

		return pruneNodeContent(ctx, tx)
	})
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	return nil
}
