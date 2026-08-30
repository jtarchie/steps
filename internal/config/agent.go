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
	// Privileged runs this command's container with `docker run --privileged`.
	// Mirrors Concourse's privileged: (concourse-ci.org/docs/steps/task/).
	// Container-only, like Network — a host command has nothing to elevate,
	// so it is a load error without image:.
	Privileged bool `yaml:"privileged,omitempty"`
	// Limits caps the container's CPU and memory. Mirrors Concourse's
	// container_limits:; container-only for the same reason Privileged is.
	Limits *ContainerLimits `yaml:"container_limits,omitempty"`
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
	// MaxTurns caps the tool-calling loop. Unset takes defaultMaxAgentTurns;
	// an explicit 0 removes the cap entirely — see dials.go for the
	// convention this shares with MaxContextBytes and Timeout.
	MaxTurns *int `yaml:"max_turns,omitempty"`
	// MaxQuestions is the ask_user budget every step of this agent gets when
	// it declares none of its own. Unset takes defaultMaxQuestions; an
	// explicit 0 removes the cap. See Step.MaxQuestions for why the dial
	// counts questions rather than turns.
	MaxQuestions *int `yaml:"max_questions,omitempty"`
	// Timeout is the wall-clock deadline every step of this agent gets when
	// it declares none of its own (e.g. "20m"). Unset takes the package
	// default (30 minutes — agent.agentStepTimeout); "0" means no deadline at
	// all, which no other step kind needs a spelling for because omitting the
	// field already says it there.
	//
	// It lives here as well as on the step because the right deadline is
	// usually a property of the AGENT — a deep reviewer needs twenty minutes
	// whichever step invokes it — and without it a shared deadline had to be
	// copied onto every step naming the agent. Not hashed, exactly as the
	// step's own timeout: is not: a deadline is not part of what a step asks
	// for.
	Timeout string `yaml:"timeout,omitempty"`
	// Attempts is how many times a step of this agent re-issues a failed
	// REQUEST when it sets no attempts: of its own (see defaultAgentAttempts,
	// and docs/attempts-timeout.md for why a retry here is a transport
	// concern rather than a re-run of the conversation). Unset takes that
	// default; 0 is a load error, not "unlimited" — see dials.go. Never
	// hashed.
	Attempts *int `yaml:"attempts,omitempty"`
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
	// model at conversation start. Unset takes DefaultMaxContextBytes; an
	// explicit 0 hands over the whole file however large it is (see dials.go).
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
	MaxContextBytes *int `yaml:"max_context_bytes,omitempty"`
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
	//
	// The ceiling covers this agent AND everything it delegates to: a
	// sub-agent draws on this allowance rather than adding to it, so the
	// number bounds the whole subtree rather than one conversation in it.
	Budget *Budget `yaml:"budget,omitempty"`
	// DelegateBudgetPercent overrides how much of this agent's REMAINING
	// allowance one sub-agent call may take, for agents whose helpers are
	// unusually cheap or expensive relative to the parent. Pipeline-wide
	// default in Defaults.DelegateBudgetPercent. Never hashed, like the
	// budget it divides.
	DelegateBudgetPercent *int `yaml:"delegate_budget_percent,omitempty"`
	// Tools is the grant: the built-in tools this agent may use plus any
	// reusable custom tool definitions. A step selects a subset by name and
	// may add its own inline custom tools. Empty grants the read-only
	// built-ins (DefaultAgentToolSpecs).
	Tools []ToolSpec `yaml:"tools,omitempty"`
	// Settings opts a CLI-backed agent into the CLI's own configuration
	// scopes. The only accepted value is "project" — load the repo's
	// checked-in project scope (.claude/ settings, CLAUDE.md, hooks).
	// Absent, the subprocess loads no settings at all: a pipeline step is
	// not a personal session, and repo config shaping an agent is a
	// capability the pipeline must declare, not inherit. Rejected on hosted
	// (HTTP) agents, where there is no CLI to configure.
	Settings string `yaml:"settings,omitempty"`
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

// defaultMaxQuestions is how many times one agent step may interrupt its end
// user when neither the step nor the agent says otherwise.
//
// Small, and the asymmetry is the argument. The cost of too low is that a
// model runs out of asks and has to proceed on its best reading — which it
// is told, as ordinary tool-result data, so it can say so in its answer. The
// cost of too high is a person answering questions all afternoon, which is
// how a feature stops being used at all. Three is enough for a step to
// resolve the ambiguity it actually hit and not enough to hold an interview.
const defaultMaxQuestions = 3

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

// validReasoningEfforts are the only accepted values for an agent's
// reasoning_effort. The corresponding genai.ThinkingLevel mapping lives in
// internal/agent, which is the only package that needs the LLM-specific
// value side of this table.
var validReasoningEfforts = map[string]bool{"low": true, "medium": true, "high": true} //nolint:gochecknoglobals // static, read-only lookup table

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
// number or know the precedence. A resolved 0 is "no ceiling", not "unset".
//
// Step wins because it is the narrower statement about the same conversation:
// context_paths: is step-level, so the step is what knows how much evidence it
// is handing over.
func resolveMaxContextBytes(step, agent *int) int {
	return orDefault(step, orDefault(agent, DefaultMaxContextBytes))
}

// validateMaxContextBytes rejects a negative ceiling, on an agent or on a
// step, and a step-level one on anything that is not an agent step. Zero is
// the documented "hand the file over whole" (see dials.go), so only a negative
// is meaningless here.
func (c *Config) validateMaxContextBytes() error {
	for _, agent := range c.Agents {
		if agent.MaxContextBytes != nil && *agent.MaxContextBytes < 0 {
			return fmt.Errorf("agent %q: max_context_bytes must not be negative (omit it for the default of %d, or set 0 for no ceiling)", agent.Name, DefaultMaxContextBytes)
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.MaxContextBytes == nil {
				return nil
			}

			if *step.MaxContextBytes < 0 {
				return fmt.Errorf("%s: max_context_bytes must not be negative (omit it to take the agent's, or set 0 for no ceiling)", label)
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
