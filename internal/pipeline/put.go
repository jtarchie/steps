package pipeline

// put: — handing a step's inputs to a resource's out: command.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/workspace"
)

// findPutTarget resolves a put step's resource and its type together, since
// neither is usable without the other. The error is unwrapped; each caller
// adds its own context.
func findPutTarget(cfg *config.Config, name string) (*config.Resource, *config.ResourceType, error) {
	resource, err := cfg.FindResource(name)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // callers wrap with their own step/put context
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // callers wrap with their own step/put context
	}

	return resource, resourceType, nil
}

// runPutStep hashes and always runs step (put steps are never skipped).
func runPutStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	resource, resourceType, err := findPutTarget(r.cfg, step.Put)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content, err := merkle.PutNodeContent(r.cfg, step, *resourceType, resource.Source, step.Params, step.InputNames(), step.InputsAll())
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindPut, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	slog.Debug("job.step", "job", r.jobName, "index", i, "kind", "put", "resource", step.Put)

	fmt.Printf("put: %s\n", step.Put)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}

	result, err := executePut(ctx, r.cfg, step, r.bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
		recordStepFailure(ctx, r, node, wrapped)

		return stepResult{}, wrapped
	}

	err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", result, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return ran(hash), nil
}

// executePut materializes a put step's input view, runs its resource's out:
// command with retries and timeout, and returns the produced version — with
// no merkle/store recording. Shared by runPutStep (which records) and hook
// execution (which does not; a put hook's result version is discarded). A
// nonzero out: exit is marked as a task-level failure so hook dispatch
// classifies it as failed; a resource lookup or workspace error stays
// unmarked → errored.
func executePut(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (map[string]any, error) {
	resource, resourceType, err := findPutTarget(cfg, step.Put)
	if err != nil {
		return nil, err
	}

	space, err := bw.PutSpace(ctx, step.Put, step.InputNames(), step.InputsAll())
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}
	defer workspace.CloseSpace(space, step.Put)

	var result map[string]any

	retryErr := retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
		fmt.Printf("put: %s (attempt %d/%d)\n", step.Put, attempt, total)
		slog.Info("job.put.attempt", "put", step.Put, "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		runResult, runErr := rsrc.RunOut(attemptCtx, cfg, *resourceType, resource.Source, step.Params, space.Dir())
		if runErr != nil {
			// Classified against the ATTEMPT's context, so a per-attempt
			// timeout is told apart from a real nonzero out:.
			return classifyRunError(attemptCtx, runErr)
		}

		result = runResult

		return nil
	})
	if retryErr != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, retryErr)
	}

	return result, nil
}
