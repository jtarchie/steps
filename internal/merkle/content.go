package merkle

import (
	"errors"
	"fmt"
	"slices"

	"github.com/jtarchie/steps/internal/config"
)

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
// between planning (this package) and real execution (internal/pipeline,
// internal/agent) so both compute identical hashes for identical steps.
//
// extraEnv is the resource INSTANCE's own env: (config.Resource.Env) — see
// the env folding below for why it has to join resourceType.Env in the hash,
// not just at run time.
func GetNodeContent(cfg *config.Config, step config.Step, resourceType config.ResourceType, extraEnv []string, source, version map[string]any) (map[string]any, error) {
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

	err := withResourceTypeEnv(cfg, resourceType, extraEnv, "in", content)
	if err != nil {
		return nil, err
	}

	return withStepModifiers(cfg, step, content)
}

// withResourceTypeEnv folds what a resource type's stage command runs as and
// in — image, env names, user, network, isolation, and the backend's own
// stage identity — into content. Shared by the get node, the put node and the
// cross-build cache key so the three cannot drift apart.
//
// The env variable NAMES, never their values: a value is a secret, and this
// map is persisted to state.db. Changing which variables a command can see
// changes what it executes against, so the names are identity; changing a
// value is the operator's environment moving under a pipeline, which this
// package has never claimed to hash (same reasoning as a model's weights).
//
// Unioned with extraEnv (the resource INSTANCE's own env:), not just
// resourceType.Env alone — widening or narrowing one resource's own
// allow-list changes what env() can see for that resource exactly the same
// way editing the type's own env: does, so leaving it out of the hash would
// be the wrong-cache-HIT failure withIsolation's doc comment describes: a get
// cached before a resource's env: named a new variable would be skipped as
// unchanged after, having never run with it visible.
func withResourceTypeEnv(cfg *config.Config, resourceType config.ResourceType, extraEnv []string, stage string, content map[string]any) error {
	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	if env := config.UnionEnv(resourceType.Env, extraEnv); len(env) > 0 {
		content["env"] = sortedEnv(env)
	}

	if resourceType.User != "" {
		content["user"] = resourceType.User
	}

	if resourceType.Network != "" {
		content["network"] = resourceType.Network
	}

	withIsolation(resourceType.Privileged, resourceType.Limits, content)

	withExprResourceStage(resourceType, stage, content)
	withGitHubKind(resourceType, content)

	return withMCPResourceStage(cfg, resourceType, stage, content)
}

// withMCPResourceStage folds an mcp-backed resource type's server identity,
// the named lifecycle stage's tool name AND its args: mapping into content,
// mirroring in_template/out_template's role for the shell backend — but only
// when that stage's *MCPToolCall is actually set (In and Out are both
// optional; an unset In means get falls back to writing version.json, with
// nothing template-shaped to hash, exactly like check: is never hashed for the
// shell backend either). A resource type with no mcp: block is unaffected
// (byte-identical to before this field existed), same as every other
// value-gated field in this file.
//
// args: belongs here for the same reason in_template does: it is what the
// call actually ASKS FOR. Hashing the tool name alone would let a fixed
// mapping — `{{ .version.channel }}` corrected to `{{ .version.thread }}` —
// hash identically to the broken one it replaced, so the get is skipped as
// unchanged and the previously fetched (wrong) artifact is reused. Also
// value-gated, so a stage that names no args: keeps every hash it already has.
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

	if len(call.Args) > 0 {
		// Normalized, not stored raw: yaml.v3 decodes a mapping whose keys
		// are not all strings into map[any]any (`args: {filters: {2024:
		// "yes"}}`, an unquoted `on:`), which encoding/json cannot marshal —
		// so HashNode would fail the whole plan with "json: unsupported type"
		// on a pipeline that loads, validates, and RUNS fine, since
		// resource.renderArgValue handles exactly that shape. The hash has to
		// accept every args: the runtime accepts.
		content["mcp_"+stage+"_args"] = normalizeMapKeys(call.Args)
	}

	return nil
}

