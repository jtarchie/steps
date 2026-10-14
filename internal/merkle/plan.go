package merkle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"

	"github.com/jtarchie/steps/internal/config"
)

// VersionResolver is how the planner resolves a get step's resource, type
// and candidate versions — *resource.Cache in practice, whose
// ResolveVersionsCached is the same resolution runSteps uses, so that logic
// stays defined in exactly one place. It is an interface so that hashing does
// not import the package that runs check commands (and with it shell, docker
// and mcp); depguard keeps that edge out.
type VersionResolver interface {
	ResolveVersionsCached(
		ctx context.Context, cfg *config.Config, step config.Step, pinned map[string]string,
	) (*config.Resource, *config.ResourceType, []map[string]any, error)
}

// PlanChains walks a job's plan exactly like the pipeline's runSteps does
// (resolving get-step versions/pins through cache, the same resolution the
// executor uses), but never executes anything — no RunIn, RunShell, or
// RunOut. It returns one Chain per leaf reached, which is more than one only
// when a get step uses version: every.
//
// cache memoizes each get step's resolved versions so that a later run-time
// call through the same cache (see resource.Cache) reuses this plan-time
// check result instead of re-running the check command — pass the same
// *resource.Cache instance runSteps will use for this same job run. A nil
// *resource.Cache is a valid resolver that memoizes nothing (CheckVersions
// runs again during real execution for any branch that ends up running); a
// nil interface cannot resolve, and a plan that reaches a get with one is an
// error.
//
// sets, when non-nil, is the fan-out: ONE chain per InputSet, each get bound
// to its set's version. This is what keeps the planner and the executor
// describing the same shape — the recursion below fans out per VERSION at
// every get it meets, which for two every-gets is an N×M cross product while
// the executor runs N lockstep builds. Sets are computed once (see
// pipeline.resolveInputSets) and walked here and there alike, so the
// disagreement cannot exist. A nil sets keeps the recursion, whose fan-out
// is identical to one-chain-per-set whenever at most one get fans out —
// pinned by TestPlanChainsSetsMatchRecursion.
func PlanChains(
	ctx context.Context, cfg *config.Config, jobName string, steps []config.Step,
	pinned map[string]string, cache VersionResolver, sets []InputSet,
) ([]Chain, error) {
	slog.DebugContext(ctx, "job.plan", "job", jobName, "steps", len(steps), "sets", len(sets))

	images := imagePlan{artifacts: cfg.ImageArtifacts(jobName)}

	if sets != nil {
		chains := make([]Chain, 0, len(sets))

		for _, set := range sets {
			chain, err := planChainForSet(ctx, cfg, steps, pinned, cache, set, images)
			if err != nil {
				return nil, err
			}

			chains = append(chains, chain)
		}

		slog.DebugContext(ctx, "job.planned", "job", jobName, "chains", len(chains))

		return chains, nil
	}

	chains, err := planSteps(ctx, cfg, steps, pinned, nil, "", false, cache, images)
	if err != nil {
		return nil, err
	}

	slog.DebugContext(ctx, "job.planned", "job", jobName, "chains", len(chains))

	return chains, nil
}

// planChainForSet walks the plan once with every get bound to the set's
// version — a straight line, since the fan-out already happened when the
// sets were made.
func planChainForSet(
	ctx context.Context, cfg *config.Config, steps []config.Step,
	pinned map[string]string, cache VersionResolver, set InputSet, images imagePlan,
) (Chain, error) {
	var (
		prefix      []Node
		parentHash  string
		unskippable bool
	)

	// base makes StepIndex match the recursion byte for byte: planSteps
	// re-slices the plan after every get, so downstream indices restart at
	// zero. Purely metadata (never hashed), but the equivalence test compares
	// whole chains, and an equivalence with an asterisk is not one.
	base := 0

	for i, step := range steps {
		var (
			node            Node
			stepUnskippable bool
			err             error
		)

		index := i - base

		if step.Get != "" {
			var fetched config.Fetched

			node, fetched, err = planBoundGetNode(ctx, cfg, step, index, pinned, cache, set, parentHash)
			images = images.with(step.Get, fetched)
			base = i + 1
		} else {
			node, stepUnskippable, err = images.planNonGetNode(cfg, step, index, parentHash)
		}

		if err != nil {
			return Chain{}, err
		}

		prefix = append(prefix, node)
		parentHash = node.Hash
		unskippable = unskippable || stepUnskippable
	}

	return Chain{Nodes: prefix, RootHash: parentHash, Unskippable: unskippable}, nil
}

