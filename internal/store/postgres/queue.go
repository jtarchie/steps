package postgres

// trigger_queue and the admission rules around it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// EnqueueJob inserts a pending row for jobName unless one is already pending.
func (s *Store) EnqueueJob(ctx context.Context, jobName, reason string) error {
	return enqueueJob(ctx, s.db, s.pipelineID, jobName, reason)
}

func enqueueJob(ctx context.Context, db executor, pipelineID int64, jobName, reason string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO trigger_queue (pipeline_id, job_name, reason, status, enqueued_at)
		VALUES ($1, $2, $3, 'pending', $4)
		ON CONFLICT (pipeline_id, job_name) WHERE status = 'pending' AND rerun_of IS NULL DO NOTHING
	`, pipelineID, jobName, clean(reason), now())
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

// EnqueueManualJob upserts, so a pending automatic row becomes manual.
func (s *Store) EnqueueManualJob(ctx context.Context, jobName, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trigger_queue (pipeline_id, job_name, reason, manual, status, enqueued_at)
		VALUES ($1, $2, $3, TRUE, 'pending', $4)
		ON CONFLICT (pipeline_id, job_name) WHERE status = 'pending' AND rerun_of IS NULL
		DO UPDATE SET manual = TRUE, reason = excluded.reason
	`, s.pipelineID, jobName, clean(reason), now())
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

// EnqueueRerunJob queues a retry of a recorded run.
func (s *Store) EnqueueRerunJob(ctx context.Context, jobName, reason, runID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trigger_queue (pipeline_id, job_name, reason, manual, rerun_of, status, enqueued_at)
		VALUES ($1, $2, $3, TRUE, $4, 'pending', $5)
		ON CONFLICT (pipeline_id, rerun_of) WHERE status = 'pending' AND rerun_of IS NOT NULL DO NOTHING
	`, s.pipelineID, jobName, clean(reason), runID, now())
	if err != nil {
		return fmt.Errorf("could not queue a rerun of %q: %w", runID, err)
	}

	return nil
}

// QueuedTrigger reads what EnqueueManualJob and EnqueueRerunJob left on a row.
func (s *Store) QueuedTrigger(ctx context.Context, id int64) (store.QueuedTrigger, error) {
	var (
		trigger store.QueuedTrigger
		rerun   sql.NullString
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT manual, rerun_of FROM trigger_queue WHERE id = $1 AND pipeline_id = $2
	`, id, s.pipelineID).Scan(&trigger.Manual, &rerun)
	if err != nil {
		return store.QueuedTrigger{}, fmt.Errorf("could not read queue row %d: %w", id, err)
	}

	trigger.RerunOf = rerun.String

	return trigger, nil
}

// ClaimNextJob transitions the oldest claimable pending row to running; see
// the sqlite driver's ClaimNextJob for what makes a row claimable.
//
// SKIP LOCKED alone does not keep the caps. It stops two claimers taking the
// SAME row, but the caps are about OTHER rows: two claimers each counting a
// job's running builds see neither's claim until it commits, and both admit.
// That is a read-then-write, so it runs under the pipeline lock (see write),
// which serializes claims within a pipeline; SKIP LOCKED then only lets a
// claim pass over a row some other statement holds rather than wait on it.
func (s *Store) ClaimNextJob(ctx context.Context) (int64, string, bool, error) {
	var (
		id      int64
		jobName string
		found   bool
	)

	err := s.write(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			UPDATE trigger_queue
			SET status = 'running', started_at = $1
			WHERE id = (
				SELECT id FROM trigger_queue AS tq
				WHERE tq.pipeline_id = $2 AND tq.status = 'pending'
				  AND (
				      SELECT COUNT(*) FROM trigger_queue AS r
				      WHERE r.pipeline_id = tq.pipeline_id AND r.job_name = tq.job_name AND r.status = 'running'
				  ) < COALESCE(
				      (SELECT c.max_in_flight FROM job_concurrency AS c
				       WHERE c.pipeline_id = tq.pipeline_id AND c.job_name = tq.job_name),
				      1
				  )
				  AND NOT EXISTS (
				      SELECT 1
				      FROM job_serial_groups AS mine
				      JOIN job_serial_groups AS theirs
				        ON theirs.pipeline_id = mine.pipeline_id AND theirs.group_name = mine.group_name
				      JOIN trigger_queue AS busy
				        ON busy.pipeline_id = theirs.pipeline_id
				       AND busy.job_name = theirs.job_name AND busy.status = 'running'
				      WHERE mine.pipeline_id = tq.pipeline_id AND mine.job_name = tq.job_name
				  )
				ORDER BY tq.id LIMIT 1
				FOR UPDATE SKIP LOCKED
			)
			RETURNING id, job_name
		`, now(), s.pipelineID).Scan(&id, &jobName)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		found = err == nil

		return err //nolint:wrapcheck // wrapped below, as every other failure to claim
	})
	if err != nil {
		return 0, "", false, fmt.Errorf("could not claim next job: %w", err)
	}

	return id, jobName, found, nil
}

// CompleteJob marks a claimed row done or failed.
func (s *Store) CompleteJob(ctx context.Context, id int64, status string, runErr error) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE trigger_queue
		SET status = $1, finished_at = $2, error = $3
		WHERE id = $4 AND pipeline_id = $5
	`, status, now(), errText(runErr), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not complete job (id %d): %w", id, err)
	}

	return nil
}

