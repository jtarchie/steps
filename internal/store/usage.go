package store

import (
	"context"
)

// Usage is the agent token and cost ledger.
type Usage interface {
	RecordAgentUsage(ctx context.Context, usage AgentUsage) error
	RunTokensSpent(ctx context.Context, runID string) (int, error)
	RunUsage(ctx context.Context, runID string) ([]AgentUsage, error)
	RunCostTotals(ctx context.Context, limit int) ([]RunTotals, error)
}

// AgentUsage is one agent step's recorded spend, as it goes in and comes back
// out of agent_usage.
//
// CostUSD is a pointer because absent and zero are different answers: only a
// CLI-backed agent reports a dollar figure at all, and rendering an unreported
// cost as $0.00 would make an unpriced run look free.
type AgentUsage struct {
	RunID     string
	StepIndex int
	StepName  string
	JobName   string
	// Kind is UsageStep, UsageHook or UsageFix; empty records as UsageStep.
	Kind string
	// NodeHash is empty for a hook or a fix, which are never hashed: their
	// rows hang off the run alone.
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

// What spent an agent_usage row's tokens. A hook's agent and a task's fix:
// agent run inside the job and spend against its budget, but have no node.
const (
	UsageStep = "step"
	UsageHook = "hook"
	UsageFix  = "fix"
)

// UsageKind is the kind a row records: Kind, or UsageStep when unset.
func (u AgentUsage) UsageKind() string {
	if u.Kind == "" {
		return UsageStep
	}

	return u.Kind
}

// RunTotals is the per-run rollup `steps runs cost` lists.
type RunTotals struct {
	RunID    string
	Tokens   int
	Cached   int
	CostUSD  *float64
	Steps    int
	Unpriced int
}
