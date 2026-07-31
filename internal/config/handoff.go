package config

// A step's handoff: — the pushed <transition_context> prompt block and the
// pulled previous_run tool, and where in a plan they are legal.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// HandoffSpec is a step's handoff: (see Step.Handoff). Context enables the
// pushed <transition_context> prompt block; Tool enables the pulled
// previous_run tool. A scalar `handoff: true` is shorthand for {context:
// true}; a mapping's context defaults to true unless explicitly set false,
// so `handoff: {tool: true}` enables both.
type HandoffSpec struct {
	Context bool
	Tool    bool
}

// UnmarshalYAML decodes a HandoffSpec from either a scalar (handoff: true/
// false, context only) or a mapping ({context, tool}) YAML node — the same
// scalar-or-mapping idiom as WhenSpec/FixSpec.
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
		err := rejectUnknownKeys(value, "step handoff", "context", "tool")
		if err != nil {
			return err
		}

		var m struct {
			Context *bool `yaml:"context"`
			Tool    bool  `yaml:"tool"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step handoff: %w", err)
		}

		h.Context = m.Context == nil || *m.Context
		h.Tool = m.Tool

		return nil
	default:
		return fmt.Errorf("step handoff at line %d must be a boolean or a {context, tool} mapping", value.Line)
	}
}

// Enabled reports whether h turns on anything. A non-nil but all-false
// HandoffSpec (handoff: false, or handoff: {context: false}) is rejected at
// LoadConfig (validateHandoffSteps) rather than silently accepted as a no-op.
func (h *HandoffSpec) Enabled() bool {
	return h.Context || h.Tool
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
		if step.Handoff == nil {
			continue
		}

		label := fmt.Sprintf("job %q step %d", job.Name, idx)

		if step.Agent == "" {
			return fmt.Errorf("%s: handoff is only valid on agent steps", label)
		}

		if !step.Handoff.Enabled() {
			return fmt.Errorf("%s: handoff enables nothing (set context and/or tool)", label)
		}

		if !targeted[stepName(step)] {
			return fmt.Errorf("%s: handoff is set but no to: route in this segment targets step %q", label, stepName(step))
		}
	}

	return nil
}
