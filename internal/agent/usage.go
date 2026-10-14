package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/usage"
)

// stepUsage tallies one conversation's provider-reported tokens and enforces
// the agent's own per-invocation ceiling.
type stepUsage struct {
	mu     sync.Mutex
	name   string
	budget int
	// run is the job-level accumulator this step rolls up into, nil outside a
	// job run. Held so a job ceiling can stop work mid-step instead of after
	// the step has already spent past it.
	run *usage.Run
	// parent is the invocation that DELEGATED to this one, nil for a
	// top-level step. A sub-agent spends its parent's allowance rather than
	// adding to it (see delegatedBudget), so its spend is charged back up the
	// chain when it finishes.
	parent *stepUsage
	// delegateFraction is how much of THIS invocation's remaining allowance
	// one of its sub-agent calls may take (config.DelegateBudgetFraction).
	// Zero outside a configured run, which leaves a child on its own budget.
	delegateFraction float64
	// delegated is what sub-agent invocations started by this step have
	// spent. It counts against this step's own ceiling — a delegation draws
	// on the parent's allowance — but is NOT rolled into the run total here,
	// because each sub-agent's own stepUsage already does that exactly once.
	delegated int
	// last is the most recent response's total, the best estimate of what the
	// next one costs: a conversation re-sends its whole context every turn.
	last         int
	prompt       int
	completion   int
	total        int
	cached       int
	reasoning    int
	costUSD      float64
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

	s.last = int(resp.UsageMetadata.TotalTokenCount)

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
	// See usage.Step.Raw for why that is deliberate and why it must not be read
	// as the conversation's total.
	encoded, err := json.Marshal(resp.UsageMetadata)
	if err == nil {
		s.raw = string(encoded)
	}

	return s.countLocked(resp.UsageMetadata)
}

// recordSummary folds in a compaction summary's usage: counted and checked
// against both ceilings like any response, but leaving the last-response
// fields alone. Those describe the conversation's own latest turn — a
// summary's total is roughly the whole old history, so as last it would fire
// a spurious wrap-up nudge, and its finish reason is not how the step ended.
func (s *stepUsage) recordSummary(resp *model.LLMResponse) (exceeded bool) {
	if s == nil || resp == nil || resp.UsageMetadata == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.countLocked(resp.UsageMetadata)
}

// countLocked adds one response's counts and reports whether a ceiling is
// now exceeded. The caller holds s.mu.
func (s *stepUsage) countLocked(meta *genai.GenerateContentResponseUsageMetadata) bool {
	s.prompt += int(meta.PromptTokenCount)
	s.completion += int(meta.CandidatesTokenCount)
	s.total += int(meta.TotalTokenCount)
	s.cached += int(meta.CachedContentTokenCount)
	s.reasoning += int(meta.ThoughtsTokenCount)

	// delegated counts here: a step that handed most of its allowance to
	// sub-agents has that much less for itself, or the ceiling would bound
	// the parent's own turns while the subtree spent freely beside it.
	if s.budget > 0 && s.total+s.delegated > s.budget {
		return true
	}

	return s.run != nil && s.run.WouldExceed(s.total)
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

// reportedCost is a dollar figure a provider actually reported, or nil.
//
// A pointer because the column has to tell "nobody priced this" apart from
// "this was free", and every HTTP path reports no price at all.
func reportedCost(cost float64) *float64 {
	if cost <= 0 {
		return nil
	}

	return &cost
}

// addCLIReport folds in the figures only a CLI subprocess reports about
// itself: how much of its prompt was served from cache, what it cost, and how
// it stopped.
//
// Separate from record() because that one reads a *model.LLMResponse, which
// this path never has — a CLI reports its own accounting in the terminal event
// of its stream (see clistream.go) and never hands over a provider response.
// Without it a CLI step's spend row said 0% cached on a conversation that was
// almost entirely cache reads, and priced a billable run at nothing.
func (s *stepUsage) addCLIReport(cached int, costUSD float64, finishReason string) {
	if cached == 0 && costUSD == 0 && finishReason == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cached += cached

	if costUSD > 0 {
		s.costUSD += costUSD
	}

	if finishReason != "" {
		s.finishReason = finishReason
	}
}

// hasCeiling reports whether this invocation declares a token budget at all.
//
// The distinction matters more than it looks: 0 means UNBOUNDED everywhere in
// this file, so a caller that reads "no allowance" off a zero cannot tell an
// agent with no budget from one that has spent all of it. Conflating the two
// refused every delegation on every pipeline that set no budgets.
func (s *stepUsage) hasCeiling() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.budget > 0
}

