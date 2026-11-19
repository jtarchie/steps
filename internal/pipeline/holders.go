package pipeline

// Recording what a placed step left on its worker (steps#138, rung 2).

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// deferrable reports whether a task's outputs can stay on its worker: nothing
// in the step itself reads them here. An assert: files: reads them, a fix:
// agent's file tools do, and a collecting cell's are composed here under
// its coordinates (findings/alpha) — a consumer of findings never resolves a
// tree held under that path.
func deferrable(step config.Step, rt config.ResolvedTask) bool {
	if rt.Fix != nil || step.OutputSubdir != "" {
		return false
	}

	return rt.Assert == nil || (len(rt.Assert.Files) == 0 && len(rt.Assert.Shape) == 0)
}

// holdRemoteOutputs records each output the worker kept as an artifact held
// there, with the pull that brings it home. Outputs the worker did not keep
// came home the ordinary way and are captured as always.
func holdRemoteOutputs(ctx context.Context, bw workspace.BuildWorkspace, outputs []string, mapping map[string]string, held map[string]string, holder string) error {
	if len(held) == 0 {
		return nil
	}

	noteHolder(ctx, holder)

	for _, out := range outputs {
		digest, kept := held[out]
		if !kept {
			continue
		}

		name := out
		if mapped, ok := mapping[out]; ok {
			name = mapped
		}

		err := bw.HoldRemote(name, workspace.RemoteArtifact{Name: out, Digest: digest, Holder: holder, Pull: pullHeld(holder, digest)})
		if err != nil {
			return err //nolint:wrapcheck // the workspace names the artifact
		}
	}

	return nil
}

// pullHeld brings a tree home from the worker that holds it.
func pullHeld(holder, digest string) func(context.Context, string) error {
	return func(ctx context.Context, dst string) error {
		_, err := venue.Pull(ctx, holder, digest, dst)

		return err //nolint:wrapcheck // the workspace names the artifact and the holder around it
	}
}

// claimTimeout bounds asking a holder to keep a cached tree: a holder that is gone must cost a cache miss, not the run.
const claimTimeout = 30 * time.Second

// claimHeld pins a cached tree on its holder for this run, which then releases it as it does what its own steps held there.
func claimHeld(ctx context.Context, holder, digest string) error {
	noteHolder(ctx, holder)

	ctx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()

	return venue.Claim(ctx, holder, digest, events.RunID(ctx)) //nolint:wrapcheck // the workspace names the entry around it
}

// unpinTimeout bounds giving a finished run's pins back to one worker that held something for it.
var unpinTimeout = 30 * time.Second //nolint:gochecknoglobals // a test seam, so a hung holder costs a test seconds

type runHoldersKey struct{}

// runHolders are the workers a run held outputs on, each pinned there until the run releases it.
type runHolders struct {
	mu      sync.Mutex
	holders map[string]bool
}

func withRunHolders(ctx context.Context) (context.Context, *runHolders) {
	holders := &runHolders{holders: map[string]bool{}}

	return context.WithValue(ctx, runHoldersKey{}, holders), holders
}

func noteHolder(ctx context.Context, holder string) {
	holders, _ := ctx.Value(runHoldersKey{}).(*runHolders)
	if holders == nil {
		return
	}

	holders.mu.Lock()
	holders.holders[holder] = true
	holders.mu.Unlock()
}

// restoredPull is pullHeld for RestoreHeld, also learning each holder a previous process recorded, so a resume that succeeds releases what the failed attempt pinned.
func restoredPull(ctx context.Context) func(holder, digest string) func(context.Context, string) error {
	return func(holder, digest string) func(context.Context, string) error {
		noteHolder(ctx, holder)

		return pullHeld(holder, digest)
	}
}

// unpin releases run's pins under its own context, the run's may be ending; an unreachable holder keeps them until they expire, which costs disk, never an output. Every holder at once, each under its own bound, so a hung one neither starves the rest nor adds its wait to theirs.
func (h *runHolders) unpin(ctx context.Context, run string) {
	ctx = context.WithoutCancel(ctx)

	h.mu.Lock()
	defer h.mu.Unlock()

	var wg sync.WaitGroup

	// ponytail: a holder whose every output a failed attempt already pulled home is not in held.json, so a resume misses it and its pins wait out pinLifetime; recording holders apart from holds closes that.
	for holder := range h.holders {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, unpinTimeout)
			defer cancel()

			err := venue.Unpin(ctx, holder, run)
			if err != nil {
				slog.WarnContext(ctx, "venue.unpin_failed", "run", run, "error", err)
			}
		})
	}

	wg.Wait()
}

// onWorker reports whether a step with image runs on a docker worker, tagged or the implicit local: one, and so can be handed inputs a worker holds; the rest run here and read every input from this disk.
func onWorker(step config.Step, image string) bool {
	return placementTag(step) != "" || image != ""
}

