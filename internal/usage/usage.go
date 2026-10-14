// Package usage is the spend domain: what an agent step reported, the
// accumulator a job rolls those reports into, and the ceilings measured
// against it. A stdlib-only leaf, so a job budget (internal/pipeline) and
// the conversation that meters it (internal/agent) share it without either
// importing the other for it.
//
// Token accounting: what an agent step actually spent, and the ceilings that
// stop it spending more.
//
// An agent step can loop, hold a long conversation where every turn re-sends
// the whole history, and retry. Until this existed there was no upper bound on
// what one run could spend and no report afterwards of what it did spend —
// which is untenable against a provider that caps usage by the dollar rather
// than by request rate.
//
// Reporting comes first and matters on its own: knowing a job spent 341K
// tokens across four agent steps carries no correctness risk, and it is what
// tells you which ceilings are even sensible to set. Enforcement sits on top
// of the same counter.
//
// The numbers are the PROVIDER's own reported usage, not the len(text)/4
// estimate compaction uses to decide when to summarize. Those two must not be
// confused: one is a size heuristic, this is accounting.
package usage

import (
	"context"
	"strconv"
	"strings"
	"sync"
)

// Step is one agent step's spend, plus the provider metadata that
// explains it.
//
// Tokens are what a budget measures. The rest is what makes a number
// actionable afterwards: which model actually answered, how much of the prompt
// was served from cache, and whether the response was cut off. Those were
// invisible until this existed, which is why every ceiling in
// examples/pr-review.yml had to be derived from terminal scrollback.
type Step struct {
	// Step is the agent name the step ran under.
	Step string
	// Prompt/Completion/Total are the provider's reported counts. Total is
	// what a budget is measured against; a provider that reports no usage at
	// all contributes zeros rather than an estimate, since a budget must never
	// trip on a number nobody reported.
	Prompt     int
	Completion int
	Total      int
	// Cached is the part of Prompt the provider served from its cache.
	//
	// This is the number that says whether prompt caching (see internal/agent/llmclient/openrouter.go)
	// is doing anything. Without it the feature is faith-based: the requests
	// carry the headers either way, and nothing reports whether they landed.
	Cached int
	// Reasoning is tokens a reasoning model spent thinking. Some providers
	// fold these into Completion and some do not, so a total that does not
	// reconcile with prompt+completion is usually this.
	Reasoning int
	// ModelServed is the model the provider says answered, which is not always
	// the one requested: openrouter/auto and router models resolve at request
	// time, and providers substitute. Empty when nothing reported one.
	ModelServed string
	// CostUSD is what the run cost, when a provider path reports one at all.
	// Only a CLI subprocess does: it meters itself and prints the figure in
	// its terminal event, while every HTTP path reports tokens and leaves
	// pricing to whoever knows the rate card. Zero means unreported, never
	// free — which is why the page shows nothing rather than $0.00.
	CostUSD float64
	// FinishReason is why the last response ended. "length" (or whatever the
	// provider spells it) means the answer was TRUNCATED by max_tokens, which
	// is otherwise indistinguishable from a model that simply said little —
	// and a truncated JSON or verdict is a failure that reads as a bad answer.
	FinishReason string
	// Raw is the provider's usage block for the LAST response of the
	// conversation, as JSON — the fields nobody has asked for yet. The store
	// has no migration, so a field not captured today cannot be backfilled
	// tomorrow; keeping the block whole costs a column and buys
	// every future question about spend.
	//
	// One response, deliberately, while every count beside it is the whole
	// conversation's. Accumulating a dozen JSON blocks per step would store
	// the transcript's weight again for a shape nothing reads yet. Do NOT
	// reconcile this against Total — they answer different questions, and a
	// 12-turn step will show them disagreeing by two orders of magnitude.
	Raw string
}

// Run accumulates spend across every agent step of one job, in execution
// order, and enforces the job-level ceiling.
//
// A job breach is cumulative, so naming only the step that crossed the line
// would be misleading — the step that trips it is rarely the one that cost the
// most. Steps keeps the per-step breakdown for exactly that reason.
type Run struct {
	mu    sync.Mutex
	steps []Step
	// prior is what EARLIER attempts of this same run already spent, read
	// back from agent_usage when resuming (see NewResumedRun). It counts
	// against the ceiling but is not a step of this invocation, so it is held
	// apart from steps rather than faked in as one.
	prior  int
	budget int
	// parent is the accumulator this one forwards its steps to, nil for the
	// job-level accumulator itself. A SCOPED accumulator (see
	// NewScopedRun) exists so a nested piece of work — an across: block —
	// can measure what ITS OWN steps spent, while every one of them still
	// counts toward the job total exactly once.
	parent *Run
}

