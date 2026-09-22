package pipeline

// Recording what a placed step left on its worker (steps#138, rung 2).

import (
	"context"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// deferrable reports whether a task's outputs can stay on its worker: nothing
// in the step itself reads them here. An assert: files: reads them, and a
// fix: agent's file tools do.
func deferrable(rt config.ResolvedTask) bool {
	if rt.Fix != nil {
		return false
	}

	return rt.Assert == nil || len(rt.Assert.Files) == 0
}

// holdRemoteOutputs records each output the worker kept as an artifact held
// there, with the pull that brings it home. Outputs the worker did not keep
// came home the ordinary way and are captured as always.
func holdRemoteOutputs(ctx context.Context, bw workspace.BuildWorkspace, outputs []string, mapping map[string]string, held map[string]string, holder string) error {
	holding, ok := bw.(workspace.RemoteHolder)
	if !ok || len(held) == 0 {
		return nil
	}

	store := artifactStoreFrom(ctx)

	for _, out := range outputs {
		digest, kept := held[out]
		if !kept {
			continue
		}

		name := out
		if mapped, ok := mapping[out]; ok {
			name = mapped
		}

		err := holding.HoldRemote(name, workspace.RemoteArtifact{
			Digest: digest,
			Holder: holder,
			Pull: func(ctx context.Context, dst string) error {
				_, err := venue.Pull(ctx, shell.RunnerSpec{Worker: holder, ArtifactStore: store}, out, digest, dst)

				return err //nolint:wrapcheck // the workspace names the artifact and the holder around it
			},
		})
		if err != nil {
			return err //nolint:wrapcheck // the workspace names the artifact
		}
	}

	return nil
}

type heldSinkKey struct{}

// heldSink carries what a stage's runner kept out of the stage that closed
// it, the way placementSink carries the machine facts.
type heldSink struct {
	held   map[string]string
	holder string
}

func withHeldSink(ctx context.Context) (context.Context, *heldSink) {
	sink := &heldSink{}

	return context.WithValue(ctx, heldSinkKey{}, sink), sink
}

// heldFrom reads what the stage's worker kept, once the stage has closed its
// runner. Nothing, for a stage that ran here or kept nothing.
func heldFrom(ctx context.Context) (map[string]string, string) {
	sink, _ := ctx.Value(heldSinkKey{}).(*heldSink)
	if sink == nil {
		return nil, ""
	}

	return sink.held, sink.holder
}

// noteHeld records what a finished runner's worker kept, if a sink is
// listening.
func noteHeld(ctx context.Context, runner shell.Runner) {
	sink, _ := ctx.Value(heldSinkKey{}).(*heldSink)
	if sink == nil {
		return
	}

	held, holder, ok := venue.HeldOf(runner)
	if !ok {
		return
	}

	sink.held, sink.holder = held, holder
}
