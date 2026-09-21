package pipeline

// Putting an agent step on the machine its tags: name.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// placedAgentRunner is the factory internal/agent builds its runner through, carrying the one thing that package deliberately cannot know: which machine a tag resolves to. Everything else on the spec is the agent's own and arrives already filled in, and an untagged step still ends at shell.NewRunner by the route a task takes.
func placedAgentRunner(step config.Step) agent.RunnerFactory {
	return func(ctx context.Context, spec shell.RunnerSpec) (shell.Runner, error) {
		placed, err := placeAgentSpec(ctx, step, spec)
		if err != nil {
			return nil, err
		}

		//nolint:contextcheck // NewRunner takes no context; opening the artifact store reads only local config
		runner, err := venue.NewRunner(placed)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", step.Agent, err)
		}

		// Unlabelled: prepareAgentStep labels what this hands back, and doing it here only to have it replaced would read as the display name mattering when nothing ever sees it.
		return runner, nil
	}
}

// placeAgentSpec adds what the agent could not know to the spec it built.
//
// Separated from the dialling so the crossing itself can be read and tested: the worst bug of the venue work was a lease that resolved a worker nothing ever dialled, sitting between two halves that were each well covered. This is where the mapping and the runner meet, and a spec leaving here without the worker on it is a step that runs on the wrong machine and reports success.
func placeAgentSpec(ctx context.Context, step config.Step, spec shell.RunnerSpec) (shell.RunnerSpec, error) {
	worker, err := workerFor(ctx, step)
	if err != nil {
		return spec, err
	}

	spec.Worker = worker
	spec.WorkerTag = placementTag(step)
	spec.ArtifactStore = artifactStoreFrom(ctx)
	// A conversation cannot survive its tree being re-sent underneath it. The venue's redial is right for a task, whose command re-runs from the top, and silently wrong here: it would restore the step's inputs over every edit the model made outside outputs:, start a fresh container, and tell the model nothing about either.
	spec.NoRedial = true

	return spec, nil
}
