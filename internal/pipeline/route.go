package pipeline

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
)

// outcomeKey collapses a step's result into the single string a to: map is
// keyed by. This is the routing extension seam: an agent's emitted verdict, or
// the binary success/failure of a task/put/verdict-less agent, and (later, with
// no change elsewhere) a task's raw exit code, all become a key here.
//
// An Errored (infrastructure) or Aborted (ctx-canceled) step is NOT routable —
// it produces no key, so the error propagates and fails/aborts the job exactly
// as today. This is what stops a to.failure loop from spinning during a
// SIGINT shutdown or masking a docker/transport outage.
func outcomeKey(ctx context.Context, stepErr error, verdict string) (key string, routable bool) {
	if verdict != "" {
		return verdict, true
	}

	switch outcome.Classify(ctx, stepErr) {
	case outcome.Succeeded:
		return "success", true
	case outcome.Failed:
		return "failure", true
	case outcome.Errored, outcome.Aborted:
		return "", false
	default:
		return "", false
	}
}

// resolveTransition decides where the plan goes after the step at position i in
// steps produced stepErr (and, for an agent, verdict). visits[i] is how many
// times step i has ALREADY executed this run — the caller increments it on a
// stepRan disposition before calling this, so a backward route that would push
// executions past step.MaxVisits exhausts instead.
//
// It returns routed=false (nextIndex unused) when the step has no to:, or its
// outcome isn't routable, or no to: key matches — in every such case the caller
// keeps today's behavior (fall through on success, fail the job on failure).
// When it routes on a failure, the caller must CONSUME the error (set it nil)
// so the job doesn't also fail — see runSteps.
func resolveTransition(ctx context.Context, steps []config.Step, i int, step config.Step, stepErr error, verdict string, visits map[int]int) (nextIndex int, routed bool, exhaustedErr error) {
	if step.To == nil {
		return 0, false, nil
	}

	key, routable := outcomeKey(ctx, stepErr, verdict)
	if !routable {
		return 0, false, nil
	}

	target, ok := step.To[key]
	if !ok {
		return 0, false, nil
	}

	targetIndex, found := indexOfStep(steps, target)
	if !found {
		// Load-time validation guarantees the target resolves within the
		// segment, so this is a defensive belt — treat an unresolvable target
		// as "don't route" rather than crash.
		return 0, false, nil
	}

	if targetIndex <= i && visits[i] >= step.MaxVisits {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return 0, false, outcome.Fail(fmt.Errorf("step %q exceeded max_visits %d", executedStepName(step), step.MaxVisits))
	}

	return targetIndex, true, nil
}

// applyRouting computes the index of the next step after step i executed with
// outcome stepErr (and verdict), and consumes the error when the step routes
// on it. It returns the default i+1 when the step has no to: or didn't run, so
// a routing-free plan behaves exactly as a straight line. A backward route past
// max_visits yields exhaustedErr (a job-level failure) instead of routing.
func applyRouting(ctx context.Context, steps []config.Step, i int, step config.Step, disposition stepDisposition, verdict string, stepErr error, visits map[int]int) (nextIndex int, consumedErr, exhaustedErr error) {
	if step.To == nil || disposition != stepRan {
		return i + 1, stepErr, nil
	}

	target, routed, exhausted := resolveTransition(ctx, steps, i, step, stepErr, verdict, visits)
	if exhausted != nil {
		return 0, stepErr, exhausted
	}

	if routed {
		return target, nil, nil // consume the outcome — the job does not also fail
	}

	return i + 1, stepErr, nil
}

// stepForcesUnskippable reports whether a step makes its chain unskippable: a
// put/agent (side effects / non-determinism), a task with a fix: — including
// one inherited from a referenced tasks: entry, resolved the same way
// merkle.planNonGetNode resolves it at plan time, so the two can't disagree —
// or a when:/to: whose runtime outcome the planner cannot know. Such a chain
// is never recorded as a reusable "this whole chain succeeded" hash.
func stepForcesUnskippable(cfg *config.Config, step config.Step) (bool, error) {
	if step.Put != "" || step.Agent != "" || step.When != nil || step.To != nil {
		return true, nil
	}

	if step.Task == "" {
		return false, nil
	}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return false, fmt.Errorf("resolve task: %w", err)
	}

	return rt.Fix != nil, nil
}

// foldStepUnskippable ORs step's own unskippability into chainUnskippable in
// one call, keeping runSteps's own branch count down.
func foldStepUnskippable(cfg *config.Config, step config.Step, chainUnskippable bool) (bool, error) {
	unskippable, err := stepForcesUnskippable(cfg, step)
	if err != nil {
		return chainUnskippable, err
	}

	return chainUnskippable || unskippable, nil
}

// indexOfStep returns the position of the step named name (its task/put/agent
// value) in steps, searching the single segment slice runSteps is handed.
func indexOfStep(steps []config.Step, name string) (int, bool) {
	for i := range steps {
		if executedStepName(steps[i]) == name {
			return i, true
		}
	}

	return 0, false
}
