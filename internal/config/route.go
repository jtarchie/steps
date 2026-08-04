package config

// Step transitions: the to: map, verdict routing, and the plan-segment rules
// that decide which jumps are legal.

import (
	"fmt"
)

// reservedRouteKeys are the outcome keys with fixed meaning in a step's to:
// map: a verdict may not reuse one, and in binary (non-verdict) mode they are
// the only keys allowed. Keeping the set closed here is what reserves the rest
// of the key space for a future exit-code routing extension.
//
//nolint:gochecknoglobals // static, read-only lookup table
var reservedRouteKeys = map[string]bool{"success": true, "failure": true}

// stepName is the name a step is referenced by as a to: jump target: whichever
// of task/put/agent is set. Duplicated (not shared with internal/pipeline's
// executedStepName) because internal/config depends on nothing internal.
func stepName(step Step) string {
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

	pos := make(map[string]int, len(segment))

	for segPos, idx := range segment {
		name := stepName(job.Plan[idx])

		_, dup := pos[name]
		if dup {
			return fmt.Errorf("job %q: step name %q is duplicated within a to:-using segment; names must be unique to be jump targets", job.Name, name)
		}

		pos[name] = segPos
	}

	for segPos, idx := range segment {
		err := validateStepRouting(job, idx, segPos, job.Plan[idx], pos)
		if err != nil {
			return err
		}
	}

	return nil
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
	if inner.Agent == "" {
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
		targetPos, ok := pos[target]
		if !ok {
			return fmt.Errorf("%s: to: %s routes to %q, which is not a step in the same segment", label, key, target)
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
