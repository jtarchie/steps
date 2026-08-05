package config

// The passed: constraint — only run against versions already green upstream.

import "fmt"

// validatePassed enforces that passed: is a get-step field naming real jobs
// that actually fetch the same resource.
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
		if !jobFetches(*upstreamJob, resource) {
			return fmt.Errorf("%s: passed names job %q, which never gets resource %q, so no version of it could ever pass there",
				label, upstream, resource)
		}
	}

	return nil
}

// jobFetches reports whether a job's plan gets the named resource, under its
// own name or as an alias.
func jobFetches(job Job, resource string) bool {
	found := false

	_ = job.visitSteps(func(_ string, step *Step) error {
		if step.Get == "" {
			return nil
		}

		name := step.Get
		if step.Resource != "" {
			name = step.Resource
		}

		if name == resource {
			found = true
		}

		return nil
	})

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