// planBoundGetNode builds a get node for the version the set assigned.
//
// The cache still resolves the resource and its type — and, memoized, is what
// keeps the check to one run — but the VERSION is the set's word, and a get
// the set never bound is an error rather than a version chosen here.
// resolveInputSets binds every get in the plan, so an unbound one means the
// two sides have stopped agreeing about what a plan contains — and a planner
// that quietly picks its own version hashes a chain the executor will not
// run, which the skip cache then records as done.
func planBoundGetNode(
	ctx context.Context, cfg *config.Config, step config.Step, i int,
	pinned map[string]string, cache VersionResolver, set InputSet, parentHash string,
) (Node, config.Fetched, error) {
	res, resourceType, _, err := resolveGet(ctx, cache, cfg, i, step, pinned)
	if err != nil {
		return Node{}, config.Fetched{}, err
	}

	version := set[step.Get]
	if version == nil {
		return Node{}, config.Fetched{}, fmt.Errorf("step %d (get %q): the input set binds no version for it", i, step.Get)
	}

	node, err := newNode(NodeKindGet, fmt.Sprintf("get %q", step.Get), i, parentHash, res.Name, func() (map[string]any, error) {
		return GetNodeContent(cfg, step, *resourceType, res.Env, res.Source, version)
	})
	if err != nil {
		return Node{}, config.Fetched{}, err
	}

	return node, config.Fetched{Source: res.Source, Version: version}, nil
}

func planSteps(
	ctx context.Context, cfg *config.Config, steps []config.Step, pinned map[string]string,
	prefix []Node, parentHash string, unskippable bool, cache VersionResolver, images imagePlan,
) ([]Chain, error) {
	for i, step := range steps {
		if step.Get != "" {
			return planGetStep(ctx, cfg, steps, i, step, pinned, prefix, parentHash, unskippable, cache, images)
		}

		node, stepUnskippable, err := images.planNonGetNode(cfg, step, i, parentHash)
		if err != nil {
			return nil, err
		}

		prefix = append(prefix, node)
		parentHash = node.Hash
		unskippable = unskippable || stepUnskippable
	}

	return []Chain{{Nodes: prefix, RootHash: parentHash, Unskippable: unskippable}}, nil
}

// imagePlan is what the planner knows about artifact images at one point in a
// chain: which names are artifacts in the job, and what each get bound so far
// fetched. Copied, never shared, so one version's branch cannot see
// another's digest.
type imagePlan struct {
	artifacts map[string]bool
	fetched   map[string]config.Fetched
}

func (p imagePlan) with(get string, fetched config.Fetched) imagePlan {
	p.fetched = maps.Clone(p.fetched)
	if p.fetched == nil {
		p.fetched = map[string]config.Fetched{}
	}

	p.fetched[get] = fetched

	return p
}

// planNonGetNode hashes step under the image the executor will resolve for
// it (config.ResolveArtifactImage, shared by both sides).
//
// A step whose image cannot be resolved here is planned under its literal
// image and made unskippable rather than failing the plan: the executor
// reports the real error in the step's own place, and a chain it cannot run
// is never recorded as done.
func (p imagePlan) planNonGetNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, bool, error) {
	resolved, _, err := config.ResolveArtifactImage(step, p.artifacts, p.fetched)
	if err != nil {
		node, _, planErr := planNonGetNode(cfg, step, i, parentHash)

		return node, true, planErr
	}

	return planNonGetNode(cfg, resolved, i, parentHash)
}

