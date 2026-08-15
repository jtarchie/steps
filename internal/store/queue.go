package store

// trigger_queue and the admission rules around it: what `steps watch` has
// queued, what it may claim, and the serial-group locks that hold it back.

import (
	"context"
	"database/sql"
	"encoding/json"
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

// QueuedVersions are the versions a poll resolved for the job it enqueued,
// keyed by resource name. Empty means none were supplied and the job resolves
// its own by running check — every hand-queued row, and every row written
// before the column existed.
type QueuedVersions map[string][]map[string]any

// EnqueueJob queues jobName with no versions attached, leaving it to resolve
// its own. This is the hand-queued path: the web UI, a manual re-run, and a
// job released because its upstreams passed.
func (s *Store) EnqueueJob(ctx context.Context, jobName, reason string) error {
	return s.EnqueueJobWithVersions(ctx, jobName, reason, nil)
}

// EnqueueJobWithVersions queues jobName along with the versions the caller
// already resolved for it.
//
// At most one pending row exists per job (idx_trigger_queue_pending_job), so
// a second enqueue before a worker claims the first has to do something with
// the versions it is carrying. It MERGES them, which is the whole reason this
// is a read-modify-write in a transaction rather than the ON CONFLICT DO
// NOTHING it used to be: dropping the second enqueue was free when the job
// went on to re-derive everything for itself, and is data loss now that the
// row IS the work. A version the poll found and nothing recorded is gone —
// steps keeps no version history to recover it from.
//
// Order is preserved, oldest first, and a version already on the row is not
// added twice.
func (s *Store) EnqueueJobWithVersions(ctx context.Context, jobName, reason string, versions QueuedVersions) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	defer func() { _ = tx.Rollback() }()

	var existingJSON string

	err = tx.QueryRowContext(ctx,
		`SELECT versions_json FROM trigger_queue WHERE job_name = ? AND status = 'pending'`,
		jobName).Scan(&existingJSON)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = insertQueueRow(ctx, tx, jobName, reason, versions)
	case err != nil:
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	default:
		err = mergeQueueRow(ctx, tx, jobName, existingJSON, versions)
	}

	if err != nil {
		return err
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

func insertQueueRow(ctx context.Context, tx *sql.Tx, jobName, reason string, versions QueuedVersions) error {
	encoded, err := encodeQueuedVersions(versions)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO trigger_queue (job_name, reason, status, enqueued_at, versions_json)
		VALUES (?, ?, 'pending', ?, ?)
	`, jobName, reason, now(), encoded)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

func mergeQueueRow(ctx context.Context, tx *sql.Tx, jobName, existingJSON string, versions QueuedVersions) error {
	if len(versions) == 0 {
		return nil // nothing to add; the pending row already covers this job
	}

	existing, err := decodeQueuedVersions(existingJSON)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	merged, err := mergeVersions(existing, versions)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	encoded, err := encodeQueuedVersions(merged)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE trigger_queue SET versions_json = ? WHERE job_name = ? AND status = 'pending'`,
		encoded, jobName)
	if err != nil {
		return fmt.Errorf("could not enqueue job %q: %w", jobName, err)
	}

	return nil
}

// ClaimedVersions returns the versions attached to a claimed queue row.
//
// A separate lookup rather than another value out of ClaimNextJob: that
// function is a single atomic UPDATE whose shape is the admission rule, and
// widening its RETURNING would touch every caller for the benefit of the one
// that needs this.
func (s *Store) ClaimedVersions(ctx context.Context, id int64) (QueuedVersions, error) {
	var encoded string

	err := s.db.QueryRowContext(ctx, `SELECT versions_json FROM trigger_queue WHERE id = ?`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		// The row is gone (finalized, or cleared by a restart). Nothing
		// supplied is a legitimate answer — the job resolves its own.
		return nil, nil //nolint:nilnil // "no versions attached" is the meaning, not a missing value
	}

	if err != nil {
		return nil, fmt.Errorf("could not read queued versions for row %d: %w", id, err)
	}

	versions, err := decodeQueuedVersions(encoded)
	if err != nil {
		return nil, fmt.Errorf("could not read queued versions for row %d: %w", id, err)
	}

	return versions, nil
}

