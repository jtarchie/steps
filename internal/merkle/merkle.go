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
	NodeKindTry   NodeKind = "try"
	// NodeKindParallel is an in_parallel: block. The block hashes its
	// branches' content, so changing any branch changes the block.
	NodeKindParallel NodeKind = "in_parallel"
	// NodeKindRace is a race: block.
	NodeKindRace NodeKind = "race"
	// NodeKindDo is a do: block — steps run in sequence as one node.
	NodeKindDo NodeKind = "do"
	// NodeKindEnsemble is an ensemble: block.
	NodeKindEnsemble NodeKind = "ensemble"
	// NodeKindAcross is an across: matrix.
	NodeKindAcross NodeKind = "across"
	// NodeKindLoadVar is a load_var: capture.
	NodeKindLoadVar NodeKind = "load_var"
	// NodeKindApproval is an approval: wait.
	NodeKindApproval NodeKind = "approval"
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

// sortedEnv returns env's names in a stable order. Unlike inputs/outputs
// (config.StableStrings, which deliberately preserves declaration order), an
// env: list is a SET: naming the same two variables in the other order asks
// for exactly the same execution environment, so it must not miss the cache.
func sortedEnv(env []string) []string {
	out := make([]string, len(env))
	copy(out, env)
	slices.Sort(out)

	return out
}

// withIsolation folds the two settings that change what a containerized
// command is ALLOWED to do into content.
//
// They are identity for the same reason image:/user:/network: are, and the
// consequence of leaving them out is a wrong cache HIT rather than a miss: a
// task cached while privileged: true would be skipped after that line is
// removed, so the narrower configuration is reported green having never run.
// Tightening container_limits.memory has the same shape — the new limit would
// never be exercised.
//
// Value-gated like everything else here, so a pipeline using neither hashes
// byte-identically to before they existed.
func withIsolation(privileged bool, limits *config.ContainerLimits, content map[string]any) {
	if privileged {
		content["privileged"] = true
	}

	if limits != nil {
		if limits.CPU > 0 {
			content["cpu_shares"] = limits.CPU
		}

		if limits.Memory > 0 {
			content["memory_bytes"] = limits.Memory
		}
	}
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

	// When a get aliases its resource (get: differs from resource:), fold the
	// artifact name in so two aliases of the same resource — identical source/
	// version — still hash distinctly, since they fetch into different
	// directories that different downstream steps name as inputs. Value-gated:
	// an unaliased get (the common case) omits this and hashes byte-identically
	// to before this field existed.
	if step.Resource != "" {
		content["artifact"] = step.Get
	}

	// A get's params: change what in: puts in the artifact — a depth: 1 clone
	// and a full one are the same version and different bytes — so two gets of
	// one version differing in params are two different fetches and must not
	// share a cache entry. Value-gated like the rest: a get with no params:
	// hashes byte-identically to before the field existed.
	if len(step.Params) > 0 {
		content["params"] = step.Params
	}

	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	// The variable NAMES, never their values: a value is a secret, and this
	// map is persisted to state.db. Changing which variables a command can see
	// changes what it executes against, so the names are identity; changing a
	// value is the operator's environment moving under a pipeline, which this
	// package has never claimed to hash (same reasoning as a model's weights).
	if len(resourceType.Env) > 0 {
		content["env"] = sortedEnv(resourceType.Env)
	}

	if resourceType.User != "" {
		content["user"] = resourceType.User
	}

	if resourceType.Network != "" {
		content["network"] = resourceType.Network
	}

	withIsolation(resourceType.Privileged, resourceType.Limits, content)

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
		// Order-significant: it is the emitted enum. A []VerdictRoute marshals
		// as an array of {name, target} objects, so the order survives into the
		// hash the way a map's keys could not — which is the whole reason the
		// targets live in this list rather than in a to: map beside it.
		content["verdicts"] = step.Verdicts
	}

	// A computed Label is the name the step is known by (see config.Step.Label),
	// and it belongs to identity for a concrete reason: a matrix whose author
	// interpolated nothing produces cells that differ ONLY by their
	// coordinates. Those coordinates used to sit in the task: field and entered
	// the hash through it; now they sit here. Leave them out and every such
	// cell collapses onto one hash — the matrix runs once and reports itself
	// cached for the rest.
	//
	// Folded in withRouting rather than at a single call site because the
	// runners hash through the leaf builders directly (TaskNodeContent,
	// AgentContentMap, PutNodeContent) while cache lookups go through
	// stepContentMap. Both paths pass through here, and a label present on one
	// but not the other is a cell that can never match its own recorded node.
	if step.Label != "" {
		content["label"] = step.Label
	}

	return content
}