// planNonGetNode builds the plan node for any step but a get and reports
// whether that step makes its chain unskippable — UncacheableReason's call,
// the same one the run prints. Unskippable is not hashed, so it only ever
// makes the cache more conservative.
func planNonGetNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, bool, error) {
	node, err := planNodeOf(cfg, step, i, parentHash)
	if err != nil {
		return node, true, err
	}

	reason, err := UncacheableReason(cfg, step)

	return node, reason != "", err
}

func planNodeOf(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	// A load_var: captures a value that does not exist until the run produces
	// it, and an approval: is a person, not a computation: neither has content
	// to hash, and neither is ever skipped.
	if step.LoadVar != "" {
		return Node{ParentHash: parentHash, Kind: NodeKindLoadVar, StepIndex: i, Resource: step.LoadVar}, nil
	}

	if step.Approval != nil {
		return Node{ParentHash: parentHash, Kind: NodeKindApproval, StepIndex: i, Resource: "approval"}, nil
	}

	// across: is a modifier rather than a kind (see internal/pipeline's
	// dispatch): the block is one plan node whose cells are hashed inside it.
	if len(step.Across) > 0 {
		return newNode(NodeKindAcross, "across", i, parentHash, "", func() (map[string]any, error) {
			return AcrossPlanContent(cfg, step, i)
		})
	}

	return planKindNode(cfg, step, i, parentHash)
}

// planKindNode builds the plan node for an ordinary (non-across) step.
func planKindNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	kind, ok := step.Kind()
	if !ok {
		return Node{}, fmt.Errorf("step %d: unrecognized step (must set exactly one step kind)", i)
	}

	switch kind { //nolint:exhaustive // default covers the kinds planNodeOf never sends here
	case config.StepKindTask:
		return planTaskNode(cfg, step, i, parentHash)
	case config.StepKindPut:
		return newNode(NodeKindPut, fmt.Sprintf("put %q", step.Put), i, parentHash, step.DisplayName(), func() (map[string]any, error) {
			return stepContentMap(cfg, step)
		})
	case config.StepKindAgent:
		return agentNode(cfg, step, i, parentHash)
	case config.StepKindTry, config.StepKindRace, config.StepKindInParallel, config.StepKindEnsemble, config.StepKindDo:
		return planContainerNode(cfg, step, kind, i, parentHash)
	default: // get, load_var:, approval: — planned before this dispatch
		return Node{}, fmt.Errorf("step %d: unexpected %s step (planned before this dispatch)", i, kind)
	}
}

func planTaskNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d: %w", i, err)
	}

	return newNode(NodeKindTask, fmt.Sprintf("task %q", rt.Name), i, parentHash, rt.Name, func() (map[string]any, error) {
		return TaskNodeContent(cfg, step, rt)
	})
}

// ContainerNode is the node a block hashes to, which the executor asks the
// skip index about before running the block.
func ContainerNode(cfg *config.Config, step config.Step, kind config.StepKind, i int, parentHash string) (Node, error) {
	return planContainerNode(cfg, step, kind, i, parentHash)
}

func planContainerNode(cfg *config.Config, step config.Step, kind config.StepKind, i int, parentHash string) (Node, error) {
	var (
		nodeKind NodeKind
		content  func(*config.Config, config.Step) (map[string]any, error)
		resource string
	)

	switch kind { //nolint:exhaustive // the caller dispatches only container kinds here
	case config.StepKindTry:
		// Resource names the wrapped step, matching what internal/pipeline
		// records at run time — without it `steps plan` printed a nameless try
		// row.
		nodeKind, content, resource = NodeKindTry, TryNodeContent, stepResourceName(step)
	case config.StepKindDo:
		nodeKind, content = NodeKindDo, DoNodeContent
	case config.StepKindRace:
		nodeKind, content = NodeKindRace, RaceNodeContent
	case config.StepKindEnsemble:
		nodeKind, content = NodeKindEnsemble, EnsembleNodeContent
	default: // config.StepKindInParallel
		nodeKind, content = NodeKindParallel, ParallelNodeContent
	}

	return newNode(nodeKind, string(nodeKind), i, parentHash, resource, func() (map[string]any, error) {
		return content(cfg, step)
	})
}