// withExprResourceStage folds an expr-backed type's program text into the
// content map, the same way in_template/out_template does for shell.
//
// Gated on the BACKEND rather than on the text being non-empty: switching a
// type from shell to expr with neither declaring an in: must still bust the
// hash, because the two fetch differently even when both are blank. Types
// that are not expr-backed fold nothing, so every hash already in a state.db
// stays byte-identical.
func withExprResourceStage(resourceType config.ResourceType, stage string, content map[string]any) {
	if resourceType.Config.Expr == nil {
		return
	}

	switch stage {
	case "in":
		content["expr_in"] = resourceType.Config.Expr.In
	case "out":
		content["expr_out"] = resourceType.Config.Expr.Out
	}
}

// withGitHubKind folds which github-* type a resource is into its node. The
// four share one backend and two share a source: shape, so without it a
// github-prs get and a github-comments get of the same repo would differ
// only by version, and a version that happened to match would serve one
// type's tree as the other's. Value-gated: every other type hashes as before.
func withGitHubKind(resourceType config.ResourceType, content map[string]any) {
	if resourceType.Config.GitHub != "" {
		content["github"] = resourceType.Config.GitHub
	}
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

	// What a step READS from other steps is part of what it is. It matters
	// most for a TASK, which is skippable: a cached command never runs, so a
	// task whose from: changed would otherwise replay an outcome produced
	// without the decision it now asks for. (An agent step is unconditionally
	// unskippable already; this keeps both kinds honest by one rule.)
	if senders := step.FromSenders(); len(senders) > 0 {
		from := step.ContextFrom()
		levels := make([]string, 0, len(senders))

		for _, sender := range senders {
			levels = append(levels, sender+"="+string(from[sender]))
		}

		content["context_from"] = levels
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

// withStepModifiers folds what every step kind shares — routing, when: and
// hooks — into content.
func withStepModifiers(cfg *config.Config, step config.Step, content map[string]any) (map[string]any, error) {
	return withHooks(cfg, step, withWhen(step, withRouting(step, content)))
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

// hookContentMap builds the hashed content for one hook step through the same
// builders a plan step uses (which recurse into the hook's own hooks). Only
// task/put/agent/try are hook bodies; LoadConfig rejects the rest, and this
// refuses them rather than hashing something no run executes.
func hookContentMap(cfg *config.Config, step config.Step) (map[string]any, error) {
	kind, _ := step.Kind()

	switch kind { //nolint:exhaustive // default covers every kind that is not a hook body
	case config.StepKindTask, config.StepKindPut, config.StepKindAgent, config.StepKindTry:
		return stepContentMap(cfg, step)
	default:
		return nil, errors.New("unrecognized hook step (must be task, put, agent, or try)")
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

	return withStepModifiers(cfg, step, content)
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
		res, err := cfg.FindResource(step.PutResourceName())
		if err != nil {
			return nil, fmt.Errorf("resolve put: %w", err)
		}

		resourceType, err := cfg.FindResourceType(res.Type)
		if err != nil {
			return nil, fmt.Errorf("resolve put: %w", err)
		}

		return PutNodeContent(cfg, step, *resourceType, res.Env, res.Source, step.Params, step.InputNames(), step.InputsAll())
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

	return withStepModifiers(cfg, step, content)
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

	return withStepModifiers(cfg, step, map[string]any{"do": steps})
}

// TaskNodeContent and PutNodeContent fold in inputs/outputs unconditionally:
// every step materializes its view from its declarations (there is no shared
// mode), so the declarations ARE part of what the step executes against and
// must bust its cache when they change.
func TaskNodeContent(cfg *config.Config, step config.Step, rt config.ResolvedTask) (map[string]any, error) {
	content := map[string]any{"run": rt.Run}

	content["inputs"] = config.StableStrings(rt.Inputs)
	content["outputs"] = config.StableStrings(rt.Outputs)

	// input_mapping/output_mapping rename what gets materialized where, so
	// they change the step's view and must bust its cache — value-gated so an
	// unmapped task hashes exactly as before the field existed.
	if len(rt.InputMapping) > 0 {
		content["input_mapping"] = rt.InputMapping // map[string]string — json.Marshal sorts keys, so the hash stays deterministic
	}

	if len(rt.OutputMapping) > 0 {
		content["output_mapping"] = rt.OutputMapping
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

	return withStepModifiers(cfg, step, content)
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

	if a.Verdict != nil {
		content["verdict"] = *a.Verdict
	}

	// files folds in for the same reason as every other field: a step cached
	// from a run BEFORE the assert existed would otherwise be skipped, and the
	// assert never evaluated on the run that added it.
	if len(a.Files) > 0 {
		content["files"] = config.StableStrings(a.Files)
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

// PutNodeContent builds the content map hashed for a put node. inputs fold
// in unconditionally (see TaskNodeContent); inputsAll folds the `inputs: all`
// escape hatch as a distinct sentinel.
//
// extraEnv is the resource INSTANCE's own env: (config.Resource.Env) — see
// GetNodeContent's env comment.
func PutNodeContent(cfg *config.Config, step config.Step, resourceType config.ResourceType, extraEnv []string, source, params map[string]any, inputs []string, inputsAll bool) (map[string]any, error) {
	content := map[string]any{
		"out_template": resourceType.Config.Out,
		"source":       source,
		"params":       params,
	}

	// A put renamed away from its resource folds the name in, because nodes
	// are keyed by hash: two sibling puts differing only in name would
	// otherwise share one row and one name would vanish from `runs steps`.
	// Gated on the name differing, so a redundant resource: hashes like an
	// unaliased put.
	if step.PutResourceName() != step.Put {
		content["step"] = step.Put
	}

	if inputsAll {
		content["inputs"] = "all"
	} else {
		content["inputs"] = config.StableStrings(inputs)
	}

	err := withResourceTypeEnv(cfg, resourceType, extraEnv, "out", content)
	if err != nil {
		return nil, err
	}

	return withStepModifiers(cfg, step, content)
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

	return withStepModifiers(cfg, step, map[string]any{"race": branches})
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

	return withStepModifiers(cfg, step, content)
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

	return withStepModifiers(cfg, step, map[string]any{"across": contents})
}

// AcrossPlanContent is a matrix's content at PLAN time, where a from_file:
// axis has no values yet.
//
// A static matrix expands and hashes its cells exactly as at run time. A
// file-driven one hashes the axes as declared — including the source path, so
// pointing an axis at a different file is a different block — plus the
// unexpanded template. The marker keeps the two spellings apart: a file matrix
// must never collide with a static one that happens to render the same way.
func AcrossPlanContent(cfg *config.Config, step config.Step, i int) (map[string]any, error) {
	if !config.HasRuntimeAxis(step) {
		cells, err := config.ExpandAcross(fmt.Sprintf("step %d", i), step)
		if err != nil {
			return nil, err //nolint:wrapcheck // newNode prefixes the step's position and kind
		}

		return AcrossNodeContent(cfg, step, cells)
	}

	template, err := stepContentMap(cfg, acrossTemplate(step))
	if err != nil {
		return nil, err
	}

	axes := make([]any, 0, len(step.Across))
	for _, axis := range step.Across {
		axes = append(axes, map[string]any{"var": axis.Var, "values": axis.Values, "from_file": axis.FromFile})
	}

	return withStepModifiers(cfg, step, map[string]any{
		"across_runtime": map[string]any{"axes": axes, "template": template},
	})
}

// acrossTemplate is the matrix step with its axes stripped: the body a cell is
// rendered from, before any substitution.
func acrossTemplate(step config.Step) config.Step {
	step.Across = nil

	return step
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
func ResourceCacheKey(
	cfg *config.Config, resourceType config.ResourceType, extraEnv []string,
	source, version, params map[string]any,
) (string, error) {
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

	// The env union matters more here than in the node: a resource that adds
	// env: so its type's in: can authenticate produces different bytes with
	// the same source and version; keying on the type alone would serve it the
	// unauthenticated tree an earlier fetch cached, and would give two
	// resources of one type that differ only in env: a single shared entry.
	err := withResourceTypeEnv(cfg, resourceType, extraEnv, "in", content)
	if err != nil {
		return "", err
	}

	// No parent hash: a cache entry is content, not a position in a plan.
	return HashNode(NodeKindGet, content, "")
}