// withHandoff folds a step's handoff: declaration into content, but only
// when set — so a step with no handoff: hashes byte-identically to before
// this field existed (the same value-gating as image:/when:/to:). Only the
// declaration itself (whether context/tool are enabled) is part of a step's
// identity: it changes what prompt suffix and tool set the step executes
// with. The actual routed-from step/key/note/visit — runtime facts the
// planner cannot know at plan time — are deliberately excluded from
// identity, the same treatment Attempts gets; agent steps are already
// unconditionally Unskippable (see planNonGetNode), so excluding them from
// identity never causes a wrong skip.
func withHandoff(step config.Step, content map[string]any) map[string]any {
	if step.Handoff != nil {
		content["handoff"] = map[string]any{"context": step.Handoff.Context, "tool": step.Handoff.Tool}
	}

	return withHandoffNote(step, content)
}

// withHandoffNote folds a step's handoff_note: participation into content,
// but only when it participates — so an unrelated step hashes byte-identically
// to before this field existed (the same value-gating as withHandoff).
//
// Both sides are identity: handoff_note adds a required write_handoff tool to
// what the step executes with, and HandoffNoteFrom (computed at load, see
// config.validateHandoffNoteSteps) adds an injected context block. The note's
// CONTENT is excluded, like the routed handoff's runtime facts above — but for
// a different reason than context_paths', which is chained through its input
// artifacts' hashes. A note's content is chained through nothing: correctness
// rests on agent steps being unconditionally Unskippable (see planNonGetNode),
// so a receiving agent step always re-runs and always re-reads the current
// note. A `task` reading handoff/*.md would NOT be safe that way — see
// docs/agents.md.
func withHandoffNote(step config.Step, content map[string]any) map[string]any {
	if step.WantsNote() {
		content["handoff_note"] = true
	}

	if len(step.HandoffNoteFrom) > 0 {
		content["handoff_note_from"] = step.HandoffNoteFrom
	}

	return withContext(step, content)
}

// withContext folds a step's context: declaration into content, but only when
// set — so a step without one hashes byte-identically to before this field
// existed (the same value-gating as withHandoff).
//
// The declaration is identity because it changes the step's tool grant: a
// context: write step is offered set_context and a plain one is not, and two
// steps that differ only in what tools they hold are not the same step. What
// the step actually STORED is excluded, for the reason the handoff note's
// content is: it cannot be known at plan time, and agent steps are
// unconditionally Unskippable (see planNonGetNode), so a run always re-executes
// and re-records rather than replaying a stale write.
// The fidelity is identity for the same reason: it decides how much of the
// recorded context is rendered into the step's opening conversation, and two
// steps shown different things are not the same step. The recorded FACTS are
// excluded — a runtime value the planner cannot know, and one that agent steps
// being Unskippable makes safe to leave out.
// The qualify is identity for the third variant of the same reason: it decides
// WHERE the step's writes land — its own per-cell scope, merged under a key
// naming the cell, rather than the run scope under the plain key. Two steps
// recording under different key names are not the same step. Without it,
// adding qualify: to a matrix of task cells is a cache hit: the cells skip, no
// per-cell scope is ever written, and the join merges nothing — so the author
// gets the OLD unqualified key with no error, which is the silent key-shape
// change qualify: exists to eliminate. Value-gated like the fidelity above, so
// every pipeline that does not set it hashes byte-identically to before.
func withContext(step config.Step, content map[string]any) map[string]any {
	if step.Context != nil {
		entry := map[string]any{"write": step.Context.Write}
		if step.Context.Fidelity != "" {
			entry["fidelity"] = string(step.Context.Fidelity)
		}

		if step.Context.Qualify {
			entry["qualify"] = true
		}

		content["context"] = entry
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
//
//nolint:cyclop // the switch over step kinds is inherently branching
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

		return PutNodeContent(cfg, step, *resourceType, res.Source, step.Params, step.InputNames(), step.InputsAll())
	case config.StepKindAgent:
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return nil, fmt.Errorf("resolve agent: %w", err)
		}

		return AgentContentMap(cfg, step, ri)
	case config.StepKindTry:
		return TryNodeContent(cfg, step)
	default: // config.StepKindGet — not a valid hook body
		return nil, errors.New("unrecognized hook step (must be task, put, or agent)")
	}
}

