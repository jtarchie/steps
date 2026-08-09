package config

// The user: field wherever it appears: which container user a command runs as.

import (
	"fmt"
	"strings"
)

// validateUserRules groups the user:-related load-time checks, mirroring
// validateImageRules so config.validate's branch count doesn't grow per rule.
func (c *Config) validateUserRules() error {
	err := c.validateUserValues()
	if err != nil {
		return err
	}

	return c.validateUserPlacement()
}

// validateUserValues rejects a user: value docker's parser would read as a
// flag rather than a user spec. Exactly the reasoning behind
// validateImageValues: user: is passed as `--user <value>` BEFORE the "--"
// that makes the rest positional, so unlike image: there is no separator
// protecting it — a value of "--privileged" would be handed to docker run as
// its own flag. This check is the only thing standing between a mistyped or
// tainted user: and docker granting whatever that flag means.
func (c *Config) validateUserValues() error {
	for i := range c.ResourceTypes {
		rt := c.ResourceTypes[i]

		err := checkUserValue(fmt.Sprintf("resource_type %q", rt.Name), rt.User)
		if err != nil {
			return err
		}
	}

	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkUserValue(fmt.Sprintf("agent %q", agent.Name), agent.User)
		if err != nil {
			return err
		}
	}

	for i := range c.Tasks {
		task := c.Tasks[i]

		err := checkUserValue(fmt.Sprintf("task %q", task.Name), task.User)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkUserValue(label, step.User)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkUserValue rejects a user: value beginning with '-'.
func checkUserValue(context, user string) error {
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("%s: user %q must not start with '-' (docker would parse it as a flag, not a user)", context, user)
	}

	return nil
}

// validateUserPlacement rejects user: on get/put steps, mirroring
// validateImages: a put runs its resource type's out:, and a get has no
// task/agent to scope.
func (c *Config) validateUserPlacement() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.User == "" {
				return nil
			}

			//kindswitch:ignore Task and Agent are the kinds user: is FOR — the cases here are the rejections
			switch {
			case step.Get != "":
				return fmt.Errorf("%s (get %q): user is not valid on get steps; set it on the resource_type instead", label, step.Get)
			case step.Put != "":
				return fmt.Errorf("%s (put %q): user is not valid on put steps; set it on the resource_type instead", label, step.Put)
			case step.Try != nil:
				return fmt.Errorf("%s: user is not valid on a try: step; set it on the step try: wraps", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