// mergeVersions unions per resource, keeping the existing order and
// appending only what is not already there. Membership is by the version's
// canonical JSON — the same encoding job_versions and job_version_cursor key
// on, so "the same version" means the same thing everywhere.
func mergeVersions(existing, incoming QueuedVersions) (QueuedVersions, error) {
	merged := make(QueuedVersions, len(existing)+len(incoming))
	for resource, versions := range existing {
		merged[resource] = versions
	}

	for resource, versions := range incoming {
		seen := make(map[string]bool, len(merged[resource]))

		for _, version := range merged[resource] {
			key, err := versionKey(version)
			if err != nil {
				return nil, err
			}

			seen[key] = true
		}

		for _, version := range versions {
			key, err := versionKey(version)
			if err != nil {
				return nil, err
			}

			if seen[key] {
				continue
			}

			seen[key] = true
			merged[resource] = append(merged[resource], version)
		}
	}

	return merged, nil
}

// versionKey is a version's canonical JSON — json.Marshal sorts map keys, so
// this is the same encoding job_versions and job_version_cursor key on, and
// "the same version" means the same thing everywhere.
func versionKey(version map[string]any) (string, error) {
	key, err := json.Marshal(version)
	if err != nil {
		return "", fmt.Errorf("could not encode version: %w", err)
	}

	return string(key), nil
}

func encodeQueuedVersions(versions QueuedVersions) (string, error) {
	if len(versions) == 0 {
		return "", nil
	}

	encoded, err := json.Marshal(versions)
	if err != nil {
		return "", fmt.Errorf("could not encode queued versions: %w", err)
	}

	return string(encoded), nil
}

func decodeQueuedVersions(encoded string) (QueuedVersions, error) {
	if encoded == "" {
		// Every hand-queued row, and every row written before the column
		// existed.
		return nil, nil //nolint:nilnil // "no versions attached" is the meaning
	}

	var versions QueuedVersions

	err := json.Unmarshal([]byte(encoded), &versions)
	if err != nil {
		return nil, fmt.Errorf("could not decode queued versions: %w", err)
	}

	return versions, nil
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
			WHERE tq.status = 'pending'
			  AND (
			      SELECT COUNT(*) FROM trigger_queue AS r
			      WHERE r.job_name = tq.job_name AND r.status = 'running'
			  ) < COALESCE(
			      (SELECT c.max_in_flight FROM job_concurrency AS c WHERE c.job_name = tq.job_name),
			      1
			  )
			  AND NOT EXISTS (
			      SELECT 1
			      FROM job_serial_groups AS mine
			      JOIN job_serial_groups AS theirs ON theirs.group_name = mine.group_name
			      JOIN trigger_queue AS busy
			        ON busy.job_name = theirs.job_name AND busy.status = 'running'
			      WHERE mine.job_name = tq.job_name
			  )
			ORDER BY tq.id LIMIT 1
		)
		RETURNING id, job_name
	`, now()).Scan(&id, &jobName)
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
		WHERE id = ?
	`, status, now(), errText(runErr), id)
	if err != nil {
		return fmt.Errorf("could not complete job (id %d): %w", id, err)
	}

	return nil
}

// ResetStaleRunning flips every running row back to pending — called once
// at Watch startup so a killed (or gracefully but incompletely shut down)
// watch process doesn't strand claimed work forever.
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
	// The superseded running row is FOLDED INTO its successor rather than
	// simply dropped. It carries the versions a poll resolved, and a crash
	// between claiming a row and finishing it must not lose them: re-deriving
	// them was the old safety net, and supplying versions is exactly the
	// removal of that net. Everything else about the row (its reason, its
	// place in the queue) is genuinely superseded.
	err := s.foldSupersededRunning(ctx)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE status = 'running'
		  AND EXISTS (
		      SELECT 1 FROM trigger_queue AS p
		      WHERE p.job_name = trigger_queue.job_name AND p.status = 'pending'
		  )
	`)
	if err != nil {
		return fmt.Errorf("could not clear superseded running jobs: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE trigger_queue SET status = 'pending', started_at = NULL WHERE status = 'running'
	`)
	if err != nil {
		return fmt.Errorf("could not reset stale running jobs: %w", err)
	}

	return nil
}

