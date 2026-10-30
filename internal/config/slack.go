package config

// The built-in slack-* types (resource_types/slack-*.yml): the shape of each one's source: and put params:, and the load-time rules that need no network.

import (
	"errors"
	"fmt"
)

// The built-in Slack type names.
const (
	slackMentionsType = "slack-mentions"
	slackReplyType    = "slack-reply"
	slackReactionType = "slack-reaction"
)

// SlackMentionsSource is a slack-mentions resource's source:.
type SlackMentionsSource struct {
	TokenEnv string `yaml:"token_env,omitempty"`
	// BaseURL replaces https://slack.com, so a test can point the type at a fake.
	BaseURL  string   `yaml:"base_url,omitempty"`
	Channels []string `yaml:"channels,omitempty"`
	Limit    int      `yaml:"limit,omitempty"`
}

// SlackReplySource is a slack-reply resource's source:.
type SlackReplySource struct {
	TokenEnv string `yaml:"token_env,omitempty"`
	BaseURL  string `yaml:"base_url,omitempty"`
}

// SlackReactionSource is a slack-reaction resource's source:. Emoji makes it a watcher a get can read; without it the resource only marks.
type SlackReactionSource struct {
	TokenEnv string `yaml:"token_env,omitempty"`
	BaseURL  string `yaml:"base_url,omitempty"`
	Emoji    string `yaml:"emoji,omitempty"`
	Done     string `yaml:"done,omitempty"`
	Limit    int    `yaml:"limit,omitempty"`
	Pages    int    `yaml:"pages,omitempty"`
}

// SlackReplyParams is a slack-reply put's params:.
type SlackReplyParams struct {
	From string `yaml:"from,omitempty"`
}

// SlackReactionParams is a slack-reaction put's params:.
type SlackReactionParams struct {
	Add    string `yaml:"add,omitempty"`
	Remove string `yaml:"remove,omitempty"`
	From   string `yaml:"from,omitempty"`
}

// validateSlackResources decodes every slack-* resource's source: and put's params: strictly, and refuses a reaction get with nothing to watch and a reaction put with nothing to do or that would answer itself.
func (c *Config) validateSlackResources() error {
	for _, resource := range c.Resources {
		err := checkSlackSource(c.nativeType(resource.Type), resource.Source)
		if err != nil {
			return fmt.Errorf("resource %q (%s): %w", resource.Name, resource.Type, err)
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(c.checkSlackStep)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkSlackSource(kind string, source map[string]any) error {
	switch kind {
	case slackMentionsType:
		return decodeSource(source, &SlackMentionsSource{})
	case slackReplyType:
		return decodeSource(source, &SlackReplySource{})
	case slackReactionType:
		return decodeSource(source, &SlackReactionSource{})
	}

	return nil
}

func (c *Config) checkSlackStep(label string, step *Step) error {
	if step.Get != "" && c.nativeResourceType(step.GetResourceName()) == slackReactionType && !watchesReactions(c, step.GetResourceName()) {
		return fmt.Errorf("%s: get %q: source.emoji is required to get from a slack-reaction resource: the emoji to watch for, no colons", label, step.Get)
	}

	if step.Put == "" {
		return nil
	}

	var err error

	switch c.nativeResourceType(step.PutResourceName()) {
	case slackReplyType:
		err = decodeParams(step.Params, &SlackReplyParams{})
	case slackReactionType:
		err = c.checkSlackReactionPut(step)
	}

	if err != nil {
		return fmt.Errorf("%s: put %q: %w", label, step.Put, err)
	}

	return nil
}

// checkSlackReactionPut refuses a put with neither params.add nor params.remove, as ParseGitHubReactionParams refuses a github-reaction one: it publishes nothing and goes green. It also refuses a put to a resource that watches for reactions, which would answer its own mark.
func (c *Config) checkSlackReactionPut(step *Step) error {
	var params SlackReactionParams

	err := decodeParams(step.Params, &params)
	if err != nil {
		return err
	}

	if params.Add == "" && params.Remove == "" {
		return errors.New("params.add or params.remove is required: the emoji to put on the message, or one to take off")
	}

	if watchesReactions(c, step.PutResourceName()) {
		return fmt.Errorf("resource %q watches for reactions (source.emoji), and a put records its own version on the resource it publishes to, which the watching get would fetch as new: mark through a second slack-reaction resource with no emoji", step.PutResourceName())
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
