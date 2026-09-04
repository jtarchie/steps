package pipeline

// Placing a resource's check, in and out: the same tag-to-machine resolution
// a task step gets, reached through internal/resource's Placer seam because
// that package may not know what a worker is.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// WithResourcePlacement installs how every resource stage under ctx finds its
// worker: by the step's tags:, which a get or put inherited from its resource
// at load when it had none of its own.
//
// Once per job and once per poll, above every stage, so the check a plan
// resolves versions with lands on the same machine as the in: that follows —
// a source only reachable from a worker has to be asked from there too.
func WithResourcePlacement(ctx context.Context) context.Context {
	return rsrc.WithPlacerFor(ctx, func(ctx context.Context, step config.Step) (rsrc.Placer, error) {
		tag := placementTag(step)
		if tag == "" {
			return nil, nil
		}

		worker, err := workerFor(ctx, step)
		if err != nil {
			return nil, err
		}

		if worker == "" {
			return nil, nil
		}

		store := artifactStoreFrom(ctx)

		return func(spec shell.RunnerSpec) (shell.Runner, error) {
			spec.Worker, spec.WorkerTag, spec.ArtifactStore = worker, tag, store

			//nolint:contextcheck // NewRunner takes no context; opening the artifact store reads only local config
			runner, err := venue.NewRunner(spec)
			if err != nil {
				return nil, err //nolint:wrapcheck // NewRunner's error already names the cause
			}

			// The stage closes the runner itself, so what the machine said
			// about itself is read on the way out — the same two facts a
			// task defers, at the same moment.
			return closingRunner{Runner: runner, onClose: func(r shell.Runner) {
				releaseIfReclaimed(ctx, step, r, worker)
				notePlacement(ctx, r)
			}}, nil
		}, nil
	})
}

// closingRunner runs onClose before Close, for a runner whose owner is the
// stage rather than this package.
type closingRunner struct {
	shell.Runner

	onClose func(shell.Runner)
}

func (c closingRunner) Close() error {
	c.onClose(c.Runner)

	return c.Runner.Close() //nolint:wrapcheck // the inner runner's own error
}

func (c closingRunner) WithLabel(label string) shell.Runner {
	return closingRunner{Runner: c.Runner.WithLabel(label), onClose: c.onClose}
}

// resourceStep is the step a check outside any plan stands in for: a get of
// the resource, carrying the resource's tags: as a plan get would have
// inherited them.
func resourceStep(cfg *config.Config, name string) config.Step {
	step := config.Step{Get: name}

	resource, err := cfg.FindResource(name)
	if err == nil {
		step.Tags = resource.Tags
	}

	return step
}

// PlaceResource installs the placer for a check of one resource outside any
// plan — the poller's, and a run's history refresh.
func PlaceResource(ctx context.Context, cfg *config.Config, name string) (context.Context, error) {
	return rsrc.Place(ctx, resourceStep(cfg, name)) //nolint:wrapcheck // Place's error names the tag and the resource
}

// runPlacedStage runs one in: or out: — the whole attempts: loop of it —
// with its worker re-acquired on eviction, on the same terms as a task's
// command: outside the attempts, within the step's whole budget. A runner per
// attempt rather than one shared, unlike a task, because a stage builds and
// closes its own; the tree an attempt re-sends is the get's still-empty
// directory or the put's declared inputs, content-keyed on the worker.
func runPlacedStage(ctx context.Context, step config.Step, stage func(context.Context) error) error {
	budget, err := stepBudget(step, step.Timeout)
	if err != nil {
		return err
	}

	return withVenueRetry(ctx, step, budget, func(ctx context.Context) (string, error) {
		// Resolved INSIDE the retry, so a re-placement dials the freshly
		// acquired machine rather than the one just forgotten.
		dialed, err := workerFor(ctx, step)
		if err != nil {
			return "", err
		}

		ctx, err = rsrc.Place(ctx, step)
		if err != nil {
			return dialed, err //nolint:wrapcheck // Place's error names the tag and the resource
		}

		return dialed, stage(ctx)
	})
}

// ValidatePipelinePlacement refuses a pipeline whose resources name workers
// this invocation cannot supply — for a poller, which checks resources
// outside any job and so outside ValidateWorkerPlacement's walk.
func ValidatePipelinePlacement(ctx context.Context, cfg *config.Config) error {
	workers := workersFrom(ctx)

	for _, resource := range cfg.Resources {
		tag := placementTag(resourceStep(cfg, resource.Name))
		if tag == "" {
			continue
		}

		worker, ok := workers[tag]
		if !ok {
			return fmt.Errorf("resource %q: no worker is registered for tag %s — map it with --worker %s=ssh://user@host, or remove the tag",
				resource.Name, tag, tag)
		}

		err := worker.PlacementCheck(artifactStoreFrom(ctx) != "")
		if err != nil {
			return fmt.Errorf("resource %q: --worker %s: %w", resource.Name, tag, err)
		}
	}

	return nil
}
