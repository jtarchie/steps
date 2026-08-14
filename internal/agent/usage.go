package agent

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

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// StepUsage is one agent step's spend, plus the provider metadata that
// explains it.
//
// Tokens are what a budget measures. The rest is what makes a number
// actionable afterwards: which model actually answered, how much of the prompt
// was served from cache, and whether the response was cut off. Those were
// invisible until this existed, which is why every ceiling in
// examples/pr-review.yml had to be derived from terminal scrollback.
type StepUsage struct {
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
	// This is the number that says whether prompt caching (see openrouter.go)
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
	// FinishReason is why the last response ended. "length" (or whatever the
	// provider spells it) means the answer was TRUNCATED by max_tokens, which
	// is otherwise indistinguishable from a model that simply said little —
	// and a truncated JSON or verdict is a failure that reads as a bad answer.
	FinishReason string
	// Raw is the provider's usage block for the LAST response of the
	// conversation, as JSON — the fields nobody has asked for yet. The store
	// has no schema versioning, so a field not captured today cannot be
	// backfilled tomorrow; keeping the block whole costs a column and buys
	// every future question about spend.
	//
	// One response, deliberately, while every count beside it is the whole
	// conversation's. Accumulating a dozen JSON blocks per step would store
	// the transcript's weight again for a shape nothing reads yet. Do NOT
	// reconcile this against Total — they answer different questions, and a
	// 12-turn step will show them disagreeing by two orders of magnitude.
	Raw string
}

// RunUsage accumulates spend across every agent step of one job, in execution
// order, and enforces the job-level ceiling.
//
// A job breach is cumulative, so naming only the step that crossed the line
// would be misleading — the step that trips it is rarely the one that cost the
// most. Steps keeps the per-step breakdown for exactly that reason.
type RunUsage struct {
	mu    sync.Mutex
	steps []StepUsage
	// prior is what EARLIER attempts of this same run already spent, read
	// back from agent_usage when resuming (see NewResumedRunUsage). It counts
	// against the ceiling but is not a step of this invocation, so it is held
	// apart from steps rather than faked in as one.
	prior  int
	budget int
}

// NewRunUsage returns an accumulator with an optional job-level token ceiling
// (0 for none).
func NewRunUsage(budgetTokens int) *RunUsage {
	return &RunUsage{budget: budgetTokens}
}

// NewResumedRunUsage is NewRunUsage for a run continuing a previous attempt:
// prior is what that attempt already spent.
//
// Without it a budget is a per-ATTEMPT ceiling wearing the name of a per-run
// one. A job stopped at 7M of an 8M allowance resumed with a fresh 8M, so
// each resume bought another full budget — and the runs most likely to need
// resuming are the expensive ones, which is where it costs most.
func NewResumedRunUsage(budgetTokens, prior int) *RunUsage {
	return &RunUsage{budget: budgetTokens, prior: prior}
}

// Prior is what earlier attempts of this run spent, 0 for a fresh run.
func (u *RunUsage) Prior() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.prior
}

// Add records one step's spend and reports whether the job's ceiling is now
// exceeded.
func (u *RunUsage) Add(step StepUsage) (exceeded bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	u.steps = append(u.steps, step)

	return u.budget > 0 && u.total() > u.budget
}

// Steps is the per-step breakdown, in execution order.
func (u *RunUsage) Steps() []StepUsage {
	u.mu.Lock()
	defer u.mu.Unlock()

	return append([]StepUsage(nil), u.steps...)
}

// Total is the tokens spent across every agent step so far.
func (u *RunUsage) Total() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.total()
}

// wouldExceed reports whether the job's ceiling is breached once pending
// tokens (an in-flight step's spend, not yet added) are counted. Checking a
// step's running spend against it is what lets a job budget stop work mid-step
// rather than after paying for the overrun.
func (u *RunUsage) wouldExceed(pending int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.budget > 0 && u.total()+pending > u.budget
}

// runningTotals renders the per-step breakdown a job breach must report,
// with the in-flight step last: "planner 120338 -> coder 79662 (tripped here)".
// A job breach is cumulative, so naming only the step that crossed the line
// tells the reader almost nothing about where the money went.
func (u *RunUsage) runningTotals(pending StepUsage) string {
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
func (u *RunUsage) Budget() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return u.budget
}

