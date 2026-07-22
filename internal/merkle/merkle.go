// Package merkle plans a job's steps into content-addressed chains and
// computes the hashes used to skip already-succeeded work. The content-map
// builders here (GetNodeContent/TaskNodeContent/PutNodeContent/
// AgentContentMap) are shared between planning (this package) and real
// execution (internal/pipeline, internal/agent) so both compute identical
// hashes for identical steps.
package merkle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// NodeKind identifies which kind of plan step a Node represents.
type NodeKind string

// The four step kinds a Node can represent.
const (
	NodeKindGet   NodeKind = "get"
	NodeKindTask  NodeKind = "task"
	NodeKindPut   NodeKind = "put"
	NodeKindAgent NodeKind = "agent"
)

// Node is one content-addressed step in a job's resolved execution chain.
// Its Hash folds in ParentHash, so Hash alone identifies the entire chain
// of steps that produced it.
type Node struct {
	Hash       string
	ParentHash string
	Kind       NodeKind
	StepIndex  int
	Resource   string // resource name (get/put) or task name (task); metadata only, not hashed
	Content    map[string]any
}

// Chain is one root-to-leaf path through a job's plan: exactly what a
// triggered build's recursion walks today (a plain chain, not a general
// multi-parent DAG, since get: version: every is the only fan-out point).
type Chain struct {
	Nodes    []Node
	RootHash string
	// Unskippable is true if any node is a put step, an agent step, or a task
	// with a fix: — all have side effects or are non-deterministic (agent
	// runs, and a fix: task's success may depend on one) and must always run
	// rather than reuse a prior hash match.
	Unskippable bool
}

