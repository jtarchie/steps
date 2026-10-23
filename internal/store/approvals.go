package store

import (
	"context"
	"errors"
	"time"
)

// ErrApprovalNotPending is a decision aimed at a row that no longer waits —
// a double click, a second approver, or the deadline getting there first. A
// sentinel so every caller can settle on the decision already recorded while
// still reporting a write that failed as a failure.
var ErrApprovalNotPending = errors.New("approval is not pending")

// Approvals is a build waiting on a person for PERMISSION.
//
// Separate from Questions, which waits on a person for a FACT: an approval
// gates work and its row is the audit trail for having allowed it, and folding
// the two together would file "which environment?" as a decision somebody
// authorised.
type Approvals interface {
	RequestApproval(ctx context.Context, jobName, message string) (int64, error)
	// RequestApprovalFor is RequestApproval for one step of one run, and asking twice for the same pair is one request: a daemon that restarts while a run waits resumes it, the step asks again, and it must find the approval it already made — pending, or already granted while nobody was looking — instead of a second row nobody is waiting on. A refused or expired one is not found: a retry asks a person afresh. An empty runID is a request with no run to tie it to, and always new.
	RequestApprovalFor(ctx context.Context, runID, stepKey, jobName, message string) (int64, error)
	DecideApproval(ctx context.Context, id int64, status ApprovalStatus, by, reason string) error
	ApprovalStatus(ctx context.Context, id int64) (Approval, error)
	// Approvals is the audit trail newest first, capped by limit (zero means
	// no limit). pendingOnly lists only what is still waiting, OLDEST first —
	// the order to work through them in — and its callers pass zero, because
	// a waiting build that scrolled off the list waits forever.
	Approvals(ctx context.Context, pendingOnly bool, limit int) ([]Approval, error)
}

// ApprovalStatus is where an approval is: pending until somebody, or the
// deadline, decides it.
type ApprovalStatus string

// MaxFreeTextBytes caps the one free-text value a person stores whole: an approval's reason, a question's answer. The browser and the CLI both refuse past it.
const MaxFreeTextBytes = 64 << 10

// The statuses an approval is recorded with.
const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	// ApprovalExpired is the deadline deciding: nobody did.
	ApprovalExpired ApprovalStatus = "expired"
)

// Approval is one recorded request for a human decision, and what became of
// it. The row IS the audit trail: who approved a deploy, when, and why a
// rejection was a rejection, are exactly the facts someone needs to
// reconstruct later — and they must not depend on external chat history.
type Approval struct {
	ID          int64
	JobName     string
	Message     string
	Status      ApprovalStatus
	RequestedAt time.Time
	// DecidedAt is zero while the approval is pending.
	DecidedAt time.Time
	DecidedBy string
	Reason    string
}
