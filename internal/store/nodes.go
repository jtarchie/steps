package store

import (
	"time"
)

// NodeRecord is the subset of a merkle plan node's fields a driver persists.
// It's a plain data shape rather than an import of merkle.Node so this leaf
// package doesn't need to depend on the planner — callers convert their own
// Node type into one of these.
type NodeRecord struct {
	Hash       string
	ParentHash string
	Kind       string
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only
	Content    map[string]any
}

// NodeRow is one recorded step, with whatever the step produced.
type NodeRow struct {
	Hash      string
	Kind      string
	JobName   string
	Resource  string
	StepIndex int
	Status    string
	Error     string
	Result    string
	CreatedAt time.Time
	// Content and ParentHash are what the hash is MADE of — populated by
	// NodesByHash (the node-detail read), left empty by the list queries,
	// whose callers want a table row rather than a whole content map.
	Content    string
	ParentHash string
}

// MaxTranscriptBytes bounds one stored transcript.
//
// internal/agent already caps a single tool RESULT (maxRecordedResultBytes),
// which bounds the widest single value in a transcript but not the transcript:
// a conversation has as many turns as it needs, and an agent that worked for an
// hour writes all of them into one row. The largest value in the schema was
// therefore the one with no total bound at all.
//
// 256KB is far past any transcript worth reading end to end while staying an
// order of magnitude below the point where one row dominates the database. A
// transcript is a diagnostic, not a ledger: nothing reads it to make a
// decision, so losing the tail of a very long one costs nothing that the
// per-step token counts in agent_usage do not still record exactly.
const MaxTranscriptBytes = 256 * 1024
