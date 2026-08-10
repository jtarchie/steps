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
	// Env names host environment variables this agent's run_shell/custom-tool
	// commands are allowed to see, on top of the always-allowed baseline (see
	// shell.HostEnv). Names only — see validateEnvValues. A step's own Env, if
	// non-nil, overrides this for that step only (see Step.Env).
	Env []string `yaml:"env,omitempty"`
	// User is the container user this agent's run_shell/custom-tool commands
	// execute as (docker's --user). Empty takes the platform default — see
	// shell's defaultContainerUser. Only meaningful alongside Image.
	User string `yaml:"user,omitempty"`
	// Network is the container network this agent's run_shell/custom-tool
	// commands join (docker's --network); "none" cuts off egress entirely.
	// Requires Image.
	Network string `yaml:"network,omitempty"`
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
	// MaxTurns caps the tool-calling loop (default defaultMaxAgentTurns).
	// Retries (attempts:) are a per-task concern and live on the step, not
	// here.
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
	// ContextWindow states this model's context window in tokens, for a model
	// contextWindows does not recognize — a local build, a release newer than
	// that table, or a HOST that serves a known model with a smaller window
	// than it has natively (a table keyed on model name cannot express the
	// last one at all).
	//
	// It is the "what the model is" knob to compact_after_tokens:' "what the
	// budget is": setting it keeps compactBudgetPercent applying, so an
	// operator says 200000 rather than computing 160000, and the run's own
	// compaction log reports a derived window instead of an assumed one.
	// compact_after_tokens:, when also set, still wins outright.
	ContextWindow int `yaml:"context_window,omitempty"`
	// MaxContextBytes caps how much of a context_paths: file is handed to the
	// model at conversation start. 0 takes DefaultMaxContextBytes.
	//
	// It exists because the byte budget priming borrows was chosen for a
	// different job: context_paths is delivered as a synthetic read_file
	// result, so it inherited read_file's tool budget, and that number is sized
	// against the spill mechanics (it sits above maxToolOutputBytes so a
	// spilled output can be read back whole). Nothing in that reasoning is
	// about how much evidence a step should open holding — and on a model whose
	// context_window: is 1M, the inherited 100KB is about 2.5% of the window.
	//
	// Over the limit the file is TRUNCATED with a pointer, never refused, so
	// raising this buys context rather than turning a warning into an error.
	// Operational, like CompactAfterTokens, and excluded from the hash for the
	// same reason: it governs how much of a conversation's own history and
	// evidence is carried, not what the step is asking for.
	MaxContextBytes int `yaml:"max_context_bytes,omitempty"`
	// Fallback lists alternate sources to use when the primary is
	// UNREACHABLE, in order. See AgentFallback.
	Fallback []AgentFallback `yaml:"fallback,omitempty"`
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

// AgentFallback is one alternate model to fall over to when the primary is
// unreachable — a different provider, a different endpoint, or the same
// endpoint's larger sibling.
//
// It exists because a provider outage is not the sort of failure retrying
// fixes. In one real run a model went unavailable upstream and killed three
// consecutive runs over roughly 50 minutes, while attempts: amplified the
// waste rather than absorbing it. The manual fix was exactly this: point the
// agent at a different model, one line.
//
// Only the source can vary. A fallback is meant to be the same agent reaching
// a different endpoint, not a different persona or a different tool grant —
// swapping those would make an outage change what the agent is allowed to do.
type AgentFallback struct {
	Source AgentSource `yaml:"source"`
}

