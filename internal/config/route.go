package config

// Step transitions: the to: map, verdict routing, and the plan-segment rules
// that decide which jumps are legal.

import (
	"fmt"
	"maps"
	"slices"
)

// reservedRouteKeys are the outcome keys with fixed meaning in a step's to:
// map: a verdict may not reuse one, and in binary (non-verdict) mode they are
// the only keys allowed. Keeping the set closed here is what reserves the rest
// of the key space for a future exit-code routing extension.
//
//nolint:gochecknoglobals // static, read-only lookup table
var reservedRouteKeys = map[string]bool{"success": true, "failure": true}

// RouteTargetNext is the reserved to: TARGET — the value side, where every
// other entry is a step name — meaning "continue in declaration order".
//
// It exists because a verdict must name a target, and a container has none:
// stepName returns "" for in_parallel:, race:, across:, ensemble: and
// approval:. An author whose next step was one of those had to route PAST it
// to some later leaf, which does not skip a formality — routing past an
// approval: skips the human gate the pipeline exists to have — or insert a
// leaf step purely to give the verdict somewhere safe to land.
//
// Positional rather than named, so it needs no naming surface on block steps,
// and it says the thing an author wants to say independently of containers:
// this outcome is not a jump. Always forward, so it never requires
// max_visits:.
//
// The cost is one reserved word on the value side. A step named "next" inside
// a to:-using segment is refused at load rather than silently shadowed —
// otherwise "next" would mean two things in the one place they collide.
const RouteTargetNext = "next"

// DisplayName is stepName for other packages: the name a step is KNOWN by,
// which is what a run's output, its recorded node, and assert.execution should
// all agree on. Distinct from the task:/agent:/put: value once an across: cell
// has been named (see Step.Label), and identical to it otherwise.
func (s Step) DisplayName() string {
	return stepName(s)
}

// stepName is the name a step is referenced by as a to: jump target: whichever
// of task/put/agent is set. Duplicated (not shared with internal/pipeline's
// executedStepName) because internal/config depends on nothing internal.
func stepName(step Step) string {
	// A computed Label is the step's identity when it has one — see Step.Label.
	// It wins over the kind fields below precisely because those are also
	// lookup keys, and an across: cell renames itself without renaming what it
	// resolves.
	if step.Label != "" {
		return step.Label
	}

	kind, ok := step.Kind()
	if !ok {
		return ""
	}

	switch kind { //nolint:exhaustive // default covers StepKindGet, which is not a valid to: target
	case StepKindTask:
		return step.Task
	case StepKindAgent:
		return step.Agent
	case StepKindPut:
		return step.Put
	case StepKindTry:
		return stepName(*step.Try)
	default:
		return ""
	}
}

// validateStepTransitions enforces the to:/max_visits:/verdicts: rules at load
// time: routing fields are invalid on get and hook steps; within any plan
// segment (bounded by get steps) that uses routing, step names must be unique,
// every to: target must resolve within the segment, a backward target requires
// max_visits, and an agent's verdict vocabulary must be complete and
// consistent with its to: keys. Also validates handoff: (validateHandoffSteps),
// since it's meaningless without a to: route targeting the step it's set on.
func (c *Config) validateStepTransitions() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitSteps(rejectRoutingOnGet)
		if err != nil {
			return err
		}

		err = job.visitHookSteps(rejectRoutingOnHook)
		if err != nil {
			return err
		}

		err = c.validatePlanSegments(job)
		if err != nil {
			return err
		}
	}

	return c.validateHandoffSteps()
}

// rejectRoutingOnGet rejects to:/max_visits:/verdicts: on a get step (a get
// fans the remainder of the plan out per version, so routing it is meaningless).
func rejectRoutingOnGet(label string, step *Step) error {
	if step.Get != "" && (step.To != nil || step.MaxVisits != 0 || len(step.Verdicts) > 0) {
		return fmt.Errorf("%s (get %q): to/max_visits/verdicts are not valid on get steps", label, step.Get)
	}

	return nil
}

// rejectRoutingOnHook rejects to:/verdicts: on a hook step (a hook is a
// reaction, not a positioned plan step, so it can't be a jump source or target).
func rejectRoutingOnHook(label string, step *Step) error {
	if step.To != nil || len(step.Verdicts) > 0 {
		return fmt.Errorf("%s: to/verdicts are not valid on hook steps", label)
	}

	return nil
}

