package store

// The watch circuit breaker: how many times in a row a job has failed, and
// whether that has taken it out of the rotation.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PausedJob is one job the breaker has stopped, with the count that stopped it.
type PausedJob struct {
	Name        string
	Consecutive int
	PausedAt    string
}

// RecordJobOutcome updates a job's consecutive-failure count and reports
// whether the job is now paused.
//
// It counts triggered RUNS, not the retries inside one — conflating the two
// would trip the breaker on ordinary flakiness that a retry would have
// absorbed, which is the opposite of the intent. maxFailures of 0 means the
// job has no breaker: the count is still kept (so turning one on later starts
// from a real number rather than zero), but it never pauses.
func (s *Store) RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (paused bool, consecutive int, err error) {
	if succeeded {
		return false, 0, s.ResetJobFailures(ctx, jobName)
	}

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO job_breaker (job_name, consecutive) VALUES (?, 1)
		ON CONFLICT (job_name) DO UPDATE SET consecutive = job_breaker.consecutive + 1
		RETURNING consecutive
	`, jobName).Scan(&consecutive)
	if err != nil {
		return false, 0, fmt.Errorf("could not record failure for job %q: %w", jobName, err)
	}

	if maxFailures <= 0 || consecutive < maxFailures {
		return false, consecutive, nil
	}

	_, err = s.db.ExecContext(ctx,
		`UPDATE job_breaker SET paused_at = ? WHERE job_name = ? AND paused_at IS NULL`,
		now(), jobName)
	if err != nil {
		return false, consecutive, fmt.Errorf("could not pause job %q: %w", jobName, err)
	}

	return true, consecutive, nil
}

// ResetJobFailures clears a job's consecutive-failure count and un-pauses it.
// Any success resets the breaker — including a manual `steps run`, which is
// the natural way to confirm a fix.
func (s *Store) ResetJobFailures(ctx context.Context, jobName string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO job_breaker (job_name, consecutive, paused_at) VALUES (?, 0, NULL)
		 ON CONFLICT (job_name) DO UPDATE SET consecutive = 0, paused_at = NULL`, jobName)
	if err != nil {
		return fmt.Errorf("could not reset the failure count for job %q: %w", jobName, err)
	}

	return nil
}

// IsJobPaused reports whether the breaker has taken a job out of the rotation.
func (s *Store) IsJobPaused(ctx context.Context, jobName string) (bool, error) {
	var pausedAt sql.NullString

	err := s.db.QueryRowContext(ctx,
		`SELECT paused_at FROM job_breaker WHERE job_name = ?`, jobName).Scan(&pausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("could not read the breaker for job %q: %w", jobName, err)
	}

	return pausedAt.Valid, nil
}

// PausedJobs lists every job currently out of the rotation, oldest pause
// first. It backs `steps jobs`, which is how an operator finds out a nightly
// job stopped without reading a weekend of logs.
func (s *Store) PausedJobs(ctx context.Context) ([]PausedJob, error) {
	return collect(ctx, s.db, "paused jobs",
		`SELECT job_name, consecutive, paused_at FROM job_breaker
		 WHERE paused_at IS NOT NULL ORDER BY paused_at`,
		nil, func(rows *sql.Rows) (PausedJob, error) {
			var job PausedJob

			return job, rows.Scan(&job.Name, &job.Consecutive, &job.PausedAt)
		})
}
