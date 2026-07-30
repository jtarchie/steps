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

	return c.validateStepTimeouts()
}

// validateTaskTimeouts checks all tasks: entries for valid timeout values.
func (c *Config) validateTaskTimeouts() error {
	for _, task := range c.Tasks {
		if task.Timeout != "" {
			_, err := ParseTimeout(task.Timeout)
			if err != nil {
				return fmt.Errorf("task %q: %w", task.Name, err)
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
func (c *Config) validateStepTimeouts() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Timeout != "" {
				_, err := ParseTimeout(step.Timeout)
				if err != nil {
					return fmt.Errorf("%s: %w", label, err)
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