// validatePlanSegments splits a job's plan into segments at each get step and
// validates transitions within each segment that uses routing. A segment is a
// maximal run of consecutive non-get plan steps; a get is a boundary belonging
// to no segment. Segment-relative positions here match what internal/pipeline's
// runSteps sees, since runSteps re-enters over a truncated slice per get.
func (c *Config) validatePlanSegments(job Job) error {
	var segment []int // plan indices of the current segment's steps

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}

		err := validateSegment(job, segment)
		segment = nil

		return err
	}

	for i := range job.Plan {
		if job.Plan[i].Get != "" {
			err := flush()
			if err != nil {
				return err
			}

			continue
		}

		segment = append(segment, i)
	}

	return flush()
}

// validateSegment validates the routing of one segment (plan indices in
// declaration order). It's a no-op unless some step in the segment uses
// routing; when it does, step names must be unique so they can be jump targets.
func validateSegment(job Job, segment []int) error {
	usesRouting := false

	for _, idx := range segment {
		// Unwrap for verdicts:, which sits on the agent step a try: may wrap —
		// otherwise a wrapped `verdicts:` with no to: skips this whole check
		// and reaches run time with a synthesized verdict tool routing nowhere.
		if job.Plan[idx].To != nil || len(job.Plan[idx].Unwrap().Verdicts) > 0 {
			usesRouting = true

			break
		}
	}

	if !usesRouting {
		return nil
	}

	pos, err := segmentPositions(job, segment)
	if err != nil {
		return err
	}

	for segPos, idx := range segment {
		err = validateStepRouting(job, idx, segPos, job.Plan[idx], pos)
		if err != nil {
			return err
		}
	}

	return nil
}

// segmentPositions maps each named step in the segment to its
// segment-relative position — what a to: target resolves against.
//
// It also rejects the two names that cannot be targets: a duplicate, which has
// no answer, and the reserved "next".
func segmentPositions(job Job, segment []int) (map[string]int, error) {
	pos := make(map[string]int, len(segment))

	for segPos, idx := range segment {
		name := stepName(job.Plan[idx])

		// A container — in_parallel:, race:, across:, approval: — has no name
		// of its own, and pos is what to: targets resolve against, so an
		// unnamed step could never be jumped to in the first place. Requiring
		// those names to be unique made two blocks in one routing segment a
		// load error reported as `step name "" is duplicated`, which named
		// neither block and described a collision between two things that
		// cannot be targeted at all. A fan-out beside a fan-in, in a job with
		// a loop, is an ordinary shape (see examples/pr-review.yml). Reaching
		// one is what RouteTargetNext is for.
		if name == "" {
			continue
		}

		// "next" is a to: target with a fixed positional meaning, so a step
		// answering to it would make one word mean two things in the one place
		// they meet. Refused only inside a to:-using segment, since that is
		// the only place the word is read at all.
		if name == RouteTargetNext {
			return nil, fmt.Errorf("job %q: step %q collides with the reserved to: target %q, which means the next step in declaration order; rename the step", job.Name, name, RouteTargetNext)
		}

		_, dup := pos[name]
		if dup {
			return nil, fmt.Errorf("job %q: step name %q is duplicated within a to:-using segment; names must be unique to be jump targets", job.Name, name)
		}

		pos[name] = segPos
	}

	return pos, nil
}

// validateStepRouting validates one step's to:/verdicts:/max_visits: against
// its segment (pos maps each segment step name to its segment-relative
// position). segPos is this step's own position.
func validateStepRouting(job Job, planIdx, segPos int, step Step, pos map[string]int) error {
	label := fmt.Sprintf("job %q step %d", job.Name, planIdx)

	// try: is transparent to routing: to:/max_visits: sit on the wrapper,
	// which is the step with a position in the plan, while verdicts: stays on
	// the agent step it wraps — that is what internal/agent reads to
	// synthesize the required verdict tool. Unwrapping here is what lets a
	// tolerated agent still route on its verdict instead of being rejected
	// with "to: key %q is not valid (expected success or failure)".
	inner := step.Unwrap()

	// An ensemble's verdicts live on the block, since every member votes in
	// the same vocabulary, and the block routes on the DECISION. Treat them as
	// the step's own for routing, or a to: naming a real verdict would be
	// rejected as "expected success or failure".
	if inner.Ensemble != nil {
		inner.Verdicts = inner.Ensemble.Verdicts
	}

	if len(inner.Verdicts) > 0 {
		err := validateVerdictMode(label, step, inner)
		if err != nil {
			return err
		}
	} else if step.To != nil {
		for key := range step.To {
			if !reservedRouteKeys[key] {
				return fmt.Errorf("%s: to: key %q is not valid (expected success or failure)", label, key)
			}
		}
	}

	return validateRouteTargets(label, segPos, step, pos)
}