// total is this run's cumulative spend: what earlier attempts spent plus what
// this invocation has. Every ceiling check reads through here, so a resumed
// run's budget picks up where the last attempt left it rather than restarting.
func (u *RunUsage) total() int {
	sum := u.prior
	for _, step := range u.steps {
		sum += step.Total
	}

	return sum
}

type runUsageKey struct{}

// WithRunUsage scopes an accumulator to one job run. internal/pipeline
// installs it; every agent step in the job adds to it.
func WithRunUsage(ctx context.Context, usage *RunUsage) context.Context {
	return context.WithValue(ctx, runUsageKey{}, usage)
}

// RunUsageFrom reads back the accumulator, or nil outside a job run.
func RunUsageFrom(ctx context.Context) *RunUsage {
	usage, _ := ctx.Value(runUsageKey{}).(*RunUsage)

	return usage
}

// stepUsage tallies one conversation's provider-reported tokens and enforces
// the agent's own per-invocation ceiling.
type stepUsage struct {
	mu     sync.Mutex
	name   string
	budget int
	// run is the job-level accumulator this step rolls up into, nil outside a
	// job run. Held so a job ceiling can stop work mid-step instead of after
	// the step has already spent past it.
	run          *RunUsage
	prompt       int
	completion   int
	total        int
	cached       int
	reasoning    int
	served       string
	finishReason string
	raw          string
}

// record folds one response's usage in and reports whether this invocation's
// own ceiling is now exceeded.
//
// A response carrying no usage metadata contributes nothing. Providers do omit
// it, and inventing an estimate here would make a budget trip on a number that
// was never reported — the one thing an accounting figure must not do.
func (s *stepUsage) record(resp *model.LLMResponse) (exceeded bool) {
	if resp == nil || resp.UsageMetadata == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.prompt += int(resp.UsageMetadata.PromptTokenCount)
	s.completion += int(resp.UsageMetadata.CandidatesTokenCount)
	s.total += int(resp.UsageMetadata.TotalTokenCount)
	s.cached += int(resp.UsageMetadata.CachedContentTokenCount)
	s.reasoning += int(resp.UsageMetadata.ThoughtsTokenCount)

	// Last response wins for these two rather than accumulating: they describe
	// the response that just arrived, and it is the LAST one that says how the
	// conversation actually ended. A "length" from turn 3 that a later turn
	// recovered from is not what the step finished on.
	if resp.ModelVersion != "" {
		s.served = resp.ModelVersion
	}

	if resp.FinishReason != "" {
		s.finishReason = string(resp.FinishReason)
	}

	// Overwritten rather than appended: this keeps the LAST response's block.
	// See StepUsage.Raw for why that is deliberate and why it must not be read
	// as the conversation's total.
	encoded, err := json.Marshal(resp.UsageMetadata)
	if err == nil {
		s.raw = string(encoded)
	}

	if s.budget > 0 && s.total > s.budget {
		return true
	}

	return s.run != nil && s.run.wouldExceed(s.total)
}

// addTokens folds in a spend reported all at once rather than response by
// response — what a CLI-backed step gets, since its conversation happened
// inside a subprocess and only its total comes back (see cli.go).
//
// Unlike record it reports no breach: by the time these numbers exist the
// process has already exited, so there is nothing left to stop. The job-level
// total still counts them, which is the point — a job budget: must see what a
// CLI agent spent even though a per-agent budget: cannot cap it.
func (s *stepUsage) addTokens(prompt, completion int) {
	if prompt == 0 && completion == 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.prompt += prompt
	s.completion += completion
	s.total += prompt + completion
}

// exceededError describes whichever ceiling this step just breached — its own
// or the job's — with the job case carrying the running per-step total.
//
// Only called after record() reports exceeded=true, which is true exactly
// when one of the two branches below applies — so this never legitimately
// falls through. It still never returns nil: record()'s two conditions and
// this function's are meant to mirror each other exactly, but they're
// checked in two places, and generateWithinBudget's caller trusts a non-nil
// error here to know resp is nil. A future edit to one side without the
// other should surface as a confusing error message, not a nil-pointer
// panic three calls away.
func (s *stepUsage) exceededError() error {
	spent := s.snapshot()

	if s.budget > 0 && spent.Total > s.budget {
		return budgetExceededError("agent "+strconv.Quote(spent.Step), spent.Total, s.budget)
	}

	if s.run != nil {
		return fmt.Errorf("job budget exceeded: cap %d tokens, spent %d\n  running total: %s",
			s.run.Budget(), s.run.Total()+spent.Total, s.run.runningTotals(spent))
	}

	return fmt.Errorf("agent budget exceeded (spent %d tokens), but neither an agent nor a job ceiling explains why", spent.Total)
}

