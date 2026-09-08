package store

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
