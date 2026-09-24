package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// response builds a scripted LLM response carrying the provider's own usage
// accounting.
func response(prompt, completion int32) *model.LLMResponse {
	return &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     prompt,
		CandidatesTokenCount: completion,
		TotalTokenCount:      prompt + completion,
	}}
}

// TestStepUsageEnforcesItsOwnCeiling covers the per-invocation budget.
func TestStepUsageEnforcesItsOwnCeiling(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{name: "writer", budget: 100}

	if usage.record(response(30, 10)) {
		t.Fatal("40 of a 100-token budget tripped the ceiling")
	}

	if !usage.record(response(50, 20)) {
		t.Fatal("110 of a 100-token budget did not trip the ceiling")
	}

	spent := usage.snapshot()
	if spent.Total != 110 || spent.Prompt != 80 || spent.Completion != 30 {
		t.Errorf("snapshot = %+v, want prompt 80 / completion 30 / total 110", spent)
	}

	err := usage.exceededError()
	if err == nil || !strings.Contains(err.Error(), `agent "writer"`) {
		t.Errorf("exceededError = %v, want it to name the agent", err)
	}
}

// TestBudgetWarningDue pins both arms separately (steps#158): the fifth-left
// floor for a conversation of small turns, and the two-turn look-ahead for one
// whose last turn alone would eat most of what remains.
func TestBudgetWarningDue(t *testing.T) {
	t.Parallel()

	due := func(u *stepUsage) bool {
		_, _, d := u.budgetWarningDue()

		return d
	}

	uncapped := &stepUsage{}
	uncapped.record(response(1_000_000, 0))

	if due(uncapped) {
		t.Error("warned with no budget to run out of")
	}

	small := &stepUsage{budget: 1000}
	for range 7 {
		small.record(response(100, 0))
	}

	if due(small) {
		t.Error("warned at 300 left with 100-token turns")
	}

	small.record(response(100, 0))

	if !due(small) {
		t.Error("no warning at a fifth left (200 of 1000)")
	}

	fits := &stepUsage{budget: 1000}
	fits.record(response(330, 0))

	if due(fits) {
		t.Error("warned at 670 left when two more 330-token turns still fit")
	}

	big := &stepUsage{budget: 1000}
	big.record(response(350, 0))

	if !due(big) {
		t.Error("no warning at 650 left when two more 350-token turns do not fit")
	}

	delegating := &stepUsage{budget: 1000}
	delegating.record(response(10, 0))
	delegating.chargeDelegated(800)

	if !due(delegating) {
		t.Error("sub-agent spend did not count toward the warning")
	}
}

// TestStepUsageIgnoresUnreportedUsage is the rule that keeps a budget honest: a
// provider that reports nothing contributes nothing. Substituting an estimate
// would make a ceiling trip on a number nobody reported, which is the one
// thing an accounting figure must never do.
func TestStepUsageIgnoresUnreportedUsage(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{name: "writer", budget: 1}

	if usage.record(&model.LLMResponse{}) {
		t.Error("a response with no usage metadata tripped a 1-token budget")
	}

	if usage.record(nil) {
		t.Error("a nil response tripped a 1-token budget")
	}

	if usage.snapshot().Total != 0 {
		t.Errorf("snapshot total = %d, want 0", usage.snapshot().Total)
	}
}

// TestStepUsageWithNoCeilingStillCounts is the reporting half: usage is
// tallied whether or not anyone set a budget, because seeing the number is
// what tells you which ceilings are worth setting.
func TestStepUsageWithNoCeilingStillCounts(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{name: "writer"}

	for range 5 {
		if usage.record(response(1000, 1000)) {
			t.Fatal("a step with no budget reported a breach")
		}
	}

	if got := usage.snapshot().Total; got != 10_000 {
		t.Errorf("snapshot total = %d, want 10000", got)
	}
}

// TestRunUsageTripsMidStep verifies a job ceiling is measured against the
// running total INCLUDING the step in flight, so the job stops at the breach
// rather than after paying for the overrun.
func TestRunUsageTripsMidStep(t *testing.T) {
	t.Parallel()

	run := NewRunUsage(100)
	run.Add(StepUsage{Step: "planner", Total: 60})

	usage := &stepUsage{name: "coder", run: run}

	if usage.record(response(20, 0)) {
		t.Fatal("80 of a 100-token job budget tripped it")
	}

	if !usage.record(response(30, 0)) {
		t.Fatal("110 of a 100-token job budget did not trip it")
	}

	err := usage.exceededError()
	if err == nil {
		t.Fatal("exceededError = nil after a job breach")
	}

	// The attribution decision: a cumulative breach names every step, not just
	// the one that happened to cross the line.
	for _, want := range []string{"job budget exceeded", "planner 60", "coder 50", "tripped here"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}

// TestRunUsageWithoutABudgetNeverTrips keeps reporting free of enforcement.
func TestRunUsageWithoutABudgetNeverTrips(t *testing.T) {
	t.Parallel()

	run := NewRunUsage(0)

	if run.Add(StepUsage{Step: "planner", Total: 1_000_000}) {
		t.Error("a job with no budget reported a breach")
	}

	if run.Total() != 1_000_000 {
		t.Errorf("Total() = %d, want 1000000 — usage is counted regardless", run.Total())
	}

	if len(run.Steps()) != 1 {
		t.Errorf("Steps() = %d entries, want 1", len(run.Steps()))
	}
}

// TestRecordSummaryCountsAndTripsBothCeilings: a compaction summary is spend
// like any response, and a nil counter (a unit test's bare conversation)
// counts nothing rather than panicking.
func TestRecordSummaryCountsAndTripsBothCeilings(t *testing.T) {
	t.Parallel()

	var none *stepUsage
	if none.recordSummary(response(1, 1)) {
		t.Error("a nil counter reported a breach")
	}

	own := &stepUsage{name: "writer", budget: 100}
	if !own.recordSummary(response(90, 20)) {
		t.Error("110 of a 100-token agent budget did not trip the ceiling")
	}

	if own.last != 0 {
		t.Errorf("last = %d, want 0 — a summary is not the conversation's latest turn", own.last)
	}

	job := &stepUsage{name: "writer", run: NewRunUsage(100)}
	if !job.recordSummary(response(90, 20)) {
		t.Error("110 of a 100-token job budget did not trip the ceiling")
	}
}
