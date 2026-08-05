package pipeline

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
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
// inputs. In the default (no workspace:) mode TaskSpace is a passthrough to
// the build root, so this costs nothing; under workspace: isolation it
// materializes the inputs and is closed WITHOUT Capture, so a guard can never
// publish artifacts — a discarded copy is the price of a guard that can read
// what the step reads.
func evaluateStepGuard(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (bool, error) {
	if step.When == nil || step.When.Run == "" {
		return true, nil
	}

	image, err := resolveStepImage(cfg, step)
	if err != nil {
		return false, err
	}

	label := executedStepName(step) + "-when"

	space, err := bw.TaskSpace(ctx, label, step.InputNames(), nil, nil, nil)
	if err != nil {
		return false, fmt.Errorf("workspace: %w", err)
	}
	defer workspace.CloseSpace(space, label)

	runner, err := shell.NewRunner(image, space.Dir())
	if err != nil {
		return false, err //nolint:wrapcheck // NewRunner's error already names the cause
	}

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

// resolveStepImage returns the image a step's own work would execute under, so
// its guard runs in the same environment — a guard that needs a tool only the
// image has must find it. Each step kind resolves its image the same way its
// runner does: a task through ResolveTask (step image overriding the tasks:
// entry), an agent through ResolveAgentInvocation, and a put from its resource
// type (a put step may not set image: itself).
func resolveStepImage(cfg *config.Config, step config.Step) (string, error) {
	kind, _ := step.Kind()

	return resolveStepImageByKind(cfg, step, kind)
}

func resolveStepImageByKind(cfg *config.Config, step config.Step, kind config.StepKind) (string, error) {
	switch kind { //nolint:exhaustive // default covers config.StepKindGet and a malformed step alike — no image to resolve here
	case config.StepKindTask:
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			return "", fmt.Errorf("resolve task: %w", err)
		}

		return rt.Image, nil
	case config.StepKindAgent:
		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			return "", fmt.Errorf("resolve agent: %w", err)
		}

		return ri.Image, nil
	case config.StepKindTry:
		return resolveStepImage(cfg, *step.Try)
	case config.StepKindPut:
		resource, err := cfg.FindResource(step.Put)
		if err != nil {
			return "", fmt.Errorf("resolve put: %w", err)
		}

		resourceType, err := cfg.FindResourceType(resource.Type)
		if err != nil {
			return "", fmt.Errorf("resolve put: %w", err)
		}

		return resourceType.Image, nil
	default: // config.StepKindGet, or a malformed step — no image to resolve here
		return "", nil
	}
}
