package config

// The env: field wherever it appears: which host environment variables a
// pipeline-defined command is allowed to see.

import (
	"fmt"
	"strings"
)

// validateEnvRules groups the env:-related load-time checks, so
// config.validate's own branch count doesn't grow with every env: rule added
// — mirroring validateImageRules.
func (c *Config) validateEnvRules() error {
	err := c.validateEnvValues()
	if err != nil {
		return err
	}

	return c.validateEnvPlacement()
}

// validateEnvPlacement rejects env: on get/put steps, mirroring
// validateImages: a put's execution environment comes from its resource type,
// and a get step has no task/agent to scope.
func (c *Config) validateEnvPlacement() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Env == nil {
				return nil
			}

			//kindswitch:ignore Task and Agent are the kinds env: is FOR — the cases here are the rejections
			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): env is not valid on get steps; set it on the resource_type instead", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): env is not valid on put steps; set it on the resource_type instead", label, step.Put)
			case step.Try != nil:
				// Same reasoning as image: on a try: wrapper — the wrapper's
				// value would be accepted and then ignored, since resolution
				// reads the wrapped step's env, never the wrapper's.
				return fmt.Errorf("%s: env is not valid on a try: step; set it on the step try: wraps", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateEnvValues rejects an env: entry that carries a value rather than
// naming one.
//
// env: is a list of variable NAMES, resolved from the operator's own
// environment when the command runs — the same name-not-value shape
// api_key_env: and webhook_token_env: use, and for the same reason: a
// resource's, task's, and agent's fields are hashed into the merkle content
// map, so a literal secret written here would be persisted to state.db in
// cleartext. Accepting "KEY=value" silently would put the value in exactly
// the place the whole convention exists to keep it out of.
func (c *Config) validateEnvValues() error {
	err := c.validateDeclaredEnvValues()
	if err != nil {
		return err
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkEnvNames(label, step.Env)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateDeclaredEnvValues checks the top-level declarations; the steps are
// walked by validateEnvValues itself. Split purely to stay inside the linter's
// per-function complexity budget.
func (c *Config) validateDeclaredEnvValues() error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := checkEnvNames(fmt.Sprintf("resource_type %q", rt.Name), rt.Env)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkEnvNames(fmt.Sprintf("agent %q", agent.Name), agent.Env)
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := checkEnvNames(fmt.Sprintf("task %q", task.Name), task.Env)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkEnvNames validates every entry of one env: list.
func checkEnvNames(context string, env []string) error {
	for _, name := range env {
		err := checkEnvName(context, name)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkEnvName rejects an env: entry that isn't a bare variable name.
func checkEnvName(context, name string) error {
	if name == "" {
		return fmt.Errorf("%s: env contains an empty variable name", context)
	}

	if strings.Contains(name, "=") {
		return fmt.Errorf("%s: env entry %q must be a variable NAME, not a KEY=VALUE pair — the value is read from the environment at run time (a literal would be hashed into state.db in cleartext)", context, name)
	}

	return nil
}
