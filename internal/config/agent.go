package config

// The agents: entry — model connection, persona, generation dials — plus the
// provider table that turns a model prefix into an endpoint and credential.

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// Agent is a named, reusable worker an `agent` step invokes: it owns the
// model connection, persona, generation dials, limits, and the tool grant
// (the set of tools a step may draw from). A step supplies the per-task
// prompt, working directory, and tool selection.
type Agent struct {
	Name   string      `yaml:"name"`
	Source AgentSource `yaml:"source"`
	// Description is the human-readable summary of what this agent does. It is
	// shown to the model when another agent grants this one as a sub-agent tool
	// (see ToolSpec.Agent) with no inline description. An agent referenced as a
	// sub-agent MUST have a description — either inline on the grant or on the
	// agent itself — or pipeline load fails with a clear error.
	Description string `yaml:"description,omitempty"`
	// File loads this agent's source/image/system/dials/tools from a YAML
	// document at a path relative to the pipeline file's directory (see
	// LoadConfig's resolveFileIncludes), so one agent definition can be
	// shared across pipelines. Any field also set inline on this entry
	// overrides the loaded document's value for that field — the same "wins
	// when set" idiom ResolveTask already uses between a step and its tasks:
	// entry. The loaded document may not itself use file:/system_file:.
	File string `yaml:"file,omitempty"`
	// Image, when set, runs this agent's run_shell/custom-tool commands in a
	// fresh `docker run --rm` container from this image instead of on the
	// host. A step's own Image, if set, overrides this for that step only
	// (see Step.Image). Empty (the default) keeps host execution.
	Image string `yaml:"image,omitempty"`
	// System is the persona/system message given to the model. Empty falls
	// back to a generic CI-agent persona.
	System string `yaml:"system,omitempty"`
	// SystemFile loads System's text from a file at a path relative to the
	// pipeline file's directory, instead of writing it inline — useful since a
	// persona is often long freeform prose. Mutually exclusive with System.
	SystemFile string `yaml:"system_file,omitempty"`
	// Generation dials, forwarded to the model when set. ReasoningEffort is
	// one of "low", "medium", "high" (for reasoning-capable models).
	Temperature     *float64 `yaml:"temperature,omitempty"`
	TopP            *float64 `yaml:"top_p,omitempty"`
	MaxTokens       int      `yaml:"max_tokens,omitempty"`
	ReasoningEffort string   `yaml:"reasoning_effort,omitempty"`
	// MaxTurns caps the tool-calling loop (default maxAgentTurns). Retries
	// (attempts:) are a per-task concern and live on the step, not here.
	MaxTurns int `yaml:"max_turns,omitempty"`
	// CompactAfterTokens caps how large a conversation's estimated token
	// count grows before older turns are summarized away and replaced by a
	// running summary (see internal/agent/compaction.go and docs/agents.md's
	// "Compacting long conversations" section). A pointer, not a plain int,
	// because unset and explicit 0 mean different things: unset resolves to
	// defaultCompactAfterTokens (compaction ON by default), while 0 disables
	// compaction entirely — the same distinction Temperature/TopP already
	// need a pointer for. Like MaxTurns, this is agent-only: no per-step
	// override.
	CompactAfterTokens *int `yaml:"compact_after_tokens,omitempty"`
	// Preflight opts this agent out of (or into) the pre-run health check.
	// A pointer so unset inherits defaults.preflight. The case it exists for
	// is a model expected to be slow to WAKE — a cold local model would fail
	// a probe that a real conversation would have waited out.
	Preflight *bool `yaml:"preflight,omitempty"`
	// Budget caps what one invocation of this agent may spend (see Budget).
	// Per invocation, not per job: a job budget is the cumulative ceiling and
	// lives on the job. Never hashed.
	Budget *Budget `yaml:"budget,omitempty"`
	// Tools is the grant: the built-in tools this agent may use plus any
	// reusable custom tool definitions. A step selects a subset by name and
	// may add its own inline custom tools. Empty grants all built-ins.
	Tools []ToolSpec `yaml:"tools,omitempty"`
}

// AgentSource selects the model and how to reach it. Model may carry a
// provider prefix (e.g. "openrouter/anthropic/claude-3.5-sonnet",
// "lmstudio/qwen2.5-coder") that resolves Endpoint and a default APIKeyEnv
// from a built-in table (see resolveAgentTarget in agent.go); Endpoint, when
// set, is the API base URL (e.g. "https://api.openai.com/v1/") and overrides
// the derived one. APIKeyEnv names an OS environment variable read at run
// time — the key is never stored in YAML.
//
// StringToolChoice overrides whether forcing a required tool (see
// forceRequiredTool in internal/agent/conversation.go) uses OpenAI's named
// tool_choice object or the string "required" fallback. Left unset, it
// defaults to the resolved provider's requiresKey (cloud providers get the
// precise named form; local/no-auth providers like lmstudio/ollama, whose
// OpenAI-compat servers often don't support the object form, get the
// fallback) or false for an explicit endpoint: with no recognized provider
// prefix. A pointer so "unset" is distinguishable from an explicit false.
type AgentSource struct {
	Endpoint         string `yaml:"endpoint,omitempty"`
	Model            string `yaml:"model"`
	APIKeyEnv        string `yaml:"api_key_env,omitempty"`
	StringToolChoice *bool  `yaml:"string_tool_choice,omitempty"`
}

