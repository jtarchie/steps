package config

// Turning source.model into an endpoint, a key requirement, and the request
// conventions that provider expects.

import (
	"fmt"
	"strings"
)

// agentProvider is a built-in base URL + default API key env var for a
// model-name prefix like "openrouter/anthropic/claude-3.5-sonnet".
type agentProvider struct {
	baseURL     string
	keyEnv      string // default api_key_env for this provider; empty for local servers
	requiresKey bool
}

//nolint:gochecknoglobals // static, read-only lookup table
var agentProviders = map[string]agentProvider{
	"openai":     {"https://api.openai.com/v1/", "OPENAI_API_KEY", true},
	"openrouter": {"https://openrouter.ai/api/v1/", "OPENROUTER_API_KEY", true},
	"groq":       {"https://api.groq.com/openai/v1/", "GROQ_API_KEY", true},
	"together":   {"https://api.together.xyz/v1/", "TOGETHER_API_KEY", true},
	"lmstudio":   {"http://localhost:1234/v1/", "", false},
	"ollama":     {"http://localhost:11434/v1/", "", false},
	"opencode":   {"https://opencode.ai/zen/go/v1/", "OPENCODE_API_KEY", true},
	"anthropic":  {"https://api.anthropic.com/v1/", "ANTHROPIC_API_KEY", true},
}

// agentTarget is a source reduced to where the conversation goes and what it
// costs to get in: either an HTTP endpoint (BaseURL set) or a CLI subprocess
// (CLI set). Exactly one of the two is ever populated.
//
// It replaces what used to be six positional return values, so a new field —
// CLI was the first — reaches every caller by name instead of by counting
// blanks at five call sites.
type agentTarget struct {
	BaseURL              string
	ModelName            string
	APIKeyEnv            string
	RequiresKey          bool
	StringOnlyToolChoice bool
	// CLI names a cliProviders entry when this source runs a coding-agent CLI
	// instead of reaching an endpoint; "" for every HTTP source. See
	// cliagent.go.
	CLI string
}

// resolveAgentTarget interprets an optional "provider/" prefix on
// source.Model (e.g. "openrouter/anthropic/claude-3.5-sonnet") against
// agentProviders, splitting on the first "/" so a provider's own slashed
// model IDs survive intact. source.Endpoint/APIKeyEnv, when set, always
// override the derived values. A model with no recognized provider prefix
// requires an explicit source.Endpoint.
//
// A model spelled "@cli/model" resolves through resolveCLITarget instead — a
// different kind of destination, not a different endpoint (see cliagent.go).
//
// StringOnlyToolChoice defaults to !provider.requiresKey for a recognized
// provider prefix (local/no-auth providers get the string-only tool_choice
// fallback; cloud providers get the precise named form) or false for an
// explicit endpoint:, and source.StringToolChoice, when set, always wins.
func resolveAgentTarget(source AgentSource) (agentTarget, error) {
	if IsCLISource(source) {
		return resolveCLITarget(source)
	}

	prefix, rest, hasPrefix := strings.Cut(source.Model, "/")

	provider, known := agentProviders[prefix]
	if hasPrefix && known && rest != "" {
		return providerTarget(source, provider, rest), nil
	}

	if source.Endpoint == "" {
		return agentTarget{}, fmt.Errorf("model %q has no known provider prefix; set source.endpoint", source.Model)
	}

	return agentTarget{
		BaseURL:              ensureTrailingSlash(source.Endpoint),
		ModelName:            source.Model,
		APIKeyEnv:            source.APIKeyEnv,
		RequiresKey:          source.APIKeyEnv != "",
		StringOnlyToolChoice: stringToolChoice(source, false),
	}, nil
}

// providerTarget resolves a source whose model carried a recognized provider
// prefix, with modelName the part after it.
func providerTarget(source AgentSource, provider agentProvider, modelName string) agentTarget {
	baseURL := source.Endpoint
	if baseURL == "" {
		baseURL = provider.baseURL
	}

	apiKeyEnv := source.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = provider.keyEnv
	}

	return agentTarget{
		BaseURL:              ensureTrailingSlash(baseURL),
		ModelName:            modelName,
		APIKeyEnv:            apiKeyEnv,
		RequiresKey:          provider.requiresKey || source.APIKeyEnv != "",
		StringOnlyToolChoice: stringToolChoice(source, !provider.requiresKey),
	}
}

// stringToolChoice applies source.StringToolChoice over a derived default.
func stringToolChoice(source AgentSource, derived bool) bool {
	if source.StringToolChoice != nil {
		return *source.StringToolChoice
	}

	return derived
}

// ensureTrailingSlash normalizes a base URL to end in "/", since the
// OpenAI-compatible client resolves request paths (e.g. "chat/completions")
// relative to it.
func ensureTrailingSlash(rawURL string) string {
	if rawURL == "" || strings.HasSuffix(rawURL, "/") {
		return rawURL
	}

	return rawURL + "/"
}
