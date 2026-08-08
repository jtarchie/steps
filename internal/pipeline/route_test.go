package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

func routeSteps() []config.Step {
	return []config.Step{
		{Task: "a"},
		{Task: "b"},
		{Task: "c"},
	}
}

func TestIndexOfStep(t *testing.T) {
	t.Parallel()

	steps := routeSteps()

	idx, ok := indexOfStep(steps, "b")
	if !ok || idx != 1 {
		t.Errorf("indexOfStep(b) = %d,%v, want 1,true", idx, ok)
	}

	_, ok = indexOfStep(steps, "missing")
	if ok {
		t.Error("indexOfStep(missing) should not be found")
	}
}

func TestResolveTransitionSuccessRoutesForward(t *testing.T) {
	t.Parallel()

	steps := routeSteps()
	step := config.Step{Task: "a", To: map[string]string{"success": "c"}}

	next, routed, key, exhausted := resolveTransition(context.Background(), steps, 0, step, nil, "", map[int]int{0: 1})
	if exhausted != nil {
		t.Fatalf("unexpected exhaustion: %v", exhausted)
	}

	if !routed || next != 2 {
		t.Errorf("got next=%d routed=%v, want 2,true (forward to c)", next, routed)
	}

	if key != "success" {
		t.Errorf("got key=%q, want \"success\"", key)
	}
}

func TestResolveTransitionNoMatchingKey(t *testing.T) {
	t.Parallel()

	steps := routeSteps()
	// success outcome but only a failure edge → no route, fall through.
	step := config.Step{Task: "a", To: map[string]string{"failure": "a"}, MaxVisits: 3}

	_, routed, key, exhausted := resolveTransition(context.Background(), steps, 0, step, nil, "", map[int]int{0: 1})
	if exhausted != nil || routed {
		t.Errorf("a success with no success edge should not route: routed=%v exhausted=%v", routed, exhausted)
	}

	if key != "" {
		t.Errorf("got key=%q, want \"\" when not routed", key)
	}
}

func TestResolveTransitionFailureRoutesBackwardAndExhausts(t *testing.T) {
	t.Parallel()

	steps := routeSteps()
	step := config.Step{Task: "b", To: map[string]string{"failure": "a"}, MaxVisits: 2}
	failErr := outcome.Fail(errors.New("nonzero exit"))

	// visits[1] == 1 (< max_visits 2): routes backward to a.
	next, routed, key, exhausted := resolveTransition(context.Background(), steps, 1, step, failErr, "", map[int]int{1: 1})
	if exhausted != nil {
		t.Fatalf("should route, not exhaust yet: %v", exhausted)
	}

	if !routed || next != 0 {
		t.Errorf("got next=%d routed=%v, want 0,true (backward to a)", next, routed)
	}

	if key != "failure" {
		t.Errorf("got key=%q, want \"failure\"", key)
	}

	// visits[1] == 2 (>= max_visits 2): exhausts.
	_, routed, key, exhausted = resolveTransition(context.Background(), steps, 1, step, failErr, "", map[int]int{1: 2})
	if exhausted == nil {
		t.Fatal("expected exhaustion at the visit cap")
	}

	if routed {
		t.Error("must not route when exhausted")
	}

	if key != "" {
		t.Errorf("got key=%q, want \"\" when exhausted", key)
	}

	if outcome.Classify(context.Background(), exhausted) != outcome.Failed {
		t.Error("exhaustion must classify as Failed (routes to the job's on_failure)")
	}
}

// TestResolveTransitionOutcomeClass proves to.failure fires only on a
// task-level Failed error — never on an errored (plain) or aborted (canceled
// ctx) step.
func TestResolveTransitionOutcomeClass(t *testing.T) {
	t.Parallel()

	steps := routeSteps()
	step := config.Step{Task: "b", To: map[string]string{"failure": "a"}, MaxVisits: 3}

	t.Run("Failed routes", func(t *testing.T) {
		t.Parallel()

		_, routed, _, _ := resolveTransition(context.Background(), steps, 1, step, outcome.Fail(errors.New("x")), "", map[int]int{1: 1})
		if !routed {
			t.Error("a Failed step should route on to.failure")
		}
	})

	t.Run("Errored propagates", func(t *testing.T) {
		t.Parallel()

		_, routed, _, _ := resolveTransition(context.Background(), steps, 1, step, errors.New("docker down"), "", map[int]int{1: 1})
		if routed {
			t.Error("an errored (non-Fail) step must NOT route — it propagates")
		}
	})

	t.Run("Aborted propagates", func(t *testing.T) {
		t.Parallel()

		canceled, cancel := context.WithCancel(context.Background())
		cancel()

		_, routed, _, _ := resolveTransition(canceled, steps, 1, step, outcome.Fail(errors.New("x")), "", map[int]int{1: 1})
		if routed {
			t.Error("an aborted step (canceled ctx) must NOT route — it stays aborted")
		}
	})
}