// defaultMaxAgentTurns is the default cap on one attempt's tool-calling loop
// when an agent doesn't set max_turns. 3-6 round trips covers a typical
// review (list a dir, read a few files, run a command, respond); 8 leaves
// headroom while still bounding a runaway loop (a model that never stops
// requesting tools) to a small, predictable number of calls.
const defaultMaxAgentTurns = 8

// defaultCompactAfterTokens is the conversation-size budget (in estimated
// tokens — see estimateContentTokens in internal/agent/compaction.go) an
// agent that sets no compact_after_tokens: gets: 80% of a 128K context
// window, the common size for current models.
//
// The 20% headroom is load-bearing, not padding. estimateContentTokens
// measures req.Contents alone — it never counts the system prompt, the tool
// schemas re-sent on every request, or the output the model still has to
// fit. A budget set at the full window would only ever fire after the
// request had already overflowed.
//
// It is only the fallback for a model whose window this package does not
// recognize (see contextWindowFor). A small local model (32K and under)
// overflows well before this and must set compact_after_tokens: lower;
// compact_after_tokens: 0 disables compaction entirely.
const defaultCompactAfterTokens = defaultContextWindow * compactBudgetPercent / 100 // 102,400

// defaultContextWindow is the window assumed for an unrecognized model: the
// common size for current models, and conservative for anything larger.
const defaultContextWindow = 128_000

// compactBudgetPercent is how much of a model's context window compaction is
// allowed to fill before it fires.
const compactBudgetPercent = 80

// contextWindows maps a fragment of a model name to that model's context
// window, most specific first. Matching is on a lowercased substring of the
// model name with any provider prefix already stripped, so it works the same
// whether the model was written as `claude-sonnet-4-5` or
// `openrouter/anthropic/claude-sonnet-4-5`.
//
// The point of the table is that a default budget must not be a guess about
// somebody else's model. Compaction defaults ON at 80% of the window, so an
// assumed 128K applied to a 1M-context model compacts at roughly a tenth of
// capacity — silently, forever, paying for a summarization call that buys
// nothing and losing conversation fidelity for no reason.
//
// It is deliberately short. An entry is only worth adding for a model whose
// window is confidently known and materially different from the 128K default;
// anything unrecognized keeps that conservative default, which is the safe
// direction to be wrong in. An operator who knows better always outranks this
// table by setting compact_after_tokens: directly.
//
//nolint:gochecknoglobals // static, read-only lookup table
var contextWindows = []struct {
	fragment string
	window   int
}{
	// Anthropic ships 1M-context variants of some models under a suffixed id;
	// checked before the family names below, which would otherwise match first.
	{"[1m]", 1_000_000},
	{"claude", 200_000},
	{"gemini", 1_000_000},
	{"gpt-4.1", 1_000_000},
	{"gpt-4o", 128_000},
	{"o3", 200_000},
	{"o4-mini", 200_000},
	{"llama-3.1", 128_000},
	{"llama-3.3", 128_000},
}

// resolveCompactionBudget decides an invocation's conversation-size budget and
// reports the context window it was derived from (0 when the model is not one
// this package recognizes, so a caller can tell "1M, derived from the model"
// from "128K, assumed because we have never heard of this model").
//
// Precedence: an explicit compact_after_tokens: always wins; otherwise the
// budget is compactBudgetPercent of a recognized window; otherwise the
// conservative default. Deriving it is the whole point — compaction defaults
// ON, so an assumed 128K applied to a 1M model compacted at a tenth of
// capacity, silently and forever, paying for a summarization call that bought
// nothing.
func resolveCompactionBudget(modelName string, explicit *int) (budget, contextWindow int) {
	window, known := contextWindowFor(modelName)

	budget = defaultCompactAfterTokens
	if known {
		budget = window * compactBudgetPercent / 100
		contextWindow = window
	}

	if explicit != nil {
		budget = *explicit
	}

	return budget, contextWindow
}

// contextWindowFor returns the context window known for a model name, and
// whether it was recognized at all. The caller distinguishes the two: a
// recognized window sets the compaction budget, an unrecognized one leaves
// today's conservative default in place.
func contextWindowFor(modelName string) (int, bool) {
	name := strings.ToLower(modelName)

	for _, entry := range contextWindows {
		if strings.Contains(name, entry.fragment) {
			return entry.window, true
		}
	}

	return defaultContextWindow, false
}

// validReasoningEfforts are the only accepted values for an agent's
// reasoning_effort. The corresponding genai.ThinkingLevel mapping lives in
// internal/agent, which is the only package that needs the LLM-specific
// value side of this table.
var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true} //nolint:gochecknoglobals // static, read-only lookup table

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