// remaining is what this invocation may still spend under its OWN ceiling,
// counting what its delegations have already taken. math.MaxInt when it
// declares no ceiling — "no budget" is unbounded, not zero.
func (s *stepUsage) remaining() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.budget <= 0 {
		return math.MaxInt
	}

	return max(s.budget-(s.total+s.delegated), 0)
}

// jobExceeded reports whether the JOB's ceiling, not this step's own, is what
// the spend so far breaches.
func (s *stepUsage) jobExceeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.run != nil && s.run.WouldExceed(s.total)
}

// budgetWarningFraction mirrors timeoutWarningFraction: warn once a fifth of
// the allowance is left.
const budgetWarningFraction = 5

// budgetWarningDue reports whether this invocation's own ceiling is close
// enough that the model should be told: a fifth of it left, or less than two
// more turns the size of the last one. The second arm is what binds in
// practice — turns grow with the context, so one late turn can cost more than
// the whole fifth, and the warning itself rides on the turn after it.
//
// ponytail: the job ceiling (s.run) is not consulted; walk usage.Run's parent chain like WouldExceed if a job budget should warn too.
func (s *stepUsage) budgetWarningDue() (spent, budget int, due bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.budget <= 0 {
		return 0, 0, false
	}

	spent = s.total + s.delegated
	left := s.budget - spent

	return spent, s.budget, left <= s.budget/budgetWarningFraction || left < 2*s.last
}

// chargeDelegated books a finished sub-agent's spend against this step and
// every step above it.
//
// Walking the whole chain is what makes the guarantee transitive: a
// grandchild's tokens have to shrink the grandparent's allowance too, or a
// deep enough delegation tree escapes the ceiling one level at a time.
func (s *stepUsage) chargeDelegated(spent int) {
	for step := s; step != nil; step = step.parentOf() {
		step.mu.Lock()
		step.delegated += spent
		step.mu.Unlock()
	}
}

// parentOf reads the delegating invocation under the lock.
func (s *stepUsage) parentOf() *stepUsage {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.parent
}

// delegatedBudget is the ceiling a sub-agent of this invocation gets: a
// fraction of what the parent has LEFT, capped by whatever the sub-agent
// declared for itself. The tighter of the two wins, so neither an inherited
// allowance nor a declared one can be exceeded.
//
// A fraction of what remains rather than of the original allowance, so
// delegation cannot drain a parent outright: each call takes a tenth of a
// shrinking number, leaving the parent something to finish its own work with
// however many helpers it asks for.
//
// A parent with no ceiling of its own has nothing to take a fraction OF, so
// its children fall back to their own declared budgets — exactly the
// behaviour before delegation drew on the parent at all.
func (s *stepUsage) delegatedBudget(own int) int {
	left := s.remaining()

	s.mu.Lock()
	fraction := s.delegateFraction
	s.mu.Unlock()

	if left == math.MaxInt || fraction <= 0 {
		return own
	}

	share := int(float64(left) * fraction)
	if own > 0 && own < share {
		return own
	}

	return share
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

	s.mu.Lock()
	delegated := s.delegated
	s.mu.Unlock()

	// Tested the same way record() trips it, delegated included. Without that
	// a breach CAUSED by delegation fell through to the job branch and
	// reported "job budget exceeded: cap 0 tokens" — a ceiling that does not
	// exist — for an agent that had spent its own allowance on helpers.
	if s.budget > 0 && spent.Total+delegated > s.budget {
		return budgetExceededError(agentBudgetLabel(spent.Step, delegated), spent.Total+delegated, s.budget)
	}

	if s.run != nil {
		return fmt.Errorf("job budget exceeded: cap %d tokens, spent %d\n  running total: %s",
			s.run.Budget(), s.run.Total()+spent.Total, s.run.RunningTotals(spent))
	}

	return fmt.Errorf("agent budget exceeded (spent %d tokens), but neither an agent nor a job ceiling explains why", spent.Total+delegated)
}

