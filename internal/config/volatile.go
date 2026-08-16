package config

// volatile: — a step declaring that its result must never be reused.

import "fmt"

// validateVolatileSteps rejects volatile: where nothing would ever have
// reused the step anyway: on a hook step, and on a step that is neither a task
// nor an agent.
//
// Rejecting rather than ignoring, because a volatile: that reads as
// configured while binding nothing is exactly the misconfiguration the field
// exists to prevent — an author who wrote it believes this step re-runs.
func (c *Config) validateVolatileSteps() error {
	for i := range c.Jobs {
		err := c.Jobs[i].visitHookSteps(rejectVolatileOnHook)
		if err != nil {
			return err
		}

		err = c.Jobs[i].visitSteps(checkVolatileStep)
		if err != nil {
			return err
		}
	}

	return nil
}

// rejectVolatileOnHook rejects volatile: on a hook step. A hook runs outside
// the plan's ordering and is never looked up in the step cache, so the field
// would load clean and do nothing.
func rejectVolatileOnHook(label string, step *Step) error {
	if step.Volatile {
		return fmt.Errorf("%s: volatile is not valid on hook steps", label)
	}

	return nil
}

// checkVolatileStep rejects volatile: on a step the cache never reuses: only a
// task and an agent produce a result keyed by their declared inputs.
func checkVolatileStep(label string, step *Step) error {
	if !step.Volatile {
		return nil
	}

	if step.Task == "" && step.Agent == "" {
		return fmt.Errorf("%s: volatile is only valid on task and agent steps", label)
	}

	return nil
}
