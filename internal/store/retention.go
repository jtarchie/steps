package store

import (
	"context"
)

// Pruning is the one place anything is DELETED, named for the verb because
// Retention beside it is the policy the verb takes.
type Pruning interface {
	// Prune applies one retention policy at the end of a build: the job's runs
	// and its finished queue rows are capped by COUNT, newest kept, and a
	// reaped run takes its events, steps, usage, placements, questions and
	// inputs with it. keepRunID is never reaped, however the counts fall.
	//
	// Newest is by start time, with insertion order breaking a tie: two runs
	// can land in the same instant, and the one to keep is the one recorded
	// last.
	//
	// The merkle caches are bounded here too, and by count alone — see Cache.
	// A transcript hangs off the node cache, not the run, and goes with its
	// node. Every call also sweeps the configuration revisions nothing refers
	// to any more; a wholly zero Retention is that sweep alone.
	Prune(ctx context.Context, policy Retention, keepRunID string) error
}

// Retention is what one build is allowed to leave behind: a policy read off
// the configuration, applied once, at the end.
//
// Zero means no limit in both fields, the convention this repo documents in
// docs/attempts-timeout.md. A wholly zero Retention is therefore the
// configuration sweep on its own — what a reload wants, having just adopted a
// configuration and orphaned the one it replaced, with no job in play and no
// run to reap.
type Retention struct {
	// JobName is the job whose history is being bounded. Per job rather than
	// globally, so one busy job cannot evict a quiet one's only run — a global
	// cap makes the least active job the least inspectable, which is backwards.
	JobName string
	// Runs is how many of that job's runs to keep, newest first.
	Runs int
	// TriggerQueue is how many of its FINISHED queue rows to keep. A separate
	// number because the queue is a work list rather than history: see
	// DefaultTriggerQueueHistory, which is what the run passes.
	TriggerQueue int
}

// DefaultTriggerQueueHistory bounds the finished rows kept in trigger_queue.
//
// The queue is a work list, not a history: a pending row means "run this job"
// and a done row means nothing at all to anything that reads the table. They
// were kept anyway, and a failed one carries the error that stopped the job —
// which for a check is the whole generated script, comments included, about
// 1.3KB. A remote that is down at a one-minute poll interval wrote that same
// 1.3KB every minute, forever.
//
// Enough rows to answer "what has this job been doing lately" from `steps
// runs`, and far too few to matter on disk.
const DefaultTriggerQueueHistory = 50

// NodesPerRetainedRun is how many cached nodes a job may keep per run it
// retains — the multiplier turning run_history: into a node cap.
//
// Not its own DSL knob: run_history: already expresses "how much of this job do
// you want kept", and a second dial would ask authors to reason about node
// counts per plan, which is an implementation detail. Scaling off it rather than
// being a flat number matters because the flat version did not bound anything at
// realistic scale — a generous constant meant the cache, not the history, was the
// term that grew, and the footprint measurement caught it.
//
// Twenty is a plan-size allowance: enough that a job of twenty steps keeps a
// full run's worth of nodes for every run it retains, and the nodes of the
// retained runs themselves are exempt from the cap anyway, so this governs
// only how much EXTRA cache is carried for its hit value.
const NodesPerRetainedRun = 20

// ChainsPerRetainedRun is the same multiplier for job_runs, which holds one row
// per whole chain rather than one per step — so a run costs a handful of rows
// rather than a plan's worth. Several because a version: every fan-out runs
// several chains under one job.
const ChainsPerRetainedRun = 5