// TryNodeContent builds the hashed content for a try wrapper by folding the
// inner step's content under a "try" key, then folding in the outer step's
// hooks/routing/when. The inner step's own node is recorded independently
// (as failed/succeeded); only the wrapper appears in the plan chain.
// Nested try steps recurse through stepContentMap.
func TryNodeContent(cfg *config.Config, step config.Step) (map[string]any, error) {
	innerContent, err := stepContentMap(cfg, *step.Try)
	if err != nil {
		return nil, fmt.Errorf("try: %w", err)
	}

	content := map[string]any{"try": innerContent}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// stepContentMap dispatches on a step's kind to build its hashed content,
// reusing the same builders a plan step uses. It exists so TryNodeContent and
// hookContentMap can share the dispatch without duplicating it.
//
//nolint:cyclop // the switch over step kinds is inherently branching
func stepContentMap(cfg *config.Config, step config.Step) (map[string]any, error) {
	kind, ok := step.Kind()
	if !ok {
		return nil, errors.New("unrecognized step")
	}

	switch kind { //nolint:exhaustive // StepKindGet is not a valid non-get step kind here
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

		return PutNodeContent(cfg, step, *resourceType, res.Source, step.Params, step.InputNames(), step.InputsAll())
	case config.StepKindAgent:
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return nil, fmt.Errorf("resolve agent: %w", err)
		}

		return AgentContentMap(cfg, step, ri)
	case config.StepKindTry:
		return TryNodeContent(cfg, step)
	case config.StepKindInParallel:
		return ParallelNodeContent(cfg, step)
	case config.StepKindDo:
		return DoNodeContent(cfg, step)
	case config.StepKindRace:
		return RaceNodeContent(cfg, step)
	case config.StepKindEnsemble:
		return EnsembleNodeContent(cfg, step)
	case config.StepKindLoadVar:
		// The captured value is a run-time fact; only the declaration is
		// knowable here.
		return map[string]any{"load_var": step.LoadVar, "file": step.VarFile}, nil
	case config.StepKindApproval:
		return map[string]any{"approval": step.Approval.Message}, nil
	default:
		return nil, errors.New("unrecognized step")
	}
}