// GetNodeContent builds the content map hashed for a get node. It, along
// with TaskNodeContent, PutNodeContent, and AgentContentMap below, is shared
// between planning (this file) and real execution (internal/pipeline,
// internal/agent) so both compute identical hashes for identical steps.
func GetNodeContent(cfg *config.Config, step config.Step, resourceType config.ResourceType, source, version map[string]any) (map[string]any, error) {
	content := map[string]any{
		"in_template": resourceType.Config.In,
		"source":      source,
		"version":     version,
	}

	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	err := withMCPResourceStage(cfg, resourceType, "in", content)
	if err != nil {
		return nil, err
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// withMCPResourceStage folds an mcp-backed resource type's server identity
// and the named lifecycle stage's tool name into content, mirroring
// in_template/out_template's role for the shell backend — but only when
// that stage's *MCPToolCall is actually set (In and Out are both optional;
// an unset In means get falls back to writing version.json, with nothing
// template-shaped to hash, exactly like check: is never hashed for the
// shell backend either). A resource type with no mcp: block is unaffected
// (byte-identical to before this field existed), same as every other
// value-gated field in this file.
func withMCPResourceStage(cfg *config.Config, resourceType config.ResourceType, stage string, content map[string]any) error {
	if resourceType.Config.MCP == nil {
		return nil
	}

	var call *config.MCPToolCall

	switch stage {
	case "in":
		call = resourceType.Config.MCP.In
	case "out":
		call = resourceType.Config.MCP.Out
	}

	if call == nil {
		return nil
	}

	server, err := mcpServerContent(cfg, resourceType.Config.MCP.Server)
	if err != nil {
		return err
	}

	content["mcp_"+stage+"_tool"] = call.Tool
	content["mcp_server"] = server

	return nil
}

// withWhen folds a step's when: guard command into content, but only when the
// step carries one — so a step without a guard hashes byte-identically to
// before this field existed (the same value-gating as image:). The guard
// decides whether the step executes at all, so changing it must invalidate the
// cache. Only the command is hashed: its *outcome* is a run-time fact the
// planner cannot know, and a cached node was by definition produced by a run
// the guard already allowed.
func withWhen(step config.Step, content map[string]any) map[string]any {
	if step.When != nil && step.When.Run != "" {
		content["when"] = step.When.Run
	}

	return content
}

// withRouting folds a step's routing surface — to:, max_visits:, and (for a
// verdict agent) verdicts: — into content, but only when set, so a step with
// no routing hashes byte-identically to before these fields existed (the same
// value-gating as image:/when:). The chain containing a routing step is already
// unconditionally unskippable (see internal/pipeline's chainUnskippable and
// planNonGetNode below), so this isn't needed for skip-decision correctness —
// it's so a node hash shared across chains (e.g. via get: version: every fan-
// out) still changes when the routing changes, instead of colliding with a
// stale cached success from before the edit. verdicts: matters because it
// changes the synthesized required verdict tool set (internal/agent), so it
// genuinely alters what the step executes.
func withRouting(step config.Step, content map[string]any) map[string]any {
	if step.To != nil {
		content["to"] = step.To // map[string]string — json.Marshal sorts keys, so the hash stays deterministic
		content["max_visits"] = step.MaxVisits
	}

	if len(step.Verdicts) != 0 {
		content["verdicts"] = step.Verdicts // order-significant: it is the emitted enum
	}

	return content
}

// withHooks folds a step's resolved hook content into content, but only when
// the step actually carries hooks — so a step with no hooks hashes
// byte-identically to before this field existed (the same value-gating as
// image:). Each hook's content is its own resolved content map, so editing a
// hook, or the tasks:/agents: entry it references, invalidates the enclosing
// step's hash. Nested hooks recurse through the same builders.
func withHooks(cfg *config.Config, step config.Step, content map[string]any) (map[string]any, error) {
	if step.Hooks.Empty() {
		return content, nil
	}

	hooks, err := hooksContent(cfg, step.Hooks)
	if err != nil {
		return nil, err
	}

	content["hooks"] = hooks

	return content, nil
}

func hooksContent(cfg *config.Config, hooks config.Hooks) (map[string]any, error) {
	out := map[string]any{}

	err := hooks.Each(func(name string, step *config.Step) error {
		hc, err := hookContentMap(cfg, *step)
		if err != nil {
			return fmt.Errorf("%s hook: %w", name, err)
		}

		out[name] = hc

		return nil
	})
	if err != nil {
		return nil, err //nolint:wrapcheck // already wrapped with the hook name above
	}

	return out, nil
}

// hookContentMap builds the hashed content for one hook step, dispatching on
// its kind through the same builders a plan step uses (which recurse into the
// hook's own hooks). get is never a valid hook (rejected at LoadConfig), so it
// is not handled here.
func hookContentMap(cfg *config.Config, step config.Step) (map[string]any, error) {
	kind, ok := step.Kind()
	if !ok {
		return nil, errors.New("unrecognized hook step (must be task, put, or agent)")
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet, not a valid hook body
	case config.StepKindTask:
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return nil, fmt.Errorf("resolve task: %w", err)
		}

		return TaskNodeContent(cfg, step, rt)
	case config.StepKindPut:
		res, err := cfg.FindResource(step.Put)
		if err != nil {
			return nil, fmt.Errorf("resolve put: %w", err)
		}

		resourceType, err := cfg.FindResourceType(res.Type)
		if err != nil {
			return nil, fmt.Errorf("resolve put: %w", err)
		}

		return PutNodeContent(cfg, step, *resourceType, res.Source, step.Params, step.Inputs)
	case config.StepKindAgent:
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return nil, fmt.Errorf("resolve agent: %w", err)
		}

		return AgentContentMap(cfg, step, ri)
	default: // config.StepKindGet — not a valid hook body
		return nil, errors.New("unrecognized hook step (must be task, put, or agent)")
	}
}