// defaultMaxAgentTurns is the default cap on one attempt's tool-calling loop
// when an agent doesn't set max_turns.
//
// It used to be 8, on the theory that 3-6 round trips covers a typical review
// (list a dir, read a few files, run a command, respond). That theory is
// contradicted by this repo's own evidence: every built-in profile overrides
// it — explorer 15, planner 25, reviewer 30, builder 50 — so the default was
// the value nothing actually wanted. It is also far below the field: goose
// defaults to 1000 (25 for subagents), pydantic-ai's request_limit to 50,
// CrewAI's max_iter to 25, smolagents' max_steps to 20, LangChain's
// max_iterations to 15, the OpenAI Agents SDK's DEFAULT_MAX_TURNS to 10, and
// the coding agents this most resembles (Claude Code, opencode, aider) impose
// no turn cap at all, bounding a conversation by context and cost instead.
//
// 30 is deliberately not "unbounded". A turn cap is the wrong instrument for
// bounding cost — budget: (tokens/USD), timeout:, the job deadline, and loop
// detection all bound a runaway conversation more precisely, and all of them
// postdate the 8. What max_turns is actually for is the case none of those
// catch: a model that keeps calling tools productively but never converges.
// 30 is enough that a real investigation finishes, and small enough that a
// non-converging conversation stops in a predictable number of calls.
//
// The failure modes are not symmetric, which is what decides the direction.
// Too low truncates a working agent mid-task and fails the step — common, and
// it looks like a model problem rather than a config one. Too high costs
// extra turns on a conversation that was going to fail anyway — rarer, and
// visible in the budget.
const defaultMaxAgentTurns = 30

// defaultAgentAttempts is how many times one agent step's requests may be
// tried when the step sets no attempts: of its own. For a hosted agent this
// feeds the retrying TRANSPORT — a failed request is re-issued with backoff —
// not a re-run of the conversation.
//
// It used to be 1, on the reasoning that "retries are a per-task concern": in
// CI a retry hides a real failure, since the same command against the same
// tree fails the same way. That is exactly backwards for a model call. A 503
// says nothing about the step and everything about the provider's minute, and
// under attempts=1 a single one destroyed a six-reviewer fan-out whose other
// five cells were healthy — twice, in two consecutive live runs.
//
// 3 matches the reference this was measured against (pr-af's
// PR_AF_AI_MAX_RETRIES, its floor for EVERY model call), and the transport
// only retries what can recover: connection errors and 5xx-class statuses,
// never a request the provider marked unretryable. Like attempts: itself it
// is never hashed — a retry policy does not change what a step is asking for.
const defaultAgentAttempts = 3

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
// recognize (see contextWindowFor) and whose agent declared no
// context_window:. A small local model (32K and under) overflows well before
// this and must state its context_window: (or set compact_after_tokens:
// lower); compact_after_tokens: 0 disables compaction entirely.
const defaultCompactAfterTokens = defaultContextWindow * compactBudgetPercent / 100 // 102,400

// defaultContextWindow is the window assumed for an unrecognized model: the
// common size for current models, and conservative for anything larger.
const defaultContextWindow = 128_000

// compactBudgetPercent is how much of a model's context window compaction is
// allowed to fill before it fires.
const compactBudgetPercent = 80

