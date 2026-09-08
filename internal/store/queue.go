package store

import (
	"context"
	"time"
)

// Queue is the work list one daemon drains, and the breaker that pauses a job
// that keeps failing.
//
// The breaker is here rather than beside it because it decides what the queue
// may hand out: a paused job is a row that will not be claimed, so splitting
// them would give a consumer the queue without the rule that governs it.
type Queue interface {
	EnqueueJob(ctx context.Context, jobName, reason string) error
	// ClaimNextJob takes the oldest pending row admitted by serial: and
	// max_in_flight, atomically, and reports false when nothing is ready.
	ClaimNextJob(ctx context.Context) (int64, string, bool, error)
	CompleteJob(ctx context.Context, id int64, status string, runErr error) error
	// ResetStaleRunning reclaims every running row as an abandoned leftover.
	// It is only true when nothing else is alive, which is why steps runs one
	// daemon per database.
	ResetStaleRunning(ctx context.Context) error
	ListTriggerQueue(ctx context.Context, limit int) ([]QueueRow, error)
	// SyncJobLimits replaces both admission mirrors from one configuration, in
	// one transaction: a job in neither map has no serial group and no limit.
	SyncJobLimits(ctx context.Context, groups map[string][]string, limits map[string]int) error
	SerialGroupHolder(ctx context.Context, jobName string) (string, error)
	RecordJobOutcome(ctx context.Context, jobName string, succeeded bool, maxFailures int) (paused bool, consecutive int, err error)
	ResetJobFailures(ctx context.Context, jobName string) error
	IsJobPaused(ctx context.Context, jobName string) (bool, error)
	PausedJobs(ctx context.Context) ([]PausedJob, error)
}

// QueueRow is one entry in the downstream-trigger queue.
type QueueRow struct {
	ID         int64
	JobName    string
	Reason     string
	Status     string
	EnqueuedAt time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	Error      string
}
