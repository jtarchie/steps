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

	err = s.db.QueryRowContext(ctx, `
		SELECT id FROM approvals
		WHERE pipeline_id = ? AND run_id = ? AND step_key = ? AND status IN ('pending', 'approved')
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

// ApprovalStatus reads one approval's current state.
func (s *Store) ApprovalStatus(ctx context.Context, id int64) (store.Approval, error) {
	approval, err := scanApproval(s.reads.QueryRowContext(ctx, `
		SELECT id, job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, '')
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
		SELECT id, job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, '')
		FROM approvals WHERE pipeline_id = ? `+where+`
		ORDER BY `+order+` LIMIT ?
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.Approval, error) {
		return scanApproval(rows)
	})
}

func scanApproval(row rowScanner) (store.Approval, error) {
	var (
		approval           store.Approval
		requested, decided string
	)

	err := row.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
		&requested, &decided, &approval.DecidedBy, &approval.Reason)
	approval.RequestedAt, approval.DecidedAt = parseTimestamp(requested), parseTimestamp(decided)

	return approval, err //nolint:wrapcheck // every caller names what it was reading
}
