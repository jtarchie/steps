package config

// Config merge: how a step, the tasks:/agents: entry it names, and the
// defaults combine into the one resolved shape that plan-time hashing and
// run-time execution both read.

import (
	"fmt"
	"strings"
)

// ResolvedTask is a task step's run/fix, resolved against either the step's
// own inline fields or a tasks: entry it references by name. Both the merkle
// planner and the executor call ResolveTask so plan-time hashing and
// run-time execution stay in lockstep.
type ResolvedTask struct {
	Name    string
	Run     string
	Fix     *FixSpec
	Inputs  []string
	Outputs []string
	// InputMapping/OutputMapping rename declared inputs/outputs onto plan-
	// artifact names under workspace: isolation (see Step.InputMapping). Always
	// from the step (tasks: entries carry no mapping); empty leaves names
	// unmapped.
	InputMapping  map[string]string
	OutputMapping map[string]string
	// Image, when non-empty, runs this task's run: (and any fix-loop
	// re-runs) in a container from this image instead of on the host. See
	// Task.Image/Step.Image.
	Image string
	// Env names the host environment variables this task's commands may see
	// beyond the baseline. See Task.Env/Step.Env.
	Env []string
	// User is the container user this task's commands run as. See
	// Task.User/Step.User.
	User string
	// Network is the container network this task's commands join. See
	// Task.Network/Step.Network.
	Network string
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// means no timeout. Step.Timeout overrides Task.Timeout when set.
	Timeout string
	// Assert, when set, checks the task's captured stdout/exit code (see
	// Assert). It always comes from the step (top-level tasks: entries carry
	// no assert), so a matching assert makes a non-zero-exit task a success.
	Assert *Assert
}

// resolveTaskRuntime merges the execution settings a step may override on the
// tasks: entry it references. The agents: sibling is resolveAgentRuntime; both
// exist to keep their callers inside the linter's complexity budget.
//
// image:, user: and network: are non-empty-wins; env: is DECLARED-wins,
// because an explicit `env: []` on a step means "nothing beyond the baseline"
// and a non-empty test would silently keep the task's list instead.
func resolveTaskRuntime(task *Task, step Step) containerSettings {
	settings := task.containerSettings()

	if step.Image != "" {
		settings.Image = step.Image
	}

	if step.Env != nil {
		settings.Env = step.Env
	}

	if step.User != "" {
		settings.User = step.User
	}

	if step.Network != "" {
		settings.Network = step.Network
	}

	return settings
}

// ResolveTask resolves step into a ResolvedTask: a step carrying its own
// run: is inline and used as-is; otherwise step.Task names a tasks: entry,
// whose run/fix/inputs/outputs/image/timeout are used, except the step's own
// fix:, inputs:, outputs:, image:, and timeout:, if set (non-nil/non-empty),
// which override the referenced task's for this step only — the same override
// idiom for all five.
func (c *Config) ResolveTask(step Step) (ResolvedTask, error) {
	if step.Run != "" {
		return ResolvedTask{
			Name: step.Task, Run: step.Run, Fix: step.Fix,
			Inputs: step.InputNames(), Outputs: step.Outputs, Image: step.Image, Env: step.Env, User: step.User, Network: step.Network, Timeout: step.Timeout,
			InputMapping: step.InputMapping, OutputMapping: step.OutputMapping,
			Assert: step.Assert,
		}, nil
	}

	task, err := c.FindTask(step.Task)
	if err != nil {
		return ResolvedTask{}, fmt.Errorf("task %q: %w", step.Task, err)
	}

	fix := task.Fix
	if step.Fix != nil {
		fix = step.Fix
	}

	inputs := task.Inputs.names()
	if step.InputsDeclared() {
		inputs = step.InputNames()
	}

	outputs := task.Outputs
	if step.Outputs != nil {
		outputs = step.Outputs
	}

	runtime := resolveTaskRuntime(task, step)

	timeout := task.Timeout
	if step.Timeout != "" {
		timeout = step.Timeout
	}

	return ResolvedTask{
		Name: step.Task, Run: task.Run, Fix: fix, Inputs: inputs, Outputs: outputs,
		Image: runtime.Image, Env: runtime.Env, User: runtime.User, Network: runtime.Network, Timeout: timeout,
		InputMapping: step.InputMapping, OutputMapping: step.OutputMapping, Assert: step.Assert,
	}, nil
}

