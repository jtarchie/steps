package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
)

// NodeKind identifies which kind of plan step a Node represents.
type NodeKind string

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

// Chain is one root-to-leaf path through a job's plan: exactly what
// runTriggeredBuild's recursion walks today (a plain chain, not a general
// multi-parent DAG, since get: version: every is the only fan-out point).
type Chain struct {
	Nodes    []Node
	RootHash string
	// Unskippable is true if any node is a put or agent step — both have
	// side effects (or are non-deterministic, in the agent's case) and must
	// always run rather than reuse a prior hash match.
	Unskippable bool
}

// getNodeContent, taskNodeContent, putNodeContent, and agentNodeContent
// build the exact content maps hashed for each step kind. They're shared
// between planning (merkle.go) and real execution (job.go) so both compute
// identical hashes for identical steps.
func getNodeContent(resourceType ResourceType, source, version map[string]any) map[string]any {
	return map[string]any{
		"in_template": resourceType.Config.In,
		"source":      source,
		"version":     version,
	}
}

func taskNodeContent(run string) map[string]any {
	return map[string]any{"run": run}
}

func putNodeContent(resourceType ResourceType, source, params map[string]any) map[string]any {
	return map[string]any{
		"out_template": resourceType.Config.Out,
		"source":       source,
		"params":       params,
	}
}

// hashNode computes a Node's content-addressed hash: sha256 hex of the
// canonical JSON of {kind, content, parent}. encoding/json.Marshal sorts
// map keys, which is what makes this deterministic — content must stay
// built from plain maps/slices/scalars for that guarantee to hold.
func hashNode(kind NodeKind, content map[string]any, parentHash string) (string, error) {
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

// PlanChains walks a job's plan exactly like runSteps does (reusing
// resolveGetVersions for get-step version/pin resolution, so that logic
// stays defined in exactly one place), but never executes anything — no
// RunIn, RunShell, or RunOut. It returns one Chain per leaf reached, which
// is more than one only when a get step uses version: every.
//
// CheckVersions therefore runs here as well as later during real execution
// for any branch that ends up running — an accepted cost, since check
// commands are expected to be read-only/idempotent within one synchronous
// CLI invocation.
func PlanChains(ctx context.Context, cfg *Config, jobName string, steps []Step, pinned map[string]string) ([]Chain, error) {
	slog.Debug("job.plan", "job", jobName, "steps", len(steps))

	chains, err := planSteps(ctx, cfg, steps, pinned, nil, "", false)
	if err != nil {
		return nil, err
	}

	slog.Debug("job.planned", "job", jobName, "chains", len(chains))

	return chains, nil
}

func planSteps(ctx context.Context, cfg *Config, steps []Step, pinned map[string]string, prefix []Node, parentHash string, unskippable bool) ([]Chain, error) {
	for i, step := range steps {
		switch {
		case step.Get != "":
			return planGetStep(ctx, cfg, steps, i, step, pinned, prefix, parentHash, unskippable)
		case step.Task != "":
			node, err := taskNode(step, i, parentHash)
			if err != nil {
				return nil, err
			}

			prefix = append(prefix, node)
			parentHash = node.Hash
		case step.Put != "":
			node, err := putNode(cfg, step, i, parentHash)
			if err != nil {
				return nil, err
			}

			prefix = append(prefix, node)
			parentHash = node.Hash
			unskippable = true
		case step.Agent != "":
			node, err := agentNode(cfg, step, i, parentHash)
			if err != nil {
				return nil, err
			}

			prefix = append(prefix, node)
			parentHash = node.Hash
			unskippable = true
		default:
			return nil, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
		}
	}

	return []Chain{{Nodes: prefix, RootHash: parentHash, Unskippable: unskippable}}, nil
}

// planGetStep resolves step's version(s) and recurses into the remainder of
// steps once per version, returning one Chain per leaf reached. It always
// terminates the calling planSteps loop, mirroring runGetStep's control flow.
func planGetStep(ctx context.Context, cfg *Config, steps []Step, i int, step Step, pinned map[string]string, prefix []Node, parentHash string, unskippable bool) ([]Chain, error) {
	resource, resourceType, versions, err := resolveGetVersions(ctx, cfg, step, pinned)
	if err != nil {
		return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	chains := make([]Chain, 0, len(versions))

	for _, version := range versions {
		content := getNodeContent(*resourceType, resource.Source, version)

		hash, err := hashNode(NodeKindGet, content, parentHash)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		node := Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		sub, err := planSteps(ctx, cfg, steps[i+1:], pinned, append(append([]Node{}, prefix...), node), hash, unskippable)
		if err != nil {
			return nil, fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		chains = append(chains, sub...)
	}

	return chains, nil
}

func taskNode(step Step, i int, parentHash string) (Node, error) {
	content := taskNodeContent(step.Run)

	hash, err := hashNode(NodeKindTask, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (task %q): %w", i, step.Task, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindTask, StepIndex: i, Resource: step.Task, Content: content}, nil
}

func putNode(cfg *Config, step Step, i int, parentHash string) (Node, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content := putNodeContent(*resourceType, resource.Source, step.Params)

	hash, err := hashNode(NodeKindPut, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}, nil
}

func agentNode(cfg *Config, step Step, i int, parentHash string) (Node, error) {
	ri, err := resolveAgentInvocation(cfg, step)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	content := agentContentMap(step, ri)

	hash, err := hashNode(NodeKindAgent, content, parentHash)
	if err != nil {
		return Node{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return Node{Hash: hash, ParentHash: parentHash, Kind: NodeKindAgent, StepIndex: i, Resource: ri.agentName, Content: content}, nil
}
