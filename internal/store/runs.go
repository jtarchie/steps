package store

import (
	"context"
	"errors"
	"time"
)

// Runs is one row per invocation: what --resume continues and what the
// history views list.
type Runs interface {
	// StartRun mints a run. An id some run already holds is ErrRunExists,
	// never an overwrite.
	StartRun(ctx context.Context, run NewRun) error
	// ResumeRun continues a run this pipeline already has, ErrNoSuchRun
	// otherwise.
	ResumeRun(ctx context.Context, id, workspaceDir, configSHA string) error
	// ResumableRuns is every run of this pipeline still recorded running, or that a queue row still waits to resume (a restart before its resume started): at startup, the ones a restart may resume, whose workspaces must survive the stale-build sweep.
	ResumableRuns(ctx context.Context) ([]RunRow, error)
	FinishRun(ctx context.Context, id string, status RunStatus) error
	// RecordRunStep marks a step of a run done. buildID is "<run>#0" inside
	// the build a get triggers and the bare run id before it.
	RecordRunStep(ctx context.Context, runID string, step RunStep) error
	RecordRunParent(ctx context.Context, runID, parentID string) error
	// RecordRunRerun marks a run as a rerun of another (fly rerun-build).
	RecordRunRerun(ctx context.Context, runID, originalID string) error
	// CompletedRunSteps returns what a run finished, in completion order.
	CompletedRunSteps(ctx context.Context, runID string) ([]RunStep, error)
	// ListRuns returns a job's runs newest first. Zero means no limit, the
	// convention everywhere here.
	ListRuns(ctx context.Context, jobName string, limit int) ([]RunRow, error)
	LatestRunByJob(ctx context.Context) (map[string]RunRow, error)
	// LatestFinishedRunByJob is each job's newest run that is no longer
	// running, reruns included, as ListRuns includes them.
	LatestFinishedRunByJob(ctx context.Context) (map[string]RunRow, error)
	RunsUsingNode(ctx context.Context, hash string, limit int) ([]RunRow, error)
	FindRunRow(ctx context.Context, id string) (RunRow, bool, error)
	FirstRunSince(ctx context.Context, jobName string, since time.Time) (RunRow, bool, error)
}

// RunStatus is where a run is: running until FinishRun records how it ended.
// There is no errored: a run the machinery broke is recorded failed, and only
// a step's own status tells the two apart.
type RunStatus string

// The statuses a run is recorded with.
const (
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
	RunAborted   RunStatus = "aborted"
)

// NewRun is what StartRun mints a run from.
type NewRun struct {
	ID      string
	JobName string
	// Workspace is the directory the run's steps execute in, and what a
	// resume continues from.
	Workspace string
	// ConfigSHA is the configuration the run executes, empty for a caller
	// that loaded no pipeline file.
	ConfigSHA string
	// QueueID is the trigger_queue row that started this run, zero for a run no queue started; recorded on that row, so a restart knows which run to resume.
	QueueID int64
	// CreatedBy is who started a run no queue row did. A queued run takes its row's requester instead, read in the same statement that records the run, so nothing between the claim and the start can lose it.
	CreatedBy string
}

// RunStep is one step a run finished. Index is relative to the walk it ran
// in — the walk after a get counts from zero again — so it names a step only
// together with BuildID.
type RunStep struct {
	BuildID string
	Index   int
	Name    string
}

// RunRow is one run invocation as the history views read it: the resume
// record plus the finish timestamp that makes a duration answerable.
type RunRow struct {
	ID         string
	JobName    string
	Workspace  string
	Status     RunStatus
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
	// RerunOf is the run this one re-ran against the versions it was created with, empty for any other run. Selected by every RunRow query for the same reason ParentRunID is.
	RerunOf string
	// CreatedBy is the stored form of who or what started the run (actor.Parse reads it), empty for a run recorded before anyone was. Selected by every RunRow query for the same reason ParentRunID is.
	CreatedBy string
}

// Rerun reports a run made by retrying another.
func (r RunRow) Rerun() bool { return r.RerunOf != "" }

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
	// ErrNoSuchRun is a RESUME of, or a run-scoped write to, a run this pipeline does not have.
	ErrNoSuchRun = errors.New("no run with this id")
)