// finish reports the step's spend and rolls it into the job total. Called once
// per conversation, however it ended: a step that failed still spent what it
// spent, and leaving that out of the job total would under-report exactly the
// runs worth investigating.
func (s *stepUsage) finish() {
	spent := s.snapshot()

	logStepUsage(spent, s.budget)

	if s.run != nil {
		_ = s.run.Add(spent)
	}
}

// snapshot is what the step contributes to the job's total.
func (s *stepUsage) snapshot() StepUsage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return StepUsage{
		Step: s.name, Prompt: s.prompt, Completion: s.completion, Total: s.total,
		Cached: s.cached, Reasoning: s.reasoning,
		ModelServed: s.served, FinishReason: s.finishReason, Raw: s.raw,
	}
}

// budgetExceededError is the failure a breached ceiling produces.
//
// It is deliberately NOT marked with outcome.Fail: an unmarked error
// classifies as errored, which is the right bucket. A budget breach is an
// operational limit being hit, not a model producing a bad answer — so
// on_error fires rather than on_failure, and no to: route can treat it as a
// decision the model made. Timeouts are classified the same way for the same
// reason.
func budgetExceededError(label string, spent, budget int) error {
	return fmt.Errorf("%s: budget exceeded: cap %d tokens, spent %d", label, budget, spent)
}

// logStepUsage reports what one agent step spent. It runs whether or not a
// ceiling is configured — being able to see the number is the half of this
// that carries no risk and tells you which ceilings are worth setting.
func logStepUsage(usage StepUsage, budget int) {
	if usage.Total == 0 {
		// The provider reported nothing. Say so at debug rather than logging
		// a confident zero, which reads as "this step was free".
		slog.Debug("agent.usage", "agent", usage.Step, "reported", false)

		return
	}

	fields := []any{
		"agent", usage.Step,
		"prompt_tokens", usage.Prompt,
		"completion_tokens", usage.Completion,
		"total_tokens", usage.Total,
	}

	if budget > 0 {
		fields = append(fields, "budget_tokens", budget)
	}

	slog.Info("agent.usage", fields...)
}

// saveUsageArgs is what a recorded agent step's spend needs beyond the tokens
// themselves: which step it was, in which run, and how long it took.
type saveUsageArgs struct {
	jobName        string
	stepIndex      int
	stepName       string
	nodeHash       string
	modelRequested string
	usage          StepUsage
	duration       time.Duration
}

// saveAgentUsage persists one agent step's spend.
//
// Best-effort, like saveAgentTranscript: a bookkeeping write must never turn a
// step that did its work into a failed one. A dropped row costs a line in a
// cost report; a failed step costs the run.
//
// Only real agent STEPS reach here. Sub-agents and fix agents record no node,
// no job_run and no transcript, and their spend already rolls into the parent
// step's total through the shared accumulator — giving them rows of their own
// would double-count every job report.
func saveAgentUsage(ctx context.Context, st *store.Store, args saveUsageArgs) {
	if st == nil {
		return
	}

	runID := events.RunID(ctx)
	if runID == "" {
		return
	}

	// A provider that reported nothing at all gets a row of zeros rather than
	// no row: "we ran this step and learned nothing about what it cost" and
	// "this step never ran" are different facts, and a missing row reads as
	// the second one.
	err := st.RecordAgentUsage(context.WithoutCancel(ctx), store.AgentUsage{
		RunID:        runID,
		StepIndex:    args.stepIndex,
		StepName:     args.stepName,
		JobName:      args.jobName,
		NodeHash:     args.nodeHash,
		ModelReq:     args.modelRequested,
		ModelServed:  args.usage.ModelServed,
		Prompt:       args.usage.Prompt,
		Completion:   args.usage.Completion,
		Total:        args.usage.Total,
		Cached:       args.usage.Cached,
		Reasoning:    args.usage.Reasoning,
		FinishReason: args.usage.FinishReason,
		DurationMS:   args.duration.Milliseconds(),
		RawMeta:      args.usage.Raw,
	})
	if err != nil {
		slog.Warn("agent.usage_unrecorded", "job", args.jobName, "step", args.stepName, "error", err)
	}
}
