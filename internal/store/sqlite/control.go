package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// Pause stamps paused_at; a second Pause keeps the first stamp, since when it was paused is the fact a reader wants.
func (s *Store) Pause(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pipelines SET paused_at = COALESCE(paused_at, ?) WHERE id = ?`, nowNano(), s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not pause pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Unpause clears the stamp.
func (s *Store) Unpause(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE pipelines SET paused_at = NULL WHERE id = ?`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not unpause pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// Paused reports whether the pipeline-level breaker is set.
func (s *Store) Paused(ctx context.Context) (bool, error) {
	var pausedAt sql.NullString

	err := s.db.QueryRowContext(ctx, `SELECT paused_at FROM pipelines WHERE id = ?`, s.pipelineID).Scan(&pausedAt)
	if err != nil {
		return false, fmt.Errorf("could not read whether pipeline %q is paused: %w", s.pipeline, err)
	}

	return pausedAt.Valid, nil
}

// Rename is one UPDATE: every scoped row reaches the pipeline by id, so history, cache and queue follow the row rather than the name.
func (s *Store) Rename(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("could not rename pipeline %q: the new name is empty", s.pipeline)
	}

	_, err := s.db.ExecContext(ctx, `UPDATE pipelines SET name = ? WHERE id = ?`, name, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not rename pipeline %q to %q: %w", s.pipeline, name, err)
	}

	return nil
}

// Delete clears the current revision pointer first because it RESTRICTs, and the pipeline's own cascade would otherwise be refused by the row being deleted.
func (s *Store) Delete(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `UPDATE pipelines SET current_revision_id = NULL WHERE id = ?`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM pipelines WHERE id = ?`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	// node_content is shared across pipelines, so the cascade cannot reach it and the sweep has to.
	err = pruneNodeContent(ctx, tx)
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not delete pipeline %q: %w", s.pipeline, err)
	}

	return nil
}
