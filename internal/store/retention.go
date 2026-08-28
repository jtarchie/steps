package store

// Retention for run state: what a finished run leaves behind, and how much of
// it is kept.
//
// resource_versions was for a long time the only table here with a prune path
// (see history.go). Everything a build writes — its run row, its events, its
// steps, its usage, its nodes and their transcripts — grew forever, so a watch
// answering a hundred things a day accumulated a couple of megabytes a day and
// never gave a byte back. Measured on the pipeline that prompted this: about
// 23KB per build, of which nodes and transcripts were three quarters.
//
// ── Two different things are bounded here, by two different rules ────────────
//
// HISTORY (runs and everything hanging off one) is bounded by RECENCY: keep the
// newest N runs of a job, because what anyone wants to look at is the last few.
//
// The CACHE (nodes, and the job_runs chain index) is bounded by COUNT ONLY,
// newest-by-insertion, and never by age. That distinction is the correction of a
// real bug rather than a nicety. Age was the first rule, and it destroyed the
// cache of pipelines that were working perfectly: a fully-cached poll records no
// new node, calls no recordChainSucceeded, and publishes its skip events with an
// EMPTY hash (see pipeline/walk.go's reportChainSkipped), so nothing about a
// cache HIT refreshes any timestamp. A stable pipeline's cache entries therefore
// looked older every poll until the retention floor swept past them — and the
// faster it polled, the sooner it lost the cache it was relying on, which is
// exactly backwards.
//
// A count cap has no such failure mode, for a reason worth stating plainly: a
// fully-cached pipeline creates no new nodes, so the count does not grow, so
// nothing is ever evicted. Eviction happens only while new entries are being
// made, which is precisely when the old ones are going stale.

import (
	"context"
	"database/sql"
	"fmt"
)

// nodesPerRetainedRun is how many cached nodes a job may keep per run it
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
// retained runs themselves are exempt from the cap anyway (see pruneNodes), so
// this governs only how much EXTRA cache is carried for its hit value.
const nodesPerRetainedRun = 20

// chainsPerRetainedRun is the same multiplier for job_runs, which holds one row
// per whole chain rather than one per step — so a run costs a handful of rows
// rather than a plan's worth. Several because a version: every fan-out runs
// several chains under one job.
const chainsPerRetainedRun = 5

