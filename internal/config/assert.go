package config

// The assert: directive in all three of its positions — pipeline (job
// names), job (step names), and step (stdout/exit code/tool-call trajectory).

import (
	"fmt"
	"sort"
)

// Assert is a self-verification directive, in one of two shapes depending on
// where it's attached:
//   - On a Config (top level) or a Job, only Execution is valid: an ordered
//     list of the names that must have run — job names for a Config, task/
//     agent/hook names for a Job. By omission it also asserts what must NOT
//     run. A matching Job assert clears the plan's failure, so one green
//     fixture can contain deliberately-failing tasks.
//   - On a task/agent Step, only Stdout/Code are valid: Stdout is a substring
//     the step's captured output must contain, Code the exact expected exit
//     code (task only). A matching assert makes a non-zero-exit task a
//     success.
type Assert struct {
	Execution []string `yaml:"execution,omitempty"`
	Stdout    *string  `yaml:"stdout,omitempty"`
	Code      *int     `yaml:"code,omitempty"`
	// ToolCalls, on an agent step, asserts the ordered trajectory of tool
	// calls the model made (see ExpectedToolCall). Agent-only: a task step
	// runs no tools. Every entry must appear, in order, as a subsequence of
	// the observed calls.
	ToolCalls []ExpectedToolCall `yaml:"tool_calls,omitempty"`
}

// ExpectedToolCall is one entry in an agent step's assert.tool_calls: the
// tool's name, plus (optionally) a subset of the arguments the model must
// have called it with. Args is a subset match — every listed key must be
// present with an equal value, and any extra actual argument is ignored.
// Values compare as strings, since every argument reaching a tool's run:
// template is rendered as one (this is a deliberate divergence from
// secret-agent's eval matcher, which coerces across int/float).
type ExpectedToolCall struct {
	Name string            `yaml:"name"`
	Args map[string]string `yaml:"args,omitempty"`
}

// validateAsserts enforces which Assert fields are valid where: a Config- or
// Job-level assert may only set execution:; a task/agent step's assert may
// only set stdout:/code: (and code: only on tasks). A step assert is rejected
// on get/put steps. Hook steps are walked too (via visitSteps), so an assert
// on a hook task/agent gets the same treatment.
func (c *Config) validateAsserts() error {
	if c.Assert != nil {
		err := requireExecutionOnly("pipeline assert", c.Assert)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		if job.Assert != nil {
			err := requireExecutionOnly(fmt.Sprintf("job %q assert", job.Name), job.Assert)
			if err != nil {
				return err
			}
		}

		err := job.visitSteps(func(label string, step *Step) error {
			stepErr := validateStepAssert(label, step)
			if stepErr != nil {
				return stepErr
			}

			return c.validateAssertPinnedArgs(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAssertPinnedArgs rejects an assert.tool_calls entry that asserts on
// an argument the pipeline pins via a custom tool's args: (see ToolSpec.Args).
// A pinned value is machine-supplied and never appears among the
// model-authored arguments a trajectory records, so such an assert could never
// match — failing the load is far clearer than a step that always fails.
//
// Best-effort by design: it fires only when the agent resolves here. An
// unresolvable agent name is left to run time, matching how every other
// agent/task reference in this package is treated.
func (c *Config) validateAssertPinnedArgs(label string, step *Step) error {
	if step.Assert == nil || len(step.Assert.ToolCalls) == 0 || step.Agent == "" {
		return nil
	}

	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable agent is caught at run time, same as everywhere else
	}

	pinned := pinnedArgsByTool(agent.Tools, step.Tools)

	for i, want := range step.Assert.ToolCalls {
		keys := make([]string, 0, len(want.Args))
		for key := range want.Args {
			keys = append(keys, key)
		}

		sort.Strings(keys) // deterministic message when several keys are pinned

		for _, key := range keys {
			if pinned[want.Name][key] {
				return fmt.Errorf("%s: assert.tool_calls[%d]: tool %q pins argument %q via args:, so it never appears in the model-authored call and can never match", label, i, want.Name, key)
			}
		}
	}

	return nil
}

// pinnedArgsByTool indexes which argument keys each named tool pins, across an
// agent's grant and a step's own inline tools.
func pinnedArgsByTool(agentTools, stepTools []ToolSpec) map[string]map[string]bool {
	index := map[string]map[string]bool{}

	add := func(specs []ToolSpec) {
		for _, spec := range specs {
			if len(spec.Args) == 0 {
				continue
			}

			name := ToolSpecName(spec)

			if index[name] == nil {
				index[name] = map[string]bool{}
			}

			for key := range spec.Args {
				index[name][key] = true
			}
		}
	}

	add(agentTools)
	add(stepTools)

	return index
}

// requireExecutionOnly rejects an execution-level assert (Config/Job) that
// carries the step-only stdout:/code: fields.
func requireExecutionOnly(label string, assert *Assert) error {
	if assert.Stdout != nil || assert.Code != nil {
		return fmt.Errorf("%s: stdout/code are only valid on task/agent step asserts, not an execution assert", label)
	}

	return nil
}

// validateStepAssert rejects a step assert that's misplaced (get/put) or
// carries the wrong fields for its step kind.
func validateStepAssert(label string, step *Step) error {
	if step.Assert == nil {
		return nil
	}

	if len(step.Assert.Execution) > 0 {
		return fmt.Errorf("%s: execution is only valid on job/pipeline asserts, not a step assert", label)
	}

	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: unrecognized step (must be get, task, put, or agent)", label)
	}

	switch kind { //nolint:exhaustive // default covers StepKindTask
	case StepKindGet:
		return fmt.Errorf("%s (get %q): assert is not valid on get steps", label, step.Get)
	case StepKindPut:
		return fmt.Errorf("%s (put %q): assert is not valid on put steps", label, step.Put)
	case StepKindAgent:
		if step.Assert.Code != nil {
			return fmt.Errorf("%s (agent %q): assert.code is not valid on agent steps (no exit code); use assert.stdout", label, step.Agent)
		}

		return validateExpectedToolCalls(fmt.Sprintf("%s (agent %q)", label, step.Agent), step.Assert.ToolCalls)
	default: // StepKindTask
		if len(step.Assert.ToolCalls) > 0 {
			return fmt.Errorf("%s: assert.tool_calls is only valid on agent steps (a task runs no tools)", label)
		}

		return nil
	}
}

// validateExpectedToolCalls rejects an assert.tool_calls entry with no name —
// there is nothing to match against, and an empty name would silently match
// the first call of any tool.
func validateExpectedToolCalls(context string, expected []ExpectedToolCall) error {
	for i, want := range expected {
		if want.Name == "" {
			return fmt.Errorf("%s: assert.tool_calls[%d]: name is required", context, i)
		}
	}

	return nil
}