// contextWindows maps a fragment of a model name to that model's context
// window, most specific first. Matching is on a substring of the NORMALIZED
// model name (see normalizeModelName) with any provider prefix already
// stripped, so it works the same whether the model was written as
// `claude-sonnet-4-5`, `claude-sonnet-4.5`, or
// `openrouter/anthropic/claude-sonnet-4.5` — fragments are therefore spelled
// in the dashed form, never the dotted one.
//
// The point of the table is that a default budget must not be a guess about
// somebody else's model. Compaction defaults ON at 80% of the window, so an
// assumed 128K applied to a 1M-context model compacts at roughly a tenth of
// capacity — silently, forever, paying for a summarization call that buys
// nothing and losing conversation fidelity for no reason.
//
// An entry is only worth adding for a model whose window is confidently known;
// anything unrecognized keeps the conservative 128K default, which is the safe
// direction to be wrong in. That rule decides the shape of the Anthropic
// block below: the 1M models are enumerated individually and the `claude`
// family entry stays at 200K, so a claude-* released after this table was
// written is under-budgeted rather than over-budgeted.
//
// The numbers are the models' own windows, taken from models.dev (the catalog
// opencode itself resolves against; `curl -s https://models.dev/api.json` and
// read .<provider>.models.<id>.limit.context). A HOST that serves a model with
// a smaller window than the model has natively is not something a table keyed
// on model name can express — that is what an explicit context_window: on the
// agent is for, and so is a local model nobody's catalog has heard of.
//
//nolint:gochecknoglobals // static, read-only lookup table
var contextWindows = []struct {
	fragment string
	window   int
}{
	// Anthropic ships 1M-context variants of some models under a suffixed id;
	// checked before the family names below, which would otherwise match first.
	{"[1m]", 1_000_000},

	// Anthropic. Everything from sonnet-4-5 forward is natively 1M; the family
	// entry catches the ones that are not (opus-4-5, haiku-4-5, opus-4-1,
	// 3-5-haiku) and anything newer than this table.
	{"claude-fable-5", 1_000_000},
	{"claude-opus-5", 1_000_000},
	{"claude-opus-4-8", 1_000_000},
	{"claude-opus-4-7", 1_000_000},
	{"claude-opus-4-6", 1_000_000},
	{"claude-sonnet-5", 1_000_000},
	{"claude-sonnet-4-6", 1_000_000},
	{"claude-sonnet-4-5", 1_000_000},
	{"claude", 200_000},

	// OpenAI. The gpt-5 family split: 5.4-and-up went to ~1M, but 5.4's own
	// mini/nano stayed at 400K and codex-spark at 128K, so those precede their
	// family entries.
	{"gpt-5-4-mini", 400_000},
	{"gpt-5-4-nano", 400_000},
	{"gpt-5-3-codex-spark", 128_000},
	{"gpt-5-6", 1_050_000},
	{"gpt-5-5", 1_050_000},
	{"gpt-5-4", 1_050_000},
	{"gpt-5", 400_000},
	{"gpt-4-1", 1_000_000},
	{"gpt-4o", 128_000},
	{"o3", 200_000},
	{"o4-mini", 200_000},

	{"gemini", 1_000_000},

	// xAI. grok-code/grok-build are 256K; the family entry is set to that
	// rather than to grok-4-5's 500K so a new grok lands low, not high.
	{"grok-4-5", 500_000},
	{"grok", 256_000},

	// Open-weight and other hosted families, each family entry pinned to the
	// SMALLEST window that family is served with (glm-5.1 is 202,752 on one
	// host and 204,800 on another; minimax-m3 is 512K on some, 1M on others).
	{"kimi-k3", 1_000_000},
	{"kimi", 262_144},
	{"glm-5-2", 1_000_000},
	{"glm", 200_000},
	{"minimax-m3", 512_000},
	{"minimax", 200_000},
	{"deepseek-v4", 1_000_000},
	{"qwen", 262_144},
	{"mimo", 200_000},
	{"hy3", 190_000},

	// Local-server staples. These match the 128K default; they earn their
	// place by making it a DERIVED window rather than an assumed one, which is
	// the difference the compaction log line reports.
	{"gpt-oss", 131_072},
	{"llama-3-1", 128_000},
	{"llama-3-3", 128_000},
}

// normalizeModelName folds the spellings of one model onto each other before
// the table is consulted: lowercase, and "." to "-" because the same model
// reaches this code as `claude-sonnet-4-5` from Anthropic and opencode but
// `claude-sonnet-4.5` from OpenRouter (likewise glm-5.2/glm-5-2,
// qwen3.7-max/qwen3-7-max). Without it every dotted id silently misses and
// falls back to the 128K assumption.
func normalizeModelName(modelName string) string {
	return strings.ReplaceAll(strings.ToLower(modelName), ".", "-")
}