// PruneRuns keeps the newest `limit` runs of a job and deletes the rest, and
// bounds the caches that job has accumulated.
//
// Per job rather than globally, so one busy job cannot evict a quiet one's only
// run — a global cap makes the least active job the least inspectable, which is
// backwards.
//
// limit <= 0 means no limit, the convention this repo documents in
// docs/attempts-timeout.md: omitted takes the default, 0 means no limit.
//
// keepRunID is the run that must survive whatever the cap says — the one whose
// build is calling this. Without it, retention could delete the run it was
// invoked from: a RESUMED run keeps its original started_at (StartRun inserts
// and ResumeRun leaves it alone, which is right — it is when the run started),
// so a resume of an older run is the OLDEST row, and it reaped itself at the
// end of its own build. That left its run_steps and agent_usage cascaded away and FindRun
// answering "no run recorded", making the run permanently unresumable. Pass ""
// when no run is in play.
//
// Almost all of the deleting is done by foreign keys: a run takes its events,
// its steps and its usage rows with it, and a node takes its transcript and its
// usage row. That is why the constraints came first — without them this would be
// eight DELETEs that have to agree with each other forever, and the one that
// gets forgotten leaves rows nothing can ever reach again.
func (s *Store) PruneRuns(ctx context.Context, jobName string, limit int, keepRunID string) error {
	if limit <= 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	defer func() { _ = tx.Rollback() }()

	deleted, err := pruneRunRows(ctx, tx, s.pipelineID, jobName, limit, keepRunID)
	if err != nil {
		return err
	}

	prunedNodes, err := pruneNodes(ctx, tx, s.pipelineID, jobName, limit*nodesPerRetainedRun)
	if err != nil {
		return err
	}

	err = pruneJobRuns(ctx, tx, s.pipelineID, jobName, limit*chainsPerRetainedRun)
	if err != nil {
		return err
	}

	err = sweepAfterNodePrune(ctx, tx, s.pipelineID, prunedNodes)
	if err != nil {
		return err
	}

	if !deleted && !prunedNodes {
		// Nothing changed, so there is nothing to commit.
		return nil
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	return nil
}

// sweepAfterNodePrune runs the two whole-table passes that only make sense once
// nodes have actually been deleted.
//
// Guarded rather than unconditional because the common case by far is a job with
// fewer runs than its cap, and every build of it would otherwise take the
// exclusive write lock (the DSN opens transactions IMMEDIATE) to rewrite nothing
// while a `steps web` on the same file waits.
func sweepAfterNodePrune(ctx context.Context, tx *sql.Tx, pipelineID int64, prunedNodes bool) error {
	if !prunedNodes {
		return nil
	}

	err := clearDanglingParents(ctx, tx, pipelineID)
	if err != nil {
		return err
	}

	return pruneNodeContent(ctx, tx)
}

// pruneRunRows deletes a job's runs past the cap, keeping the newest and
// reporting whether any went.
//
// Two rows are never candidates whatever their age. A run still RUNNING is
// another build in flight (max_in_flight, or a watch alongside a browser
// trigger) and deleting it would make its own event and usage inserts fail the
// foreign keys they now declare. And keepRunID is the caller's own run — see
// PruneRuns.
//
// started_at is compared as text, which is only correct because it is written in
// a zero-padded fixed-width layout; see sortableNano for what went wrong when it
// was not.
func pruneRunRows(
	ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, limit int, keepRunID string,
) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM runs
		WHERE pipeline_id = ? AND job_name = ?
		  AND status <> 'running'
		  AND id <> ?
		  AND id NOT IN (
		      SELECT id FROM runs WHERE pipeline_id = ? AND job_name = ?
		      ORDER BY started_at DESC, rowid DESC
		      LIMIT ?
		  )
	`, pipelineID, jobName, keepRunID, pipelineID, jobName, limit)
	if err != nil {
		return false, fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	return affected > 0, nil
}

// pruneNodes bounds a job's merkle cache to keep entries, keeping the most
// recently inserted, and reports whether any went.
//
// rowid is insertion order — exact, monotonic, and needing no parsing. It
// replaced a created_at comparison that had two problems: nodes are stamped to
// the whole second while runs are stamped to the nanosecond, so nothing compared
// them safely; and, far worse, age is the wrong question entirely for a cache
// (see this file's header).
//
// Nodes a surviving run's events still name are exempt regardless of the cap.
// Not every node gets such an event — a step that failed inside try: records a
// node and publishes nothing carrying its hash — so this is a floor under the
// cap, not the rule itself: it keeps the current run's own nodes from being cut
// by a boundary that happens to fall inside it. A container node (do:,
// in_parallel:) is recorded AFTER the children that hashed under it, so within
// one run a child's rowid is lower than its parent's, and without this exemption
// a cap boundary landing mid-run would take children out from under a live
// build.
//
// Events are not the only way a surviving run points at a node, and reading them
// as if they were cost real records. run_placements and agent_usage both cascade
// off nodes, and both are written under a hash the run's events may never carry:
// a FAILED step returns a zero stepResult (pipeline/task.go), so the
// step_finished its walk publishes carries an EMPTY hash — after recordPlacement
// has already filed the row under the real one. A run well inside run_history:
// therefore kept every GREEN record and lost exactly its RED ones, which is the
// opposite of what someone debugging a placed step or auditing what a failed
// agent spent is looking for. What those two tables hold is HISTORY, bounded by
// the run cap; letting the CACHE's cap reach them made two bounds out of one.
//
// Both clauses are questions about SURVIVING runs only because pruneRunRows has
// already run: a deleted run's placements and usage rows cascaded away with it.
func pruneNodes(ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, keep int) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM nodes
		WHERE pipeline_id = ? AND job_name = ?
		  AND hash NOT IN (
		      SELECT e.hash FROM run_events e
		      JOIN runs r ON r.id = e.run_id
		      WHERE e.hash <> '' AND r.pipeline_id = ?
		  )
		  AND hash NOT IN (SELECT node_hash FROM run_placements WHERE pipeline_id = ?)
		  AND hash NOT IN (SELECT node_hash FROM agent_usage WHERE pipeline_id = ?)
		  AND rowid NOT IN (
		      SELECT rowid FROM nodes WHERE pipeline_id = ? AND job_name = ?
		      ORDER BY rowid DESC
		      LIMIT ?
		  )
	`, pipelineID, jobName, pipelineID, pipelineID, pipelineID, pipelineID, jobName, keep)
	if err != nil {
		return false, fmt.Errorf("could not prune the nodes of %q: %w", jobName, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not prune the nodes of %q: %w", jobName, err)
	}

	return affected > 0, nil
}

