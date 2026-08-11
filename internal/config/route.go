package config

// Step transitions: the to: map, verdict routing, and the plan-segment rules
// that decide which jumps are legal.

import (
	"fmt"
	"maps"
	"slices"
)

// reservedRouteKeys are the outcome keys with fixed meaning in a step's to:
// map, and the only keys it may carry — to: is binary mode now that verdict
// routing lives in the verdicts: list itself. Keeping the set closed here is
// what reserves the rest of the key space for a future exit-code routing
// extension.
//
//nolint:gochecknoglobals // static, read-only lookup table
var reservedRouteKeys = map[string]bool{"success": true, verdictFailureKey: true}

// Routes reports whether this step can send the plan somewhere other than the
// next step in declaration order — a to: map, or a verdicts: entry that names
// a target. A verdicts: list of bare names routes nothing: it records what the
// model decided and carries on.
func (s Step) Routes() bool {
	if s.To != nil {
		return true
	}

	for _, route := range s.VerdictRoutes() {
		if route.Target != "" {
			return true
		}
	}

	return false
}

// RouteFor resolves the target an outcome key sends the plan to, looking in
// the verdicts: list first and the to: map second (a step has one or the
// other; validateStepRouting rejects both at once). ok is false when the key
// routes nowhere — an unmatched outcome, or a bare verdict — and the caller
// falls through in declaration order.
func (s Step) RouteFor(key string) (target string, ok bool) {
	for _, route := range s.VerdictRoutes() {
		if route.Name == key {
			return route.Target, route.Target != ""
		}
	}

	target, ok = s.To[key]

	return target, ok
}

// RouteEntries returns every (outcome key, target) pair this step routes by,
// from whichever of verdicts:/to: it declares — the input to target
// resolution, which treats both the same way. Bare verdicts are omitted: they
// name no target to resolve.
func (s Step) RouteEntries() []VerdictRoute {
	entries := make([]VerdictRoute, 0, len(s.To))

	for _, route := range s.VerdictRoutes() {
		if route.Target != "" {
			entries = append(entries, route)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(s.To)) {
		entries = append(entries, VerdictRoute{Name: key, Target: s.To[key]})
	}

	return entries
}

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
// time: routing fields are invalid on get and hook steps; a verdicts: list is
// agent-only, well formed, and never accompanied by to:; and within any plan
// segment (bounded by get steps) that uses routing, step names must be unique,
// every target must resolve within the segment, and a backward target requires
// max_visits. Also validates handoff: (validateHandoffSteps), since it's
// meaningless without a route targeting the step it's set on.
func (c *Config) validateStepTransitions() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitSteps(rejectRoutingOnGet)
		if err != nil {
			return err
		}

		err = job.visitSteps(validateVerdictShape)
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
		// Step.Routes looks through a try: wrapper and into an ensemble block,
		// so a wrapped verdicts: still brings its segment under these rules
		// rather than reaching run time with targets nobody resolved.
		if job.Plan[idx].Routes() {
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

// validateStepRouting validates one step's route targets against its segment
// (pos maps each segment step name to its segment-relative position). segPos is
// this step's own position. The shape of verdicts: itself — names, agent-only,
// the to: conflict — is checked for every step by validateVerdictShape, since a
// list of bare verdicts routes nothing and would never reach here.
func validateStepRouting(job Job, planIdx, segPos int, step Step, pos map[string]int) error {
	label := fmt.Sprintf("job %q step %d", job.Name, planIdx)

	if len(step.VerdictRoutes()) == 0 {
		for key := range step.To {
			if !reservedRouteKeys[key] {
				return fmt.Errorf("%s: to: key %q is not valid (expected success or failure)", label, key)
			}
		}
	}

	return validateRouteTargets(label, segPos, step, pos)
}

// validateVerdictShape enforces everything about a verdicts: list that needs no
// segment context: it belongs on an agent, it does not coexist with to:, and
// each entry is well formed.
//
// It runs over every step rather than only routing segments because a list of
// bare verdicts is legal and common — an agent that classifies, records what it
// decided, and lets the plan carry on — and it must still be checked.
func validateVerdictShape(label string, step *Step) error {
	routes := step.VerdictRoutes()
	if len(routes) == 0 {
		return nil
	}

	inner := step.Unwrap()

	// An ensemble is the one non-agent step that produces a verdict: its
	// members are agents and the block routes on their combined decision.
	if inner.Agent == "" && inner.Ensemble == nil {
		return fmt.Errorf("%s: verdicts is only valid on agent steps", label)
	}

	// The hard cutover. Verdict targets used to live in a parallel to: map
	// that had to agree with this list key for key; they live in the list
	// itself now, so a step carrying both is the old spelling and gets told
	// the new one rather than a puzzle about which key is unexpected.
	if step.To != nil {
		return fmt.Errorf("%s: to: is not valid on a step with verdicts:; a verdict's target now lives in the verdicts: list itself — write\n  verdicts:\n    - %s: <step name, or %s>\n    - %s: <step name>   # the reserved catch for an errored step",
			label, routes[0].Name, RouteTargetNext, verdictFailureKey)
	}

	return validateVerdictEntries(label, routes)
}

// validateVerdictEntries checks each entry is non-empty, unique, not the
// reserved success key, and — for the reserved failure catch — routed.
func validateVerdictEntries(label string, routes []VerdictRoute) error {
	declared := make(map[string]bool, len(routes))

	for _, route := range routes {
		switch {
		case route.Name == "":
			return fmt.Errorf("%s: verdicts must not contain an empty name", label)
		case route.Name == "success":
			return fmt.Errorf("%s: verdict %q collides with a reserved key; a verdict already IS this step's success, so name it for the decision it reports", label, route.Name)
		case declared[route.Name]:
			return fmt.Errorf("%s: verdict %q is declared more than once", label, route.Name)
		// A bare `failure` would mean "the step errored, carry on regardless",
		// which is precisely try:. Two spellings of tolerance is how one of
		// them drifts, so this one is refused.
		case route.Name == verdictFailureKey && route.Target == "":
			return fmt.Errorf("%s: verdict %q is the reserved catch for an errored step and must name a target — to tolerate the failure and carry on, wrap the step in a try", label, route.Name)
		}

		declared[route.Name] = true
	}

	return nil
}

// validateRouteTargets resolves every to: target within the segment and
// requires max_visits when any target routes backward (segment-relative
// position at or before the declaring step).
func validateRouteTargets(label string, segPos int, step Step, pos map[string]int) error {
	backward := false

	for _, entry := range step.RouteEntries() {
		// The one target that is not a name. It is the position after this
		// step, so it always exists (falling off the end of a segment is what
		// an unrouted step does anyway) and is always forward.
		if entry.Target == RouteTargetNext {
			continue
		}

		targetPos, ok := pos[entry.Target]
		if !ok {
			return fmt.Errorf("%s: %s routes to %q, which is not a step in the same segment%s", label, entry.Name, entry.Target, suggestion(entry.Target, append(slices.Sorted(maps.Keys(pos)), RouteTargetNext)))
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