// ResolvedInvocation is an agent + step reduced to everything needed to hash
// and run the step: the resolved connection, persona, dials, limits, and the
// effective (merged) tool set. ResolveAgentInvocation produces it for both
// planning (merkle hashing) and execution, so both compute identical hashes.
type ResolvedInvocation struct {
	AgentName   string
	Description string
	BaseURL     string
	ModelName   string
	APIKeyEnv   string
	RequiresKey bool
	// CLI, when non-empty, names the coding-agent CLI that runs this
	// invocation as a subprocess instead of an HTTP conversation (see
	// cliagent.go). BaseURL is empty in that case, and ModelName is the model
	// the CLI is asked for (e.g. "sonnet"). Hashed as `cli`, so moving an
	// agent between a CLI and a hosted provider invalidates its cache.
	CLI     string
	Persona string
	// ContextPaths mirrors Step.ContextPaths once resolved — populated from
	// the step, not the agent definition, since concrete input paths are
	// only known at the step level.
	ContextPaths []string
	// Generation dials, mirroring Agent's own fields once resolved. Kept flat
	// here (rather than a nested type) so this package doesn't need to depend
	// on anything LLM-client-specific — internal/agent assembles its own
	// request-config shape from these.
	Temperature     *float64
	TopP            *float64
	MaxTokens       int
	ReasoningEffort string // "", "low", "medium", or "high"
	MaxTurns        int
	Attempts        int
	Timeout         string // wall-clock deadline per attempt; empty means no timeout
	// CompactAfterTokens is the already-resolved conversation-size budget (see
	// Agent.CompactAfterTokens); 0 means compaction is disabled for this
	// invocation.
	CompactAfterTokens int
	// ContextWindow is the model's context window when this package
	// recognizes the model (see contextWindows), 0 when it does not. It is
	// what CompactAfterTokens was derived from, carried so a run can SAY
	// "compacting at 102400 against a 1000000 window" — the mismatch that was
	// previously invisible until compaction stalled.
	ContextWindow int
	// BudgetTokens is the ceiling on provider-reported tokens this one
	// invocation may spend (see Agent.Budget); 0 means no ceiling. Never
	// hashed — it is an operational limit, like Timeout.
	BudgetTokens int
	// BudgetUSD is the dollar ceiling a CLI agent hands its subprocess (see
	// Budget.USD); 0 means no ceiling. Never hashed, for the same reason
	// BudgetTokens is not.
	BudgetUSD float64
	ToolSpecs []ToolSpec
	// StringOnlyToolChoice, when true, forces a required tool call (see
	// forceRequiredTool in internal/agent) via tool_choice: "required"
	// instead of a named function object — for providers whose
	// OpenAI-compat server rejects the object form. See resolveAgentTarget.
	StringOnlyToolChoice bool
	// Env names the host environment variables this step's commands may see
	// beyond the baseline. See Agent.Env/Step.Env.
	Env []string
	// User is the container user this step's commands run as. See
	// Agent.User/Step.User.
	User string
	// Network is the container network this step's commands join. See
	// Agent.Network/Step.Network.
	Network string
	// Image, when non-empty, runs this step's run_shell/custom-tool commands
	// in a container from this image instead of on the host. See
	// Agent.Image/Step.Image.
	Image string
}

// resolveAgentRuntime merges the three container settings a step may override
// on its agents: entry. Split out of ResolveAgentInvocation only to keep that
// function inside the linter's complexity budget.
//
// image: and user: are non-empty-wins; env: is DECLARED-wins, because an
// explicit `env: []` on a step means "nothing beyond the baseline" and a
// non-empty test would silently keep the agent's list instead.
func resolveAgentRuntime(agent *Agent, step Step) (settings containerSettings) {
	settings = agent.containerSettings()

	if step.Image != "" {
		settings.Image = step.Image
	}

	if step.Env != nil {
		settings.Env = step.Env
	}

	if step.User != "" {
		settings.User = step.User
	}

	if step.Network != "" {
		settings.Network = step.Network
	}

	return settings
}