// foldSupersededRunning merges each about-to-be-deleted running row's
// versions into the pending row that supersedes it.
func (s *Store) foldSupersededRunning(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_name, versions_json FROM trigger_queue
		WHERE status = 'running'
		  AND versions_json != ''
		  AND EXISTS (
		      SELECT 1 FROM trigger_queue AS p
		      WHERE p.job_name = trigger_queue.job_name AND p.status = 'pending'
		  )
	`)
	if err != nil {
		return fmt.Errorf("could not read superseded running jobs: %w", err)
	}

	stranded, err := scanStrandedVersions(rows)
	if err != nil {
		return fmt.Errorf("could not read superseded running jobs: %w", err)
	}

	for jobName, encoded := range stranded {
		versions, decodeErr := decodeQueuedVersions(encoded)
		if decodeErr != nil {
			return decodeErr
		}

		err = s.EnqueueJobWithVersions(ctx, jobName, "resumed after restart", versions)
		if err != nil {
			return err
		}
	}

	return nil
}

// scanStrandedVersions collects job_name -> versions_json for the running
// rows about to be deleted.
func scanStrandedVersions(rows *sql.Rows) (map[string]string, error) {
	defer func() { _ = rows.Close() }()

	stranded := map[string]string{}

	for rows.Next() {
		var jobName, encoded string

		err := rows.Scan(&jobName, &encoded)
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller names what it was reading
		}

		stranded[jobName] = encoded
	}

	return stranded, rows.Err() //nolint:wrapcheck // same
}

// ListTriggerQueue returns the most recent trigger-queue entries, newest
// first — what `steps watch` has queued, run, or failed to run.
func (s *Store) ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error) {
	return collect(ctx, s.db, "trigger_queue", `
		SELECT id, job_name, reason, status, enqueued_at, started_at, finished_at, error
		FROM trigger_queue
		ORDER BY id DESC
		LIMIT ?
	`, []any{limit}, func(rows *sql.Rows) (QueueRow, error) {
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
				err := exec(`INSERT INTO job_serial_groups (job_name, group_name) VALUES (?, ?)
					 ON CONFLICT (job_name, group_name) DO NOTHING`, jobName, group)
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
			err := exec(`INSERT INTO job_concurrency (job_name, max_in_flight) VALUES (?, ?)`, jobName, limit)
			if err != nil {
				return fmt.Errorf("could not record concurrency for job %q: %w", jobName, err)
			}
		}

		return nil
	})
}

// execFunc is one statement inside replaceAll's transaction.
type execFunc func(query string, args ...any) error

// replaceAll empties table and refills it from fill, in one transaction. Both
// config-synced tables are declarative mirrors of the YAML, so a partial
// rewrite is never a valid state to leave behind.
func (s *Store) replaceAll(ctx context.Context, what, table string, fill func(execFunc) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not sync %s: %w", what, err)
	}

	defer func() { _ = tx.Rollback() }()

	//nolint:gosec // G202: table is a package-internal literal, never input
	_, err = tx.ExecContext(ctx, `DELETE FROM `+table)
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
		JOIN job_serial_groups AS theirs ON theirs.group_name = mine.group_name
		JOIN trigger_queue AS busy ON busy.job_name = theirs.job_name AND busy.status = 'running'
		WHERE mine.job_name = ?
		LIMIT 1
	`, jobName).Scan(&holder)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("could not read the serial-group holder for job %q: %w", jobName, err)
	}

	return holder, nil
}
