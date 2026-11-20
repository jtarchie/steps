package sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// RetireAbsent is one transaction, so a name is never half forgotten. Nearly all of the deleting is the foreign keys': every table keyed by a job's or a resource's name cascades off its jobs or resources row.
func (s *Store) RetireAbsent(ctx context.Context, jobs, resources []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not retire what the pipeline stopped declaring: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	declared := jsonList(jobs)

	for _, statement := range []struct{ query, list string }{
		{`DELETE FROM resources WHERE pipeline_id = ? AND name NOT IN (SELECT value FROM json_each(?))`, jsonList(resources)},
		// A job declared again before its last build ended is a job again.
		{`UPDATE jobs SET retired = 0 WHERE pipeline_id = ? AND retired = 1 AND name IN (SELECT value FROM json_each(?))`, declared},
		{`UPDATE jobs SET retired = 1 WHERE pipeline_id = ? AND retired = 0 AND name NOT IN (SELECT value FROM json_each(?))`, declared},
	} {
		_, err = tx.ExecContext(ctx, statement.query, s.pipelineID, statement.list)
		if err != nil {
			return fmt.Errorf("could not retire what the pipeline stopped declaring: %w", err)
		}
	}

	err = sweepRetiredJobs(ctx, tx, s.pipelineID)
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not retire what the pipeline stopped declaring: %w", err)
	}

	return nil
}

// sweepRetired is the retirement a running build put off, tried again where a build ends (Prune, CompleteJob): the common call finds no retired job and takes no write lock.
func (s *Store) sweepRetired(ctx context.Context) error {
	var pending bool

	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE pipeline_id = ? AND retired = 1)`, s.pipelineID).Scan(&pending)
	if err != nil {
		return fmt.Errorf("could not read retired jobs: %w", err)
	}

	if !pending {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not sweep retired jobs: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	err = sweepRetiredJobs(ctx, tx, s.pipelineID)
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not sweep retired jobs: %w", err)
	}

	return nil
}

// sweepRetiredJobs deletes every retired job nothing is running, and sweeps what its cascade orphaned and cannot reach: content only its nodes named, parent links to them, and the configurations only its runs ran.
//
// A job still building is left: its build writes events and usage under its run to the end, and a write after the delete would file the name again with nothing left to retire it.
func sweepRetiredJobs(ctx context.Context, tx *sql.Tx, pipelineID int64) error {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM jobs
		WHERE pipeline_id = ? AND retired = 1
		  AND NOT EXISTS (
		      SELECT 1 FROM runs r
		      WHERE r.pipeline_id = jobs.pipeline_id AND r.job_name = jobs.name AND r.status = 'running'
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM trigger_queue q
		      WHERE q.pipeline_id = jobs.pipeline_id AND q.job_name = jobs.name AND q.status = 'running'
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

	// The whole table, as Delete sweeps it: the cascade removed nodes this transaction never saw.
	err = pruneNodeContent(ctx, tx)
	if err != nil {
		return err
	}

	_, err = pruneRevisions(ctx, tx, pipelineID)

	return err
}
