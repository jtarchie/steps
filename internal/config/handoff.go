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
// predecessors), and on a plan step it is agent-only, must enable at least
// one of context/tool, and must be the target of at least one to: route
// within its own get-segment — otherwise the field is dead config.
func (c *Config) validateHandoffSteps() error {
	for i := range c.Jobs {
		job := c.Jobs[i]

		err := job.visitHookSteps(rejectHandoffOnHook)
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

	for _, idx := range segment {
		for _, target := range job.Plan[idx].To {
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

		// Recurse into in_parallel children: the wrapper itself has no agent,
		// but children inside a branch may carry handoff: and need validation.
		if step.InParallel != nil {
			err = checkInParallelHandoff(job, idx, step, targeted)
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

// checkInParallelHandoff validates handoff: on each child of an in_parallel
// block, recursing into nested in_parallel blocks. The wrapper itself has no
// agent; children inside a branch carry handoff: and need validation.
func checkInParallelHandoff(job Job, planIdx int, step Step, targeted map[string]bool) error {
	for childIdx := range step.InParallel.Steps {
		child := &step.InParallel.Steps[childIdx]
		label := fmt.Sprintf("job %q step %d (child %d)", job.Name, planIdx, childIdx)

		err := checkHandoffStep(label, *child, targeted)
		if err != nil {
			return err
		}

		if child.Try != nil {
			err = checkHandoffStep(label, child.Unwrap(), targeted)
			if err != nil {
				return err
			}
		}

		if child.InParallel != nil {
			err = checkInParallelHandoff(job, planIdx, *child, targeted)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
