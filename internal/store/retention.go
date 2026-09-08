package store

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