// ParallelNodeContent hashes an in_parallel: block: its branches' own content,
// in declaration order, plus the two settings that change what running it
// means.
//
// limit and fail_fast are included deliberately, unlike the operational limits
// elsewhere (attempts:, timeout:, budget:). They are not "how hard to try" —
// they change which steps run: fail_fast decides whether the siblings of a
// failing branch execute at all, and limit decides the order work is attempted
// in. A cached result from one setting must not satisfy the other.
func ParallelNodeContent(cfg *config.Config, step config.Step) (map[string]any, error) {
	branches := make([]any, 0, len(step.InParallel.Steps))

	for i := range step.InParallel.Steps {
		branchContent, err := stepContentMap(cfg, step.InParallel.Steps[i])
		if err != nil {
			return nil, fmt.Errorf("in_parallel branch %d: %w", i, err)
		}

		branches = append(branches, branchContent)
	}

	content := map[string]any{
		"in_parallel": branches,
		"limit":       step.InParallel.Limit,
		"fail_fast":   step.InParallel.FailsFast(),
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// DoNodeContent hashes a do: block: its children's own content, in
// declaration order.
//
// There is no limit/fail_fast counterpart to fold in, as ParallelNodeContent
// has — a do: block has no settings. Sequence IS its meaning, so the ordered
// list of children is the whole of its identity, and two blocks with the same
// children in a different order are correctly two different nodes.
func DoNodeContent(cfg *config.Config, step config.Step) (map[string]any, error) {
	steps := make([]any, 0, len(step.Do))

	for i := range step.Do {
		childContent, err := stepContentMap(cfg, step.Do[i])
		if err != nil {
			return nil, fmt.Errorf("do step %d: %w", i, err)
		}

		steps = append(steps, childContent)
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, map[string]any{"do": steps})))
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

		// input_mapping/output_mapping rename what gets materialized where, so
		// they change the step's view and must bust its cache — but, like
		// inputs/outputs, only under a workspace: block, and value-gated so an
		// unmapped task hashes exactly as before this field existed.
		if len(rt.InputMapping) > 0 {
			content["input_mapping"] = rt.InputMapping // map[string]string — json.Marshal sorts keys, so the hash stays deterministic
		}

		if len(rt.OutputMapping) > 0 {
			content["output_mapping"] = rt.OutputMapping
		}
	}

	if rt.Image != "" {
		content["image"] = rt.Image
	}

	// Names only — see the get node's env comment.
	if len(rt.Env) > 0 {
		content["env"] = sortedEnv(rt.Env)
	}

	if rt.User != "" {
		content["user"] = rt.User
	}

	if rt.Network != "" {
		content["network"] = rt.Network
	}

	withIsolation(rt.Privileged, rt.Limits, content)

	if rt.Assert != nil {
		content["assert"] = assertContent(rt.Assert)
	}

	// A task's context: is identity too, and this is the step kind where it
	// actually bites: an agent is never cacheable, so hashing its declaration
	// alone would be a no-op, while a task CELL of a matrix is the one thing a
	// rerun can skip. Without this, adding qualify: to a matrix of task cells
	// was a cache hit that recorded nothing.
	return withHooks(cfg, step, withWhen(step, withRouting(step, withContext(step, content))))
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
// this differs from the inputs/ws gating. inputsAll folds the `inputs: all`
// escape hatch as a distinct sentinel, but — like the name list — only under a
// workspace: block, since without one declarations don't change what the step
// sees and so must not affect its hash.
func PutNodeContent(cfg *config.Config, step config.Step, resourceType config.ResourceType, source, params map[string]any, inputs []string, inputsAll bool) (map[string]any, error) {
	content := map[string]any{
		"out_template": resourceType.Config.Out,
		"source":       source,
		"params":       params,
	}

	if cfg.Workspace != nil {
		if inputsAll {
			content["inputs"] = "all"
		} else {
			content["inputs"] = config.StableStrings(inputs)
		}
	}

	// The implicit get after a put is part of what the step DOES — it
	// materializes an artifact later steps read — so both switches that change
	// it are identity, not an operational limit. Value-gated: a put that
	// spells neither hashes byte-identically to before they existed.
	if len(step.GetParams) > 0 {
		content["get_params"] = step.GetParams
	}

	if step.NoGet {
		content["no_get"] = true
	}

	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	// Names only — see the get node's env comment.
	if len(resourceType.Env) > 0 {
		content["env"] = sortedEnv(resourceType.Env)
	}

	if resourceType.User != "" {
		content["user"] = resourceType.User
	}

	if resourceType.Network != "" {
		content["network"] = resourceType.Network
	}

	withIsolation(resourceType.Privileged, resourceType.Limits, content)

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

		// Same value-gating: max_output_bytes narrows what a call returns to
		// the model, so it changes the conversation the step produces.
		if t.MaxOutputBytes != 0 {
			content["max_output_bytes"] = t.MaxOutputBytes
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

	if t.MaxOutputBytes != 0 {
		content["max_output_bytes"] = t.MaxOutputBytes
	}

	return content, nil
}

// mcpServerContent builds the hashed identity of a configured mcp_servers:
// entry, folded into any tool grant or resource-type stage that references
// it. Endpoint/auth type/the api_key_env *name* (never its value, mirroring
// AgentSource's api_key_env exclusion exactly)/scopes determine behavior
// and are hashed; nothing token-shaped is ever computed here, since this
// package never imports internal/mcp. For a stdio server, command/args/cwd
// are its equivalent spawn identity and are value-gated the same way —
// note args is NOT sorted (unlike mcp_tools below): argv order is
// semantic, so reordering it must bust the hash. endpoint/auth_type stay
// unconditional so an existing HTTP server's hash is unaffected by this
// field set existing.
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

	if srv.Command != "" {
		content["command"] = srv.Command
	}

	if len(srv.Args) != 0 {
		content["args"] = srv.Args
	}

	if srv.Cwd != "" {
		content["cwd"] = srv.Cwd
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

	// Names only — see the get node's env comment.
	if len(ri.Env) > 0 {
		content["env"] = sortedEnv(ri.Env)
	}

	if ri.User != "" {
		content["user"] = ri.User
	}

	if ri.Network != "" {
		content["network"] = ri.Network
	}

	withIsolation(ri.Privileged, ri.Limits, content)

	return content, nil
}

// AgentContentMap is the content hashed for an agent node: everything that
// determines the model's output (agent, prompt, dir, resolved model/endpoint,
// persona, dials, and the effective tool set — including any sub-agent tools,
// folded in via toolSpecsContent). Attempts is excluded (a pure retry policy
// doesn't change the intended result); CompactAfterTokens is excluded for the
// same reason — it's an operational context-budget knob governing how a
// conversation manages its own history, not a determinant of the intended
// result, so a pipeline gains or loses compaction without invalidating any
// cached step; the API key and its env var name are excluded (nothing
// secret-adjacent belongs in hashed content). inputs/outputs are folded in
// only when ws is non-nil (workspace: configured) — see TaskNodeContent's
// doc comment for why.
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
		content["inputs"] = config.StableStrings(step.InputNames())
		content["outputs"] = config.StableStrings(step.Outputs)
	}

	if len(ri.ContextPaths) > 0 {
		// Paths, not file contents: the files live inside the step's
		// workspace (loadContextBlocks confines them), so their content is
		// already chained through the input artifacts' own hashes.
		content["context_paths"] = config.StableStrings(ri.ContextPaths)
	}

	if ri.Image != "" {
		content["image"] = ri.Image
	}

	// Names only — see the get node's env comment.
	if len(ri.Env) > 0 {
		content["env"] = sortedEnv(ri.Env)
	}

	if ri.User != "" {
		content["user"] = ri.User
	}

	if ri.Network != "" {
		content["network"] = ri.Network
	}

	withIsolation(ri.Privileged, ri.Limits, content)

	// Which CLI runs the conversation, when one does — value-gated so every
	// pre-existing HTTP agent hashes exactly as it did before CLI sources
	// existed. The CLI's own version is deliberately NOT folded in: it changes
	// under the operator the same way a hosted model's weights do, and this
	// package has never claimed to hash the thing on the other end of the
	// wire, only which thing was asked.
	if ri.CLI != "" {
		content["cli"] = ri.CLI
	}

	if step.Assert != nil {
		content["assert"] = assertContent(step.Assert)
	}

	return withHooks(cfg, step, withWhen(step, withHandoff(step, withRouting(step, content))))
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
	// A load_var: captures a value that does not exist until the run produces
	// it, so the planner cannot know what it will be — and every step after it
	// depends on that value. Unskippable, always.
	if step.LoadVar != "" {
		return Node{ParentHash: parentHash, Kind: NodeKindLoadVar, StepIndex: i, Resource: step.LoadVar}, true, nil
	}

	// An approval: is a person, not a computation. Nothing about it is
	// knowable at plan time, and a cached "they said yes once" must never
	// stand in for asking again.
	if step.Approval != nil {
		return Node{ParentHash: parentHash, Kind: NodeKindApproval, StepIndex: i, Resource: "approval"}, true, nil
	}

	// across: is a modifier rather than a kind (see internal/pipeline's
	// dispatch): the block is one plan node whose cells are hashed inside it.
	if len(step.Across) > 0 {
		node, err := acrossNode(cfg, step, i, parentHash)

		return node, true, err
	}

	return planKindNode(cfg, step, i, parentHash)
}