// agentBudgetLabel names the breaching agent, saying so when its sub-agents
// are what spent the allowance — otherwise the number in the message looks
// unrelated to anything the agent itself did.
func agentBudgetLabel(step string, delegated int) string {
	label := "agent " + strconv.Quote(step)
	if delegated > 0 {
		label += " (including " + strconv.Itoa(delegated) + " tokens spent by its sub-agents)"
	}

	return label
}

// finish reports the step's spend and rolls it into the job total. Called once
// per conversation, however it ended: a step that failed still spent what it
// spent, and leaving that out of the job total would under-report exactly the
// runs worth investigating.
func (s *stepUsage) finish(ctx context.Context) {
	spent := s.snapshot()

	logStepUsage(ctx, spent, s.budget)

	if s.run != nil {
		_ = s.run.Add(spent)
	}

	// A delegation spends its parent's allowance. Charged once, here, rather than per response: a hosted turn executes its sub-agent calls one at a time, so the parent's remaining is accurate by the time it decides whether to delegate again. A CLI parent's calls arrive on the bridge's own goroutines and CAN overlap, where each in-flight delegation sizes itself against a remaining the others have not been debited from yet — the job ceiling still bounds them (record consults it live), the agent's own is soft under a concurrent fan-out. The run total is untouched — s.run.Add above already counted these tokens exactly once.
	if parent := s.parentOf(); parent != nil {
		parent.chargeDelegated(spent.Total)
	}
}

// persistedSnapshot is what the step's agent_usage ROW records: its own spend
// plus everything its sub-agents spent.
//
// Deliberately not snapshot(). That one feeds the in-memory usage.Run, which
// each sub-agent already adds to under its own name, so folding delegated in
// there would count the subtree twice. Nothing persists a row for a
// sub-agent, though, so without this the durable record — what `steps cost`
// reports, and what a resumed run reads its prior spend from — omitted every
// delegated token. A run that delegates heavily restored most of its
// allowance on resume.
//
// It also makes the reported number the more useful one: what this step cost,
// helpers included.
func (s *stepUsage) persistedSnapshot() usage.Step {
	spent := s.snapshot()

	s.mu.Lock()
	defer s.mu.Unlock()

	spent.Total += s.delegated

	return spent
}

// snapshot is what the step contributes to the job's total.
func (s *stepUsage) snapshot() usage.Step {
	s.mu.Lock()
	defer s.mu.Unlock()

	return usage.Step{
		Step: s.name, Prompt: s.prompt, Completion: s.completion, Total: s.total,
		Cached: s.cached, Reasoning: s.reasoning, CostUSD: s.costUSD,
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
func logStepUsage(ctx context.Context, spent usage.Step, budget int) {
	if spent.Total == 0 {
		// The provider reported nothing. Say so at debug rather than logging
		// a confident zero, which reads as "this step was free".
		slog.DebugContext(ctx, "agent.usage", "agent", spent.Step, "reported", false)

		return
	}

	fields := []any{
		"agent", spent.Step,
		"prompt_tokens", spent.Prompt,
		"completion_tokens", spent.Completion,
		"total_tokens", spent.Total,
	}

	if budget > 0 {
		fields = append(fields, "budget_tokens", budget)
	}

	slog.InfoContext(ctx, "agent.usage", fields...)
}

// saveUsageArgs is what a recorded agent step's spend needs beyond the tokens
// themselves: which step it was, in which run, and how long it took.
type saveUsageArgs struct {
	jobName        string
	stepIndex      int
	stepName       string
	nodeHash       string
	modelRequested string
	usage          usage.Step
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
func saveAgentUsage(ctx context.Context, st store.Usage, args saveUsageArgs) {
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
		CostUSD:      reportedCost(args.usage.CostUSD),
		FinishReason: args.usage.FinishReason,
		DurationMS:   args.duration.Milliseconds(),
		RawMeta:      args.usage.Raw,
	})
	if err != nil {
		slog.WarnContext(ctx, "agent.usage_unrecorded", "job", args.jobName, "step", args.stepName, "error", err)
	}
}
