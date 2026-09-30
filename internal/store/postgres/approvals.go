package postgres

// approvals: the record of every human decision on an approval: step.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// RequestApproval records a pending approval and returns its id.
func (s *Store) RequestApproval(ctx context.Context, jobName, message string) (int64, error) {
	var id int64

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO approvals (pipeline_id, job_name, message, status, requested_at)
		VALUES ($1, $2, $3, 'pending', $4)
		RETURNING id
	`, s.pipelineID, jobName, clean(message), now()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("could not request approval for job %q: %w", jobName, err)
	}

	return id, nil
}

// DecideApproval records a decision, refusing to overwrite one already made.
func (s *Store) DecideApproval(ctx context.Context, id int64, status, by, reason string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE approvals SET status = $1, decided_at = $2, decided_by = $3, reason = $4
		WHERE id = $5 AND pipeline_id = $6 AND status = 'pending'
	`, status, now(), clean(by), clean(reason), id, s.pipelineID)
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

const approvalColumns = `id, job_name, message, status, requested_at, decided_at,
		       COALESCE(decided_by, ''), COALESCE(reason, '')`

// scanApproval renders the timestamps RFC3339, as the sqlite driver stores
// them for this table.
func scanApproval(row rowScanner) (store.Approval, error) {
	var (
		approval  store.Approval
		requested time.Time
		decided   sql.NullTime
	)

	err := row.Scan(&approval.ID, &approval.JobName, &approval.Message, &approval.Status,
		&requested, &decided, &approval.DecidedBy, &approval.Reason)

	approval.RequestedAt = requested.UTC().Format(time.RFC3339)
	approval.DecidedAt = stamp(decided, time.RFC3339)

	return approval, err //nolint:wrapcheck // every caller names what it was reading
}

// ApprovalStatus reads one approval's current state.
func (s *Store) ApprovalStatus(ctx context.Context, id int64) (store.Approval, error) {
	approval, err := scanApproval(s.db.QueryRowContext(ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE id = $1 AND pipeline_id = $2`, id, s.pipelineID))
	if err != nil {
		return store.Approval{}, fmt.Errorf("could not read approval %d: %w", id, err)
	}

	return approval, nil
}

// Approvals lists what a pipeline has asked a person to decide: waiting ones
// oldest first and uncapped, the audit listing newest first.
func (s *Store) Approvals(ctx context.Context, pendingOnly bool, limit int) ([]store.Approval, error) {
	where, order, what := `AND status = 'pending'`, `id`, "pending approvals"
	if !pendingOnly {
		where, order, what = ``, `id DESC`, "approvals"
	}

	return collect(ctx, s.db, what, `
		SELECT `+approvalColumns+`
		FROM approvals WHERE pipeline_id = $1 `+where+`
		ORDER BY `+order+` LIMIT $2
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.Approval, error) {
		return scanApproval(rows)
	})
}
