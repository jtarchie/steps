package config

// Timeout parsing, and the timeout: field wherever it appears.

import (
	"fmt"
	"time"
)

// validateTimeouts checks that all timeout: fields contain valid Go duration
// strings (e.g., "2m", "30s", "1h30m"). Empty timeout is valid (no timeout).
func (c *Config) validateTimeouts() error {
	err := c.validateTaskTimeouts()
	if err != nil {
		return err
	}

	err = c.validateJobTimeouts()
	if err != nil {
		return err
	}

	err = c.validateResourceTimeouts()
	if err != nil {
		return err
	}

	err = c.validateParksWithinJobs()
	if err != nil {
		return err
	}

	return c.validateStepTimeouts()
}

// validateParksWithinJobs refuses a park whose own timeout: outlives its job's. The job's deadline is checked between steps, so the park would wait out its whole timeout and the job would then fail at the next step, after somebody had answered.
func (c *Config) validateParksWithinJobs() error {
	for _, job := range c.Jobs {
		limit, err := ParseTimeout(job.Timeout)
		if job.Timeout == "" || err != nil {
			continue
		}

		err = job.visitSteps(func(label string, step *Step) error {
			for _, park := range c.parkTimeouts(step) {
				wait, err := ParseTimeout(park.timeout)
				if err == nil && wait > limit {
					return fmt.Errorf("%s: %s %s outlives the job's timeout: %s; the job's deadline is checked only between steps, so the step would wait it out and the job then fail after somebody answered; shorten one or lengthen the other",
						label, park.field, park.timeout, job.Timeout)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

type parkTimeout struct{ field, timeout string }

// parkTimeouts are the explicit waits on a person a step declares; a default wait is not counted.
func (c *Config) parkTimeouts(step *Step) []parkTimeout {
	if step.Approval != nil && step.Approval.Timeout != "" {
		return []parkTimeout{{"approval.timeout", step.Approval.Timeout}}
	}

	if step.Ask != "" && step.Timeout != "" {
		return []parkTimeout{{"ask timeout", step.Timeout}}
	}

	if step.Agent == "" {
		return nil
	}

	// An agent that does not resolve is another validator's to report.
	invocation, err := c.ResolveAgentInvocation(*step)
	if err != nil {
		return nil
	}

	var parks []parkTimeout

	for _, spec := range invocation.ToolSpecs {
		if spec.Builtin == AskUserBuiltinName && spec.Timeout != "" {
			parks = append(parks, parkTimeout{AskUserBuiltinName + " timeout", spec.Timeout})
		}
	}

	return parks
}

// validateResourceTimeouts checks check_timeout: zero is refused for the reason a job's timeout: is, and the empty field means the default.
func (c *Config) validateResourceTimeouts() error {
	for _, res := range c.Resources {
		if res.CheckTimeout == "" {
			continue
		}

		d, err := ParseTimeout(res.CheckTimeout)
		if err != nil {
			return fmt.Errorf("resource %q: check_timeout: %w", res.Name, err)
		}

		if d <= 0 {
			return fmt.Errorf("resource %q: check_timeout must be a positive duration (omit it for the default of %s)", res.Name, DefaultCheckTimeout)
		}
	}

	return nil
}

// validateJobTimeouts checks the job-level wall-clock deadline.
func (c *Config) validateJobTimeouts() error {
	for _, job := range c.Jobs {
		if job.Timeout == "" {
			continue
		}

		d, err := ParseTimeout(job.Timeout)
		if err != nil {
			return fmt.Errorf("job %q: timeout: %w", job.Name, err)
		}

		// Zero would read as "no deadline" to anyone writing it and as "expire
		// immediately" to anything enforcing it. Neither reading is safe to
		// guess at, so it is a load error and the empty field means none.
		if d <= 0 {
			return fmt.Errorf("job %q: timeout must be a positive duration (omit it entirely for no deadline)", job.Name)
		}
	}

	return nil
}

// validateTaskTimeouts checks all tasks: entries for valid timeout values.
func (c *Config) validateTaskTimeouts() error {
	for _, task := range c.Tasks {
		if task.Timeout != "" {
			d, err := ParseTimeout(task.Timeout)
			if err != nil {
				return fmt.Errorf("task %q: %w", task.Name, err)
			}

			if d == 0 {
				return fmt.Errorf("task %q: timeout must be a positive duration (omit it entirely for no deadline)", task.Name)
			}
		}

		if task.Fix != nil && task.Fix.Timeout != "" {
			_, err := ParseTimeout(task.Fix.Timeout)
			if err != nil {
				return fmt.Errorf("task %q fix: %w", task.Name, err)
			}
		}
	}

	return nil
}

// validateStepTimeouts checks all step timeout: fields for valid values.
//
// A zero duration is accepted on an AGENT step and rejected everywhere else.
// The asymmetry is not a special case so much as the absence of one: an agent
// step is the only kind that gets a deadline it never asked for (see
// DefaultAgentStepTimeout below), so it is the only kind where "no deadline"
// needs a spelling at all. On a task/get/put step the empty field already
// says it, and a 0 there would be the ambiguity validateJobTimeouts rejects
// for the same reason.
func (c *Config) validateStepTimeouts() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Timeout != "" {
				d, err := ParseTimeout(step.Timeout)
				if err != nil {
					return fmt.Errorf("%s: %w", label, err)
				}

				if d == 0 && step.Agent == "" {
					return fmt.Errorf("%s: timeout must be a positive duration (omit it entirely for no deadline)", label)
				}
			}

			if step.Fix != nil && step.Fix.Timeout != "" {
				_, err := ParseTimeout(step.Fix.Timeout)
				if err != nil {
					return fmt.Errorf("%s fix: %w", label, err)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// DefaultAgentStepTimeout is the wall-clock ceiling an agent step gets when
// it declares no timeout: of its own (see docs/attempts-timeout.md, "Agent
// steps: the one implicit deadline"). Kept here rather than in
// internal/agent so internal/web can resolve the same effective deadline a
// running step is actually held to — depguard forbids internal/web from
// importing internal/agent.
const DefaultAgentStepTimeout = 30 * time.Minute

// ResolvedAgentTimeout applies the default-and-zero-means-unlimited
// convention documented in docs/attempts-timeout.md to an agent step's
// timeout: string: empty resolves to DefaultAgentStepTimeout, "0" resolves
// to 0 (no deadline — the caller's convention for "unlimited"), and an
// (unreachable against a validated config) parse error falls back to the
// default rather than to "no deadline" — a typo must never silently produce
// an unbounded step.
func ResolvedAgentTimeout(riTimeout string) time.Duration {
	if riTimeout == "" {
		return DefaultAgentStepTimeout
	}

	parsed, err := ParseTimeout(riTimeout)
	if err != nil {
		return DefaultAgentStepTimeout
	}

	return parsed
}

// ParseTimeout parses a timeout string into a time.Duration. Empty string
// is valid and returns 0 (no timeout). Returns an error for invalid Go
// duration format (e.g., "2m", "30s", "1h30m" are valid; "2 minutes" is not).
func ParseTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid timeout %q: %w", s, err)
	}

	if d < 0 {
		return 0, fmt.Errorf("invalid timeout %q: must not be negative", s)
	}

	return d, nil
}
