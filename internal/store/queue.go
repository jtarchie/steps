package store

import (
	"context"
	"time"
)

// Queue is the work list one daemon drains, and the breaker that holds a job
// that keeps failing. Held is the breaker's word, for one job; paused is
// Control's, for the whole pipeline.
//
// The breaker is here rather than beside it because it decides what the queue
// may hand out: a held job is a row that will not be claimed, so splitting
// them would give a consumer the queue without the rule that governs it.
type Queue interface {
	// EnqueueJob and its two siblings take by, the stored form of who or what asked (actor.Actor.String), which the run the row starts inherits.
	EnqueueJob(ctx context.Context, jobName, reason, by string) error
	// EnqueueManualJob is EnqueueJob for a person's trigger: the row is marked manual, and a pending row it merges into becomes manual too, because the breaker holds back only what was triggered automatically. The merged row takes this person as its requester, since they are why it is manual.
	EnqueueManualJob(ctx context.Context, jobName, reason, by string) error
	// EnqueueRerunJob queues a retry of a recorded run. It is a person's request, so it is manual; it is not merged into the job's ordinary pending row, since it builds different versions.
	EnqueueRerunJob(ctx context.Context, jobName, reason, runID, by string) error
	// ClaimNextJob takes the oldest pending row admitted by serial: and
	// max_in_flight, atomically, and reports false when nothing is ready. The
	// row comes back from the statement that claimed it, so nothing can be
	// read between the claim and what the trigger asked for.
	ClaimNextJob(ctx context.Context) (ClaimedJob, bool, error)
	CompleteJob(ctx context.Context, id int64, status QueueStatus, runErr error) error
	// AbortQueuedJob finalizes the job's pending row as aborted so it is never claimed, reporting false when nothing was pending. A running row is not its to touch: that build is stopped through its context, and holds its serial slot until it actually ends.
	AbortQueuedJob(ctx context.Context, jobName string) (bool, error)
	// ResetStaleRunning reclaims every running row as an abandoned leftover.
	// It is only true when nothing else is alive, which is why steps runs one
	// daemon per database.
	// A row whose run is still running goes back to pending carrying that run (ClaimedJob.Resume); a row whose run already finished is closed with the run's status, so a run somebody aborted is never built again.
	ResetStaleRunning(ctx context.Context) error
	ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error)
	// SyncJobLimits replaces both admission mirrors from one configuration, in
	// one transaction. A job in neither map has no serial group and is
	// admitted one at a time: a missing row means the job left the pipeline
	// between enqueue and claim, and serializing what nobody can describe is
	// the conservative reading.
	SyncJobLimits(ctx context.Context, groups map[string][]string, limits map[string]int) error
	SerialGroupHolder(ctx context.Context, jobName string) (string, error)
	RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (held bool, consecutive int, err error)
	ResetJobFailures(ctx context.Context, jobName string) error
	IsJobHeld(ctx context.Context, jobName string) (bool, error)
	HeldJobs(ctx context.Context) ([]HeldJob, error)
}

// ClaimedJob is a queue row a worker has just taken: its job, and what the
// trigger asks for beyond it.
type ClaimedJob struct {
	ID      int64
	JobName string
	// Manual is a person's trigger, which the breaker does not hold back.
	Manual bool
	// RerunOf is the run this one retries against the versions it was
	// created with, empty for any other trigger.
	RerunOf string
	// Resume is the run a restart interrupted, which this claim continues rather than starting a new one; empty for any other trigger.
	Resume string
}

// QueueStatus is where a trigger-queue row is: pending until claimed, running until CompleteJob (or ResetStaleRunning, which copies the run's status) closes it.
type QueueStatus string

// The statuses a queue row is stored with; the SQL in both drivers spells the same strings.
const (
	QueuePending   QueueStatus = "pending"
	QueueRunning   QueueStatus = "running"
	QueueSucceeded QueueStatus = "succeeded"
	QueueFailed    QueueStatus = "failed"
	QueueAborted   QueueStatus = "aborted"
	// QueueSkipped is a row the circuit breaker took out of the rotation without running it.
	QueueSkipped QueueStatus = "skipped"
)

// QueueRow is one entry in the downstream-trigger queue.
type QueueRow struct {
	ID         int64
	JobName    string
	Reason     string
	Status     QueueStatus
	EnqueuedAt time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}
