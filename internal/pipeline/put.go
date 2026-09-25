package pipeline

// put: — handing a step's inputs to a resource's out: command.

import (
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/venue"
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

// putLabel is a put's terminal line: its name, and the resource it publishes
// to when resource: renamed it, so a failed publish names its target.
func putLabel(step config.Step) string {
	if target := step.PutResourceName(); target != step.Put {
		return fmt.Sprintf("%s (resource: %s)", step.Put, target)
	}

	return step.Put
}

// runPutStep hashes and always runs step (put steps are never skipped).
func runPutStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	resource, resourceType, err := findPutTarget(r.cfg, step.PutResourceName())
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content, err := merkle.PutNodeContent(r.cfg, step, *resourceType, resource.Env, resource.Source, step.Params, step.InputNames(), step.InputsAll())
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindPut, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	logFrom(ctx).Debug("job.step", "step", step.Put, "resource", step.PutResourceName())

	if step.PutResourceName() != step.Put {
		notef(ctx, "put: %s", putLabel(step))
	}

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindPut, StepIndex: i, Resource: step.DisplayName(), Content: content}

	ctx, placed := withPlacementSink(ctx)

	result, err := executePut(ctx, r.cfg, step, r.bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
		recordStepFailure(ctx, r, node, wrapped)
		recordPlacement(ctx, r, placed, i, step.Put, hash, hash)

		return stepResult{}, wrapped
	}

	err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", result, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	// After the node, never before: run_placements references it.
	recordPlacement(ctx, r, placed, i, step.Put, hash, hash)

	recordPutOrder(ctx, r.st, resource.Name, result)
	recordBuildVersion(ctx, resource.Name, result)

	return ran(hash), nil
}

// executePut materializes a put step's input view, runs its resource's out:
// command with retries and timeout, and returns the produced version — with
// no merkle/store recording. Shared by runPutStep and hook execution, which each record
// the version it returns. A
// nonzero out: exit is marked as a task-level failure so hook dispatch
// classifies it as failed; a resource lookup or workspace error stays
// unmarked → errored.
func executePut(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (map[string]any, error) {
	// Named here, not left to the caller: runHookStep's put branch adds no
	// context of its own, so an unwrapped lookup failure reaches a reader as a
	// bare "no resource type named x" with nothing saying it came from a put.
	resource, resourceType, err := findPutTarget(cfg, step.PutResourceName())
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	space, remote, err := placedPutSpace(ctx, bw, step)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}
	defer workspace.CloseSpace(space, step.Put)

	ctx = withRemoteInputs(ctx, remote)

	// Once, outside the retries: every attempt and re-placement reads the
	// same versions, and none of them needs anything on disk to do it.
	inputs := putInputs(ctx, step)

	var result map[string]any

	// The venue retry wraps the attempts: loop, as a task's does — see
	// runPlacedStage.
	retryErr := runPlacedStage(ctx, step, func(ctx context.Context) error {
		return retryWithTimeout(ctx, step.Attempts, step.Timeout, func(attempt, total int) {
			notef(ctx, "put: %s (attempt %d/%d)", putLabel(step), attempt, total)
			logFrom(ctx).Info("job.put.attempt", "put", step.Put, "attempt", attempt, "total_attempts", total)
		}, func(attemptCtx context.Context) error {
			runResult, runErr := rsrc.RunOut(attemptCtx, cfg, *resourceType, resource.Env, resource.Source, step.Params, inputs, space.Dir())
			if runErr != nil {
				// An eviction ends the attempts loop rather than spending
				// it — the machine is gone, and the venue retry re-places.
				if errors.Is(runErr, venue.ErrEvicted) {
					return retry.Stop(runErr)
				}

				// Classified against the ATTEMPT's context, so a per-attempt
				// timeout is told apart from a real nonzero out:.
				return classifyRunError(attemptCtx, runErr)
			}

			result = runResult

			return nil
		})
	})
	if retryErr != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, retryErr)
	}

	if result == nil {
		notef(ctx, "put: %s (no version)", putLabel(step))
	} else {
		notef(ctx, "put: %s (version: %s)", putLabel(step), versionText(result))
	}

	return result, nil
}
