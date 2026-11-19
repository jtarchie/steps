package sqlite

// approvals: the record of every human decision on an approval: step.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RequestApproval records a pending approval and returns its id.
func (s *Store) RequestApproval(ctx context.Context, jobName, message string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (pipeline_id, job_name, message, status, requested_at)
		VALUES (?, ?, ?, 'pending', ?)
	`, s.pipelineID, jobName, message, now())
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	return id, nil
}

// RequestApprovalFor finds the live approval this step of this run already made, or makes one. The insert does nothing on a conflict with the live one, and the read after it is what answers either way.
func (s *Store) RequestApprovalFor(ctx context.Context, runID, stepKey, jobName, message string) (int64, error) {
	if runID == "" {
		return s.RequestApproval(ctx, jobName, message)
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (pipeline_id, job_name, message, status, requested_at, run_id, step_key)
		VALUES (?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT (run_id, step_key) WHERE run_id IS NOT NULL AND status IN ('pending', 'approved') DO NOTHING
	`, s.pipelineID, jobName, message, now(), runID, stepKey)
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	var id int64

	// Not filtered by status: a decision can land between the insert and this read, and the row is still the one this request made or found. The unique index admits one pending-or-approved row per step, so the newest is always it.
	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM approvals
		WHERE pipeline_id = ? AND run_id = ? AND step_key = ?
		ORDER BY id DESC LIMIT 1
	`, s.pipelineID, runID, stepKey).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	return id, nil
}

// DecideApproval records a decision, refusing to overwrite one already made.
func (s *Store) DecideApproval(ctx context.Context, id int64, status store.ApprovalStatus, by, reason string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = ?, decided_at = ?, decided_by = ?, reason = ?
		WHERE id = ? AND pipeline_id = ? AND status = 'pending'
	`, string(status), now(), by, reason, id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("approval %d: %w (already decided, expired, or never existed)", id, store.ErrApprovalNotPending)
	}

	return nil
}

// MarkApprovalNotified stamps when the approval's on_pending was attempted.
func (s *Store) MarkApprovalNotified(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE approvals SET notified_at = ? WHERE id = ? AND pipeline_id = ?`, now(), id, s.pipelineID)
	if err != nil {
		return fmt.Errorf("could not mark approval %d notified: %w", id, err)
	}

	return nil
}

// ApprovalStatus reads one approval's current state.
func (s *Store) ApprovalStatus(ctx context.Context, id int64) (store.Approval, error) {
	approval, err := scanApproval(s.reads.QueryRowContext(ctx, `
		SELECT id, COALESCE(run_id, ''), job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, ''), COALESCE(notified_at, '')
		FROM approvals WHERE id = ? AND pipeline_id = ?
	`, id, s.pipelineID))
	if err != nil {
		return store.Approval{}, fmt.Errorf("could not read approval %d: %w", id, err)
	}

	return approval, nil
}

// Approvals lists what a pipeline has asked a person to decide. pendingOnly
// narrows it to the requests still waiting; limit <= 0 means no limit, the
// convention this repo uses everywhere.
//
// Two orders, for the reason Store.Questions has two. Waiting requests read
// oldest-first, the order somebody should work through them in, and are never
// capped — a job is parked behind each one. The audit listing reads
// newest-first and is capped, because it is history.
func (s *Store) Approvals(ctx context.Context, pendingOnly bool, limit int) ([]store.Approval, error) {
	where, order, what := `AND status = 'pending'`, `id`, "pending approvals"
	if !pendingOnly {
		where, order, what = ``, `id DESC`, "approvals"
	}

	return collect(ctx, s.reads, what, `
		SELECT id, COALESCE(run_id, ''), job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, ''), COALESCE(notified_at, '')
		FROM approvals WHERE pipeline_id = ? `+where+`
		ORDER BY `+order+` LIMIT ?
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.Approval, error) {
		return scanApproval(rows)
	})
}

func scanApproval(row rowScanner) (store.Approval, error) {
	var (
		approval                     store.Approval
		requested, decided, notified string
	)

	err := row.Scan(&approval.ID, &approval.RunID, &approval.JobName, &approval.Message, &approval.Status,
		&requested, &decided, &approval.DecidedBy, &approval.Reason, &notified)
	approval.RequestedAt, approval.DecidedAt, approval.NotifiedAt = parseTimestamp(requested), parseTimestamp(decided), parseTimestamp(notified)

	return approval, err //nolint:wrapcheck // every caller names what it was reading
}

// FindApprovals is the trail narrowed and paged, newest first.
func (s *Store) FindApprovals(ctx context.Context, query store.ApprovalQuery) ([]store.Approval, error) {
	filter, args := byJob(query.Job, []any{s.pipelineID})

	if query.ID > 0 {
		filter += ` AND id = ?`
		args = append(args, query.ID)
	}

	if len(query.Statuses) > 0 {
		filter += ` AND status IN (SELECT value FROM json_each(?))`
		args = append(args, jsonList(query.Statuses))
	}

	if query.Text != "" {
		filter += `
		  AND (instr(lower(message), lower(?)) > 0 OR instr(lower(COALESCE(reason, '')), lower(?)) > 0
		    OR instr(lower(COALESCE(decided_by, '')), lower(?)) > 0 OR instr(lower(COALESCE(run_id, '')), lower(?)) > 0
		    OR instr(lower(job_name), lower(?)) > 0 OR instr(lower(status), lower(?)) > 0)`
		args = append(args, query.Text, query.Text, query.Text, query.Text, query.Text, query.Text)
	}

	if query.Before > 0 {
		filter += ` AND id < ?`
		args = append(args, query.Before)
	}

	return collect(ctx, s.reads, "approvals", `
		SELECT id, COALESCE(run_id, ''), job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, ''), COALESCE(notified_at, '')
		FROM approvals WHERE pipeline_id = ?`+filter+`
		ORDER BY id DESC LIMIT ?
	`, append(args, rowLimit(query.Limit)), func(rows *sql.Rows) (store.Approval, error) {
		return scanApproval(rows)
	})
}
