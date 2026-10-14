package store

import (
	"context"
	"time"
)

// Activity is the log of what people did to a pipeline: set, trigger,
// approve, abort, pause and the rest. Automatic work is not here — a run a
// check started says so on the run — so routine polling cannot push a
// person's action out of a log bounded by count.
//
// A row is written through the handle of the pipeline acted on and keeps
// that pipeline's NAME beside its id, so destroying a pipeline leaves the
// record of its destruction readable.
type Activity interface {
	// RecordActivity appends one entry and trims the whole log, across every
	// pipeline, to the newest limit entries (zero is no limit). The log is
	// one per database because who did what is a question about the daemon,
	// not about any one pipeline.
	RecordActivity(ctx context.Context, entry ActivityEntry, limit int) error
}

// ActivityEntry is one thing a person did.
type ActivityEntry struct {
	ID int64
	// Pipeline is the name it was done to, as it was named then.
	Pipeline string
	// Actor is who did it, actor.Actor's stored form; empty where nobody
	// signs in.
	Actor string
	// Action is the verb: set, trigger, rerun, abort, approve, reject,
	// answer, pause, unpause, release, rename, destroy, mcp.connect, mcp.test.
	Action string
	// Target is what the verb was done to: a job, a run id, a revision sha.
	Target string
	// Detail is a JSON object of anything else worth reading, never secrets.
	Detail string
	At     time.Time
}

// ActivityFilter narrows Reader.Activity. Empty fields match everything.
type ActivityFilter struct {
	// Pipeline matches the name an entry was recorded under, or the current
	// name of the pipeline it was recorded against, so a rename keeps its
	// history.
	Pipeline string
	Actor    string
	Action   string
	// Actions keeps entries with any of these actions; empty keeps all. Beside Action because a page about one thing — an export — asks for every verb that thing has.
	Actions []string
	// Target matches what the verb was done to: a job, a run, an export.
	Target string
	// Since keeps only entries at or after it; zero keeps all.
	Since time.Time
	// Before pages: only entries older than this id.
	Before int64
	Limit  int
}

// DefaultActivityCap is how many entries the log keeps when nothing says otherwise.
const DefaultActivityCap = 10000
