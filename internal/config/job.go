package config

// A job: its plan, and the walks over the steps and hooks in it.

import (
	"fmt"
	"log/slog"
)

// Job is a named sequence of steps to run.
type Job struct {
	Name  string `yaml:"name"`
	Plan  []Step `yaml:"plan"`
	Hooks Hooks  `yaml:",inline"`
	// Assert, on a job, names the ordered set of task/agent/hook names the
	// job's run must have produced (see Assert). A match clears the plan's
	// failure; a mismatch fails the job. Never hashed.
	Assert *Assert `yaml:"assert,omitempty"`
}

// visitHookSteps calls fn for every hook step reachable from a job (each plan
// step's hooks, recursively, and each job-level hook) — but NOT the plan steps
// themselves, so a validator can treat hook steps differently from plan steps.
func (j Job) visitHookSteps(fn func(label string, step *Step) error) error {
	jobLabel := fmt.Sprintf("job %q", j.Name)

	for i := range j.Plan {
		err := j.Plan[i].Hooks.Each(func(name string, hook *Step) error {
			return visitStepTree(fmt.Sprintf("%s step %d (%s hook)", jobLabel, i, name), hook, fn)
		})
		if err != nil {
			return err
		}
	}

	return j.Hooks.Each(func(name string, hook *Step) error {
		return visitStepTree(fmt.Sprintf("%s %s hook", jobLabel, name), hook, fn)
	})
}

// visitSteps calls fn for every step reachable from a job: each plan step,
// each job-level hook, and recursively every hook carried by any of those
// steps. label is a human-readable path such as
// `job "deploy" step 2 (on_failure hook)`, so a validator's error message
// points at the exact step. Used to give hook steps identical treatment to
// plan steps in the image/artifact/fix validators below.
func (j Job) visitSteps(fn func(label string, step *Step) error) error {
	jobLabel := fmt.Sprintf("job %q", j.Name)

	for i := range j.Plan {
		err := visitStepTree(fmt.Sprintf("%s step %d", jobLabel, i), &j.Plan[i], fn)
		if err != nil {
			return err
		}
	}

	return j.Hooks.Each(func(name string, step *Step) error {
		return visitStepTree(fmt.Sprintf("%s %s hook", jobLabel, name), step, fn)
	})
}

// FindJob returns the job with the given name, or an error if not found.
func (c *Config) FindJob(name string) (*Job, error) {
	slog.Debug("job.find", "name", name)

	for i := range c.Jobs {
		if c.Jobs[i].Name == name {
			slog.Debug("job.find", "name", name, "steps", len(c.Jobs[i].Plan), "found", true)

			return &c.Jobs[i], nil
		}
	}

	return nil, fmt.Errorf("no job named %q (available: %v)", name, c.JobNames())
}

// JobNames returns the names of every job in the pipeline, in declaration
// order, for use in "which job?" error messages.
func (c *Config) JobNames() []string {
	names := make([]string, 0, len(c.Jobs))
	for _, j := range c.Jobs {
		names = append(names, j.Name)
	}

	return names
}
