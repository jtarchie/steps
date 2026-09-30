package postgres

// agent_usage: what each agent step spent, and the provider metadata that
// explains it.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

const usageColumns = `run_id, step_index, step_name, job_name, node_hash,
	model_requested, model_served,
	prompt_tokens, completion_tokens, total_tokens,
	cached_tokens, reasoning_tokens, cost_usd,
	finish_reason, duration_ms, raw_meta`

// RecordAgentUsage stores what one agent step spent, ACCUMULATING the counts
// on conflict (every attempt was paid for) and replacing the descriptive
// fields. A cost is unpriced only when both sides are.
func (s *Store) RecordAgentUsage(ctx context.Context, usage store.AgentUsage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_usage (pipeline_id, `+usageColumns+`, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (pipeline_id, run_id, node_hash) DO UPDATE SET
			step_index = excluded.step_index,
			step_name = excluded.step_name,
			model_served = excluded.model_served,
			prompt_tokens = agent_usage.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = agent_usage.completion_tokens + excluded.completion_tokens,
			total_tokens = agent_usage.total_tokens + excluded.total_tokens,
			cached_tokens = agent_usage.cached_tokens + excluded.cached_tokens,
			reasoning_tokens = agent_usage.reasoning_tokens + excluded.reasoning_tokens,
			cost_usd = CASE
				WHEN agent_usage.cost_usd IS NULL AND excluded.cost_usd IS NULL THEN NULL
				ELSE COALESCE(agent_usage.cost_usd, 0) + COALESCE(excluded.cost_usd, 0)
			END,
			finish_reason = excluded.finish_reason,
			duration_ms = agent_usage.duration_ms + excluded.duration_ms,
			raw_meta = excluded.raw_meta,
			created_at = excluded.created_at
	`,
		s.pipelineID,
		usage.RunID, usage.StepIndex, clean(usage.StepName), usage.JobName, usage.NodeHash,
		clean(usage.ModelReq), clean(usage.ModelServed),
		usage.Prompt, usage.Completion, usage.Total,
		usage.Cached, usage.Reasoning, usage.CostUSD,
		clean(usage.FinishReason), usage.DurationMS, clean(usage.RawMeta),
		now())
	if err != nil {
		return fmt.Errorf("could not record agent usage for run %q: %w", usage.RunID, err)
	}

	return nil
}

// RunTokensSpent is the total tokens already recorded against a run.
func (s *Store) RunTokensSpent(ctx context.Context, runID string) (int, error) {
	var total int

	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0)::bigint FROM agent_usage WHERE pipeline_id = $1 AND run_id = $2`,
		s.pipelineID, runID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("could not read spend for run %q: %w", runID, err)
	}

	return total, nil
}

// RunUsage is every agent step's spend for one run, in step order.
func (s *Store) RunUsage(ctx context.Context, runID string) ([]store.AgentUsage, error) {
	return collect(ctx, s.db, "usage for run "+runID,
		`SELECT `+usageColumns+` FROM agent_usage
		 WHERE pipeline_id = $1 AND run_id = $2 ORDER BY step_index, seq`,
		[]any{s.pipelineID, runID}, func(rows *sql.Rows) (store.AgentUsage, error) {
			var usage store.AgentUsage

			return usage, rows.Scan(&usage.RunID, &usage.StepIndex, &usage.StepName, &usage.JobName, &usage.NodeHash,
				&usage.ModelReq, &usage.ModelServed,
				&usage.Prompt, &usage.Completion, &usage.Total,
				&usage.Cached, &usage.Reasoning, &usage.CostUSD,
				&usage.FinishReason, &usage.DurationMS, &usage.RawMeta)
		})
}

// RunCostTotals rolls agent_usage up per run, newest first, counting the
// unpriced steps so a partial total is shown as partial.
func (s *Store) RunCostTotals(ctx context.Context, limit int) ([]store.RunTotals, error) {
	return collect(ctx, s.db, "the usage rollup", `
		SELECT run_id,
		       SUM(total_tokens)::bigint, SUM(cached_tokens)::bigint,
		       SUM(COALESCE(cost_usd, 0)), COUNT(*),
		       COUNT(*) FILTER (WHERE cost_usd IS NULL)
		FROM agent_usage
		WHERE pipeline_id = $1
		GROUP BY run_id
		ORDER BY MAX(created_at) DESC, MAX(seq) DESC
		LIMIT $2
	`, []any{s.pipelineID, rowLimit(limit)}, func(rows *sql.Rows) (store.RunTotals, error) {
		var (
			totals store.RunTotals
			cost   float64
		)

		err := rows.Scan(&totals.RunID, &totals.Tokens, &totals.Cached, &cost, &totals.Steps, &totals.Unpriced)
		if totals.Unpriced < totals.Steps {
			totals.CostUSD = &cost
		}

		return totals, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}
