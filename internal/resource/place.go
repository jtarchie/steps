package resource

// Where a resource's commands run.
//
// This package may not know about workers: the venue tier sits above shell
// and below pipeline, and a resource is a leaf that speaks shell.Runner. So
// the decision arrives from above, on the context, as a function that builds
// the runner — nil meaning this machine, exactly as it always was.

import (
	"context"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// Placer builds the runner one resource stage runs on. The stage fills in
// what it knows — image, tree, env, what to fetch back — and the placer adds
// the machine.
type Placer func(shell.RunnerSpec) (shell.Runner, error)

// PlacerFor answers the placer for one get or put step, or nil for a step
// that runs here. The step carries the tag; the caller maps it to a machine.
type PlacerFor func(ctx context.Context, step config.Step) (Placer, error)

type (
	placerForKey struct{}
	placerKey    struct{}
)

// WithPlacerFor installs how every resource stage under ctx finds its worker.
func WithPlacerFor(ctx context.Context, fn PlacerFor) context.Context {
	return context.WithValue(ctx, placerForKey{}, fn)
}

// Place resolves the placer for step and installs it for the stage calls that
// follow. Without a PlacerFor, or for an untagged step, ctx comes back as it
// was and the stage runs here.
func Place(ctx context.Context, step config.Step) (context.Context, error) {
	fn, _ := ctx.Value(placerForKey{}).(PlacerFor)
	if fn == nil {
		return ctx, nil
	}

	placer, err := fn(ctx, step)
	if err != nil {
		return ctx, err
	}

	if placer == nil {
		return context.WithValue(ctx, placerKey{}, Placer(nil)), nil
	}

	return context.WithValue(ctx, placerKey{}, placer), nil
}

// newRunner builds a stage's runner wherever ctx says it runs.
func newRunner(ctx context.Context, spec shell.RunnerSpec) (shell.Runner, error) {
	placer, _ := ctx.Value(placerKey{}).(Placer)
	if placer == nil {
		return shell.NewRunner(spec) //nolint:wrapcheck // the local path is shell's answer, returned as shell phrased it
	}

	return placer(spec)
}
