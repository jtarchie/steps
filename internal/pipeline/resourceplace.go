package pipeline

// Placing a resource's check, in and out: the same tag-to-machine resolution
// a task step gets, reached through internal/resource's Placer seam because
// that package may not know what a worker is.

import (
	"context"
	"errors"
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

		store, keep := artifactStoreFrom(ctx), keepFrom(ctx)

		return func(spec shell.RunnerSpec) (shell.Runner, error) {
			spec.Worker, spec.WorkerTag, spec.ArtifactStore, spec.Keep = worker, tag, store, keep

			runner, err := placedRunner(ctx, step, spec)
			if err != nil {
				return nil, err
			}

			// A check has no tree, so it can be asked again of a fresh
			// machine when the first is reclaimed under it: the retry a
			// task's command gets from withVenueRetry, applied to the one
			// call a check makes. An in: or out: is retried by its stage.
			if spec.Cwd == "" {
				return checkRunner{Runner: runner, step: step, spec: spec}, nil
			}

			return runner, nil
		}, nil
	})
}

// placedRunner builds one stage's runner on its worker, reading what the
// machine said about itself on the way out — the same two facts a task
// defers, at the same moment — since the stage closes the runner itself.
func placedRunner(ctx context.Context, step config.Step, spec shell.RunnerSpec) (shell.Runner, error) {
	//nolint:contextcheck // NewRunner takes no context; opening the artifact store reads only local config
	runner, err := venue.NewRunner(spec)
	if err != nil {
		return nil, err //nolint:wrapcheck // NewRunner's error already names the cause
	}

	return closingRunner{Runner: runner, onClose: func(r shell.Runner) {
		releaseIfReclaimed(ctx, step, r, spec.Worker)
		notePlacement(ctx, r)
	}}, nil
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

// checkRunner re-places a check whose worker was reclaimed mid-command.
//
// Only RunCapture, because that is the one call a check makes; the runner it
// wraps is the first machine's, and a re-placement builds another from the
// same spec against the freshly acquired one, closing the dead one first.
// The call's context carries the leases and the placement sink, being a
// descendant of the one the placer was resolved under.
type checkRunner struct {
	shell.Runner

	step config.Step
	spec shell.RunnerSpec
}

func (c checkRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	var out []byte

	current := c.Runner

	err := withVenueRetry(ctx, c.step, 0, func(retryCtx context.Context) (string, error) {
		if current == nil {
			worker, err := workerFor(retryCtx, c.step)
			if err != nil {
				return "", err
			}

			spec := c.spec
			spec.Worker = worker

			runner, err := placedRunner(retryCtx, c.step, spec)
			if err != nil {
				return worker, err
			}

			current = runner.WithLabel(c.spec.WorkerTag + " check")
		}

		captured, err := current.RunCapture(ctx, command)
		if errors.Is(err, venue.ErrEvicted) {
			// Closed here rather than by the stage, which only ever sees
			// the runner it was handed — the last one.
			_ = current.Close()
			current = nil
		}

		out = captured

		if err != nil {
			return c.spec.Worker, fmt.Errorf("%w", err)
		}

		return c.spec.Worker, nil
	})

	// The stage closes what it was handed; hand it whatever survived.
	if current != nil {
		c.Runner = current
	}

	return out, err
}

func (c checkRunner) WithLabel(label string) shell.Runner {
	return checkRunner{Runner: c.Runner.WithLabel(label), step: c.step, spec: c.spec}
}

// keepKey types the context value carrying --keep-workspace, for a worker's
// scratch: on this machine the workspace decides its own fate, but a resource
// stage has no StepSpace to ask, so the flag travels on the context.
type keepKey struct{}

// WithKeepWorkspace records --keep-workspace for the stages that have no
// workspace of their own to read it from.
func WithKeepWorkspace(ctx context.Context, keep bool) context.Context {
	if !keep {
		return ctx
	}

	return context.WithValue(ctx, keepKey{}, true)
}

func keepFrom(ctx context.Context) bool {
	keep, _ := ctx.Value(keepKey{}).(bool)

	return keep
}

// stepPlacementTags is every tag a get or put dials: its own, and its
// resource's when that differs — the check's — deduplicated. A task's is just
// its own.
func stepPlacementTags(cfg *config.Config, step config.Step) []string {
	tags := []string{}

	if tag := placementTag(step); tag != "" {
		tags = append(tags, tag)
	}

	inner := step
	if inner.Try != nil {
		inner = *inner.Try
	}

	name, ok := stepResourceName(inner)
	if !ok {
		return tags
	}

	if tag := placementTag(resourceStep(cfg, name)); tag != "" && (len(tags) == 0 || tags[0] != tag) {
		tags = append(tags, tag)
	}

	return tags
}

// stepResourceName is the resource a get or put names, and false otherwise.
func stepResourceName(step config.Step) (string, bool) {
	//kindswitch:ignore only a get or a put names a resource; the other kinds are the false answer
	switch {
	case step.Get != "":
		return step.GetResourceName(), true
	case step.Put != "":
		return step.Put, true
	default:
		return "", false
	}
}

// checkWorkerAcquirable reports whether a resource's check would have to
// acquire its machine — start or launch it — rather than dial one that exists.
func checkWorkerAcquirable(ctx context.Context, cfg *config.Config, name string) bool {
	tag := placementTag(resourceStep(cfg, name))
	if tag == "" {
		return false
	}

	worker, ok := workersFrom(ctx)[tag]

	return ok && worker.Acquirable()
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
		// acquired machine rather than the one just forgotten. Place asks
		// the leases the same question a moment later and gets the same
		// memoized answer, so this names the machine the stage dials.
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

// ValidatePipelinePlacement refuses a poller whose resources name workers this
// invocation cannot supply, or would have to acquire.
//
// Only the resources the poller checks (names), not every tagged one in the
// file: a put-only resource's tag is a job's business, validated when that
// job runs. And an acquisition rung is refused outright: a poll and a running
// job hold independent leases with no notion of who owns the machine, so a
// poll giving its machine back would stop the instance a job was mid-step on
// — and a check that runs once an interval is the wrong thing to launch a
// billed machine for. A resource checked by the poller needs a worker that
// already exists.
func ValidatePipelinePlacement(ctx context.Context, cfg *config.Config, names []string) error {
	workers := workersFrom(ctx)

	for _, name := range names {
		tag := placementTag(resourceStep(cfg, name))
		if tag == "" {
			continue
		}

		worker, ok := workers[tag]
		if !ok {
			return fmt.Errorf("resource %q: no worker is registered for tag %s — map it with --worker %s=ssh://user@host, or remove the tag",
				name, tag, tag)
		}

		if worker.Acquirable() {
			return fmt.Errorf("resource %q: --worker %s names a machine steps would have to acquire (%s), and a polled check needs one that already exists — map the tag to ssh://, local:, or a running instance",
				name, tag, worker.Address())
		}

		err := worker.PlacementCheck(artifactStoreFrom(ctx) != "")
		if err != nil {
			return fmt.Errorf("resource %q: --worker %s: %w", name, tag, err)
		}
	}

	return nil
}