func agentNode(cfg *config.Config, step config.Step, i int, parentHash string) (Node, error) {
	what := fmt.Sprintf("agent %q", step.Agent)

	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (%s): %w", i, what, err)
	}

	return newNode(NodeKindAgent, what, i, parentHash, ri.AgentName, func() (map[string]any, error) {
		return AgentContentMap(cfg, step, ri)
	})
}

// newNode builds and hashes step i's plan node. what names the step in an
// error, after its position.
func newNode(kind NodeKind, what string, i int, parentHash, resource string, build func() (map[string]any, error)) (Node, error) {
	content, err := build()
	if err != nil {
		return Node{}, fmt.Errorf("step %d (%s): %w", i, what, err)
	}

	hash, err := HashNode(kind, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (%s): %w", i, what, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: kind, StepIndex: i, Resource: resource, Content: content}, nil
}

// planGetStep resolves step's version(s) and recurses into the remainder of
// steps once per version, returning one Chain per leaf reached. It always
// terminates the calling planSteps loop, mirroring the pipeline's
// runGetStep control flow.
func planGetStep(
	ctx context.Context, cfg *config.Config, steps []config.Step, i int, step config.Step, pinned map[string]string,
	prefix []Node, parentHash string, unskippable bool, cache VersionResolver, images imagePlan,
) ([]Chain, error) {
	res, resourceType, versions, err := resolveGet(ctx, cache, cfg, i, step, pinned)
	if err != nil {
		return nil, err
	}

	chains := make([]Chain, 0, len(versions))

	for _, version := range versions {
		node, err := newNode(NodeKindGet, fmt.Sprintf("get %q", step.Get), i, parentHash, res.Name, func() (map[string]any, error) {
			return GetNodeContent(cfg, step, *resourceType, res.Env, res.Source, version)
		})
		if err != nil {
			return nil, err
		}

		branch := images.with(step.Get, config.Fetched{Source: res.Source, Version: version})

		sub, err := planSteps(ctx, cfg, steps[i+1:], pinned, append(append([]Node{}, prefix...), node), node.Hash, unskippable, cache, branch)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		chains = append(chains, sub...)
	}

	return chains, nil
}

// stepResourceName is the name a node is displayed under: whichever of
// task/put/agent a step sets, looking through any try: wrappers.
func stepResourceName(step config.Step) string {
	inner := step.Unwrap()

	switch kind, _ := inner.Kind(); kind {
	case config.StepKindTask:
		return inner.Task
	case config.StepKindPut:
		return inner.Put
	case config.StepKindAgent:
		return inner.Agent
	case config.StepKindGet, config.StepKindTry, config.StepKindInParallel, config.StepKindRace,
		config.StepKindEnsemble, config.StepKindLoadVar, config.StepKindApproval, config.StepKindDo:
		// Unwrap() already resolved Try away, and a get node takes its name from the resource, not here.
	}

	return ""
}

var errNoResolver = errors.New("no version resolver to resolve a get with")

func resolveGet(
	ctx context.Context, cache VersionResolver, cfg *config.Config, i int, step config.Step, pinned map[string]string,
) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	if cache == nil {
		return nil, nil, nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, errNoResolver)
	}

	res, resourceType, versions, err := cache.ResolveVersionsCached(ctx, cfg, step, pinned)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	return res, resourceType, versions, nil
}