// NewRun returns an accumulator with an optional job-level token ceiling
// (0 for none).
func NewRun(budgetTokens int) *Run {
	return &Run{budget: budgetTokens}
}

// NewResumedRun is NewRun for a run continuing a previous attempt:
// prior is what that attempt already spent.
//
// Without it a budget is a per-ATTEMPT ceiling wearing the name of a per-run
// one. A job stopped at 7M of an 8M allowance resumed with a fresh 8M, so
// each resume bought another full budget — and the runs most likely to need
// resuming are the expensive ones, which is where it costs most.
func NewResumedRun(budgetTokens, prior int) *Run {
	return &Run{budget: budgetTokens, prior: prior}
}

// NewScopedRun returns an accumulator that measures only the steps
// recorded through IT, while forwarding each of them to parent.
//
// It exists because a nested budget cannot be a delta on the job total. An
// across: block used to measure its spend as "how much the job's number moved
// while I ran", which silently charged it for every agent step running
// CONCURRENTLY elsewhere in the plan — an in_parallel: sibling's tokens would
// exhaust a matrix's allowance and stop it admitting cells that had spent
// nothing. A scoped accumulator sees its own cells and nothing else.
//
// The job ceiling still governs: Add and WouldExceed both consult the parent
// chain, so a scoped child can never be a way around it.
func NewScopedRun(parent *Run) *Run {
	return &Run{parent: parent}
}

// Prior is what earlier attempts of this run spent, 0 for a fresh run.
func (u *Run) Prior() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.prior
}

// Add records one step's spend and reports whether the job's ceiling is now
// exceeded.
func (u *Run) Add(step Step) (exceeded bool) {
	u.mu.Lock()
	u.steps = append(u.steps, step)
	exceeded = u.budget > 0 && u.total() > u.budget
	parent := u.parent
	u.mu.Unlock()

	// Forwarded OUTSIDE the lock: the parent takes its own, and holding both
	// down a chain is how a lock order gets invented by accident.
	if parent != nil && parent.Add(step) {
		exceeded = true
	}

	return exceeded
}

// Steps is the per-step breakdown, in execution order.
func (u *Run) Steps() []Step {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]Step(nil), u.steps...)
}

// Total is the tokens spent across every agent step so far.
func (u *Run) Total() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.total()
}

// WouldExceed reports whether the job's ceiling is breached once pending
// tokens (an in-flight step's spend, not yet added) are counted. Checking a
// step's running spend against it is what lets a job budget stop work mid-step
// rather than after paying for the overrun.
func (u *Run) WouldExceed(pending int) bool {
	u.mu.Lock()
	own := u.budget > 0 && u.total()+pending > u.budget
	parent := u.parent
	u.mu.Unlock()

	if own {
		return true
	}

	// A scoped accumulator usually has no ceiling of its own; the job's still
	// has to stop work mid-step, so the question passes up the chain.
	return parent != nil && parent.WouldExceed(pending)
}

// RunningTotals renders the per-step breakdown a job breach must report,
// with the in-flight step last: "planner 120338 -> coder 79662 (tripped here)".
// A job breach is cumulative, so naming only the step that crossed the line
// tells the reader almost nothing about where the money went.
func (u *Run) RunningTotals(pending Step) string {
	u.mu.Lock()
	defer u.mu.Unlock()

	parts := make([]string, 0, len(u.steps)+1)
	for _, step := range u.steps {
		parts = append(parts, step.Step+" "+strconv.Itoa(step.Total))
	}

	parts = append(parts, pending.Step+" "+strconv.Itoa(pending.Total)+" (tripped here)")

	return strings.Join(parts, " -> ")
}

// Budget is the job's ceiling, or 0 when it has none.
func (u *Run) Budget() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.budget
}

// total is this run's cumulative spend: what earlier attempts spent plus what
// this invocation has. Every ceiling check reads through here, so a resumed
// run's budget picks up where the last attempt left it rather than restarting.
func (u *Run) total() int {
	sum := u.prior
	for _, step := range u.steps {
		sum += step.Total
	}

	return sum
}

type runKey struct{}

// With scopes an accumulator to one job run. internal/pipeline
// installs it; every agent step in the job adds to it.
func With(ctx context.Context, usage *Run) context.Context {
	return context.WithValue(ctx, runKey{}, usage)
}

// From reads back the accumulator, or nil outside a job run.
func From(ctx context.Context) *Run {
	usage, _ := ctx.Value(runKey{}).(*Run)

	return usage
}
