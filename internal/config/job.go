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
	// Budget caps what every agent step in this job may spend TOGETHER (see
	// Budget). A cumulative ceiling, so the step that trips it is rarely the
	// one that cost the most. Never hashed.
	Budget *Budget `yaml:"budget,omitempty"`
	// Serial states that two builds of this job must never run at once.
	//
	// It is a statement of intent rather than a switch: this runner always
	// serializes builds of one job, so serial: true records that a pipeline
	// depends on that. There is deliberately no serial: false — it would
	// promise a parallelism this runner does not offer.
	Serial bool `yaml:"serial,omitempty"`
	// SerialGroups names locks this job holds while it runs. Two jobs sharing
	// a group never run at the same time, which is what stops two different
	// deploy jobs mutating one target concurrently.
	SerialGroups []string `yaml:"serial_groups,omitempty"`
	// MaxConsecutiveFailures pauses this job under `steps watch` once it has
	// failed this many triggered RUNS in a row, until someone resumes it.
	//
	// It counts runs, not the attempts: retries inside one — conflating them
	// would trip the breaker on ordinary flakiness a retry would have
	// absorbed, which is the opposite of the intent. 0 (the default) means no
	// breaker.
	MaxConsecutiveFailures int `yaml:"max_consecutive_failures,omitempty"`
	// Line is the job's source line in the pipeline file, filled in after
	// decoding (see stampLines). Never written in YAML and never hashed.
	Line int `yaml:"-"`
}

// visitHookSteps calls fn for every hook step reachable from a job (each plan
// step's hooks, recursively, and each job-level hook) — but NOT the plan steps
// themselves, so a validator can treat hook steps differently from plan steps.
func (j Job) visitHookSteps(fn func(label string, step *Step) error) error {
	jobLabel := fmt.Sprintf("job %q", j.Name)

	for i := range j.Plan {
		err := j.Plan[i].Hooks.Each(func(name string, hook *Step) error {
			return visitStepTree(fmt.Sprintf("%s step %d%s (%s hook)", jobLabel, i, j.Plan[i].at(), name), hook, fn)
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
		err := visitStepTree(fmt.Sprintf("%s step %d%s", jobLabel, i, j.Plan[i].at()), &j.Plan[i], fn)
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

// AgentNames lists every agent this job's plan invokes — plan steps, hook
// steps, any task's fix: agent, and any sub-agent granted inline on a step —
// in plan order, deduplicated.
//
// It exists for preflight, whose whole point is to check only what THIS job
// needs: a pipeline with ten agents whose job uses two must probe two.
// Sub-agents granted on an agents: entry rather than inline are expanded by
// the caller, which has the Config needed to resolve them.
func (j Job) AgentNames() []string {
	var names []string

	seen := map[string]bool{}

	add := func(name string) {
		if name == "" || seen[name] {
			return
		}

		seen[name] = true
		names = append(names, name)
	}

	_ = j.visitSteps(func(_ string, step *Step) error {
		add(step.Agent)

		if step.Fix != nil {
			add(step.Fix.Agent)
		}

		for _, spec := range step.Tools {
			add(spec.Agent) // a sub-agent grant is another model this job reaches
		}

		return nil
	})

	return names
}