// planKindNode builds the plan node for an ordinary (non-across) step.
func planKindNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, bool, error) {
	kind, ok := step.Kind()
	if !ok {
		return Node{}, false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet — planNonGetNode is only ever called for non-get steps
	case config.StepKindTask:
		return planTaskNode(cfg, step, i, parentHash)
	case config.StepKindPut:
		node, err := putNode(cfg, step, i, parentHash)

		return node, true, err
	case config.StepKindAgent:
		node, err := agentNode(cfg, step, i, parentHash)

		return node, true, err
	case config.StepKindTry, config.StepKindRace, config.StepKindInParallel, config.StepKindEnsemble, config.StepKindDo:
		// Every container is unskippable. Its branches run inside it rather
		// than as chain nodes of their own, so the planner cannot reason about
		// which of them a cached success covers — and a branch may be a put or
		// an agent, which are never skippable anyway.
		node, err := planContainerNode(cfg, step, kind, i, parentHash)

		return node, true, err
	default: // config.StepKindGet — planNonGetNode is only ever called for non-get steps
		return Node{}, false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// RaceNodeContent hashes a race: block: its branches' content, in declaration
// order. Which branch wins is a run-time race, not part of the step's
// identity — the block means "any of these", and a cached success is a success
// whichever one produced it.
func RaceNodeContent(cfg *config.Config, step config.Step) (map[string]any, error) {
	branches := make([]any, 0, len(step.Race.Steps))

	for i := range step.Race.Steps {
		branchContent, err := stepContentMap(cfg, step.Race.Steps[i])
		if err != nil {
			return nil, fmt.Errorf("race branch %d: %w", i, err)
		}

		branches = append(branches, branchContent)
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, map[string]any{"race": branches})))
}

