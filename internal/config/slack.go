package config

import "fmt"

// slackReactionType is the built-in slack-reaction type (resource_types/slack-reaction.yml).
const slackReactionType = "slack-reaction"

// validateSlackReactionPuts refuses a slack-reaction put with neither params.add nor params.remove, as ParseGitHubReactionParams refuses a github-reaction one: it publishes nothing and goes green, so a misspelt key is otherwise never noticed.
func (c *Config) validateSlackReactionPuts() error {
	builtin, err := ReadBuiltinResourceType(slackReactionType)
	if err != nil || builtin.Config.Expr == nil {
		return nil //nolint:nilerr // the embedded type is read and checked by registerBuiltinResourceTypes
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Put == "" || !c.putsToSlackReaction(step, builtin) {
				return nil
			}

			add, _ := step.Params["add"].(string)
			remove, _ := step.Params["remove"].(string)

			if add == "" && remove == "" {
				return fmt.Errorf("%s: put %q: params.add or params.remove is required: the emoji to put on the message, or one to take off", label, step.Put)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// putsToSlackReaction reports whether step puts to a resource of the built-in slack-reaction type, which builtin is. A type the pipeline declares under the same name replaces the built-in, and its params are its own.
func (c *Config) putsToSlackReaction(step *Step, builtin ResourceType) bool {
	resource, err := c.FindResource(step.PutResourceName())
	if err != nil {
		return false
	}

	resourceType, err := c.FindResourceType(resource.Type)
	if err != nil || resourceType.Name != slackReactionType || resourceType.Config.Expr == nil {
		return false
	}

	return resourceType.Config.Expr.Out == builtin.Config.Expr.Out
}
