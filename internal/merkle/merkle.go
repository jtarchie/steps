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
	"fmt"
	"log/slog"

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
func GetNodeContent(resourceType config.ResourceType, source, version map[string]any) map[string]any {
	return map[string]any{
		"in_template": resourceType.Config.In,
		"source":      source,
		"version":     version,
	}
}

// TaskNodeContent and PutNodeContent fold in inputs/outputs only when ws is
// non-nil (workspace: configured), so a pipeline that never opts in hashes
// byte-identically to before this field existed — switching a task between
// shared and isolated execution of the same run: must invalidate its cache,
// but the mere existence of the feature must not invalidate anyone else's.
func TaskNodeContent(rt config.ResolvedTask, ws *config.WorkspaceConfig) map[string]any {
	content := map[string]any{"run": rt.Run}

	if ws != nil {
		content["inputs"] = config.StableStrings(rt.Inputs)
		content["outputs"] = config.StableStrings(rt.Outputs)
	}

	return content
}

// PutNodeContent builds the content map hashed for a put node.
func PutNodeContent(resourceType config.ResourceType, source, params map[string]any, inputs []string, ws *config.WorkspaceConfig) map[string]any {
	content := map[string]any{
		"out_template": resourceType.Config.Out,
		"source":       source,
		"params":       params,
	}

	if ws != nil {
		content["inputs"] = config.StableStrings(inputs)
	}

	return content
}

// AgentContentMap is the content hashed for an agent node: everything that
// determines the model's output (agent, prompt, dir, resolved model/endpoint,
// persona, dials, and the effective tool set). Attempts is excluded (a pure
// retry policy doesn't change the intended result); the API key and its env
// var name are excluded (nothing secret-adjacent belongs in hashed content).
// inputs/outputs are folded in only when ws is non-nil (workspace:
// configured) — see TaskNodeContent's doc comment for why.
func AgentContentMap(step config.Step, ri config.ResolvedInvocation, ws *config.WorkspaceConfig) map[string]any {
	toolsContent := make([]map[string]any, len(ri.ToolSpecs))
	for i, t := range ri.ToolSpecs {
		toolsContent[i] = map[string]any{
			"builtin":     t.Builtin,
			"name":        t.Name,
			"description": t.Description,
			"run":         t.Run,
		}
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

	if ws != nil {
		content["inputs"] = config.StableStrings(step.Inputs)
		content["outputs"] = config.StableStrings(step.Outputs)
	}

	return content
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
// CheckVersions therefore runs here as well as later during real execution
// for any branch that ends up running — an accepted cost, since check
// commands are expected to be read-only/idempotent within one synchronous
// CLI invocation.
func PlanChains(ctx context.Context, cfg *config.Config, jobName string, steps []config.Step, pinned map[string]string) ([]Chain, error) {
	slog.Debug("job.plan", "job", jobName, "steps", len(steps))

	chains, err := planSteps(ctx, cfg, steps, pinned, nil, "", false)
	if err != nil {
		return nil, err
	}

	slog.Debug("job.planned", "job", jobName, "chains", len(chains))

	return chains, nil
}

func planSteps(ctx context.Context, cfg *config.Config, steps []config.Step, pinned map[string]string, prefix []Node, parentHash string, unskippable bool) ([]Chain, error) {
	for i, step := range steps {
		if step.Get != "" {
			return planGetStep(ctx, cfg, steps, i, step, pinned, prefix, parentHash, unskippable)
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
	switch {
	case step.Task != "":
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return Node{}, false, fmt.Errorf("step %d: %w", i, err)
		}

		node, err := taskNode(rt, i, parentHash, cfg.Workspace)

		return node, rt.Fix != nil, err
	case step.Put != "":
		node, err := putNode(cfg, step, i, parentHash)

		return node, true, err
	case step.Agent != "":
		node, err := agentNode(cfg, step, i, parentHash)

		return node, true, err
	default:
		return Node{}, false, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// planGetStep resolves step's version(s) and recurses into the remainder of
// steps once per version, returning one Chain per leaf reached. It always
// terminates the calling planSteps loop, mirroring the pipeline's
// runGetStep control flow.
func planGetStep(ctx context.Context, cfg *config.Config, steps []config.Step, i int, step config.Step, pinned map[string]string, prefix []Node, parentHash string, unskippable bool) ([]Chain, error) {
	res, resourceType, versions, err := rsrc.ResolveVersions(ctx, cfg, step, pinned)
	if err != nil {
		return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	chains := make([]Chain, 0, len(versions))

	for _, version := range versions {
		content := GetNodeContent(*resourceType, res.Source, version)

		hash, err := HashNode(NodeKindGet, content, parentHash)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindGet, StepIndex: i, Resource: res.Name, Content: content}

		sub, err := planSteps(ctx, cfg, steps[i+1:], pinned, append(append([]Node{}, prefix...), node), hash, unskippable)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		chains = append(chains, sub...)
	}

	return chains, nil
}

func taskNode(rt config.ResolvedTask, i int, parentHash string, ws *config.WorkspaceConfig) (Node, error) {
	content := TaskNodeContent(rt, ws)

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

	content := PutNodeContent(*resourceType, res.Source, step.Params, step.Inputs, cfg.Workspace)

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

	content := AgentContentMap(step, ri, cfg.Workspace)

	hash, err := HashNode(NodeKindAgent, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAgent, StepIndex: i, Resource: ri.AgentName, Content: content}, nil
}
