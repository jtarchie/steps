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
	// Timeout is a wall-clock deadline per attempt (e.g., "2m", "30s"). Empty
	// means no timeout. Step.Timeout overrides Task.Timeout when set.
	Timeout string
	// Assert, when set, checks the task's captured stdout/exit code (see
	// Assert). It always comes from the step (top-level tasks: entries carry
	// no assert), so a matching assert makes a non-zero-exit task a success.
	Assert *Assert
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
			Inputs: step.InputNames(), Outputs: step.Outputs, Image: step.Image, Timeout: step.Timeout,
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

	image := task.Image
	if step.Image != "" {
		image = step.Image
	}

	timeout := task.Timeout
	if step.Timeout != "" {
		timeout = step.Timeout
	}

	return ResolvedTask{
		Name: step.Task, Run: task.Run, Fix: fix, Inputs: inputs, Outputs: outputs, Image: image, Timeout: timeout,
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
	Persona     string
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
	ToolSpecs    []ToolSpec
	// StringOnlyToolChoice, when true, forces a required tool call (see
	// forceRequiredTool in internal/agent) via tool_choice: "required"
	// instead of a named function object — for providers whose
	// OpenAI-compat server rejects the object form. See resolveAgentTarget.
	StringOnlyToolChoice bool
	// Image, when non-empty, runs this step's run_shell/custom-tool commands
	// in a container from this image instead of on the host. See
	// Agent.Image/Step.Image.
	Image string
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

	baseURL, modelName, apiKeyEnv, requiresKey, stringOnlyToolChoice, err := resolveAgentTarget(agent.Source)
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

	compactAfterTokens, contextWindow := resolveCompactionBudget(modelName, agent.CompactAfterTokens)

	image := agent.Image
	if step.Image != "" {
		image = step.Image
	}

	return ResolvedInvocation{
		AgentName:            agent.Name,
		Description:          agent.Description,
		BaseURL:              baseURL,
		ModelName:            modelName,
		APIKeyEnv:            apiKeyEnv,
		RequiresKey:          requiresKey,
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
		ToolSpecs:            toolSpecs,
		StringOnlyToolChoice: stringOnlyToolChoice,
		Image:                image,
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
func (ri ResolvedInvocation) WithSource(source AgentSource, explicitCompactAfterTokens *int) (ResolvedInvocation, error) {
	baseURL, modelName, apiKeyEnv, requiresKey, stringOnlyToolChoice, err := resolveAgentTarget(source)
	if err != nil {
		return ResolvedInvocation{}, fmt.Errorf("fallback source: %w", err)
	}

	ri.BaseURL = baseURL
	ri.ModelName = modelName
	ri.APIKeyEnv = apiKeyEnv
	ri.RequiresKey = requiresKey
	ri.StringOnlyToolChoice = stringOnlyToolChoice

	// The compaction budget follows the model that will actually serve the
	// conversation — a 200K fallback must not inherit a 1M primary's budget.
	// An explicit compact_after_tokens: still wins, since the operator set it
	// for this agent, not for one of its endpoints.
	ri.CompactAfterTokens, ri.ContextWindow = resolveCompactionBudget(modelName, explicitCompactAfterTokens)

	return ri, nil
}
