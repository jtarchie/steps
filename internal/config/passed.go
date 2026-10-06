package config

// The passed: constraint — only run against versions already green upstream.

import (
	"fmt"
	"slices"
)

// validatePassed enforces that passed: is a get-step field naming real jobs
// that actually get or put the same resource.
//
// This is a correctness gap rather than a convenience: without it, `steps
// watch` can trigger `deploy` on a commit that the `test` job already FAILED
// on, and there is no way to express "don't deploy unless the tests were green
// for this exact commit".
func (c *Config) validatePassed() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.Passed) == 0 {
				return nil
			}

			return c.validatePassedStep(label, job.Name, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validatePassedStep(label, jobName string, step *Step) error {
	if step.Get == "" {
		return fmt.Errorf("%s: passed is only valid on get steps; it constrains which VERSION is fetched", label)
	}

	resource := step.Get
	if step.Resource != "" {
		resource = step.Resource
	}

	seen := map[string]bool{}

	for _, upstream := range step.Passed {
		if upstream == jobName {
			return fmt.Errorf("%s: passed names its own job %q, which can never have passed a version before this run of it", label, jobName)
		}

		if seen[upstream] {
			return fmt.Errorf("%s: passed names job %q twice", label, upstream)
		}

		seen[upstream] = true

		upstreamJob, err := c.FindJob(upstream)
		if err != nil {
			return fmt.Errorf("%s: passed: %w", label, err)
		}

		// An upstream job that never fetches this resource can never mark a
		// version of it green, so the constraint would block forever — a
		// deadlock spelled as a typo.
		if !jobUses(*upstreamJob, resource) {
			return fmt.Errorf("%s: passed names job %q, which neither gets nor puts resource %q, so no version of it could ever pass there",
				label, upstream, resource)
		}
	}

	return nil
}

// jobUses reports whether a job's plan gets the named resource (under its own
// name or as an alias) or puts it: both record a version against a green
// build. A put has no alias. Job-level on_failure/on_error/on_abort hooks run
// only when the build is not green, so a put there can never pass and is not
// counted; step-level failure hooks are, since under try: their build can be
// green.
func jobUses(job Job, resource string) bool {
	found := false

	check := func(_ string, step *Step) error {
		name := step.Put
		if step.Get != "" {
			name = step.Get
			if step.Resource != "" {
				name = step.Resource
			}
		}

		if name == resource {
			found = true
		}

		return nil
	}

	for i := range job.Plan {
		_ = visitStepTree("", &job.Plan[i], check)
	}

	for _, hook := range []*Step{job.Hooks.OnSuccess, job.Hooks.Ensure} {
		if hook != nil {
			_ = visitStepTree("", hook, check)
		}
	}

	return found
}

// PassedConstraints returns every (resource, upstream jobs) pair a job's plan
// declares, so the trigger can ask whether a version is green everywhere it
// needs to be before enqueueing.
func (j Job) PassedConstraints() map[string][]string {
	constraints := map[string][]string{}

	_ = j.visitSteps(func(_ string, step *Step) error {
		if step.Get == "" || len(step.Passed) == 0 {
			return nil
		}

		resource := step.Get
		if step.Resource != "" {
			resource = step.Resource
		}

		constraints[resource] = append(constraints[resource], step.Passed...)

		return nil
	})

	return constraints
}

// JobInput is one resource a job's plan gets, as the pipeline graph draws it:
// whether any get of it triggers the job, and every job it must have passed.
type JobInput struct {
	Resource string
	Trigger  bool
	Passed   []string
}

// Inputs is every resource a job gets, in plan order, hooks included — the
// same walk as Concourse's JobConfig.Inputs. Concourse lists each get step;
// this folds gets of one resource together (trigger if any triggers, passed
// unioned), since the graph draws one edge per resource and job either way.
func (j Job) Inputs() []JobInput {
	var inputs []JobInput

	at := map[string]int{}

	_ = j.visitSteps(func(_ string, step *Step) error {
		if step.Get == "" {
			return nil
		}

		resource := step.GetResourceName()

		i, seen := at[resource]
		if !seen {
			i = len(inputs)
			at[resource] = i
			inputs = append(inputs, JobInput{Resource: resource})
		}

		inputs[i].Trigger = inputs[i].Trigger || step.Trigger

		for _, upstream := range step.Passed {
			if !slices.Contains(inputs[i].Passed, upstream) {
				inputs[i].Passed = append(inputs[i].Passed, upstream)
			}
		}

		return nil
	})

	return inputs
}

// Outputs is every resource a job puts, in plan order and once each, hooks
// included — a put in on_failure is still something the job can change, and
// Concourse's JobConfig.Outputs counts it too.
func (j Job) Outputs() []string {
	var outputs []string

	_ = j.visitSteps(func(_ string, step *Step) error {
		if step.Put != "" && !slices.Contains(outputs, step.PutResourceName()) {
			outputs = append(outputs, step.PutResourceName())
		}

		return nil
	})

	return outputs
}
