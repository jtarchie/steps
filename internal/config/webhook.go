package config

// webhook_token_env: — the credential half of webhook-triggered checks.

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// envVarPattern is what a plausible environment variable name looks like.
var envVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`) //nolint:gochecknoglobals // compiled once, read-only

// validateWebhookTokens rejects a webhook_token_env: that is not a variable
// name.
//
// The check exists because the most likely mistake is writing the token
// itself. That would both fail to authenticate anything and write the secret
// into state.db via the resource's hashed content — so it has to be caught at
// load, where a value that looks like a secret rather than a variable name is
// still obvious.
func (c *Config) validateWebhookTokens() error {
	for _, resource := range c.Resources {
		if resource.WebhookTokenEnv == "" {
			continue
		}

		if !envVarPattern.MatchString(resource.WebhookTokenEnv) {
			return fmt.Errorf(
				"resource %q: webhook_token_env must be the NAME of an environment variable holding the token (e.g. GITHUB_WEBHOOK_TOKEN), not the token itself — a literal would be hashed into state.db in cleartext",
				resource.Name)
		}
	}

	return nil
}

// WebhookResources maps each resource that accepts webhooks to the environment
// variable holding its token.
func (c *Config) WebhookResources() map[string]string {
	byName := map[string]string{}

	for _, resource := range c.Resources {
		if strings.TrimSpace(resource.WebhookTokenEnv) != "" {
			byName[resource.Name] = resource.WebhookTokenEnv
		}
	}

	return byName
}

// WebhookSource is a webhook resource's source:, typed. Config knows its shape; the provider table and the expression compiler it names live in internal/webhook and internal/exprlang, which is where resource.Receiver finishes the job.
type WebhookSource struct {
	Provider  string `yaml:"provider"`
	SecretEnv string `yaml:"secret_env"`
	MaxBody   int64  `yaml:"max_body,omitempty"`
}

// WebhookSource decodes this resource's source: strictly, so a misspelled key is a load error rather than a filter nobody applies.
func (r Resource) WebhookSource() (WebhookSource, error) {
	var source WebhookSource

	raw, err := yaml.Marshal(r.Source)
	if err != nil {
		return source, fmt.Errorf("resource %q: %w", r.Name, err)
	}

	err = strictUnmarshal(raw, &source)
	if err != nil {
		return source, fmt.Errorf("resource %q: source: %w", r.Name, err)
	}

	switch {
	case source.Provider == "":
		return source, fmt.Errorf("resource %q: a webhook resource needs source.provider, the sender whose signature it checks", r.Name)
	case !envVarPattern.MatchString(source.SecretEnv):
		return source, fmt.Errorf(
			"resource %q: source.secret_env must be the NAME of an environment variable holding the secret (e.g. GITHUB_WEBHOOK_SECRET), not the secret itself — a literal would be hashed into state.db in cleartext",
			r.Name)
	case source.MaxBody < 0:
		return source, fmt.Errorf("resource %q: source.max_body must be positive", r.Name)
	}

	return source, nil
}

func (c *Config) validateWebhookResources() error {
	for _, rt := range c.ResourceTypes {
		if rt.Name == WebhookType && !rt.Config.Webhook {
			return fmt.Errorf("resource_type %q: the name is built in — a webhook resource needs no resource_types: entry, so name this type something else", rt.Name)
		}
	}

	for _, resource := range c.Resources {
		if resource.Type != WebhookType {
			continue
		}

		_, err := resource.WebhookSource()
		if err != nil {
			return err
		}
	}

	return nil
}
