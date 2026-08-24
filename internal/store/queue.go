package store

// trigger_queue and the admission rules around it: what `steps watch` has
// queued, what it may claim, and the serial-group locks that hold it back.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// QueueRow is one entry in the downstream-trigger queue.
type QueueRow struct {
	ID         int64
	JobName    string
	Reason     string
	Status     string
	EnqueuedAt time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}

// EnqueueJob inserts a pending trigger_queue row for jobName, unless one
// already exists — idx_trigger_queue_pending_job makes that a no-op dedup
// rather than an error, so a resource going dirty twice before a worker
// claims the row doesn't queue jobName twice.
//
// The row carries a job NAME and a reason, and deliberately nothing about
// versions. It used to carry the versions the poll resolved, because a
// cursor-driven check could not be asked twice — which made a dropped
// enqueue data loss, and needed a merge, a restart fold and a
// hand-queued clear rule to be safe. A job now reads what it should build
// from resource_versions, so dropping a duplicate enqueue is once again
// what it looks like: the job is already queued.
func (s *Store) EnqueueJob(ctx context.Context, jobName, reason string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO trigger_queue (pipeline_id, job_name, reason, status, enqueued_at)
		VALUES (?, ?, ?, 'pending', ?)
		ON CONFLICT (pipeline_id, job_name) WHERE status = 'pending' DO NOTHING
	`, s.pipelineID, jobName, reason, now())
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

// ClaimNextJob atomically transitions the oldest claimable pending row to
// running and returns its id/jobName; found=false when nothing is claimable.
//
// Two rows are not claimable. One whose job already has max_in_flight builds
// running — see config.Job.EffectiveMaxInFlight, which has already folded
// serial:/serial_groups: down to 1 by the time the row is synced. And one
// whose job shares a serial_groups: entry with a DIFFERENT job that is
// currently running, which is what stops two jobs mutating the same deploy
// target at once.
//
// A job with no row defaults to 1, not to unlimited. A missing row means the
// job left the pipeline between enqueue and claim, and serializing something
// nobody can describe is the conservative reading.
//
// Both conditions are inside the single UPDATE...RETURNING rather than checked
// beforehand, so two workers can never claim conflicting rows — a
// read-then-claim would have a race exactly where the lock is supposed to be.
func (s *Store) ClaimNextJob(ctx context.Context) (int64, string, bool, error) {
	var (
		id      int64
		jobName string
	)

	err := s.db.QueryRowContext(ctx, `
		UPDATE trigger_queue
		SET status = 'running', started_at = ?
		WHERE id = (
			SELECT id FROM trigger_queue AS tq
			WHERE tq.pipeline_id = ? AND tq.status = 'pending'
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
		)
		RETURNING id, job_name
	`, now(), s.pipelineID).Scan(&id, &jobName)
	if err == sql.ErrNoRows {
		return 0, "", false, nil
	}

	if err != nil {
		return 0, "", false, fmt.Errorf("could not claim next job: %w", err)
	}

	return id, jobName, true, nil
}

// CompleteJob marks a claimed row done or failed. Callers must not call this
// for a run that stopped because ctx was canceled (SIGINT/SIGTERM) — that
// row should stay running so ResetStaleRunning re-queues it on the next
// watch startup, since a graceful shutdown isn't a real failure and nothing
// else will re-trigger the job.
func (s *Store) CompleteJob(ctx context.Context, id int64, status string, runErr error) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE trigger_queue
		SET status = ?, finished_at = ?, error = ?
		WHERE id = ? AND pipeline_id = ?
	`, status, now(), errText(runErr), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not complete job (id %d): %w", id, err)
	}

	return nil
}