// placedTaskSpace is the step space for a task, leaving inputs on their
// holders whenever the step is placed — the venue moves them through the
// store when one is configured and pipes them through this machine when not;
// otherwise the ordinary space, which pulls what it needs here.
func placedTaskSpace(ctx context.Context, bw workspace.BuildWorkspace, step config.Step, rt config.ResolvedTask, outputMapping map[string]string) (workspace.StepSpace, map[string]shell.RemoteInput, error) {
	if !onWorker(step, rt.Image) {
		space, err := bw.TaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs, rt.InputMapping, outputMapping)

		return space, nil, err //nolint:wrapcheck // the caller names the task
	}

	// Only an image: task reads its inputs through volumes alone: a fix: agent, an assert: files: check or a tagged host command read this machine's copy.
	lazy := rt.Image != "" && deferrable(step, rt)

	space, remote, err := bw.PlacedTaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs, rt.InputMapping, outputMapping, lazy)

	return space, remoteInputs(remote), err
}

// runnerImage is the image a resource type's stages run in, or empty for a backend that builds no runner: an mcp: out reads its files from this disk whatever image: says.
func runnerImage(rt config.ResourceType) string {
	switch rt.Config.Backend() {
	case config.BackendShell, config.BackendConcourse:
		return rt.Image
	case config.BackendMCP, config.BackendExpr, config.BackendWebhook, config.BackendCron, config.BackendGitHub, config.BackendLinear, config.BackendRegistry, config.BackendService:
	}

	return ""
}

// placedPutSpace is placedTaskSpace for a put.
func placedPutSpace(ctx context.Context, bw workspace.BuildWorkspace, step config.Step, resourceType config.ResourceType) (workspace.StepSpace, map[string]shell.RemoteInput, error) {
	if !onWorker(step, runnerImage(resourceType)) {
		space, err := bw.PutSpace(ctx, step.Put, step.InputNames(), step.InputsAll())

		return space, nil, err //nolint:wrapcheck // the caller names the put
	}

	space, remote, err := bw.PlacedPutSpace(ctx, step.Put, step.InputNames(), step.InputsAll())

	return space, remoteInputs(remote), err
}

func remoteInputs(remote map[string]workspace.RemoteArtifact) map[string]shell.RemoteInput {
	if len(remote) == 0 {
		return nil
	}

	inputs := make(map[string]shell.RemoteInput, len(remote))
	for name, held := range remote {
		inputs[name] = shell.RemoteInput{Digest: held.Digest, Holder: held.Holder}
	}

	return inputs
}

type remoteInputsKey struct{}

// withRemoteInputs carries a put's remote inputs to the placer that builds
// its runner, which is the one place the spec is assembled for a resource
// stage.
func withRemoteInputs(ctx context.Context, inputs map[string]shell.RemoteInput) context.Context {
	if len(inputs) == 0 {
		return ctx
	}

	return context.WithValue(ctx, remoteInputsKey{}, inputs)
}

func remoteInputsFrom(ctx context.Context) map[string]shell.RemoteInput {
	inputs, _ := ctx.Value(remoteInputsKey{}).(map[string]shell.RemoteInput)

	return inputs
}

type heldSinkKey struct{}

// heldSink carries what a stage's runner kept out of the stage that closed
// it, the way placementSink carries the machine facts.
type heldSink struct {
	held   map[string]string
	holder string
	// as is the artifact name a get's tree is held under, for a stage whose runner is built without the step.
	as string
}

func withHeldSink(ctx context.Context, as string) (context.Context, *heldSink) {
	sink := &heldSink{as: as}

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

// attachTaskCaches puts a step's caches: in its directory. Not for a placed or image: step: its worker (the implicit local: one for an untagged image) holds the cache, and a local one attached here would be packed into the tree sent over.
func attachTaskCaches(ctx context.Context, bw workspace.BuildWorkspace, step config.Step, space workspace.StepSpace, rt config.ResolvedTask) func() {
	if len(rt.Caches) == 0 || placementTag(step) != "" || rt.Image != "" {
		return func() {}
	}

	return bw.AttachCaches(ctx, space.Dir(), taskCacheKeys(ctx, step.Task, rt.Caches))
}

func taskCacheKeys(ctx context.Context, name string, caches []config.TaskCache) map[string]string {
	meta := shell.MetadataFrom(ctx)

	return workspace.TaskCacheKeys(meta.PipelineName, meta.JobName, name, caches)
}

// inputDigests is the space's view of the build's digest memo; a space without one (a test double) walks every input.
func inputDigests(space workspace.StepSpace) shell.InputDigests {
	digests, _ := space.(shell.InputDigests)

	return digests
}

// heldAs is the name the stage's sink holds a get's tree under; empty outside a stage, which holds nothing.
func heldAs(ctx context.Context) string {
	sink, _ := ctx.Value(heldSinkKey{}).(*heldSink)
	if sink == nil {
		return ""
	}

	return sink.as
}
