package config

// Per-tool guards — required:, max_calls:, args:, max_output_bytes: — and
// which tool forms each is valid on.

import (
	"fmt"
)

// validateToolCallGuards rejects max_calls:/args: set on anything but a
// custom tool: both fields change what a custom tool's run: command executes
// with, and neither has meaning on a builtin (whose implementation is fixed
// Go code) or a sub-agent tool (whose only argument is the model-authored
// request string — pinning or budgeting that is not the shape this feature
// targets; revisit if a real need shows up).
func (c *Config) validateToolCallGuards() error {
	err := c.validateAgentToolCallGuards()
	if err != nil {
		return err
	}

	err = c.validateTaskFixToolCallGuards()
	if err != nil {
		return err
	}

	return c.validateStepToolCallGuards()
}

func (c *Config) validateAgentToolCallGuards() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkToolCallGuardSpecs(fmt.Sprintf("agent %q", agent.Name), agent.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateTaskFixToolCallGuards() error {
	for i := range c.Tasks {
		fix := c.Tasks[i].Fix
		if fix == nil {
			continue
		}

		err := checkToolCallGuardSpecs(fmt.Sprintf("task %q fix", c.Tasks[i].Name), fix.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateStepToolCallGuards() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := checkToolCallGuardSpecs(label, step.Tools)
			if err != nil {
				return err
			}

			if step.Fix == nil {
				return nil
			}

			return checkToolCallGuardSpecs(label+" fix", step.Fix.Tools)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkToolCallGuardSpecs runs validateToolCallGuardShape over every spec in
// specs, stopping at the first error.
func checkToolCallGuardSpecs(context string, specs []ToolSpec) error {
	for _, spec := range specs {
		err := validateToolCallGuardShape(context, spec)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateToolCallGuardShape rejects max_calls:/args: on a builtin or
// sub-agent tool, a negative max_calls: on any tool, and (since a mapping-
// form builtin — {builtin: name, ...} — is otherwise indistinguishable from a
// custom tool once other fields are present) a builtin mixed with name:/run:.
func validateToolCallGuardShape(context string, spec ToolSpec) error {
	if spec.Builtin != "" && (spec.Name != "" || spec.Run != "") {
		return fmt.Errorf("%s: builtin tool %q must not also set name/run", context, spec.Builtin)
	}

	if spec.MaxCalls < 0 {
		return fmt.Errorf("%s: tool %q: max_calls must be >= 0", context, ToolSpecName(spec))
	}

	err := validateMaxOutputBytesShape(context, spec)
	if err != nil {
		return err
	}

	guarded := spec.MaxCalls != 0 || spec.Args != nil
	if !guarded {
		return nil
	}

	if spec.Builtin != "" {
		return fmt.Errorf("%s: builtin tool %q: max_calls/args are only valid on custom tools", context, spec.Builtin)
	}

	if spec.Agent != "" {
		return fmt.Errorf("%s: sub-agent tool %q: max_calls/args are only valid on custom tools", context, spec.Agent)
	}

	return nil
}

// validateMaxOutputBytesShape enforces where max_output_bytes: may appear.
// It tunes a tool's inline output budget, which only makes sense for a
// tool whose output volume is not already a designed property: a built-in
// carries its own contract (read_file pages, list_dir counts, search_files
// is bounded by arithmetic), and a sub-agent's result is a considered
// answer rather than a data dump. See ToolSpec.MaxOutputBytes.
func validateMaxOutputBytesShape(context string, spec ToolSpec) error {
	if spec.MaxOutputBytes == 0 {
		return nil
	}

	if spec.MaxOutputBytes < 0 {
		return fmt.Errorf("%s: tool %q: max_output_bytes must be >= 0", context, ToolSpecName(spec))
	}

	if spec.Builtin != "" {
		return fmt.Errorf(
			"%s: builtin tool %q: max_output_bytes is only valid on custom tools and mcp grants; built-ins carry their own output contract",
			context, spec.Builtin,
		)
	}

	if spec.Agent != "" {
		return fmt.Errorf(
			"%s: sub-agent tool %q: max_output_bytes is only valid on custom tools and mcp grants",
			context, spec.Agent,
		)
	}

	return nil
}
