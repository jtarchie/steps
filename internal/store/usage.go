package store

import ()

// AgentUsage is one agent step's recorded spend, as it goes in and comes back
// out of agent_usage.
//
// CostUSD is a pointer because absent and zero are different answers: only a
// CLI-backed agent reports a dollar figure at all, and rendering an unreported
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

// RunTotals is the per-run rollup `steps runs cost` lists.
type RunTotals struct {
	RunID    string
	Tokens   int
	Cached   int
	CostUSD  *float64
	Steps    int
	Unpriced int
}