// TaskNodeContent and PutNodeContent fold in inputs/outputs only when ws is
// non-nil (workspace: configured), so a pipeline that never opts in hashes
// byte-identically to before this field existed — switching a task between
// shared and isolated execution of the same run: must invalidate its cache,
// but the mere existence of the feature must not invalidate anyone else's.
// image, by contrast, is folded in whenever it's non-empty, regardless of
// ws: unlike inputs/outputs (whose relevance is gated by the workspace
// feature existing at all), an image change alters what a run: command
// actually executes against no matter which workspace mode is active — so
// the gate is on the value itself. A pipeline that never sets image: still
// hashes byte-identically to before this field existed.
func TaskNodeContent(cfg *config.Config, step config.Step, rt config.ResolvedTask) (map[string]any, error) {
	content := map[string]any{"run": rt.Run}

	if cfg.Workspace != nil {
		content["inputs"] = config.StableStrings(rt.Inputs)
		content["outputs"] = config.StableStrings(rt.Outputs)
	}

	if rt.Image != "" {
		content["image"] = rt.Image
	}

	if rt.Assert != nil {
		content["assert"] = assertContent(rt.Assert)
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// assertContent builds the stable content map for a task/agent step's assert
// (see config.Assert), folded into the node hash whenever set — an assert
// changes the step's success criteria, so it must invalidate the cache. Only
// the fields that are set appear, so absent-field hashes stay stable.
func assertContent(a *config.Assert) map[string]any {
	content := map[string]any{}

	if a.Stdout != nil {
		content["stdout"] = *a.Stdout
	}

	if a.Code != nil {
		content["code"] = *a.Code
	}

	// tool_calls folds in only when set, like every other assert field: it
	// changes the step's success criteria, so it must bust the cache, but an
	// assert without it hashes exactly as before this field existed. Each
	// entry is flattened to plain maps/scalars so json.Marshal's key sorting
	// still makes the hash deterministic (see HashNode).
	if len(a.ToolCalls) > 0 {
		calls := make([]map[string]any, len(a.ToolCalls))

		for i, call := range a.ToolCalls {
			entry := map[string]any{"name": call.Name}
			if len(call.Args) > 0 {
				entry["args"] = call.Args
			}

			calls[i] = entry
		}

		content["tool_calls"] = calls
	}

	return content
}

// PutNodeContent builds the content map hashed for a put node. image is
// folded in whenever non-empty — see TaskNodeContent's doc comment for why
// this differs from the inputs/ws gating.
func PutNodeContent(cfg *config.Config, step config.Step, resourceType config.ResourceType, source, params map[string]any, inputs []string) (map[string]any, error) {
	content := map[string]any{
		"out_template": resourceType.Config.Out,
		"source":       source,
		"params":       params,
	}

	if cfg.Workspace != nil {
		content["inputs"] = config.StableStrings(inputs)
	}

	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	err := withMCPResourceStage(cfg, resourceType, "out", content)
	if err != nil {
		return nil, err
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// toolSpecsContent builds the hashed content for an agent's effective tool
// list. A builtin/custom tool hashes exactly as before this helper existed
// ({builtin, name, description, run}) — so an agent with no sub-agent tools
// hashes byte-identically. A sub-agent tool (see config.ToolSpec.Agent) folds
// in the child agent's own resolved invocation content
// (subAgentInvocationContent), recursively, so editing a child — or a
// grandchild — busts the parent step's hash. Recursion terminates because
// LoadConfig's validateAgentGraph rejects cycles and caps nesting depth.
func toolSpecsContent(cfg *config.Config, specs []config.ToolSpec) ([]map[string]any, error) {
	out := make([]map[string]any, len(specs))

	for i, t := range specs {
		if t.Agent != "" {
			invocation, err := subAgentInvocationContent(cfg, t.Agent)
			if err != nil {
				return nil, err
			}

			out[i] = map[string]any{
				"agent":       t.Agent,
				"description": t.Description,
				"invocation":  invocation,
			}

			continue
		}

		if t.MCP != "" {
			content, err := mcpToolSpecContent(cfg, t)
			if err != nil {
				return nil, err
			}

			out[i] = content

			continue
		}

		content := map[string]any{
			"builtin":     t.Builtin,
			"name":        t.Name,
			"description": t.Description,
			"run":         t.Run,
		}

		// max_calls/args fold in only when set — a tool with neither hashes
		// byte-identically to before this feature existed. Both change what a
		// call actually executes with (a budget bounds it; pinned args alter
		// its arguments), the same argument TaskNodeContent's doc comment
		// makes for image: over the workspace-gated inputs/outputs.
		if t.MaxCalls != 0 {
			content["max_calls"] = t.MaxCalls
		}

		if len(t.Args) != 0 {
			content["args"] = t.Args
		}

		out[i] = content
	}

	return out, nil
}

// mcpToolSpecContent builds the hashed content for one of the three MCP
// tool-grant forms (see config.ToolSpec.MCP/MCPTool/MCPTools): mcp_tool for
// the single-tool form, mcp_tools (sorted, for hash determinism regardless
// of declaration order) for the named-subset form, or neither for the bare
// "grant everything" form — which is a deliberate, documented limitation:
// this package depends on config/resource only (never internal/mcp), so it
// cannot list a live server's tools at plan time, and the bare form's hash
// is therefore a static marker that a server's own tool list changing does
// not, by itself, bust. description/max_calls fold in only when set, the
// same value-gating every other tool kind here uses.
func mcpToolSpecContent(cfg *config.Config, t config.ToolSpec) (map[string]any, error) {
	server, err := mcpServerContent(cfg, t.MCP)
	if err != nil {
		return nil, err
	}

	content := map[string]any{"mcp": t.MCP, "server": server}

	if t.MCPTool != "" {
		content["mcp_tool"] = t.MCPTool
	}

	if len(t.MCPTools) != 0 {
		sorted := slices.Clone(t.MCPTools)
		slices.Sort(sorted)
		content["mcp_tools"] = sorted
	}

	if t.Description != "" {
		content["description"] = t.Description
	}

	if t.MaxCalls != 0 {
		content["max_calls"] = t.MaxCalls
	}

	return content, nil
}

// mcpServerContent builds the hashed identity of a configured mcp_servers:
// entry, folded into any tool grant or resource-type stage that references
// it. Endpoint/auth type/the api_key_env *name* (never its value, mirroring
// AgentSource's api_key_env exclusion exactly)/scopes determine behavior
// and are hashed; nothing token-shaped is ever computed here, since this
// package never imports internal/mcp.
func mcpServerContent(cfg *config.Config, name string) (map[string]any, error) {
	srv, err := cfg.FindMCPServer(name)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", name, err)
	}

	content := map[string]any{
		"name":      srv.Name,
		"endpoint":  srv.Endpoint,
		"auth_type": srv.Auth.Type,
	}

	if srv.Auth.APIKeyEnv != "" {
		content["api_key_env"] = srv.Auth.APIKeyEnv
	}

	if len(srv.Auth.Scopes) != 0 {
		content["scopes"] = srv.Auth.Scopes
	}

	return content, nil
}

// subAgentInvocationContent builds the hashed identity of a sub-agent as
// invoked through a tool: everything that determines its output regardless of
// the per-call request — resolved model/endpoint, persona, dials, max_turns,
// image, and its own tool set (recursively). Prompt/dir/inputs/outputs/assert/
// hooks are deliberately excluded: a sub-agent has no step, so those are not
// part of its identity. The API key and its env var name are excluded for the
// same reason AgentContentMap excludes them.
func subAgentInvocationContent(cfg *config.Config, name string) (map[string]any, error) {
	ri, err := cfg.ResolveAgentInvocation(config.Step{Agent: name})
	if err != nil {
		return nil, fmt.Errorf("sub-agent %q: %w", name, err)
	}

	tools, err := toolSpecsContent(cfg, ri.ToolSpecs)
	if err != nil {
		return nil, err
	}

	content := map[string]any{
		"agent":            name,
		"model":            ri.ModelName,
		"endpoint":         ri.BaseURL,
		"system":           ri.Persona,
		"temperature":      ri.Temperature,
		"top_p":            ri.TopP,
		"max_tokens":       ri.MaxTokens,
		"reasoning_effort": ri.ReasoningEffort,
		"max_turns":        ri.MaxTurns,
		"tools":            tools,
	}

	if ri.Image != "" {
		content["image"] = ri.Image
	}

	return content, nil
}

// AgentContentMap is the content hashed for an agent node: everything that
// determines the model's output (agent, prompt, dir, resolved model/endpoint,
// persona, dials, and the effective tool set — including any sub-agent tools,
// folded in via toolSpecsContent). Attempts is excluded (a pure retry policy
// doesn't change the intended result); the API key and its env var name are
// excluded (nothing secret-adjacent belongs in hashed content). inputs/outputs
// are folded in only when ws is non-nil (workspace: configured) — see
// TaskNodeContent's doc comment for why.
func AgentContentMap(cfg *config.Config, step config.Step, ri config.ResolvedInvocation) (map[string]any, error) {
	toolsContent, err := toolSpecsContent(cfg, ri.ToolSpecs)
	if err != nil {
		return nil, err
	}

	content := map[string]any{
		"agent":            step.Agent,
		"prompt":           step.Prompt,
		"dir":              step.Dir,
		"model":            ri.ModelName,
		"endpoint":         ri.BaseURL,
		"system":           ri.Persona,
		"temperature":      ri.Temperature,
		"top_p":            ri.TopP,
		"max_tokens":       ri.MaxTokens,
		"reasoning_effort": ri.ReasoningEffort,
		"max_turns":        ri.MaxTurns,
		"tools":            toolsContent,
	}

	if cfg.Workspace != nil {
		content["inputs"] = config.StableStrings(step.Inputs)
		content["outputs"] = config.StableStrings(step.Outputs)
	}

	if ri.Image != "" {
		content["image"] = ri.Image
	}

	if step.Assert != nil {
		content["assert"] = assertContent(step.Assert)
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// HashNode computes a Node's content-addressed hash: sha256 hex of the
// canonical JSON of {kind, content, parent}. encoding/json.Marshal sorts
// map keys, which is what makes this deterministic — content must stay
// built from plain maps/slices/scalars for that guarantee to hold.
func HashNode(kind NodeKind, content map[string]any, parentHash string) (string, error) {
	payload := map[string]any{
		"kind":    string(kind),
		"content": content,
		"parent":  parentHash,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("hash %s node: %w", kind, err)
	}

	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:]), nil
}

// PlanChains walks a job's plan exactly like the pipeline's runSteps does
// (reusing resource.ResolveVersions for get-step version/pin resolution, so
// that logic stays defined in exactly one place), but never executes
// anything — no RunIn, RunShell, or RunOut. It returns one Chain per leaf
// reached, which is more than one only when a get step uses version: every.
//
// cache, when non-nil, memoizes each get step's resolved versions so that a
// later run-time call through the same cache (see resource.Cache) reuses
// this plan-time check result instead of re-running the check command — pass
// the same *resource.Cache instance runSteps will use for this same job run.
// A nil cache reproduces today's behavior exactly (CheckVersions runs again
// during real execution for any branch that ends up running).
func PlanChains(ctx context.Context, cfg *config.Config, jobName string, steps []config.Step, pinned map[string]string, cache *rsrc.Cache) ([]Chain, error) {
	slog.Debug("job.plan", "job", jobName, "steps", len(steps))

	chains, err := planSteps(ctx, cfg, steps, pinned, nil, "", false, cache)
	if err != nil {
		return nil, err
	}

	slog.Debug("job.planned", "job", jobName, "chains", len(chains))

	return chains, nil
}

func planSteps(
	ctx context.Context, cfg *config.Config, steps []config.Step, pinned map[string]string,
	prefix []Node, parentHash string, unskippable bool, cache *rsrc.Cache,
) ([]Chain, error) {
	for i, step := range steps {
		if step.Get != "" {
			return planGetStep(ctx, cfg, steps, i, step, pinned, prefix, parentHash, unskippable, cache)
		}

		node, stepUnskippable, err := planNonGetNode(cfg, step, i, parentHash)
		if err != nil {
			return nil, err
		}

		prefix = append(prefix, node)
		parentHash = node.Hash
		unskippable = unskippable || stepUnskippable
	}

	return []Chain{{Nodes: prefix, RootHash: parentHash, Unskippable: unskippable}}, nil
}

// planNonGetNode builds the plan node for a task/put/agent step and reports
// whether that step makes its chain unskippable: put/agent always do (side
// effects, non-determinism), and a task does when it has a fix: (its success
// may depend on non-deterministic agent work) — including one inherited from
// a referenced tasks: entry.
func planNonGetNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, bool, error) {
	kind, ok := step.Kind()
	if !ok {
		return Node{}, false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet — planNonGetNode is only ever called for non-get steps
	case config.StepKindTask:
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return Node{}, false, fmt.Errorf("step %d: %w", i, err)
		}

		node, err := taskNode(cfg, step, rt, i, parentHash)

		// A task is unskippable if it has a fix:, or carries a when: guard or
		// to: routing — the latter two have run-time-only outcomes the planner
		// can't know, so a chain through them must never be recorded as a
		// reusable success. This mirrors internal/pipeline's runtime
		// chainUnskippable and makes the plan-time flag accurate rather than
		// relying on that runtime flag alone (Unskippable is not hashed, so this
		// only ever makes the cache more conservative).
		return node, rt.Fix != nil || step.When != nil || step.To != nil, err
	case config.StepKindPut:
		node, err := putNode(cfg, step, i, parentHash)

		return node, true, err
	case config.StepKindAgent:
		node, err := agentNode(cfg, step, i, parentHash)

		return node, true, err
	default: // config.StepKindGet — planNonGetNode is only ever called for non-get steps
		return Node{}, false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// planGetStep resolves step's version(s) and recurses into the remainder of
// steps once per version, returning one Chain per leaf reached. It always
// terminates the calling planSteps loop, mirroring the pipeline's
// runGetStep control flow.
func planGetStep(
	ctx context.Context, cfg *config.Config, steps []config.Step, i int, step config.Step, pinned map[string]string,
	prefix []Node, parentHash string, unskippable bool, cache *rsrc.Cache,
) ([]Chain, error) {
	res, resourceType, versions, err := cache.ResolveVersionsCached(ctx, cfg, step, pinned)
	if err != nil {
		return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	chains := make([]Chain, 0, len(versions))

	for _, version := range versions {
		content, err := GetNodeContent(cfg, step, *resourceType, res.Source, version)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		hash, err := HashNode(NodeKindGet, content, parentHash)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindGet, StepIndex: i, Resource: res.Name, Content: content}

		sub, err := planSteps(ctx, cfg, steps[i+1:], pinned, append(append([]Node{}, prefix...), node), hash, unskippable, cache)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		chains = append(chains, sub...)
	}

	return chains, nil
}

func taskNode(cfg *config.Config, step config.Step, rt config.ResolvedTask, i int, parentHash string) (Node, error) {
	content, err := TaskNodeContent(cfg, step, rt)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	hash, err := HashNode(NodeKindTask, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindTask, StepIndex: i, Resource: rt.Name, Content: content}, nil
}

func putNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	res, err := cfg.FindResource(step.Put)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(res.Type)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content, err := PutNodeContent(cfg, step, *resourceType, res.Source, step.Params, step.Inputs)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	hash, err := HashNode(NodeKindPut, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindPut, StepIndex: i, Resource: res.Name, Content: content}, nil
}

func agentNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	content, err := AgentContentMap(cfg, step, ri)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	hash, err := HashNode(NodeKindAgent, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAgent, StepIndex: i, Resource: ri.AgentName, Content: content}, nil
}