// resolveCompactionBudget decides an invocation's conversation-size budget and
// reports the context window it was derived from (0 when the model is not one
// this package recognizes and the agent declared no context_window:, so a
// caller can tell "1M, derived from the model" from "128K, assumed because we
// have never heard of this model").
//
// Precedence: an explicit compact_after_tokens: always wins; otherwise the
// budget is compactBudgetPercent of the window — an explicit context_window:
// if the agent set one, else a recognized window from the table; otherwise the
// conservative default. Deriving it is the whole point — compaction defaults
// ON, so an assumed 128K applied to a 1M model compacted at a tenth of
// capacity, silently and forever, paying for a summarization call that bought
// nothing.
//
// The two overrides are not redundant. context_window: says what the model IS
// and lets the percentage keep applying; compact_after_tokens: overrides the
// result outright. An operator who knows their host serves 200K of a 1M model
// wants the former and should not have to compute 160,000 by hand.
func resolveCompactionBudget(modelName string, explicitWindow int, explicit *int) (budget, contextWindow int) {
	window, known := contextWindowFor(modelName)

	if explicitWindow > 0 {
		window, known = explicitWindow, true
	}

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
	name := normalizeModelName(modelName)

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
//
// context_window: is held to the same rule with one difference: 0 is its
// "unset", since a model with no context window is not a thing anyone means.
func (c *Config) validateAgentCompaction() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		if agent.CompactAfterTokens != nil && *agent.CompactAfterTokens < 0 {
			return fmt.Errorf("agent %q: compact_after_tokens must not be negative", agent.Name)
		}

		if agent.ContextWindow < 0 {
			return fmt.Errorf("agent %q: context_window must not be negative", agent.Name)
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

		err := checkEndpointCredentials(agent.Name, "source", agent.Source)
		if err != nil {
			return err
		}

		// Every fallback source gets the same check. Easy to miss precisely
		// because it is a repeated structure rather than a single field, and
		// a credential smuggled into a backup endpoint is no less a leak for
		// being on the path nobody exercises until an outage.
		for j := range agent.Fallback {
			err = checkEndpointCredentials(agent.Name, fmt.Sprintf("fallback[%d].source", j), agent.Fallback[j].Source)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func checkEndpointCredentials(agentName, field string, source AgentSource) error {
	if source.Endpoint == "" {
		return nil
	}

	parsed, err := url.Parse(source.Endpoint)
	if err != nil {
		return nil //nolint:nilerr // an unparsable endpoint is left for resolveAgentTarget/the HTTP client to reject at run time
	}

	if parsed.User != nil {
		return fmt.Errorf("agent %q: %s.endpoint must not embed credentials (userinfo); use api_key_env instead", agentName, field)
	}

	return nil
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

// DefaultMaxContextBytes is how much of one context_paths: file reaches the
// model when an agent sets no max_context_bytes:.
//
// 100,000 is what priming used before the knob existed — read_file's own tool
// budget, inherited because a context file is delivered as a synthetic
// read_file result. Keeping it as the default is what makes this field free to
// add: every pipeline that does not set it is handed exactly what it was
// handed before.
const DefaultMaxContextBytes = 100_000

// resolveMaxContextBytes picks the ceiling in force: the STEP's if it set one,
// else the agent's, else the default — so nothing downstream has to nil-check a
// number or know the precedence.
//
// Step wins because it is the narrower statement about the same conversation:
// context_paths: is step-level, so the step is what knows how much evidence it
// is handing over.
func resolveMaxContextBytes(step, agent int) int {
	if step > 0 {
		return step
	}

	if agent > 0 {
		return agent
	}

	return DefaultMaxContextBytes
}

// validateMaxContextBytes rejects a negative ceiling, on an agent or on a
// step, and a step-level one on anything that is not an agent step. Zero is the
// documented "take the default"/"defer to the agent", so only a negative is
// meaningless here.
func (c *Config) validateMaxContextBytes() error {
	for _, agent := range c.Agents {
		if agent.MaxContextBytes < 0 {
			return fmt.Errorf("agent %q: max_context_bytes must be a positive number of bytes (omit it for the default of %d)", agent.Name, DefaultMaxContextBytes)
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.MaxContextBytes == 0 {
				return nil
			}

			if step.MaxContextBytes < 0 {
				return fmt.Errorf("%s: max_context_bytes must be a positive number of bytes (omit it to take the agent's)", label)
			}

			if step.Agent == "" {
				return fmt.Errorf("%s: max_context_bytes is only valid on agent steps", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