// ResetStaleRunning flips this pipeline's running rows back to pending —
// called once at Watch startup so a killed (or gracefully but incompletely
// shut down) watch process doesn't strand claimed work forever.
//
// It assumes ONE steps process is polling a pipeline at a time, and that
// assumption is now the whole guard. A file lock used to enforce it, so that a
// second `steps watch` (or a `steps web` that polls) would give way rather than
// treat a live build's row as an abandoned leftover — flip it, let the job be
// claimed twice, and silently defeat serial:/max_in_flight. The lock is gone
// deliberately: a state file belongs to one process, and the ways to run two
// against it (a watch beside a polling web) are a deployment mistake rather
// than a case to support. `steps web --no-watch` is still how a UI defers the
// polling to a separate watcher.
//
// The rows are scoped to this pipeline because a state file may hold several,
// and one pipeline's watcher starting up must not reach into another's
// in-flight builds.
//
// A running row may coexist with a pending row for the same job (a version
// change enqueued while that job was mid-run). Flipping the running row to
// pending would then create two pending rows for one job, violating
// idx_trigger_queue_pending_job. So any running row whose job already has a
// pending successor is deleted instead of flipped — that later pending row
// already covers re-running the job, and the interrupted build's own outputs
// were never captured, so there is nothing to preserve. Called once at
// startup before any worker/poller goroutine exists, so the two statements
// run without concurrent writers.
func (s *Store) ResetStaleRunning(ctx context.Context) error {
	// A running row superseded by a pending one is simply dropped: both mean
	// "run this job", and the pending one is the later word. Nothing is lost
	// with it, because the row no longer carries the work — what the job
	// builds is read from resource_versions when it runs.
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE pipeline_id = ? AND status = 'running'
		  AND EXISTS (
		      SELECT 1 FROM trigger_queue AS p
		      WHERE p.pipeline_id = trigger_queue.pipeline_id
		        AND p.job_name = trigger_queue.job_name AND p.status = 'pending'
		  )
	`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not clear superseded running jobs: %w", err)
	}

	// Whatever is left goes back to pending so it is claimed again. Several
	// builds of one job may have been in flight (max_in_flight), and only one
	// pending row per job is allowed, so they collapse into one — which is
	// right: the job needs to run, not to run N times.
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE pipeline_id = ? AND status = 'running' AND rowid NOT IN (
			SELECT MIN(rowid) FROM trigger_queue
			WHERE pipeline_id = ? AND status = 'running' GROUP BY job_name
		)
	`, s.pipelineID, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not collapse stale running jobs: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE trigger_queue SET status = 'pending', started_at = NULL
		WHERE pipeline_id = ? AND status = 'running'
	`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not reset stale running jobs: %w", err)
	}

	return nil
}

// ListTriggerQueue returns the most recent trigger-queue entries, newest
// first — what `steps watch` has queued, run, or failed to run.
func (s *Store) ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error) {
	return collect(ctx, s.db, "trigger_queue", `
		SELECT id, job_name, reason, status, enqueued_at, started_at, finished_at, error
		FROM trigger_queue
		WHERE pipeline_id = ?
		ORDER BY id DESC
		LIMIT ?
	`, []any{s.pipelineID, limit}, func(rows *sql.Rows) (QueueRow, error) {
		var (
			row                       QueueRow
			started, finished, errCol sql.NullString
			enqueuedAt                string
		)

		err := rows.Scan(&row.ID, &row.JobName, &row.Reason, &row.Status,
			&enqueuedAt, &started, &finished, &errCol)

		row.EnqueuedAt = parseTimestamp(enqueuedAt)
		row.StartedAt = parseTimestamp(started.String)
		row.FinishedAt = parseTimestamp(finished.String)
		row.Error = errCol.String

		return row, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}

// SyncSerialGroups replaces the recorded job/serial-group membership with
// what the pipeline currently declares.
//
// Replaced wholesale rather than merged: a group removed from the YAML must
// stop holding a lock, and a stale row would keep two jobs apart forever with
// nothing in the pipeline to explain why.
func (s *Store) SyncSerialGroups(ctx context.Context, groups map[string][]string) error {
	return s.replaceAll(ctx, "serial groups", "job_serial_groups", func(exec execFunc) error {
		for jobName, names := range groups {
			for _, group := range names {
				err := exec(`INSERT INTO job_serial_groups (pipeline_id, job_name, group_name) VALUES (?, ?, ?)
					 ON CONFLICT (pipeline_id, job_name, group_name) DO NOTHING`, s.pipelineID, jobName, group)
				if err != nil {
					return fmt.Errorf("could not record serial group %q for job %q: %w", group, jobName, err)
				}
			}
		}

		return nil
	})
}

// SyncMaxInFlight replaces the recorded per-job concurrency with what the
// pipeline currently declares, for the reason SyncSerialGroups replaces rather
// than merges: a limit removed from the YAML must stop applying, and a stale
// row would throttle a job with nothing in the pipeline to explain why.
func (s *Store) SyncMaxInFlight(ctx context.Context, limits map[string]int) error {
	return s.replaceAll(ctx, "job concurrency", "job_concurrency", func(exec execFunc) error {
		for jobName, limit := range limits {
			err := exec(`INSERT INTO job_concurrency (pipeline_id, job_name, max_in_flight) VALUES (?, ?, ?)`,
				s.pipelineID, jobName, limit)
			if err != nil {
				return fmt.Errorf("could not record concurrency for job %q: %w", jobName, err)
			}
		}

		return nil
	})
}

// execFunc is one statement inside replaceAll's transaction.
type execFunc func(query string, args ...any) error

// replaceAll empties THIS PIPELINE's rows in table and refills them from fill,
// in one transaction. Both config-synced tables are declarative mirrors of the
// YAML, so a partial rewrite is never a valid state to leave behind — and the
// delete is scoped, since a shared state file holds other pipelines' mirrors
// of their own YAML.
func (s *Store) replaceAll(ctx context.Context, what, table string, fill func(execFunc) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not sync %s: %w", what, err)
	}

	defer func() { _ = tx.Rollback() }()

	//nolint:gosec // G202: table is a package-internal literal, never input
	_, err = tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE pipeline_id = ?`, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not clear %s: %w", what, err)
	}

	err = fill(func(query string, args ...any) error {
		_, execErr := tx.ExecContext(ctx, query, args...)

		return execErr //nolint:wrapcheck // the caller names the row it was writing
	})
	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not sync %s: %w", what, err)
	}

	return nil
}

// SerialGroupHolder names a running job that shares a serial group with
// jobName, or "" when nothing is holding the lock. It exists so a waiting job
// can say WHO it is waiting for — "queued" and "blocked on a lock" are
// different states, and a reader who cannot tell them apart cannot tell a
// stuck pipeline from a busy one.
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
		WHERE mine.pipeline_id = ? AND mine.job_name = ?
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
