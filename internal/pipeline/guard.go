package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// evaluateStepGuard runs a step's when: guard and reports whether the step
// should run. A step with no guard always runs.
//
// The contract is the exit code and nothing else: 0 runs the step, nonzero
// skips it. A nonzero exit is a legitimate "false" — `grep -q` finding no
// match is the canonical case — so it is never an error. Only a runner-level
// failure (the command could not be started at all: an unusable cwd, a bad
// image, a docker daemon that isn't running) returns an error, which fails the
// step rather than being silently read as "skip". Getting that distinction
// wrong would turn an infrastructure outage into a quietly-skipped pipeline.
//
// The guard runs in the same view the step itself gets: under the step's
// resolved image, in a directory materialized from the step's declared
// inputs, closed WITHOUT Capture so a guard can never publish artifacts — a
// discarded copy is the price of a guard that can read what the step reads.
//
// GuardSpace, not TaskSpace, because the two views differ in exactly one
// respect: an input the build never produced is absent here rather than an
// error. That is the whole point of a guard downstream of another guard —
// see GuardSpace's own doc for why the leniency stops there.
func evaluateStepGuard(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (bool, error) {
	if step.When == nil || step.When.Run == "" {
		return true, nil
	}

	spec, view, err := resolveStepRuntime(cfg, step)
	if err != nil {
		return false, err
	}

	label := executedStepName(step) + "-when"

	space, err := bw.GuardSpace(ctx, label, view.inputs, view.mapping, view.all)
	if err != nil {
		return false, fmt.Errorf("workspace: %w", err)
	}
	defer workspace.CloseSpace(space, label)

	spec.Cwd = space.Dir()
	// The guard goes wherever the step goes. resolveStepRuntime's own contract
	// is that a guard runs "in the same view the step itself gets", and a
	// guard evaluated here about a step that runs elsewhere would be reading a
	// different machine's answer. Nothing comes back: a guard is closed
	// without Capture, so it has no outputs to fetch.
	worker, err := workerFor(ctx, step)
	if err != nil {
		return false, fmt.Errorf("guard for %q: %w", label, err)
	}

	spec.Worker = worker
	spec.WorkerTag = placementTag(step)
	spec.ArtifactStore = artifactStoreFrom(ctx)
	spec.Keep = workspace.Kept(space)

	//nolint:contextcheck // NewRunner takes no context; opening the artifact store reads only local config
	runner, err := venue.NewRunner(spec)
	if err != nil {
		return false, err //nolint:wrapcheck // NewRunner's error already names the cause
	}

	defer shell.CloseRunner(runner, label)

	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, step.When.Run)
	if err != nil {
		return false, fmt.Errorf("guard command %q could not run: %w", step.When.Run, err)
	}

	// RunCaptureFull reports even a signal-killed process (e.g. from a
	// canceled ctx) as data, not err — so a guard interrupted by shutdown
	// must check ctx itself, rather than reading its exit code as a genuine
	// true/false answer the guard never actually got to give.
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return false, fmt.Errorf("guard command %q: %w", step.When.Run, cancelErr)
	}

	slog.Debug("step.when",
		"step", executedStepName(step),
		"command", step.When.Run,
		"exit_code", exitCode,
		"stdout", stdout,
		"stderr", stderr,
	)

	return exitCode == 0, nil
}

// guardInputs is the read half of the view a guard runs in, resolved the same
// way the step's own runner resolves it. The step's declared names alone are
// NOT that view: a task's inputs may come from the tasks: entry it references
// rather than from the step, input_mapping renames each one onto the plan
// artifact it actually draws from, and a put's `inputs: all` names none of
// them. Reading the declared names instead handed such a guard an empty
// directory and a silent, permanent false.
type guardInputs struct {
	inputs  []string
	mapping map[string]string
	all     bool
}

// resolveStepRuntime returns how a step's own work would execute — image,
// env:, user: — and which inputs it would see, so its guard runs in the same
// environment over the same files. A guard that needs a tool only the image
// has, or a credential only env: passes through, must find it. Each step kind
// resolves the same way its runner does: a task through ResolveTask (step
// values overriding the tasks: entry), an agent through
// ResolveAgentInvocation, and a put from its resource type (a put step may set
// none of them itself). Cwd is left to the caller, which is the only part a
// guard supplies for itself.
func resolveStepRuntime(cfg *config.Config, step config.Step) (shell.RunnerSpec, guardInputs, error) {
	kind, _ := step.Kind()

	switch kind { //nolint:exhaustive // default covers config.StepKindGet and a malformed step alike — nothing to resolve here
	case config.StepKindTask:
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return shell.RunnerSpec{}, guardInputs{}, fmt.Errorf("resolve task: %w", err)
		}

		return shell.RunnerSpec{Image: rt.Image, Env: rt.Env, User: rt.User, Network: rt.Network,
				Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()},
			guardInputs{inputs: rt.Inputs, mapping: rt.InputMapping}, nil
	case config.StepKindAgent:
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return shell.RunnerSpec{}, guardInputs{}, fmt.Errorf("resolve agent: %w", err)
		}

		// An agent step's own space is staged from the step's declared inputs
		// with no mapping (see internal/agent's prepareAgentStep), so this is
		// the whole of its view.
		return shell.RunnerSpec{Image: ri.Image, Env: ri.Env, User: ri.User, Network: ri.Network,
				Privileged: ri.Privileged, CPUShares: ri.Limits.CPUShares(), MemoryBytes: ri.Limits.MemoryBytes()},
			guardInputs{inputs: step.InputNames()}, nil
	case config.StepKindTry:
		return resolveStepRuntime(cfg, *step.Try)
	case config.StepKindPut:
		resource, err := cfg.FindResource(step.Put)
		if err != nil {
			return shell.RunnerSpec{}, guardInputs{}, fmt.Errorf("resolve put: %w", err)
		}

		resourceType, err := cfg.FindResourceType(resource.Type)
		if err != nil {
			return shell.RunnerSpec{}, guardInputs{}, fmt.Errorf("resolve put: %w", err)
		}

		return shell.RunnerSpec{Image: resourceType.Image, Env: resourceType.Env, User: resourceType.User, Network: resourceType.Network},
			guardInputs{inputs: step.InputNames(), all: step.InputsAll()}, nil
	default: // config.StepKindGet, or a malformed step — nothing to resolve here
		return shell.RunnerSpec{}, guardInputs{}, nil
	}
}