// ResolveAgentInvocation resolves the agent named by step against c,
// applying provider-prefix resolution, tool-grant merging, and defaulting
// (step.Attempts defaults to 1 — retries are a per-task concern, not part of
// the agent's config; agent.MaxTurns defaults to defaultMaxAgentTurns;
// agent.CompactAfterTokens, when nil, defaults to defaultCompactAfterTokens —
// unlike every other field resolved here, an explicit zero value is
// meaningfully different from "unset" and is preserved as 0, not defaulted).
func (c *Config) ResolveAgentInvocation(step Step) (ResolvedInvocation, error) {
	agent, err := c.FindAgent(step.Agent)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	target, err := resolveAgentTarget(agent.Source)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	toolSpecs, err := resolveEffectiveTools(agent.Tools, step.Tools)
	if err != nil {
		return ResolvedInvocation{}, err
	}

	reasoning := strings.ToLower(agent.ReasoningEffort)
	if reasoning != "" && !validReasoningEfforts[reasoning] {
		return ResolvedInvocation{}, fmt.Errorf("reasoning_effort %q must be one of low, medium, high", agent.ReasoningEffort)
	}

	maxTurns := agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxAgentTurns
	}

	attempts := step.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	compactAfterTokens, contextWindow := resolveCompactionBudget(target.ModelName, agent.ContextWindow, agent.CompactAfterTokens)

	runtime := resolveAgentRuntime(agent, step)

	return ResolvedInvocation{
		AgentName:            agent.Name,
		Description:          agent.Description,
		BaseURL:              target.BaseURL,
		ModelName:            target.ModelName,
		APIKeyEnv:            target.APIKeyEnv,
		RequiresKey:          target.RequiresKey,
		CLI:                  target.CLI,
		Persona:              agent.System,
		ContextPaths:         step.ContextPaths,
		Temperature:          agent.Temperature,
		TopP:                 agent.TopP,
		MaxTokens:            agent.MaxTokens,
		ReasoningEffort:      reasoning,
		MaxTurns:             maxTurns,
		Attempts:             attempts,
		Timeout:              step.Timeout,
		CompactAfterTokens:   compactAfterTokens,
		ContextWindow:        contextWindow,
		BudgetTokens:         budgetTokens(agent.Budget),
		BudgetUSD:            budgetUSD(agent.Budget),
		ToolSpecs:            toolSpecs,
		StringOnlyToolChoice: target.StringOnlyToolChoice,
		Image:                runtime.Image,
		Env:                  runtime.Env,
		User:                 runtime.User,
		Network:              runtime.Network,
	}, nil
}

// WithSource returns a copy of ri reaching a different endpoint/model/
// credential, leaving everything else — persona, dials, limits, tool grant —
// exactly as resolved.
//
// It backs agent failover: an outage changes where a step's requests GO, and
// must change nothing about what the agent is or is allowed to do. It also
// keeps the failover path out of hashing: the caller hashes the primary
// invocation and runs this one, so which source actually served a run is
// availability, not content, and a fallback firing on one run cannot
// invalidate a cache entry.
// It takes the whole agent, not just its compact_after_tokens:, because the
// compaction budget is now derived from two of the agent's own fields and a
// second positional override would be one more blank to count at every call
// site — the same argument agentTarget's doc comment makes. A nil agent means
// "no explicit overrides", which is what the two nil-able knobs it reads
// already mean individually.
func (ri ResolvedInvocation) WithSource(source AgentSource, agent *Agent) (ResolvedInvocation, error) {
	target, err := resolveAgentTarget(source)
	if err != nil {
		return ResolvedInvocation{}, fmt.Errorf("fallback source: %w", err)
	}

	if agent == nil {
		agent = &Agent{}
	}

	ri.BaseURL = target.BaseURL
	ri.ModelName = target.ModelName
	ri.APIKeyEnv = target.APIKeyEnv
	ri.RequiresKey = target.RequiresKey
	ri.StringOnlyToolChoice = target.StringOnlyToolChoice
	// Set AND cleared: failing over between a CLI and a hosted provider in
	// either direction has to change which machinery runs the conversation.
	ri.CLI = target.CLI

	// The compaction budget follows the model that will actually serve the
	// conversation — a 200K fallback must not inherit a 1M primary's budget.
	// An explicit compact_after_tokens:/context_window: still wins, since the
	// operator set those for this agent, not for one of its endpoints.
	ri.CompactAfterTokens, ri.ContextWindow = resolveCompactionBudget(target.ModelName, agent.ContextWindow, agent.CompactAfterTokens)

	return ri, nil
}
