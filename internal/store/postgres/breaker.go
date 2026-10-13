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
// whether the job is now held. maxFailures of 0 means no breaker.
func (s *Store) RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (held bool, consecutive int, err error) {
	if succeeded {
		return false, 0, s.ResetJobFailures(ctx, jobName)
	}

	// The count and the hold land in one statement, as in the sqlite
	// driver: a reader between them cannot see a job at its limit and not
	// yet held, and a failure past the limit is one round trip, not two.
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO job_breaker (pipeline_id, job_name, consecutive, paused_at)
		VALUES ($1, $2, 1, CASE WHEN $3::int = 1 THEN $4::timestamptz END)
		ON CONFLICT (pipeline_id, job_name) DO UPDATE SET
			consecutive = job_breaker.consecutive + 1,
			paused_at = CASE WHEN $3::int > 0 AND job_breaker.consecutive + 1 >= $3::int
				THEN COALESCE(job_breaker.paused_at, $4::timestamptz) ELSE job_breaker.paused_at END
		RETURNING consecutive
	`, s.pipelineID, jobName, maxFailures, now()).Scan(&consecutive)
	if err != nil {
		return false, 0, fmt.Errorf("could not record failure for job %q: %w", jobName, err)
	}

	return maxFailures > 0 && consecutive >= maxFailures, consecutive, nil
}

// ResetJobFailures clears a job's consecutive-failure count and releases it.
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

// IsJobHeld reports whether the breaker has taken a job out of the rotation.
func (s *Store) IsJobHeld(ctx context.Context, jobName string) (bool, error) {
	var heldAt sql.NullTime

	err := s.db.QueryRowContext(ctx,
		`SELECT paused_at FROM job_breaker WHERE pipeline_id = $1 AND job_name = $2`,
		s.pipelineID, jobName).Scan(&heldAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not read the breaker for job %q: %w", jobName, err)
	}

	return heldAt.Valid, nil
}

// HeldJobs lists every job currently out of the rotation, oldest hold
// first.
func (s *Store) HeldJobs(ctx context.Context) ([]store.HeldJob, error) {
	return collect(ctx, s.db, "paused jobs",
		`SELECT job_name, consecutive, paused_at FROM job_breaker
		 WHERE pipeline_id = $1 AND paused_at IS NOT NULL ORDER BY paused_at, seq`,
		[]any{s.pipelineID}, func(rows *sql.Rows) (store.HeldJob, error) {
			var (
				job    store.HeldJob
				heldAt sql.NullTime
			)

			err := rows.Scan(&job.Name, &job.Consecutive, &heldAt)
			// Whole seconds, the resolution the sqlite driver stores it at.
			job.HeldAt = utc(heldAt).Truncate(time.Second)

			return job, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}
