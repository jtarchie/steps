package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// RetireAbsent holds the pipeline lock: it decides what to delete from a read of what is running. The foreign keys do nearly all of the deleting.
func (s *Store) RetireAbsent(ctx context.Context, jobs, resources []string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, statement := range []struct {
			query string
			names []string
		}{
			// COALESCE: a nil list is a NULL array, and a comparison with one deletes nothing.
			{`DELETE FROM resources WHERE pipeline_id = $1 AND name <> ALL(COALESCE($2::text[], '{}'))`, resources},
			// A job declared again before its last build ended is a job again.
			{`UPDATE jobs SET retired = false WHERE pipeline_id = $1 AND retired AND name = ANY(COALESCE($2::text[], '{}'))`, jobs},
			{`UPDATE jobs SET retired = true WHERE pipeline_id = $1 AND NOT retired AND name <> ALL(COALESCE($2::text[], '{}'))`, jobs},
		} {
			_, err := tx.ExecContext(ctx, statement.query, s.pipelineID, statement.names)
			if err != nil {
				return err //nolint:wrapcheck // wrapped below
			}
		}

		return sweepRetiredJobs(ctx, tx, s.pipelineID)
	})
	if err != nil {
		return fmt.Errorf("could not retire what the pipeline stopped declaring: %w", err)
	}

	return nil
}

// sweepRetired is the retirement a running build put off, tried again where a build ends (Prune, CompleteJob). It reads before it takes the pipeline lock, so the common call, which finds no retired job, costs one indexed read.
func (s *Store) sweepRetired(ctx context.Context) error {
	var pending bool

	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE pipeline_id = $1 AND retired)`, s.pipelineID).Scan(&pending)
	if err != nil {
		return fmt.Errorf("could not read retired jobs: %w", err)
	}

	if !pending {
		return nil
	}

	err = s.write(ctx, func(tx *sql.Tx) error { return sweepRetiredJobs(ctx, tx, s.pipelineID) })
	if err != nil {
		return fmt.Errorf("could not sweep retired jobs: %w", err)
	}

	return nil
}

// sweepRetiredJobs deletes every retired job nothing is running, and sweeps what its cascade orphaned and cannot reach, as sqlite's does and for its reasons.
func sweepRetiredJobs(ctx context.Context, tx *sql.Tx, pipelineID int64) error {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM jobs j
		WHERE j.pipeline_id = $1 AND j.retired
		  AND NOT EXISTS (
		      SELECT 1 FROM runs r
		      WHERE r.pipeline_id = j.pipeline_id AND r.job_name = j.name AND r.status = 'running'
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM trigger_queue q
		      WHERE q.pipeline_id = j.pipeline_id AND q.job_name = j.name AND q.status = 'running'
		  )
	`, pipelineID)
	if err != nil {
		return fmt.Errorf("could not delete retired jobs: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not delete retired jobs: %w", err)
	}

	if deleted == 0 {
		return nil
	}

	err = clearDanglingParents(ctx, tx, pipelineID)
	if err != nil {
		return err
	}

	err = pruneNodeContent(ctx, tx, nil)
	if err != nil {
		return err
	}

	return pruneRevisions(ctx, tx, pipelineID)
}
