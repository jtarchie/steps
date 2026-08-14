package store

// approvals: the record of every human decision on an approval: step.

import (
	"context"
	"database/sql"
	"fmt"
)

// Approval is one recorded request for a human decision, and what became of
// it. The row IS the audit trail: who approved a deploy, when, and why a
// rejection was a rejection, are exactly the facts someone needs to
// reconstruct later — and they must not depend on external chat history.
type Approval struct {
	ID          int64
	JobName     string
	Message     string
	Status      string // pending, approved, rejected, expired
	RequestedAt string
	DecidedAt   string
	DecidedBy   string
	Reason      string
}

// RequestApproval records a pending approval and returns its id.
func (s *Store) RequestApproval(ctx context.Context, jobName, message string) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO approvals (job_name, message, status, requested_at)
		VALUES (?, ?, 'pending', ?)
	`, jobName, message, now())
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	return id, nil
}

// DecideApproval records a decision, refusing to overwrite one already made.
func (s *Store) DecideApproval(ctx context.Context, id int64, status, by, reason string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = ?, decided_at = ?, decided_by = ?, reason = ?
		WHERE id = ? AND status = 'pending'
	`, status, now(), by, reason, id)
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not decide approval %d: %w", id, err)
	}

	if affected == 0 {
		return fmt.Errorf("approval %d is not pending (already decided, expired, or never existed)", id)
	}

	return nil
}

// ApprovalStatus reads one approval's current state.
func (s *Store) ApprovalStatus(ctx context.Context, id int64) (Approval, error) {
	var approval Approval

	err := s.db.QueryRowContext(ctx, `
		SELECT id, job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, '')
		FROM approvals WHERE id = ?
	`, id).Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
		&approval.RequestedAt, &approval.DecidedAt, &approval.DecidedBy, &approval.Reason)
	if err != nil {
		return Approval{}, fmt.Errorf("could not read approval %d: %w", id, err)
	}

	return approval, nil
}

// PendingApprovals lists every approval still waiting, oldest first.
func (s *Store) PendingApprovals(ctx context.Context) ([]Approval, error) {
	return collect(ctx, s.db, "pending approvals", `
		SELECT id, job_name, message, requested_at FROM approvals
		WHERE status = 'pending' ORDER BY id
	`, nil, func(rows *sql.Rows) (Approval, error) {
		approval := Approval{Status: "pending"}

		return approval, rows.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.RequestedAt)
	})
}

// AllApprovals lists every approval decision, newest first — the audit trail
// PendingApprovals deliberately does not carry.
func (s *Store) AllApprovals(ctx context.Context, limit int) ([]Approval, error) {
	return collect(ctx, s.db, "approvals", `
		SELECT id, job_name, message, status, requested_at,
		       COALESCE(decided_at, ''), COALESCE(decided_by, ''), COALESCE(reason, '')
		FROM approvals ORDER BY id DESC LIMIT ?
	`, []any{limit}, func(rows *sql.Rows) (Approval, error) {
		var approval Approval

		return approval, rows.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
			&approval.RequestedAt, &approval.DecidedAt, &approval.DecidedBy, &approval.Reason)
	})
}
