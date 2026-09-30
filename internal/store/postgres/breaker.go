package postgres

// The watch circuit breaker: how many times in a row a job has failed, and
// whether that has taken it out of the rotation.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// RecordJobOutcome updates a job's consecutive-failure count and reports
// whether the job is now paused. maxFailures of 0 means no breaker.
func (s *Store) RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (paused bool, consecutive int, err error) {
	if succeeded {
		return false, 0, s.ResetJobFailures(ctx, jobName)
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO job_breaker (pipeline_id, job_name, consecutive) VALUES ($1, $2, 1)
		ON CONFLICT (pipeline_id, job_name) DO UPDATE SET consecutive = job_breaker.consecutive + 1
		RETURNING consecutive
	`, s.pipelineID, jobName).Scan(&consecutive)
	if err != nil {
		return false, 0, fmt.Errorf("could not record failure for job %q: %w", jobName, err)
	}

	if maxFailures <= 0 || consecutive < maxFailures {
		return false, consecutive, nil
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE job_breaker SET paused_at = $1 WHERE pipeline_id = $2 AND job_name = $3 AND paused_at IS NULL`,
		now(), s.pipelineID, jobName)
	if err != nil {
		return false, consecutive, fmt.Errorf("could not pause job %q: %w", jobName, err)
	}

	return true, consecutive, nil
}

// ResetJobFailures clears a job's consecutive-failure count and un-pauses it.
func (s *Store) ResetJobFailures(ctx context.Context, jobName string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_breaker (pipeline_id, job_name, consecutive, paused_at) VALUES ($1, $2, 0, NULL)
		 ON CONFLICT (pipeline_id, job_name) DO UPDATE SET consecutive = 0, paused_at = NULL`,
		s.pipelineID, jobName)
	if err != nil {
		return fmt.Errorf("could not reset the failure count for job %q: %w", jobName, err)
	}

	return nil
}

// IsJobPaused reports whether the breaker has taken a job out of the rotation.
func (s *Store) IsJobPaused(ctx context.Context, jobName string) (bool, error) {
	var pausedAt sql.NullTime

	err := s.db.QueryRowContext(ctx,
		`SELECT paused_at FROM job_breaker WHERE pipeline_id = $1 AND job_name = $2`,
		s.pipelineID, jobName).Scan(&pausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not read the breaker for job %q: %w", jobName, err)
	}

	return pausedAt.Valid, nil
}

// PausedJobs lists every job currently out of the rotation, oldest pause
// first.
func (s *Store) PausedJobs(ctx context.Context) ([]store.PausedJob, error) {
	return collect(ctx, s.db, "paused jobs",
		`SELECT job_name, consecutive, paused_at FROM job_breaker
		 WHERE pipeline_id = $1 AND paused_at IS NOT NULL ORDER BY paused_at, seq`,
		[]any{s.pipelineID}, func(rows *sql.Rows) (store.PausedJob, error) {
			var (
				job      store.PausedJob
				pausedAt sql.NullTime
			)

			err := rows.Scan(&job.Name, &job.Consecutive, &pausedAt)
			job.PausedAt = stamp(pausedAt, time.RFC3339)

			return job, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}