func TestResolveTransitionVerdictRoutes(t *testing.T) {
	t.Parallel()

	steps := []config.Step{{Agent: "writer"}, {Agent: "critic"}, {Agent: "publish"}}
	step := config.Step{
		Agent:     "critic",
		Verdicts:  []string{"approve", "revise"},
		To:        map[string]string{"approve": "publish", "revise": "writer"},
		MaxVisits: 3,
	}

	t.Run("approve routes forward", func(t *testing.T) {
		t.Parallel()

		next, routed, key, _ := resolveTransition(context.Background(), steps, 1, step, nil, "approve", map[int]int{1: 1})
		if !routed || next != 2 {
			t.Errorf("got next=%d routed=%v, want 2,true (publish)", next, routed)
		}

		if key != "approve" {
			t.Errorf("got key=%q, want \"approve\"", key)
		}
	})

	t.Run("revise routes backward", func(t *testing.T) {
		t.Parallel()

		next, routed, key, _ := resolveTransition(context.Background(), steps, 1, step, nil, "revise", map[int]int{1: 1})
		if !routed || next != 0 {
			t.Errorf("got next=%d routed=%v, want 0,true (writer)", next, routed)
		}

		if key != "revise" {
			t.Errorf("got key=%q, want \"revise\"", key)
		}
	})

	t.Run("failure key on a failed verdict agent", func(t *testing.T) {
		t.Parallel()

		withFailure := step
		withFailure.To = map[string]string{"approve": "publish", "revise": "writer", "failure": "publish"}

		next, routed, key, _ := resolveTransition(context.Background(), steps, 1, withFailure, outcome.Fail(errors.New("no verdict")), "", map[int]int{1: 1})
		if !routed || next != 2 {
			t.Errorf("a failed verdict agent should route on to.failure: next=%d routed=%v", next, routed)
		}

		if key != "failure" {
			t.Errorf("got key=%q, want \"failure\"", key)
		}
	})
}

func TestResolveTransitionNoTo(t *testing.T) {
	t.Parallel()

	steps := routeSteps()

	_, routed, key, exhausted := resolveTransition(context.Background(), steps, 0, config.Step{Task: "a"}, nil, "", map[int]int{})
	if routed || exhausted != nil {
		t.Error("a step with no to: never routes")
	}

	if key != "" {
		t.Errorf("got key=%q, want \"\"", key)
	}
}

// TestResolveTransitionNextIsPositional covers the reserved to: target: it
// resolves to the following position rather than to a name, which is what lets
// a verdict land on a container that has none.
func TestResolveTransitionNextIsPositional(t *testing.T) {
	t.Parallel()

	steps := []config.Step{
		{Agent: "critic", Verdicts: []string{"approve"}, To: map[string]string{"approve": config.RouteTargetNext}},
		{Approval: &config.Approval{Message: "publish it?"}}, // no name of its own
		{Put: "release"},
	}

	next, routed, key, exhausted := resolveTransition(
		context.Background(), steps, 0, steps[0], nil, "approve", map[int]int{0: 1})
	if exhausted != nil {
		t.Fatalf("unexpected exhaustion: %v", exhausted)
	}

	if !routed || next != 1 {
		t.Errorf("got next=%d routed=%v, want 1,true — the approval gate, not the put past it", next, routed)
	}

	if key != "approve" {
		t.Errorf("got key=%q, want the verdict that matched", key)
	}
}

// TestApplyRoutingNextConsumesAFailure is why `next` routes rather than
// falling through: `to: { failure: next }` means "carry on anyway", and only a
// real route consumes the error so the job does not also fail.
func TestApplyRoutingNextConsumesAFailure(t *testing.T) {
	t.Parallel()

	steps := []config.Step{
		{Task: "flaky", To: map[string]string{"failure": config.RouteTargetNext}},
		{Task: "after"},
	}

	stepErr := outcome.Fail(errors.New("flaky failed"))

	next, key, consumed, exhausted := applyRouting(
		context.Background(), steps, 0, steps[0], stepRan, "", stepErr, map[int]int{0: 1})
	if exhausted != nil {
		t.Fatalf("unexpected exhaustion: %v", exhausted)
	}

	if next != 1 || key != "failure" {
		t.Errorf("got next=%d key=%q, want 1,\"failure\"", next, key)
	}

	if consumed != nil {
		t.Errorf("error = %v, want it consumed by the route", consumed)
	}
}

// TestApplyRoutingNextOffTheEndOfASegment pins the boundary: `next` on the
// last step resolves one past the end, which is where an unrouted final step
// goes anyway. reportRoute names that rather than indexing it — a bare lookup
// panics on exactly the pipeline whose last outcome says "just carry on".
func TestApplyRoutingNextOffTheEndOfASegment(t *testing.T) {
	t.Parallel()

	steps := []config.Step{
		{Task: "last", To: map[string]string{"failure": config.RouteTargetNext}},
	}

	stepErr := outcome.Fail(errors.New("last failed"))

	next, _, consumed, exhausted := applyRouting(
		context.Background(), steps, 0, steps[0], stepRan, "", stepErr, map[int]int{0: 1})
	if exhausted != nil {
		t.Fatalf("unexpected exhaustion: %v", exhausted)
	}

	if next != 1 {
		t.Errorf("next = %d, want 1 (one past the only step)", next)
	}

	if consumed != nil {
		t.Errorf("error = %v, want it consumed by the route", consumed)
	}
}
