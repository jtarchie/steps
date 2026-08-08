package config

// A step's handoff: — the pushed <transition_context> prompt block and the
// pulled previous_run tool, and where in a plan they are legal.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// HandoffSpec is a step's handoff: — everything an agent step exchanges with
// the other agent steps around it, in one key.
//
// Two directions, three switches:
//
//	context: receive a <transition_context> block from the step that routed here
//	tool:    receive a previous_run tool to pull that step's run on demand
//	note:    write a note for the next agent step in this segment to read
//
// context/tool look BACKWARD along a to:/verdicts: route; note looks FORWARD
// along the plan. note: was its own top-level key (handoff_note:) until the
// two proved impossible to tell apart by name — the docs needed a dedicated
// aside explaining that handoff: and handoff_note: were unrelated features
// pointing opposite ways. Direction is now a field, not a spelling.
//
// A scalar `handoff: true` means {context: true}. In the mapping form every
// field means exactly what it says and defaults to off: an earlier rule where
// context turned itself on unless explicitly disabled made `handoff: {tool:
// true}` quietly enable two things, and would have made a note-only mapping
// demand a to: route it has nothing to do with.
type HandoffSpec struct {
	Context bool
	Tool    bool
	Note    bool
}

// UnmarshalYAML decodes a HandoffSpec from either a scalar (handoff: true/
// false, context only) or a mapping ({context, tool, note}) YAML node — the
// same scalar-or-mapping idiom as WhenSpec/FixSpec.
func (h *HandoffSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		var enabled bool

		err := value.Decode(&enabled)
		if err != nil {
			return fmt.Errorf("step handoff: %w", err)
		}

		h.Context = enabled

		return nil
	case yaml.MappingNode:
		err := rejectUnknownKeys(value, "step handoff", "context", "tool", "note")
		if err != nil {
			return err
		}

		var m struct {
			Context bool `yaml:"context"`
			Tool    bool `yaml:"tool"`
			Note    bool `yaml:"note"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step handoff: %w", err)
		}

		h.Context, h.Tool, h.Note = m.Context, m.Tool, m.Note

		return nil
	default:
		return fmt.Errorf("step handoff at line %d must be a boolean or a {context, tool, note} mapping", value.Line)
	}
}

// Enabled reports whether h turns on anything. A non-nil but all-false
// HandoffSpec (handoff: false, or handoff: {context: false}) is rejected at
// LoadConfig (validateHandoffSteps) rather than silently accepted as a no-op.
func (h *HandoffSpec) Enabled() bool {
	return h.Context || h.Tool || h.Note
}

// Receives reports whether h asks for anything from the step that routed
// here — the backward half, and the only half that requires a to: route.
func (h *HandoffSpec) Receives() bool {
	return h.Context || h.Tool
}

// WantsNote reports whether this step must write a handoff note before its
// conversation may end (the forward half).
func (s Step) WantsNote() bool {
	return s.Handoff != nil && s.Handoff.Note
}

// validateHandoffSteps enforces the handoff: rules at load time: it is never
// valid on a hook step (a hook is a reaction, not a positioned step with
// predecessors), never inside a concurrent block or on an across: step (see
// rejectHandoffInBranches/rejectHandoffOnAcross), and on a plan step it is
// agent-only, must enable at least one of context/tool, and must be the
// target of at least one to: route within its own get-segment — otherwise the
// field is dead config.
func (c *Config) validateHandoffSteps() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitHookSteps(rejectHandoffOnHook)
		if err != nil {
			return err
		}

		err = rejectHandoffInBranches(job)
		if err != nil {
			return err
		}

		err = rejectHandoffOnAcross(job)
		if err != nil {
			return err
		}

		err = c.validateHandoffSegments(job)
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectHandoffInBranches rejects handoff: on any step nested inside a
// concurrent block (an in_parallel:/race: branch, an ensemble member). The
// segment walks below iterate only top-level plan steps, so a branch's
// handoff: was never validated OR wired: it loaded clean, then forced the
// agent to write a note nothing received (note:), or promised a transition
// context no route could deliver (context:/tool:). A load error until
// branches participate in handoff, rather than dead config.
func rejectHandoffInBranches(job Job) error {
	for i := range job.Plan {
		label := fmt.Sprintf("job %q step %d", job.Name, i)

		for kind, branches := range branchesOf(unwrapStep(&job.Plan[i])) {
			for b := range branches {
				err := visitStepTree(fmt.Sprintf("%s (%s branch %d)", label, kind, b), &branches[b], rejectNestedHandoff)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// rejectNestedHandoff rejects the BACKWARD half of handoff: inside a
// concurrent block. context:/tool: describe arriving via a to:/verdicts:
// route, and a branch is not a routable position — nothing can route to it, so
// the fields would be permanently dead.
//
// note: is legal here and is the fan-out/fan-in case: a branch receives what
// was pending before the block and sends to the step after it (see
// wireHandoffNoteBlock).
func rejectNestedHandoff(label string, step *Step) error {
	if step.Handoff != nil && step.Handoff.Receives() {
		return fmt.Errorf("%s: handoff context/tool is not supported inside a concurrent block — a branch is not a routable position, so no to: route could ever reach it", label)
	}

	return nil
}

// rejectHandoffOnAcross rejects the BACKWARD half of handoff: on an across:
// step (looking through a try: wrapper, where the cell body lives).
//
// context:/tool: describe arriving via a to:/verdicts: route. A matrix is one
// plan step expanding into siblings, and a route targets the step rather than
// any one cell, so there is no cell a transition could be said to have entered.
//
// note: is supported: each cell writes handoff/<its label>.md and the step
// after the matrix receives one per cell (see acrossSenderNames). That became
// possible only once a cell's identity was separated from what it resolves
// through — before, every cell wrote the same file and only the last survived.
func rejectHandoffOnAcross(job Job) error {
	for i := range job.Plan {
		step := job.Plan[i]
		if len(step.Across) == 0 {
			continue
		}

		if handoff := step.Unwrap().Handoff; handoff != nil && handoff.Receives() {
			return fmt.Errorf("job %q step %d: handoff context/tool is not supported on an across: step — a to: route targets the matrix, not any one cell, so no cell can be said to have been routed to", job.Name, i)
		}
	}

	return nil
}

// rejectHandoffOnHook rejects handoff: on a hook step.
func rejectHandoffOnHook(label string, step *Step) error {
	if step.Handoff != nil {
		return fmt.Errorf("%s: handoff is not valid on hook steps", label)
	}

	return nil
}

// validateHandoffSegments splits job's plan into get-bounded segments (the
// same split validatePlanSegments uses) and validates each segment's
// handoff: steps against it.
func (c *Config) validateHandoffSegments(job Job) error {
	var segment []int

	flush := func() error {
		if len(segment) == 0 {
			return nil
		}

		err := validateHandoffSegment(job, segment)
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

// validateHandoffSegment checks every handoff: step in segment (plan indices
// in declaration order) is an agent step, enables something, and is named by
// at least one to: target anywhere in the segment.
func validateHandoffSegment(job Job, segment []int) error {
	targeted := map[string]bool{}

	for segPos, idx := range segment {
		for _, target := range job.Plan[idx].To {
			// `next` names no step, but the step it lands on is as much a
			// route target as a named one — it can be arrived at by a verdict,
			// so it can want a handoff. Resolve it to that step's name, or
			// this rejects a legitimate handoff: as unreachable.
			if target == RouteTargetNext {
				if segPos+1 < len(segment) {
					targeted[stepName(job.Plan[segment[segPos+1]])] = true
				}

				continue
			}

			targeted[target] = true
		}
	}

	for _, idx := range segment {
		step := job.Plan[idx]
		label := fmt.Sprintf("job %q step %d", job.Name, idx)

		err := checkHandoffStep(label, step, targeted)
		if err != nil {
			return err
		}

		// A try: wrapper is transparent to handoff:, which belongs on the
		// agent step it wraps — the wrapper is not itself an agent step, so
		// the check above rejects it there. Without this second pass the
		// wrapped agent's handoff: went entirely unvalidated AND unwired: it
		// loaded clean and then reached the agent as nil, so a tolerated agent
		// answered a redo as if freshly started.
		if step.Try != nil {
			err = checkHandoffStep(label, step.Unwrap(), targeted)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// checkHandoffStep validates one step's handoff:, if it sets one: agent-only,
// enables something, and — for the backward (receiving) half — is named by at
// least one to: route in its segment. targeted is that segment's set of route
// target names.
func checkHandoffStep(label string, step Step, targeted map[string]bool) error {
	if step.Handoff == nil {
		return nil
	}

	if step.Agent == "" {
		return fmt.Errorf("%s: handoff is only valid on agent steps", label)
	}

	if !step.Handoff.Enabled() {
		return fmt.Errorf("%s: handoff enables nothing (set context, tool, and/or note)", label)
	}

	// Only the backward half needs a route to receive from; a note-only
	// handoff writes forward along the plan and has no such requirement.
	if step.Handoff.Receives() && !targeted[stepName(step)] {
		return fmt.Errorf("%s: handoff sets context/tool but no to: route in this segment targets step %q", label, stepName(step))
	}

	return nil
}
