package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/usage"
)

func TestStepUsageCeilingIsReachableNotExceeded(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{name: "writer", budget: 100}

	if usage.record(response(60, 40)) {
		t.Error("spending exactly the 100-token agent budget exceeded it")
	}
}

func TestAddTokensKeepsAOneSidedReport(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{}
	usage.addTokens(5, 0)
	usage.addTokens(0, 7)

	if got := usage.snapshot(); got.Prompt != 5 || got.Completion != 7 || got.Total != 12 {
		t.Errorf("snapshot = %+v, want prompt 5 / completion 7 / total 12", got)
	}
}

func TestAddCLIReportKeepsEachFigureAlone(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{}

	usage.addCLIReport(0, 0, "end_turn")
	usage.addCLIReport(3, 0, "")
	usage.addCLIReport(0, 0.5, "")

	got := usage.snapshot()
	if got.Cached != 3 || !closeEnough(got.CostUSD, 0.5) || got.FinishReason != "end_turn" {
		t.Errorf("snapshot = %+v, want cached 3 / cost 0.5 / finish end_turn kept across reports that lack one", got)
	}
}

func TestBudgetWarningDueWhenExactlyTwoTurnsRemain(t *testing.T) {
	t.Parallel()

	usage := &stepUsage{budget: 900}
	usage.record(response(300, 0))

	if _, _, due := usage.budgetWarningDue(); due {
		t.Error("warned at 600 left when exactly two more 300-token turns still fit")
	}
}

func TestDelegatedBudgetWithNoFractionIsTheChildsOwn(t *testing.T) {
	t.Parallel()

	parent := &stepUsage{budget: 1000}

	if got := parent.delegatedBudget(70); got != 70 {
		t.Errorf("delegatedBudget = %d, want the child's own 70: no fraction configured means no share to take", got)
	}
}

func TestExceededErrorNamesTheCeilingThatWasCrossed(t *testing.T) {
	t.Parallel()

	atItsOwn := &stepUsage{name: "writer", budget: 100, run: usage.NewRun(50)}
	atItsOwn.record(response(100, 0))

	err := atItsOwn.exceededError().Error()
	if !strings.Contains(err, "job budget exceeded: cap 50 tokens, spent 100") {
		t.Errorf("an agent at exactly its own budget blamed it rather than the job: %s", err)
	}

	job := usage.NewRun(150)
	job.Add(usage.Step{Step: "planner", Total: 120})

	inJob := &stepUsage{name: "coder", run: job}
	inJob.record(response(40, 0))

	if err := inJob.exceededError().Error(); !strings.Contains(err, "cap 150 tokens, spent 160") {
		t.Errorf("job breach = %s, want the job total including this step (160)", err)
	}

	delegating := &stepUsage{name: "lead", budget: 100}
	delegating.record(response(60, 0))
	delegating.chargeDelegated(50)

	err = delegating.exceededError().Error()
	if !strings.Contains(err, `agent "lead" (including 50 tokens spent by its sub-agents): budget exceeded: cap 100 tokens, spent 110`) {
		t.Errorf("delegated breach = %s", err)
	}

	if label := agentBudgetLabel("solo", 0); label != `agent "solo"` {
		t.Errorf("label with nothing delegated = %q", label)
	}
}

// Not t.Parallel(): swaps the default logger.
func TestLogStepUsageNamesTheBudgetOnlyWhenThereIsOne(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logStepUsage(t.Context(), usage.Step{Step: "a", Total: 3}, 0)

	if strings.Contains(buf.String(), "budget_tokens") {
		t.Errorf("an unbudgeted step logged a budget: %s", buf.String())
	}

	buf.Reset()
	logStepUsage(t.Context(), usage.Step{Step: "a", Total: 3}, 100)

	if !strings.Contains(buf.String(), "budget_tokens=100") {
		t.Errorf("a budgeted step did not log its budget: %s", buf.String())
	}
}
