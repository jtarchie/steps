package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
)

// TestNextPendingHandoffNotRouted proves a step that didn't route (routedKey
// == "") builds no pending Handoff — the common straight-line case.
func TestNextPendingHandoffNotRouted(t *testing.T) {
	t.Parallel()

	steps := []config.Step{{Task: "a"}, {Task: "b"}}

	got := nextPendingHandoff("j", steps[0], steps, "", nonGetOutcome{}, map[int]int{}, 1)
	if got != nil {
		t.Errorf("nextPendingHandoff = %#v, want nil when routedKey is empty", got)
	}
}

// TestNextPendingHandoffFromTask proves a task/put router (never an agent)
// still builds a Handoff carrying from/key, but with a nil Previous — there
// is no agent run to report.
func TestNextPendingHandoffFromTask(t *testing.T) {
	t.Parallel()

	steps := []config.Step{{Task: "scout", To: map[string]string{"failure": "handle"}, MaxVisits: 2}, {Agent: "handle"}}

	got := nextPendingHandoff("j", steps[0], steps, "failure", nonGetOutcome{}, map[int]int{}, 1)
	if got == nil {
		t.Fatal("expected a Handoff even when the router was a task")
	}

	if got.FromStep != "scout" || got.RouteKey != "failure" {
		t.Errorf("got FromStep=%q RouteKey=%q, want scout/failure", got.FromStep, got.RouteKey)
	}

	if got.Previous != nil {
		t.Errorf("Previous = %#v, want nil (the routing step was not an agent)", got.Previous)
	}

	if got.MaxVisits != 0 {
		t.Errorf("MaxVisits = %d, want 0 (target step declares none)", got.MaxVisits)
	}
}

// TestNextPendingHandoffFromAgentCarriesPrevious proves an agent router's
// note and PreviousRun are threaded through into the built Handoff.
func TestNextPendingHandoffFromAgentCarriesPrevious(t *testing.T) {
	t.Parallel()

	steps := []config.Step{
		{Agent: "critic", Verdicts: []string{"approve", "revise"}, To: map[string]string{"approve": "publish", "revise": "writer"}, MaxVisits: 3},
		{Agent: "writer"},
		{Agent: "publish"},
	}

	prev := &agent.PreviousRun{Agent: "critic", Response: "not quite", Verdict: "revise", Note: "tighten it"}
	no := nonGetOutcome{verdict: "revise", note: "tighten it", previous: prev}

	got := nextPendingHandoff("judge", steps[0], steps, "revise", no, map[int]int{1: 0}, 1)
	if got == nil {
		t.Fatal("expected a Handoff")
	}

	if got.FromStep != "critic" || got.RouteKey != "revise" || got.Note != "tighten it" {
		t.Errorf("got FromStep=%q RouteKey=%q Note=%q, want critic/revise/\"tighten it\"", got.FromStep, got.RouteKey, got.Note)
	}

	if got.Previous != prev {
		t.Errorf("Previous = %v, want the same *PreviousRun the outcome carried", got.Previous)
	}

	if got.StepIndex != 1 || got.PlanLen != 3 {
		t.Errorf("got StepIndex=%d PlanLen=%d, want 1,3", got.StepIndex, got.PlanLen)
	}

	if got.MaxVisits != 0 {
		t.Errorf("MaxVisits = %d, want 0 (writer declares none)", got.MaxVisits)
	}
}

// TestNextPendingHandoffVisitPreviewsNextExecution proves Visit previews the
// TARGET step's upcoming execution count (visits[nextIndex]+1), not the
// router's own visit count — the value runSteps reads before its own
// increment happens on the following iteration.
func TestNextPendingHandoffVisitPreviewsNextExecution(t *testing.T) {
	t.Parallel()

	steps := []config.Step{
		{Agent: "critic", Verdicts: []string{"revise"}, To: map[string]string{"revise": "writer"}, MaxVisits: 5},
		{Agent: "writer"},
	}

	// writer (index 1) has already run twice this invocation.
	got := nextPendingHandoff("j", steps[0], steps, "revise", nonGetOutcome{}, map[int]int{1: 2}, 1)
	if got.Visit != 3 {
		t.Errorf("Visit = %d, want 3 (the upcoming, 3rd execution of the target step)", got.Visit)
	}
}
