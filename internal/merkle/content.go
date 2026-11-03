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

// withEnv folds env: by name only (see the get node's env comment) and secrets: by reference, never value, so rotating a credential re-runs nothing and the value never reaches the store.
func withEnv(content map[string]any, env []string, secrets map[string]string) {
	if len(env) > 0 {
		content["env"] = sortedEnv(env)
	}

	if len(secrets) > 0 {
		content["secrets"] = secrets
	}
}

// withSandbox keys sandbox: only when set, so an unsandboxed node hashes as it did before the field existed.
func withSandbox(sandbox bool, content map[string]any) {
	if sandbox {
		content["sandbox"] = true
	}
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

		return PutNodeContent(cfg, step, *resourceType, res.HashedEnv(), res.Source, step.Params, step.InputNames(), step.InputsAll())
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

	withEnv(content, rt.Env, rt.Secrets)

	if rt.User != "" {
		content["user"] = rt.User
	}

	if rt.Network != "" {
		content["network"] = rt.Network
	}

	withSandbox(rt.Sandbox, content)

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

	// A nudge can turn a run that would have failed into a success, so a result produced with one must not stand in for the step without it.
	if a.Nudge {
		content["nudge"] = true
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