// validateVerdictMode enforces the verdict-mode shape: agent-only, well-formed
// verdict names, and a complete, consistent mapping between the declared
// verdicts and the to: keys. step is the plan-positioned step (which carries
// to:); inner is what it runs (which carries agent:/verdicts:). The two are the
// same step unless a try: wraps it.
func validateVerdictMode(label string, step, inner Step) error {
	// An ensemble is the one non-agent step that produces a verdict: its
	// members are agents and the block routes on their combined decision.
	if inner.Agent == "" && inner.Ensemble == nil {
		return fmt.Errorf("%s: verdicts is only valid on agent steps", label)
	}

	if step.To == nil {
		return fmt.Errorf("%s: verdicts requires a to: map", label)
	}

	declared, err := validateVerdictNames(label, step, inner)
	if err != nil {
		return err
	}

	return validateVerdictToKeys(label, step, declared)
}

// validateVerdictNames checks each declared verdict is non-empty, unique, not a
// reserved key, and has a to: target; it returns the set of declared names.
func validateVerdictNames(label string, step, inner Step) (map[string]bool, error) {
	declared := make(map[string]bool, len(inner.Verdicts))

	for _, verdict := range inner.Verdicts {
		switch {
		case verdict == "":
			return nil, fmt.Errorf("%s: verdicts must not contain an empty name", label)
		case reservedRouteKeys[verdict]:
			return nil, fmt.Errorf("%s: verdict %q collides with a reserved key (success/failure)", label, verdict)
		case declared[verdict]:
			return nil, fmt.Errorf("%s: verdict %q is declared more than once", label, verdict)
		}

		declared[verdict] = true

		_, routed := step.To[verdict]
		if !routed {
			return nil, fmt.Errorf("%s: verdict %q has no to: target", label, verdict)
		}
	}

	return declared, nil
}

// validateVerdictToKeys checks every to: key in verdict mode is either the
// reserved failure catch or a declared verdict — and rejects a generic success
// key, since a verdict replaces it.
func validateVerdictToKeys(label string, step Step, declared map[string]bool) error {
	for key := range step.To {
		switch {
		case key == "success":
			return fmt.Errorf("%s: to: success is not valid in verdict mode (a verdict replaces generic success)", label)
		case key == "failure":
			continue // reserved catch for "never produced a verdict"
		case !declared[key]:
			return fmt.Errorf("%s: to: key %q is not a declared verdict", label, key)
		}
	}

	return nil
}

// validateRouteTargets resolves every to: target within the segment and
// requires max_visits when any target routes backward (segment-relative
// position at or before the declaring step).
func validateRouteTargets(label string, segPos int, step Step, pos map[string]int) error {
	backward := false

	for key, target := range step.To {
		// The one target that is not a name. It is the position after this
		// step, so it always exists (falling off the end of a segment is what
		// an unrouted step does anyway) and is always forward.
		if target == RouteTargetNext {
			continue
		}

		targetPos, ok := pos[target]
		if !ok {
			return fmt.Errorf("%s: to: %s routes to %q, which is not a step in the same segment%s", label, key, target, suggestion(target, append(slices.Sorted(maps.Keys(pos)), RouteTargetNext)))
		}

		if targetPos <= segPos {
			backward = true
		}
	}

	if backward && step.MaxVisits <= 0 {
		return fmt.Errorf("%s: to: routes backward, so max_visits must be set (> 0)", label)
	}

	if backward && step.MaxVisits > maxVisitsLimit {
		return fmt.Errorf("%s: max_visits %d exceeds the maximum of %d", label, step.MaxVisits, maxVisitsLimit)
	}

	return nil
}