// validateStepContextPaths rejects context_paths: on non-agent steps — a
// path like "repo/CLAUDE.md" only has meaning when an agent step knows its
// workspace inputs, and the synthetic read_file injection at run time
// requires read_file to be in the tool grant.
func (c *Config) validateStepContextPaths() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.ContextPaths) == 0 {
				return nil
			}

			if step.Agent == "" {
				return fmt.Errorf("%s: context_paths is only valid on agent steps", label)
			}

			for _, p := range step.ContextPaths {
				if strings.TrimSpace(p) == "" {
					return fmt.Errorf("%s: context_paths must not contain an empty path", label)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAgentCompaction checks every agents: entry's compact_after_tokens:
// is not negative. Nil (unset) and 0 (explicitly disabled) are both valid;
// only a negative value has no meaning — mirrors ParseTimeout's own "reject
// negative, don't second-guess otherwise" precedent.
func (c *Config) validateAgentCompaction() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		if agent.CompactAfterTokens != nil && *agent.CompactAfterTokens < 0 {
			return fmt.Errorf("agent %q: compact_after_tokens must not be negative", agent.Name)
		}
	}

	return nil
}

// FindAgent returns the agent with the given name, or an error if not found.
func (c *Config) FindAgent(name string) (*Agent, error) {
	slog.Debug("agent.find", "name", name)

	for i := range c.Agents {
		if c.Agents[i].Name == name {
			slog.Debug("agent.find", "name", name, "found", true)

			return &c.Agents[i], nil
		}
	}

	return nil, notFound("agent", name, names(c.Agents, func(a Agent) string { return a.Name }))
}

// validateAgentEndpoints rejects an agents: entry whose source.endpoint:
// embeds userinfo (a "https://user:token@host/" style credential). Nothing
// in resolveAgentTarget/ResolveAgentInvocation scrubs it: the resolved
// BaseURL — endpoint included — is folded verbatim into AgentContentMap /
// subAgentInvocationContent (internal/merkle) and persisted through
// store.RecordNode, contradicting this codebase's own documented claim that
// hashed content excludes anything "secret-adjacent" (that exclusion only
// actually covers api_key_env's name/value, never a credential living in
// endpoint itself). Rejecting at load, rather than silently stripping the
// credential before hashing, surfaces the mistake immediately and points the
// operator at api_key_env — the mechanism this project already has for
// exactly this purpose — instead of a config that "works" while quietly
// leaking a credential into .steps/state.db on every run.
func (c *Config) validateAgentEndpoints() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		endpoint := agent.Source.Endpoint
		if endpoint == "" {
			continue
		}

		parsed, err := url.Parse(endpoint)
		if err != nil {
			continue // an unparsable endpoint is left for resolveAgentTarget/the HTTP client to reject at run time
		}

		if parsed.User != nil {
			return fmt.Errorf("agent %q: source.endpoint must not embed credentials (userinfo); use source.api_key_env instead", agent.Name)
		}
	}

	return nil
}

// resolveAgentTarget interprets an optional "provider/" prefix on
// source.Model (e.g. "openrouter/anthropic/claude-3.5-sonnet") against
// agentProviders, splitting on the first "/" so a provider's own slashed
// model IDs survive intact. source.Endpoint/APIKeyEnv, when set, always
// override the derived values. A model with no recognized provider prefix
// requires an explicit source.Endpoint.
//
// stringOnlyToolChoice defaults to !provider.requiresKey for a recognized
// provider prefix (local/no-auth providers get the string-only tool_choice
// fallback; cloud providers get the precise named form) or false for an
// explicit endpoint:, and source.StringToolChoice, when set, always wins.
func resolveAgentTarget(source AgentSource) (baseURL, modelName, apiKeyEnv string, requiresKey, stringOnlyToolChoice bool, err error) {
	prefix, rest, hasPrefix := strings.Cut(source.Model, "/")

	provider, known := agentProviders[prefix]
	if hasPrefix && known && rest != "" {
		baseURL = source.Endpoint
		if baseURL == "" {
			baseURL = provider.baseURL
		}

		apiKeyEnv = source.APIKeyEnv
		if apiKeyEnv == "" {
			apiKeyEnv = provider.keyEnv
		}

		stringOnlyToolChoice = !provider.requiresKey
		if source.StringToolChoice != nil {
			stringOnlyToolChoice = *source.StringToolChoice
		}

		return ensureTrailingSlash(baseURL), rest, apiKeyEnv, provider.requiresKey || source.APIKeyEnv != "", stringOnlyToolChoice, nil
	}

	if source.Endpoint == "" {
		return "", "", "", false, false, fmt.Errorf("model %q has no known provider prefix; set source.endpoint", source.Model)
	}

	stringOnlyToolChoice = false
	if source.StringToolChoice != nil {
		stringOnlyToolChoice = *source.StringToolChoice
	}

	return ensureTrailingSlash(source.Endpoint), source.Model, source.APIKeyEnv, source.APIKeyEnv != "", stringOnlyToolChoice, nil
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
