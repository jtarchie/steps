package config

// The Concourse-style hook set (on_success/on_failure/on_error/on_abort/
// ensure) a job or step can carry, and what a hook step may not do.

import (
	"fmt"
)

// Hooks is the Concourse-style hook set a Step or a Job can carry. Each hook
// is itself a full Step restricted to task/put/agent kinds (get is rejected at
// LoadConfig time); a hook may recursively carry its own Hooks. on_success
// runs after a green outcome, on_failure/on_error/on_abort after the matching
// failure classification, and ensure always runs last regardless of outcome.
type Hooks struct {
	OnSuccess *Step `yaml:"on_success,omitempty"`
	OnFailure *Step `yaml:"on_failure,omitempty"`
	OnError   *Step `yaml:"on_error,omitempty"`
	OnAbort   *Step `yaml:"on_abort,omitempty"`
	Ensure    *Step `yaml:"ensure,omitempty"`
}

// Empty reports whether no hook is set.
func (h Hooks) Empty() bool {
	return h.OnSuccess == nil && h.OnFailure == nil && h.OnError == nil && h.OnAbort == nil && h.Ensure == nil
}

// Each calls fn for every non-nil hook, in a fixed order (on_success,
// on_failure, on_error, on_abort, ensure), passing the hook's YAML name.
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

// validateHooks enforces the hook-body restrictions: a hook must be a
// task/put/agent step (get is rejected — a get step fans the remainder of the
// plan out per version, which has no meaning inside a hook), and a job-level
// hook — and everything nested under it, recursively — may not declare
// inputs:/outputs: (a job-level hook runs in the job's own build workspace,
// which for a get-leading plan holds no artifacts; a nested hook runs in that
// exact same workspace, not a fresh one, so it has no more claim to a
// coherent artifact scope than its parent). Nested hooks recurse throughout.
func (c *Config) validateHooks() error {
	for _, job := range c.Jobs {
		for i := range job.Plan {
			err := validateHookTree(fmt.Sprintf("job %q step %d", job.Name, i), job.Plan[i].Hooks, false)
			if err != nil {
				return err
			}
		}

		err := validateHookTree(fmt.Sprintf("job %q", job.Name), job.Hooks, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// noArtifacts is true for a job-level hook and everything nested under it —
// see validateHooks.
func validateHookTree(parentLabel string, hooks Hooks, noArtifacts bool) error {
	return hooks.Each(func(name string, step *Step) error {
		label := fmt.Sprintf("%s (%s hook)", parentLabel, name)

		if noArtifacts && (step.InputsDeclared() || step.Outputs != nil) {
			return fmt.Errorf("%s: inputs/outputs are not valid on job-level hooks", label)
		}

		err := validateHookStep(label, step)
		if err != nil {
			return err
		}

		return validateHookTree(label, step.Hooks, noArtifacts)
	})
}

func validateHookStep(label string, step *Step) error {
	kind, ok := step.Kind()
	if !ok {
		return fmt.Errorf("%s: unrecognized hook step (must be task, put, or agent)", label)
	}

	if kind == StepKindGet {
		return fmt.Errorf("%s: get is not valid in a hook; hooks must be task, put, or agent steps", label)
	}

	return nil
}
