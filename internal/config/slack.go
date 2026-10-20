package config

import "fmt"

// slackReactionType is the built-in slack-reaction type (resource_types/slack-reaction.yml).
const slackReactionType = "slack-reaction"

// validateSlackReactionGets refuses a slack-reaction get whose resource names no source.emoji. The check would fail on its first poll anyway; at load it fails before anything else in the pipeline has run.
func (c *Config) validateSlackReactionGets() error {
	builtin, err := ReadBuiltinResourceType(slackReactionType)
	if err != nil || builtin.Config.Expr == nil {
		return nil //nolint:nilerr // the embedded type is read and checked by registerBuiltinResourceTypes
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Get == "" || !c.isSlackReaction(step.GetResourceName(), builtin) {
				return nil
			}

			if !watchesReactions(c, step.GetResourceName()) {
				return fmt.Errorf("%s: get %q: source.emoji is required to get from a slack-reaction resource: the emoji to watch for, no colons", label, step.Get)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// watchesReactions reports whether the named resource sets source.emoji, which is what makes a slack-reaction resource a watcher.
func watchesReactions(c *Config, name string) bool {
	resource, err := c.FindResource(name)
	if err != nil {
		return false
	}

	emoji, _ := resource.Source["emoji"].(string)

	return emoji != ""
}

// validateSlackReactionPuts refuses a slack-reaction put with neither params.add nor params.remove, as ParseGitHubReactionParams refuses a github-reaction one: it publishes nothing and goes green, so a misspelt key is otherwise never noticed. It also refuses a put to a resource that watches for reactions, which would answer its own mark.
func (c *Config) validateSlackReactionPuts() error {
	builtin, err := ReadBuiltinResourceType(slackReactionType)
	if err != nil || builtin.Config.Expr == nil {
		return nil //nolint:nilerr // the embedded type is read and checked by registerBuiltinResourceTypes
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Put == "" || !c.isSlackReaction(step.PutResourceName(), builtin) {
				return nil
			}

			add, _ := step.Params["add"].(string)
			remove, _ := step.Params["remove"].(string)

			if add == "" && remove == "" {
				return fmt.Errorf("%s: put %q: params.add or params.remove is required: the emoji to put on the message, or one to take off", label, step.Put)
			}

			if watchesReactions(c, step.PutResourceName()) {
				return fmt.Errorf("%s: put %q: resource %q watches for reactions (source.emoji), and a put records its own version on the resource it publishes to, which the watching get would fetch as new: mark through a second slack-reaction resource with no emoji", label, step.Put, step.PutResourceName())
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// isSlackReaction reports whether the named resource is of the built-in slack-reaction type, which builtin is. A type the pipeline declares under the same name replaces the built-in, and its params and source are its own.
func (c *Config) isSlackReaction(name string, builtin ResourceType) bool {
	resource, err := c.FindResource(name)
	if err != nil {
		return false
	}

	resourceType, err := c.FindResourceType(resource.Type)
	if err != nil || resourceType.Name != slackReactionType || resourceType.Config.Expr == nil {
		return false
	}

	return resourceType.Config.Expr.Out == builtin.Config.Expr.Out
}
