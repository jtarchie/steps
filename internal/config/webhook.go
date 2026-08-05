package config

// webhook_token_env: — the credential half of webhook-triggered checks.

import (
	"fmt"
	"regexp"
	"strings"
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
