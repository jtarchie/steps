package config

// A step's context: — reading named earlier steps' decisions. See
// contextfrom.go for the from: mapping itself and its validation.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ContextSpec is a step's context: — what this step reads from named earlier
// steps.
type ContextSpec struct {
	// From is what this step reads from named earlier steps: their verdict,
	// and optionally the note and response that came with it. Declared on the
	// READER — nothing arrives unasked — and the demand is what makes the
	// sender's note required. See contextfrom.go.
	From map[string]FromLevel
}

// UnmarshalYAML decodes a ContextSpec from its one mapping key, from:.
func (c *ContextSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("step context at line %d must be a {from: ...} mapping", value.Line)
	}

	err := rejectUnknownKeys(value, "step context", "from")
	if err != nil {
		return err
	}

	var m struct {
		From yaml.Node `yaml:"from"`
	}

	err = value.Decode(&m)
	if err != nil {
		return fmt.Errorf("step context: %w", err)
	}

	if m.From.IsZero() {
		return fmt.Errorf("step context at line %d enables nothing (set from:)", value.Line)
	}

	c.From, err = decodeContextFrom(&m.From)

	return err
}

// validateContextSteps rejects context: on a hook step. A hook is a reaction
// that runs outside the plan's ordering and outside validateContextFrom's walk
// (which only visits job.Plan), so a hook's context: from: would otherwise
// load clean and silently do nothing.
func (c *Config) validateContextSteps() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitHookSteps(rejectContextOnHook)
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectContextOnHook rejects context: on a hook step.
func rejectContextOnHook(label string, step *Step) error {
	if step.Context != nil {
		return fmt.Errorf("%s: context is not valid on hook steps", label)
	}

	return nil
}