// EnsembleNodeContent hashes an ensemble: block — its members' content, the
// vocabulary they vote in, and the rule that combines them. All three change
// what the block decides, so all three are part of its identity.
func EnsembleNodeContent(cfg *config.Config, step config.Step) (map[string]any, error) {
	members := make([]any, 0, len(step.Ensemble.Agents))

	for i := range step.Ensemble.Agents {
		member := step.Ensemble.Agents[i]
		member.Verdicts = step.Ensemble.EnsembleVerdictsFor()

		memberContent, err := stepContentMap(cfg, member)
		if err != nil {
			return nil, fmt.Errorf("ensemble member %d: %w", i, err)
		}

		members = append(members, memberContent)
	}

	content := map[string]any{
		"ensemble":      members,
		"verdicts":      step.Ensemble.Verdicts,
		"decide":        step.Ensemble.Decide,
		"member_errors": step.Ensemble.MemberErrors,
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
}

// AcrossNodeContent hashes an across: block from its cells' content. The
// axes themselves are not hashed separately: two different matrices that
// expand to the same cells ARE the same work.
func AcrossNodeContent(cfg *config.Config, step config.Step, cells []config.Step) (map[string]any, error) {
	contents := make([]any, 0, len(cells))

	for i := range cells {
		cellContent, err := stepContentMap(cfg, cells[i])
		if err != nil {
			return nil, fmt.Errorf("across cell %d: %w", i, err)
		}

		contents = append(contents, cellContent)
	}

	return withHooks(cfg, step, withWhen(step, withRouting(step, map[string]any{"across": contents})))
}

// AcrossPlanContent is a matrix's content at PLAN time, where a runtime axis
// has no values yet.
//
// A static matrix expands and hashes its cells exactly as before. A runtime
// one hashes the axes as declared — including the source key, so pointing an
// axis at a different key is a different block — plus the unexpanded template.
// The marker keeps the two spellings apart: a runtime matrix must never
// collide with a static one that happens to render the same way.
func AcrossPlanContent(cfg *config.Config, step config.Step, i int) (map[string]any, error) {
	if !config.HasRuntimeAxis(step) {
		cells, err := config.ExpandAcross(fmt.Sprintf("step %d", i), step)
		if err != nil {
			return nil, fmt.Errorf("step %d (across): %w", i, err)
		}

		content, err := AcrossNodeContent(cfg, step, cells)
		if err != nil {
			return nil, fmt.Errorf("step %d (across): %w", i, err)
		}

		return content, nil
	}

	template, err := stepContentMap(cfg, acrossTemplate(step))
	if err != nil {
		return nil, fmt.Errorf("step %d (across): %w", i, err)
	}

	axes := make([]any, 0, len(step.Across))
	for _, axis := range step.Across {
		axes = append(axes, map[string]any{"var": axis.Var, "values": axis.Values, "from": axis.From})
	}

	content, err := withHooks(cfg, step, withWhen(step, withRouting(step, map[string]any{
		"across_runtime": map[string]any{"axes": axes, "template": template},
	})))
	if err != nil {
		return nil, fmt.Errorf("step %d (across): %w", i, err)
	}

	return content, nil
}

// acrossTemplate is the matrix step with its axes stripped: the body a cell is
// rendered from, before any substitution.
func acrossTemplate(step config.Step) config.Step {
	step.Across = nil

	return step
}

// CellHash is one across: cell's own content hash, plus whether the cell is
// the kind of step that may be skipped when it matches.
//
// A put or an agent cell is never cacheable, for the same reason a put or
// agent step never is: side effects, and non-determinism.
func CellHash(cfg *config.Config, cell config.Step, parentHash string) (string, bool, error) {
	content, err := stepContentMap(cfg, cell)
	if err != nil {
		return "", false, fmt.Errorf("across cell: %w", err)
	}

	kind, _ := cell.Kind()

	hash, err := HashNode(NodeKind(kind), content, parentHash)
	if err != nil {
		return "", false, fmt.Errorf("across cell: %w", err)
	}

	cacheable := kind == config.StepKindTask && cell.Fix == nil && cell.When == nil && cell.To == nil

	return hash, cacheable, nil
}

// planTaskNode builds a task step's plan node and decides whether a chain
// through it may be skipped.
//
// A task is unskippable if it has a fix:, or carries a when: guard or to:
// routing — the latter two have run-time-only outcomes the planner can't know,
// so a chain through them must never be recorded as a reusable success. This
// mirrors internal/pipeline's runtime chainUnskippable and makes the plan-time
// flag accurate rather than relying on that runtime flag alone (Unskippable is
// not hashed, so this only ever makes the cache more conservative).
func planTaskNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, bool, error) {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return Node{}, false, fmt.Errorf("step %d: %w", i, err)
	}

	node, err := taskNode(cfg, step, rt, i, parentHash)

	return node, rt.Fix != nil || step.When != nil || step.Routes(), err
}

