package pipeline

import (
	"context"
	"fmt"
	"log/slog"

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
// It returns routed=false (nextIndex/key unused) when the step has no to:, or
// its outcome isn't routable, or no to: key matches — in every such case the
// caller keeps today's behavior (fall through on success, fail the job on
// failure). When it routes on a failure, the caller must CONSUME the error
// (set it nil) so the job doesn't also fail — see runSteps. key is the
// outcome key the transition actually matched (a verdict, or "success"/
// "failure"); runSteps surfaces it into the routed-to step's Handoff (see
// internal/agent's Handoff.RouteKey) rather than discarding it as before.
func resolveTransition(ctx context.Context, steps []config.Step, i int, step config.Step, stepErr error, verdict string, visits map[int]int) (nextIndex int, routed bool, key string, exhaustedErr error) {
	if !step.Routes() {
		return 0, false, "", nil
	}

	matchedKey, routable := outcomeKey(ctx, stepErr, verdict)
	if !routable {
		return 0, false, "", nil
	}

	// A verdict the author declared but gave no target — the classify-and-carry-on
	// spelling — routes nowhere, exactly like an unmatched key: the plan falls
	// through in declaration order and the verdict stays on the record.
	target, ok := step.RouteFor(matchedKey)
	if !ok {
		return 0, false, "", nil
	}

	// The reserved positional target: continue in declaration order. It still
	// ROUTES rather than falling through — a to: failure: next means "carry on
	// anyway", which only works if the caller consumes the error the way it
	// does for any other route.
	if target == config.RouteTargetNext {
		return i + 1, true, matchedKey, nil
	}

	targetIndex, found := indexOfStep(steps, target)
	if !found {
		// Load-time validation guarantees the target resolves within the
		// segment, so this is a defensive belt — treat an unresolvable target
		// as "don't route" rather than crash.
		return 0, false, "", nil
	}

	if targetIndex <= i && visits[i] >= step.MaxVisits {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return 0, false, "", outcome.Fail(fmt.Errorf("step %q exceeded max_visits %d", executedStepName(step), step.MaxVisits))
	}

	return targetIndex, true, matchedKey, nil
}

// applyRouting computes the index of the next step after step i executed with
// outcome stepErr (and verdict), and consumes the error when the step routes
// on it. It returns the default i+1 (and routedKey="") when the step has no
// to: or didn't run, so a routing-free plan behaves exactly as a straight
// line. A backward route past max_visits yields exhaustedErr (a job-level
// failure) instead of routing. routedKey, non-empty only when the step
// actually routed, is what runSteps uses to build the next step's Handoff.
func applyRouting(ctx context.Context, steps []config.Step, i int, step config.Step, disposition stepDisposition, verdict string, stepErr error, visits map[int]int) (nextIndex int, routedKey string, consumedErr, exhaustedErr error) {
	if !step.Routes() || disposition != stepRan {
		return i + 1, "", stepErr, nil
	}

	target, routed, key, exhausted := resolveTransition(ctx, steps, i, step, stepErr, verdict, visits)
	if exhausted != nil {
		return 0, "", stepErr, exhausted
	}

	if routed {
		reportRoute(steps, i, step, target, key, visits)

		return target, key, nil, nil // consume the outcome — the job does not also fail
	}

	return i + 1, "", stepErr, nil
}

// reportRoute prints the jump a to:/verdicts: transition just took.
//
// Routing was entirely invisible: the computation happened here and said
// nothing, so a loop looked like a step repeating for no reason, and the first
// hint that jumping was even involved was "exceeded max_visits" at the end.
//
// The counter tracks the ROUTING step, not the target: max_visits bounds how
// many times this step may execute (see resolveTransition), so its own count
// against its own bound is the number that says how close the loop is to
// exhausting.
func reportRoute(steps []config.Step, i int, step config.Step, target int, key string, visits map[int]int) {
	from, to := executedStepName(step), routeTargetName(steps, target)

	progress := fmt.Sprintf("visit %d", visits[i])
	if step.MaxVisits > 0 {
		progress = fmt.Sprintf("visit %d/%d", visits[i], step.MaxVisits)
	}

	fmt.Printf("route: %s --%s--> %s (%s)\n", from, key, to, progress)
	slog.Info("job.route", "from", from, "key", key, "to", to, "visit", visits[i], "max_visits", step.MaxVisits, "index", i)
}

// stepForcesUnskippable reports whether a step makes its chain unskippable: a
// put/agent (side effects / non-determinism), a task with a fix: — including
// one inherited from a referenced tasks: entry, resolved the same way
// merkle.planNonGetNode resolves it at plan time, so the two can't disagree —
// or a when:/to: whose runtime outcome the planner cannot know. Such a chain
// is never recorded as a reusable "this whole chain succeeded" hash.
func stepForcesUnskippable(cfg *config.Config, step config.Step) (bool, error) {
	if unskippableReason(step) != "" {
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

// unskippableReason names why a step makes its chain uncacheable, or "" when
// it doesn't (or when the reason is a fix:, which needs task resolution — see
// stepForcesUnskippable).
//
// It exists so the run can SAY so. That a when: guard or a to: route silently
// disables caching for everything downstream is real, documented behavior that
// produced no output at all: the user saw steps re-running and had to infer
// the rule.
func unskippableReason(step config.Step) string {
	switch {
	case step.Put != "":
		return "put step"
	case step.Agent != "":
		return "agent step"
	case step.Try != nil:
		return "try step"
	case step.When != nil:
		return "when: guard"
	case step.Routes():
		return "to: routing"
	default:
		return ""
	}
}

// foldStepUnskippable ORs step's own unskippability into chainUnskippable in
// one call, keeping runSteps's own branch count down. On the false->true
// transition it prints why, once — the point at which the rest of this chain
// stopped being cacheable.
func foldStepUnskippable(cfg *config.Config, step config.Step, chainUnskippable bool) (bool, error) {
	unskippable, err := stepForcesUnskippable(cfg, step)
	if err != nil {
		return chainUnskippable, err
	}

	if unskippable && !chainUnskippable {
		reason := unskippableReason(step)
		if reason == "" {
			reason = "fix: agent"
		}

		name := executedStepName(step)

		fmt.Printf("note: %s makes this chain uncacheable (%s)\n", name, reason)
		slog.Debug("job.uncacheable", "step", name, "reason", reason)
	}

	return chainUnskippable || unskippable, nil
}

// routeTargetName names the step a route landed on, for the log line.
//
// `to: next` on the LAST step of a segment resolves one past the end, which is
// the same place an unrouted final step goes. That is a legal destination and
// not a step, so it is named rather than indexed — a bare steps[target] here
// panics on exactly the pipeline whose last outcome says "just carry on".
func routeTargetName(steps []config.Step, target int) string {
	if target >= len(steps) {
		return "end of plan"
	}

	return executedStepName(steps[target])
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