// AbortQueuedJob keeps the row, so the queue still says somebody stopped it.
func (s *Store) AbortQueuedJob(ctx context.Context, jobName string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE trigger_queue SET status = 'aborted', finished_at = $1
		WHERE pipeline_id = $2 AND job_name = $3 AND status = 'pending'
	`, now(), s.pipelineID, jobName)
	if err != nil {
		return false, fmt.Errorf("could not abort the queued run of %q: %w", jobName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not abort the queued run of %q: %w", jobName, err)
	}

	return affected > 0, nil
}

// ResetStaleRunning flips this pipeline's running rows back to pending, at
// daemon startup. Like the sqlite driver's, it assumes it is ALONE: one
// `steps web` per state database, and nothing here arbitrates a second.
func (s *Store) ResetStaleRunning(ctx context.Context) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM trigger_queue
			WHERE pipeline_id = $1 AND status = 'running'
			  AND EXISTS (
			      SELECT 1 FROM trigger_queue AS p
			      WHERE p.pipeline_id = trigger_queue.pipeline_id
			        AND p.job_name = trigger_queue.job_name AND p.status = 'pending'
			        AND p.rerun_of IS NOT DISTINCT FROM trigger_queue.rerun_of
			  )
		`, s.pipelineID)
		if err != nil {
			return fmt.Errorf("could not clear superseded running jobs: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			DELETE FROM trigger_queue
			WHERE pipeline_id = $1 AND status = 'running' AND id NOT IN (
				SELECT MIN(id) FROM trigger_queue
				WHERE pipeline_id = $1 AND status = 'running' GROUP BY job_name, rerun_of
			)
		`, s.pipelineID)
		if err != nil {
			return fmt.Errorf("could not collapse stale running jobs: %w", err)
		}

		_, err = tx.ExecContext(ctx, `
			UPDATE trigger_queue SET status = 'pending', started_at = NULL
			WHERE pipeline_id = $1 AND status = 'running'
		`, s.pipelineID)
		if err != nil {
			return fmt.Errorf("could not reset stale running jobs: %w", err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not reset stale running jobs: %w", err)
	}

	return nil
}

// ListTriggerQueue returns the most recent trigger-queue entries, newest
// first.
func (s *Store) ListTriggerQueue(ctx context.Context, limit int) ([]store.QueueRow, error) {
	return collect(ctx, s.db, "trigger_queue", `
		SELECT id, job_name, reason, status, enqueued_at, started_at, finished_at, error
		FROM trigger_queue
		WHERE pipeline_id = $1
		ORDER BY id DESC
		LIMIT $2
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.QueueRow, error) {
		var (
			row               store.QueueRow
			enqueued          sql.NullTime
			started, finished sql.NullTime
			errCol            sql.NullString
		)

		err := rows.Scan(&row.ID, &row.JobName, &row.Reason, &row.Status,
			&enqueued, &started, &finished, &errCol)

		row.EnqueuedAt, row.StartedAt, row.FinishedAt = utc(enqueued), utc(started), utc(finished)
		row.Error = errCol.String

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// SyncJobLimits replaces the recorded admission rules with what the pipeline
// currently declares, in one transaction — and under the pipeline lock, so a
// claim never counts against half of a swap.
func (s *Store) SyncJobLimits(ctx context.Context, groups map[string][]string, limits map[string]int) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"job_serial_groups", "job_concurrency"} {
			//nolint:gosec // G202: table is a package-internal literal, never input
			_, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE pipeline_id = $1`, s.pipelineID)
			if err != nil {
				return fmt.Errorf("could not clear %s: %w", table, err)
			}
		}

		for jobName, names := range groups {
			for _, group := range names {
				_, err := tx.ExecContext(ctx,
					`INSERT INTO job_serial_groups (pipeline_id, job_name, group_name) VALUES ($1, $2, $3)
					 ON CONFLICT (pipeline_id, job_name, group_name) DO NOTHING`, s.pipelineID, jobName, group)
				if err != nil {
					return fmt.Errorf("could not record serial group %q for job %q: %w", group, jobName, err)
				}
			}
		}

		for jobName, limit := range limits {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO job_concurrency (pipeline_id, job_name, max_in_flight) VALUES ($1, $2, $3)`,
				s.pipelineID, jobName, limit)
			if err != nil {
				return fmt.Errorf("could not record concurrency for job %q: %w", jobName, err)
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not sync job limits: %w", err)
	}

	return nil
}

// SerialGroupHolder names a running job that shares a serial group with
// jobName, or "" when nothing is holding the lock.
func (s *Store) SerialGroupHolder(ctx context.Context, jobName string) (string, error) {
	var holder string

	err := s.db.QueryRowContext(ctx, `
		SELECT busy.job_name
		FROM job_serial_groups AS mine
		JOIN job_serial_groups AS theirs
		  ON theirs.pipeline_id = mine.pipeline_id AND theirs.group_name = mine.group_name
		JOIN trigger_queue AS busy
		  ON busy.pipeline_id = theirs.pipeline_id
		 AND busy.job_name = theirs.job_name AND busy.status = 'running'
		WHERE mine.pipeline_id = $1 AND mine.job_name = $2
		LIMIT 1
	`, s.pipelineID, jobName).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("could not read the serial-group holder for job %q: %w", jobName, err)
	}

	return holder, nil
}