// parallelNode builds the plan node for an in_parallel: block.
func parallelNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := ParallelNodeContent(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	hash, err := HashNode(NodeKindParallel, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindParallel, StepIndex: i, Content: content}, nil
}

// planContainerNode builds the plan node for whichever container kind step is.
func planContainerNode(cfg *config.Config, step config.Step, kind config.StepKind, i int, parentHash string) (Node, error) {
	switch kind { //nolint:exhaustive // the caller dispatches only container kinds here
	case config.StepKindTry:
		return tryNode(cfg, step, i, parentHash)
	case config.StepKindDo:
		return doNode(cfg, step, i, parentHash)
	case config.StepKindRace:
		return raceNode(cfg, step, i, parentHash)
	case config.StepKindEnsemble:
		return ensembleNode(cfg, step, i, parentHash)
	default: // config.StepKindInParallel
		return parallelNode(cfg, step, i, parentHash)
	}
}

// doNode builds the plan node for a do: block.
func doNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := DoNodeContent(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (do): %w", i, err)
	}

	hash, err := HashNode(NodeKindDo, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (do): %w", i, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindDo, StepIndex: i, Content: content}, nil
}

// raceNode builds the plan node for a race: block.
func raceNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := RaceNodeContent(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	hash, err := HashNode(NodeKindRace, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindRace, StepIndex: i, Content: content}, nil
}

