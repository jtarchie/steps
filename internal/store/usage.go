package store

// agent_usage: what each agent step spent, and the provider metadata that
// explains it.

import (
	"context"
	"database/sql"
	"fmt"
)

// AgentUsage is one agent step's recorded spend, as it goes in and comes back
// out of agent_usage.
//
// CostUSD is a pointer because absent and zero are different answers: no
// provider path reports a dollar figure today, and rendering an unreported
// cost as $0.00 would make an unpriced run look free.
type AgentUsage struct {
	RunID        string
	StepIndex    int
	StepName     string
	JobName      string
	NodeHash     string
	ModelReq     string
	ModelServed  string
	Prompt       int
	Completion   int
	Total        int
	Cached       int
	Reasoning    int
	CostUSD      *float64
	FinishReason string
	DurationMS   int64
	RawMeta      string
}

// RunTotals is the per-run rollup `steps runs --cost` lists.
type RunTotals struct {
	RunID    string
	Tokens   int
	Cached   int
	CostUSD  *float64
	Steps    int
	Unpriced int
}

// usageColumns is the column list RecordAgentUsage writes and RunUsage reads
// back, in the field order of AgentUsage.
const usageColumns = `run_id, step_index, step_name, job_name, node_hash,
	model_requested, model_served,
	prompt_tokens, completion_tokens, total_tokens,
	cached_tokens, reasoning_tokens, cost_usd,
	finish_reason, duration_ms, raw_meta`

// RecordAgentUsage stores what one agent step spent.
//
// ACCUMULATES the token counts on conflict, and replaces the descriptive
// fields.
//
// A step can execute more than once against the same node hash: a to: route
// sending the plan back over it, or a resumed run re-running the step its
// predecessor died on. You paid for every one of those attempts, so the
// counts are a running total — replacing them reported the last attempt as
// though it were the whole bill, and made this table unusable as the ledger a
// resumed run reads its prior spend from (RunTokensSpent).
//
// The descriptive columns still take the newest value: model_served,
// finish_reason and raw_meta describe ONE response and cannot be summed, and
// the last attempt is the one whose result the run actually used.
//
// The conflict is on the node hash, so two DIFFERENT steps sharing a plan
// index — the cells of a matrix, the members of an ensemble — never collide.
func (s *Store) RecordAgentUsage(ctx context.Context, usage AgentUsage) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agent_usage (`+usageColumns+`, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (run_id, node_hash) DO UPDATE SET
			step_index = excluded.step_index,
			step_name = excluded.step_name,
			model_served = excluded.model_served,
			prompt_tokens = agent_usage.prompt_tokens + excluded.prompt_tokens,
			completion_tokens = agent_usage.completion_tokens + excluded.completion_tokens,
			total_tokens = agent_usage.total_tokens + excluded.total_tokens,
			cached_tokens = agent_usage.cached_tokens + excluded.cached_tokens,
			reasoning_tokens = agent_usage.reasoning_tokens + excluded.reasoning_tokens,
			cost_usd = agent_usage.cost_usd + excluded.cost_usd,
			finish_reason = excluded.finish_reason,
			duration_ms = agent_usage.duration_ms + excluded.duration_ms,
			raw_meta = excluded.raw_meta,
			created_at = excluded.created_at
	`,
		usage.RunID, usage.StepIndex, usage.StepName, usage.JobName, usage.NodeHash,
		usage.ModelReq, usage.ModelServed,
		usage.Prompt, usage.Completion, usage.Total,
		usage.Cached, usage.Reasoning, usage.CostUSD,
		usage.FinishReason, usage.DurationMS, usage.RawMeta,
		now())
	if err != nil {
		return fmt.Errorf("could not record agent usage for run %q: %w", usage.RunID, err)
	}

	return nil
}

// RunTokensSpent is the total tokens already recorded against a run.
//
// Read when RESUMING, so a job budget continues from what earlier attempts of
// the same run spent instead of restarting at zero. Summed in SQL rather than
// by walking RunUsage: the caller wants one number, and a resumed run may
// carry hundreds of step rows.
//
// A run with no agent steps yet returns 0, which is also what a fresh run's
// id yields — both mean "nothing spent", so there is nothing to distinguish.
func (s *Store) RunTokensSpent(ctx context.Context, runID string) (int, error) {
	var total int

	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(total_tokens), 0) FROM agent_usage WHERE run_id = ?`, runID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("could not read spend for run %q: %w", runID, err)
	}

	return total, nil
}

// RunUsage is every agent step's spend for one run, in step order.
func (s *Store) RunUsage(ctx context.Context, runID string) ([]AgentUsage, error) {
	return collect(ctx, s.db, "usage for run "+runID,
		`SELECT `+usageColumns+` FROM agent_usage WHERE run_id = ? ORDER BY step_index, rowid`,
		[]any{runID}, func(rows *sql.Rows) (AgentUsage, error) {
			var usage AgentUsage

			return usage, rows.Scan(&usage.RunID, &usage.StepIndex, &usage.StepName, &usage.JobName, &usage.NodeHash,
				&usage.ModelReq, &usage.ModelServed,
				&usage.Prompt, &usage.Completion, &usage.Total,
				&usage.Cached, &usage.Reasoning, &usage.CostUSD,
				&usage.FinishReason, &usage.DurationMS, &usage.RawMeta)
		})
}

// RunCostTotals rolls agent_usage up per run, newest first.
//
// Unpriced counts the steps with no reported cost, so a partial dollar total
// can be shown AS partial rather than presented as the whole bill.
func (s *Store) RunCostTotals(ctx context.Context, limit int) ([]RunTotals, error) {
	return collect(ctx, s.db, "the usage rollup", `
		SELECT run_id,
		       SUM(total_tokens), SUM(cached_tokens),
		       SUM(COALESCE(cost_usd, 0)), COUNT(*),
		       SUM(CASE WHEN cost_usd IS NULL THEN 1 ELSE 0 END)
		FROM agent_usage
		GROUP BY run_id
		ORDER BY MAX(created_at) DESC
		LIMIT ?
	`, []any{limit}, func(rows *sql.Rows) (RunTotals, error) {
		var (
			totals RunTotals
			cost   float64
		)

		err := rows.Scan(&totals.RunID, &totals.Tokens, &totals.Cached, &cost, &totals.Steps, &totals.Unpriced)
		if totals.Unpriced < totals.Steps {
			totals.CostUSD = &cost
		}

		return totals, err //nolint:wrapcheck // collect wraps with the thing being read
	})
}
