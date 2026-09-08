package store

import (
	"time"
)

// RunEventRow is one persisted run event — the stored form of events.Event,
// which is what a finished run is replayed from.
type RunEventRow struct {
	Seq       int64
	RunID     string
	Type      string
	StepIndex int
	StepName  string
	StepKind  string
	// StepID/ParentStepID carry the display tree — see events.Event, which
	// documents why it is minted rather than read off the merkle chain.
	StepID       int64
	ParentStepID int64
	Status       string
	Hash         string
	Text         string
	Name         string
	Detail       string
	DurationMS   int64
	// Worker is where a placed step ran; empty for this machine. See
	// events.Event.Worker.
	Worker string
	At     time.Time
}

// MaxEventTextBytes bounds the free-text columns of one run event.
//
// The publishers already bound what they MEAN to store — a step's output at
// 32,000 bytes (pipeline's maxPublishedOutputBytes), a tool result at 16,384
// (agent's maxRecordedResultBytes) — but nothing bounded an event carrying an
// ERROR, and one error is routinely enormous: a failing check or task reports
// `command %q failed`, where the command is the whole generated shell script,
// about 1.3KB for the built-in git check. MaxStoredErrorBytes caps that for the
// node and queue error columns; this event went in verbatim, into the table with the most
// rows, on every failing step of every poll.
//
// Set above every deliberate publisher cap so it never truncates something a
// publisher chose to keep — it is the backstop for the columns nobody capped,
// not a second opinion on the ones they did.
const MaxEventTextBytes = 64 * 1024
