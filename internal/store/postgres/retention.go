package postgres

// Retention for run state. The policy — history bounded by recency, the
// cache bounded by COUNT and never by age, and the reasons for both — is the
// sqlite driver's (internal/store/sqlite/retention.go). What this file owes
// that one is the same answers; the count caps read seq where sqlite reads
// rowid.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// Prune applies one build's retention policy. keepRunID survives whatever the
// cap says: it is the run whose build is calling this.
func (s *Store) Prune(ctx context.Context, policy store.Retention, keepRunID string) error {
	return errors.Join(
		s.pruneRuns(ctx, policy.JobName, policy.Runs, keepRunID),
		s.pruneTriggerQueue(ctx, policy.JobName, policy.TriggerQueue),
	)
}

// pruneRuns keeps the newest limit runs of a job, bounds the caches that job
// has accumulated, and sweeps what those deletions orphaned. Foreign keys do
// most of the deleting.
//
// It holds the pipeline lock: every statement here decides what to delete
// from a read of what survives, and a run or node recorded concurrently by
// the same pipeline must not land in the middle of that.
func (s *Store) pruneRuns(ctx context.Context, jobName string, limit int, keepRunID string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		if limit <= 0 {
			// Unbounded runs still leave orphaned configurations behind.
			return pruneRevisions(ctx, tx, s.pipelineID)
		}

		err := pruneRunRows(ctx, tx, s.pipelineID, jobName, limit, keepRunID)
		if err != nil {
			return err
		}

		prunedContent, err := pruneNodes(ctx, tx, s.pipelineID, jobName, limit*store.NodesPerRetainedRun)
		if err != nil {
			return err
		}

		err = pruneJobRuns(ctx, tx, s.pipelineID, jobName, limit*store.ChainsPerRetainedRun)
		if err != nil {
			return err
		}

		if len(prunedContent) > 0 {
			err = clearDanglingParents(ctx, tx, s.pipelineID)
			if err != nil {
				return err
			}

			err = pruneNodeContent(ctx, tx, prunedContent)
			if err != nil {
				return err
			}
		}

		return pruneRevisions(ctx, tx, s.pipelineID)
	})
	if err != nil {
		return fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	return nil
}

// pruneRunRows deletes a job's runs past the cap, never one still running and
// never keepRunID.
func pruneRunRows(ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, limit int, keepRunID string) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM runs
		WHERE pipeline_id = $1 AND job_name = $2
		  AND status <> 'running'
		  AND id <> $3
		  AND id NOT IN (
		      SELECT id FROM runs WHERE pipeline_id = $1 AND job_name = $2
		      ORDER BY started_at DESC, seq DESC
		      LIMIT $4
		  )
		  AND id NOT IN (
		      SELECT run_id FROM trigger_queue
		      WHERE pipeline_id = $1 AND status = 'pending' AND run_id IS NOT NULL
		  )
	`, pipelineID, jobName, keepRunID, limit)
	if err != nil {
		return fmt.Errorf("could not prune runs of %q: %w", jobName, err)
	}

	return nil
}

// pruneNodes bounds a job's merkle cache to keep entries, newest inserted first, exempting every node a surviving run still names (by event, placement or usage), and returns the content hashes of the nodes that went: the only rows the content sweep has reason to look at.
func pruneNodes(ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, keep int) ([]string, error) {
	hashes, err := collect(ctx, tx, "the pruned nodes", `
		DELETE FROM nodes
		WHERE pipeline_id = $1
		  AND seq IN (
		      SELECT seq FROM nodes WHERE pipeline_id = $1 AND job_name = $2
		      ORDER BY seq DESC
		      OFFSET $3
		  )
		  -- NOT EXISTS, never NOT IN: a hook placement's NULL node_hash would
		  -- make a NOT IN match nothing.
		  AND NOT EXISTS (
		      SELECT 1 FROM run_events e
		      JOIN runs r ON r.id = e.run_id
		      WHERE e.hash = nodes.hash AND r.pipeline_id = nodes.pipeline_id
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM run_placements p
		      WHERE p.pipeline_id = nodes.pipeline_id AND p.node_hash = nodes.hash
		  )
		  AND NOT EXISTS (
		      SELECT 1 FROM agent_usage u
		      WHERE u.pipeline_id = nodes.pipeline_id AND u.node_hash = nodes.hash
		  )
		RETURNING content_hash
	`, []any{pipelineID, jobName, keep}, scanString)
	if err != nil {
		return nil, fmt.Errorf("could not prune the nodes of %q: %w", jobName, err)
	}

	if err != nil {
		return nil, fmt.Errorf("could not prune the nodes of %q: %w", jobName, err)
	}

	return hashes, nil
}

// clearDanglingParents nulls a parent link that names no node, which is what
// ON DELETE SET NULL would have done for a column that cannot declare one.
func clearDanglingParents(ctx context.Context, tx *sql.Tx, pipelineID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE nodes SET parent_hash = NULL
		WHERE pipeline_id = $1 AND parent_hash IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.pipeline_id = $1 AND n.hash = nodes.parent_hash)
	`, pipelineID)
	if err != nil {
		return fmt.Errorf("could not clear dangling node parents: %w", err)
	}

	return nil
}

// pruneJobRuns bounds the chain-level skip index to keep entries per job,
// newest inserted first.
func pruneJobRuns(ctx context.Context, tx *sql.Tx, pipelineID int64, jobName string, keep int) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM job_runs
		WHERE pipeline_id = $1 AND job_name = $2
		  AND seq NOT IN (
		      SELECT seq FROM job_runs WHERE pipeline_id = $1 AND job_name = $2
		      ORDER BY seq DESC
		      LIMIT $3
		  )
	`, pipelineID, jobName, keep)
	if err != nil {
		return fmt.Errorf("could not prune the chain cache of %q: %w", jobName, err)
	}

	return nil
}

// pruneNodeContent drops interned content nothing points at, across every pipeline, since content is shared by construction. candidates narrows the sweep to the content of nodes the caller just deleted (the caller skips the call, and the lock, when it deleted none); nil sweeps the whole table (Delete, whose cascade removed nodes this transaction never saw). The EXCLUSIVE content lock is against RecordNode's shared hold, and nothing takes it and a pipeline lock in the other order.
func pruneNodeContent(ctx context.Context, tx *sql.Tx, candidates []string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, classContent)
	if err != nil {
		return fmt.Errorf("could not lock interned node content: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		DELETE FROM node_content c
		WHERE ($1::text[] IS NULL OR c.content_hash = ANY($1::text[]))
		  AND NOT EXISTS (SELECT 1 FROM nodes n WHERE n.content_hash = c.content_hash)
	`, candidates)
	if err != nil {
		return fmt.Errorf("could not prune interned node content: %w", err)
	}

	return nil
}

// pruneTriggerQueue keeps the newest finished rows of a job; pending and
// running rows are the work list and are never touched. limit <= 0 is no
// limit.
func (s *Store) pruneTriggerQueue(ctx context.Context, jobName string, limit int) error {
	if limit <= 0 {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		DELETE FROM trigger_queue
		WHERE pipeline_id = $1 AND job_name = $2
		  AND status NOT IN ('pending', 'running')
		  AND id NOT IN (
		      SELECT id FROM trigger_queue
		      WHERE pipeline_id = $1 AND job_name = $2 AND status NOT IN ('pending', 'running')
		      ORDER BY id DESC
		      LIMIT $3
		  )
	`, s.pipelineID, jobName, limit)
	if err != nil {
		return fmt.Errorf("could not prune the trigger queue of %q: %w", jobName, err)
	}

	return nil
}