// clearDanglingParents nulls a parent link that names no node.
//
// parent_hash carries no foreign key (see the schema for why), so this does by
// hand what ON DELETE SET NULL would have done for links retention broke. The
// node is still a valid cache entry — every lookup is by its own hash — it has
// only lost the link a detail page renders, and a link to nothing is worse than
// none.
//
// Retention is not the only source, and that is worth knowing before reading a
// count of these as damage: a do: block records NO node of its own (its children
// record themselves), so every step chaining under one names a hash that never
// had a row, from the moment it was written. Verified on a real run — six
// dangling links, all of them a do: block's child and its successor. So this
// normalizes two different things that look identical from here, and cannot
// distinguish them. Cosmetic either way.
func clearDanglingParents(ctx context.Context, tx *sql.Tx, pipelineID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE nodes SET parent_hash = NULL
		WHERE pipeline_id = ? AND parent_hash IS NOT NULL
		  AND parent_hash NOT IN (SELECT hash FROM nodes WHERE pipeline_id = ?)
	`, pipelineID, pipelineID)
	if err != nil {
		return fmt.Errorf("could not clear dangling node parents: %w", err)
	}

	return nil
}

// pruneJobRuns bounds the chain-level skip index to keep entries per job, keeping
// the most recently inserted.
//
// By count and never by age, for the reason in this file's header: a cache hit
// does not reach recordChainSucceeded, so a working pipeline's chain entries
// never have their timestamp refreshed and an age rule deleted precisely the
// entries that were doing their job. Deleting one is not a correctness problem —
// the chain simply runs again — but it re-fetches resources and re-runs tasks
// the cache existed to avoid, on a pipeline where nothing was wrong.
//
// This table carries no reference to nodes (see the schema), so its bound is
// independent of theirs by construction.
func pruneJobRuns(ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, keep int) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM job_runs
		WHERE pipeline_id = ? AND job_name = ?
		  AND rowid NOT IN (
		      SELECT rowid FROM job_runs WHERE pipeline_id = ? AND job_name = ?
		      ORDER BY rowid DESC
		      LIMIT ?
		  )
	`, pipelineID, jobName, pipelineID, jobName, keep)
	if err != nil {
		return fmt.Errorf("could not prune the chain cache of %q: %w", jobName, err)
	}

	return nil
}

// pruneNodeContent drops interned content nothing points at any more.
//
// Swept rather than cascaded, because a cascade would be wrong: one row is
// shared by however many nodes rendered identical content, so a single node's
// deletion is no evidence the content is unused. The reference declares
// RESTRICT for the same reason — it turns getting this wrong into an error
// instead of a dangling node.
//
// Not scoped to a job, and — since a state file may hold several pipelines —
// not to a pipeline either: content is shared across both by construction, so
// "referenced by nothing" is the only question that can be asked about it. The
// anti-join must therefore stay pipeline-blind; narrowing it to this pipeline's
// nodes would delete rows another pipeline still points at, which the RESTRICT
// on that reference would turn into a failed prune.
func pruneNodeContent(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM node_content
		WHERE content_hash NOT IN (SELECT content_hash FROM nodes)
	`)
	if err != nil {
		return fmt.Errorf("could not prune interned node content: %w", err)
	}

	return nil
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

// PruneTriggerQueue keeps the newest finished rows of a job and deletes the
// rest. Pending and running rows are never touched — they are the work list.
//
// limit <= 0 means no limit, the same way it does everywhere else here. It read
// the other way round at first — 0 meaning "use the default" — which made the
// one call site, passing a literal 0, mean the opposite of what it looked like
// and left no value that could switch this off.
func (s *Store) PruneTriggerQueue(ctx context.Context, jobName string, limit int) error {
	if limit <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE pipeline_id = ? AND job_name = ?
		  AND status NOT IN ('pending', 'running')
		  AND id NOT IN (
		      SELECT id FROM trigger_queue
		      WHERE pipeline_id = ? AND job_name = ? AND status NOT IN ('pending', 'running')
		      ORDER BY id DESC
		      LIMIT ?
		  )
	`, s.pipelineID, jobName, s.pipelineID, jobName, limit)
	if err != nil {
		return fmt.Errorf("could not prune the trigger queue of %q: %w", jobName, err)
	}

	return nil
}
