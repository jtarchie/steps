package store

import (
	"errors"
	"time"
)

// RunRow is one run invocation as the history views read it: the resume
// record plus the finish timestamp that makes a duration answerable.
type RunRow struct {
	ID         string
	JobName    string
	Workspace  string
	Status     string
	StartedAt  time.Time
	FinishedAt time.Time
	// ParentRunID is the run a replay forked from, empty for an ordinary run.
	// It is what lets a tuning session read as a session rather than as a pile
	// of unrelated runs that happen to share a job.
	//
	// Filled by EVERY query that builds a RunRow, deliberately: it was briefly
	// selected by only one of them, which made Replayed() answer differently
	// depending on which call site had loaded the row — the jobs list said a
	// forked run was ordinary while its own page linked its parent. That is
	// what the driver's one shared column list exists to make impossible.
	ParentRunID string
	// ConfigSHA is the configuration this run executed, empty for a run
	// started by a caller that loaded no pipeline file. Selected by every
	// RunRow query for the same reason ParentRunID is.
	ConfigSHA string
}

// Replayed reports a run forked from another by --replay.
func (r RunRow) Replayed() bool { return r.ParentRunID != "" }

// Duration is how long the run took, or how long it has been going when it
// has not finished. Zero when the run never started.
func (r RunRow) Duration() time.Duration {
	if r.StartedAt.IsZero() {
		return 0
	}

	if r.FinishedAt.IsZero() {
		return time.Since(r.StartedAt)
	}

	return r.FinishedAt.Sub(r.StartedAt)
}

var (
	// ErrRunExists is a MINT against an id some run already holds.
	//
	// Loud on purpose, and the whole reason StartRun is not an upsert. A run
	// id is a single global key, and an upsert answered a collision by taking
	// the existing row OVER: a finished run flipped back to running and its
	// workspace repointed, while the row kept the old job name and the old
	// pipeline — so every child row the new run wrote hung off a record
	// describing a different run of a different job. A build that refuses to
	// start is a bad afternoon; that was silent history corruption, and
	// nothing downstream could tell it had happened.
	ErrRunExists = errors.New("a run with this id already exists")
	// ErrNoSuchRun is a RESUME of a run this pipeline does not have.
	ErrNoSuchRun = errors.New("no run with this id")
)
