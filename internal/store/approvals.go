package store

import (
	"context"
)

// Approvals is a build waiting on a person for PERMISSION.
//
// Separate from Questions, which waits on a person for a FACT: an approval
// gates work and its row is the audit trail for having allowed it, and folding
// the two together would file "which environment?" as a decision somebody
// authorised.
type Approvals interface {
	RequestApproval(ctx context.Context, jobName, message string) (int64, error)
	DecideApproval(ctx context.Context, id int64, status, by, reason string) error
	ApprovalStatus(ctx context.Context, id int64) (Approval, error)
	// Approvals is the audit trail newest first, capped by limit (zero means
	// no limit). pendingOnly lists only what is still waiting, OLDEST first —
	// the order to work through them in — and its callers pass zero, because
	// a waiting build that scrolled off the list waits forever.
	Approvals(ctx context.Context, pendingOnly bool, limit int) ([]Approval, error)
}

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
