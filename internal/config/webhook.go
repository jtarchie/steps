package config

// webhook_token_env: — the credential half of webhook-triggered checks.

import (
	"fmt"
	"regexp"
	"slices"
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
	// Filter, ID and Version are expressions over the verified delivery; see exprlang.Webhook.
	Filter    string            `yaml:"filter,omitempty"`
	ID        string            `yaml:"id,omitempty"`
	Version   map[string]string `yaml:"version,omitempty"`
	Signature *WebhookSignature `yaml:"signature,omitempty"`
}

// WebhookSignature is provider: custom — an HMAC in one header, described rather than coded. Signed and Timestamp are expressions that build strings; whether the signature is valid is only ever decided in Go.
type WebhookSignature struct {
	Header    string `yaml:"header"`
	Prefix    string `yaml:"prefix,omitempty"`
	Encoding  string `yaml:"encoding,omitempty"`
	Algorithm string `yaml:"algorithm,omitempty"`
	Signed    string `yaml:"signed,omitempty"`
	Timestamp string `yaml:"timestamp,omitempty"`
}

// WebhookCustom is the provider whose scheme is the resource's own signature:.
const WebhookCustom = "custom"

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
	case hasKey(source.Version, "id") || hasKey(source.Version, "event"):
		return source, fmt.Errorf("resource %q: source.version cannot set id or event — every webhook version already carries both (use source.id to replace the delivery id)", r.Name)
	}

	return source, source.Signature.validate(r.Name, source.Provider)
}

func (s *WebhookSignature) validate(resource, provider string) error {
	switch {
	case provider == WebhookCustom && s == nil:
		return fmt.Errorf("resource %q: provider: custom needs source.signature to say how the sender signs", resource)
	case provider != WebhookCustom && s != nil:
		return fmt.Errorf("resource %q: source.signature describes a custom scheme; provider %q already has one", resource, provider)
	case s == nil:
		return nil
	case s.Header == "":
		return fmt.Errorf("resource %q: source.signature.header names where the signature arrives, and is required", resource)
	case !slices.Contains([]string{"", "hex", "base64"}, s.Encoding):
		return fmt.Errorf("resource %q: source.signature.encoding must be hex or base64, not %q", resource, s.Encoding)
	case !slices.Contains([]string{"", "sha256", "sha1", "sha512"}, s.Algorithm):
		return fmt.Errorf("resource %q: source.signature.algorithm must be sha256, sha1 or sha512, not %q", resource, s.Algorithm)
	}

	return nil
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

func hasKey(m map[string]string, key string) bool {
	_, ok := m[key]

	return ok
}