// acrossNode builds the plan node for an across: matrix.
//
// A matrix with a from: axis takes its values from what an earlier step
// records, so at PLAN time it has no cells to fold in: the array does not
// exist yet. It hashes its declaration instead (see AcrossPlanContent), which
// means the planner cannot predict what such a block — or anything downstream
// of it — will do. That is honest rather than unfortunate: the width of the
// matrix is genuinely not knowable until the run reaches it, and a plan that
// claimed otherwise would be predicting a skip it cannot make good on.
func acrossNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := AcrossPlanContent(cfg, step, i)
	if err != nil {
		return Node{}, err
	}

	hash, err := HashNode(NodeKindAcross, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (across): %w", i, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAcross, StepIndex: i, Content: content}, nil
}

// ensembleNode builds the plan node for an ensemble: block.
func ensembleNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := EnsembleNodeContent(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (ensemble): %w", i, err)
	}

	hash, err := HashNode(NodeKindEnsemble, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (ensemble): %w", i, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindEnsemble, StepIndex: i, Content: content}, nil
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

	content, err := PutNodeContent(cfg, step, *resourceType, res.Source, step.Params, step.InputNames(), step.InputsAll())
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

func tryNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	content, err := TryNodeContent(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	hash, err := HashNode(NodeKindTry, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (try): %w", i, err)
	}

	// Resource names the wrapped step, matching what internal/pipeline records
	// at run time — without it `steps plan` printed a nameless try row.
	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindTry, StepIndex: i, Resource: stepResourceName(step), Content: content}, nil
}

// stepResourceName is the name a node is displayed under: whichever of
// task/put/agent a step sets, looking through any try: wrappers.
func stepResourceName(step config.Step) string {
	inner := step.Unwrap()

	//kindswitch:ignore Unwrap() already resolved Try away, and a get node takes its name from the resource, not here
	switch {
	case inner.Task != "":
		return inner.Task
	case inner.Put != "":
		return inner.Put
	case inner.Agent != "":
		return inner.Agent
	default:
		return ""
	}
}

// ResourceCacheKey identifies a fetched resource version for the cross-build
// resource cache (see internal/workspace's resourceCache).
//
// It is deliberately NARROWER than GetNodeContent, which hashes a get NODE:
// that includes the artifact name an alias fetches into, and it chains onto a
// parent hash, so two jobs fetching the identical version of the identical
// resource produce different node hashes. Both of those are right for deciding
// whether a step can be skipped and wrong for deciding whether a fetch can be
// reused — the bytes on disk do not care which job asked for them, and keying
// the cache on the node hash would give every job its own copy of the same
// content, which is most of the win gone.
//
// What it does fold in is everything that changes what in: produces: the
// command itself, the source and version it runs against, and the execution
// settings that decide what environment it runs in. An MCP-backed resource
// type folds in its check/in tool identity through withMCPResourceStage, the
// same as the node content does.
func ResourceCacheKey(cfg *config.Config, resourceType config.ResourceType, source, version, params map[string]any) (string, error) {
	content := map[string]any{
		"in_template": resourceType.Config.In,
		"source":      source,
		"version":     version,
	}

	// Params must key the cross-build cache for the same reason they enter the
	// get node's hash (see GetNodeContent): a shallow fetch and a full one of
	// one version are different bytes, and reusing one for the other is a
	// wrong answer that looks like a cache hit. Value-gated, so a pipeline
	// with no get params: keeps every entry it has already cached.
	if len(params) > 0 {
		content["params"] = params
	}

	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	if len(resourceType.Env) > 0 {
		content["env"] = sortedEnv(resourceType.Env)
	}

	if resourceType.User != "" {
		content["user"] = resourceType.User
	}

	if resourceType.Network != "" {
		content["network"] = resourceType.Network
	}

	withIsolation(resourceType.Privileged, resourceType.Limits, content)

	err := withMCPResourceStage(cfg, resourceType, "in", content)
	if err != nil {
		return "", err
	}

	// No parent hash: a cache entry is content, not a position in a plan.
	return HashNode(NodeKindGet, content, "")
}
