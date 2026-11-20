package config

// The Concourse-style hook set (on_success/on_failure/on_error/on_abort/
// ensure) a job or step can carry, and what a hook step may not do.

import (
	"errors"
	"fmt"
)

// Hooks is the Concourse-style hook set a Step or a Job can carry. Each hook
// is itself a full Step, blocks included, short of the kinds hookBodyRefusal
// names; a hook may recursively carry its own Hooks. on_success
// runs after a green outcome, on_failure/on_error/on_abort after the matching
// failure classification, and ensure always runs last regardless of outcome.
type Hooks struct {
	OnSuccess *Step `yaml:"on_success,omitempty"`
	OnFailure *Step `yaml:"on_failure,omitempty"`
	OnError   *Step `yaml:"on_error,omitempty"`
	OnAbort   *Step `yaml:"on_abort,omitempty"`
	Ensure    *Step `yaml:"ensure,omitempty"`
	// OnPending runs when a request is newly parked for a person, before the wait, reading the park as ((.:pending.*)). Unlike the others it fires mid-step and only the nearest enclosing one fires: both firing would send every notice twice.
	OnPending *Step `yaml:"on_pending,omitempty"`
}

// Empty reports whether no hook is set.
func (h Hooks) Empty() bool {
	return h.OnSuccess == nil && h.OnFailure == nil && h.OnError == nil && h.OnAbort == nil && h.Ensure == nil && h.OnPending == nil
}

// Each calls fn for every non-nil hook, in a fixed order (on_success,
// on_failure, on_error, on_abort, ensure, on_pending), passing the hook's YAML name.
func (h Hooks) Each(fn func(name string, step *Step) error) error {
	pairs := []struct {
		name string
		step *Step
	}{
		{"on_success", h.OnSuccess},
		{"on_failure", h.OnFailure},
		{"on_error", h.OnError},
		{"on_abort", h.OnAbort},
		{"ensure", h.Ensure},
		{"on_pending", h.OnPending},
	}

	for _, p := range pairs {
		if p.step == nil {
			continue
		}

		err := fn(p.name, p.step)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateHooks enforces the hook-body restrictions. A hook body is any step,
// blocks included, as in Concourse — except the kinds hookBodyRefusal names,
// anywhere in the body. A job-level hook — and every hook nested under it —
// may not declare inputs:/outputs: (a job-level hook runs in the job's own
// build workspace, which for a get-leading plan holds no artifacts; a nested
// hook runs in that same workspace, so it has no more claim to a coherent
// artifact scope than its parent).
func (c *Config) validateHooks() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Get != "" && step.Hooks.OnPending != nil {
				return fmt.Errorf("%s: on_pending is not valid on a get; a get never waits on a person, so there is nothing to announce", label)
			}

			return c.validateHookTree(label, step.Hooks, false)
		})
		if err != nil {
			return err
		}

		err = c.validateHookTree(fmt.Sprintf("job %q", job.Name), job.Hooks, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateHookTree checks each hook's body for a refused kind at any depth,
// and noArtifacts — true for a job-level hook — on the body itself and the
// hooks it carries, recursively. What a block or try: in the body contains
// declares its own artifacts, as the step a job-level try: wraps always has.
func (c *Config) validateHookTree(parentLabel string, hooks Hooks, noArtifacts bool) error {
	return hooks.Each(func(name string, body *Step) error {
		label := fmt.Sprintf("%s (%s hook)", parentLabel, name)

		if noArtifacts && (body.InputsDeclared() || body.Outputs != nil) {
			return fmt.Errorf("%s: inputs/outputs are not valid on job-level hooks", label)
		}

		err := visitStepTree(label, body, func(label string, step *Step) error {
			err := hookBodyRefusal(step)
			if err == nil && name == "on_pending" {
				err = c.parkRefusal(step)
			}

			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}

			return nil
		})
		if err != nil {
			return err
		}

		return c.validateHookTree(label, body.Hooks, noArtifacts)
	})
}

// parkRefusal refuses an agent granted ask_user in an on_pending body: a park cannot park.
func (c *Config) parkRefusal(step *Step) error {
	if step.Agent == "" {
		return nil
	}

	// An agent that does not resolve is another validator's to report.
	invocation, err := c.ResolveAgentInvocation(*step)
	if err == nil && GrantsAskUser(invocation.ToolSpecs) {
		return fmt.Errorf("agent %q is granted %s, which is not valid in an on_pending body; a park cannot park", step.Agent, AskUserBuiltinName)
	}

	return nil
}

// hookBodyRefusal refuses what a hook body may not hold at any depth; everything else runs (internal/pipeline runHookStep), so a hook that loads is a hook that runs.
func hookBodyRefusal(step *Step) error {
	kind, ok := step.Kind()
	if !ok {
		return errors.New("unrecognized hook step (must name a step kind such as task, put, agent or do)")
	}

	if step.Hooks.OnPending != nil {
		return errors.New("on_pending is not valid inside a hook; a hook never waits on a person, so it has nothing to announce")
	}

	switch kind { //nolint:exhaustive // default admits every kind the runner executes
	case StepKindGet:
		// Deliberate divergence (docs/conformance.md): a Concourse get in a
		// hook is a job input the scheduler picks a version for before the
		// build starts. Here a get fans the rest of the plan out per version,
		// which has nowhere to go inside a hook.
		return errors.New("get is not valid in a hook; a hook can run any other step, but a get picks the versions a build runs against, which a hook reacting to that build cannot")
	case StepKindLoadVar:
		return errors.New("load_var: is not valid in a hook; a hook runs after the step it guards, so a value it loaded would reach the plan steps after it and let a hook's outcome steer the plan")
	case StepKindApproval:
		return errors.New("approval: is not valid in a hook; a hook reacts to an outcome, and an on_abort or ensure waiting on a person would hold a stopping job open")
	case StepKindAsk:
		return errors.New("ask: is not valid in a hook; a hook reacts to an outcome, and one waiting on a person would hold a stopping job open")
	default:
		return nil
	}
}
