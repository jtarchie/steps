package pipeline

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/venue"
)

// placementSinkKey types the context value carrying one step's placement
// facts.
type placementSinkKey struct{}

// placementSink is where a step leaves what its worker said about itself, for
// the enclosing step to record once the node that row references exists.
//
// It is a handoff and not a return value because the facts are only whole at
// a point nothing on the call path can see: a session dials LAZILY, so the
// workdir and filesystem arrive with the first command, and the byte count is
// final only when the runner closes. The one place that knows all of it is a
// defer inside the venue-retry closure, several frames below the caller that
// has the node hash.
//
// Last write wins, which is the answer a re-placed step wants: a step evicted
// off one machine and finished on another ran on the second, and the first
// is already reported as an eviction.
type placementSink struct {
	mu    sync.Mutex
	last  venue.Placement
	noted bool
	// watched is read when the step is recorded rather than noted by a defer: an agent's runner is built and closed inside internal/agent, where no defer of this package's can reach.
	watched shell.Runner
}

func (s *placementSink) note(placement venue.Placement) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.last, s.noted = placement, true
}

func (s *placementSink) taken() (venue.Placement, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.noted && s.watched != nil {
		return venue.PlacementOf(s.watched)
	}

	return s.last, s.noted
}

// watchPlacement leaves the runner itself, for a step whose runner outlives every frame here that could note it.
func watchPlacement(ctx context.Context, runner shell.Runner) {
	sink, _ := ctx.Value(placementSinkKey{}).(*placementSink)
	if sink == nil {
		return
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()

	sink.watched = runner
}

// withPlacementSink gives one step somewhere to leave its placement facts.
//
// Installed per STEP rather than per job: the sink is keyed by nothing, so a
// job-wide one would have parallel steps overwriting each other's machine.
func withPlacementSink(ctx context.Context) (context.Context, *placementSink) {
	sink := &placementSink{}

	return context.WithValue(ctx, placementSinkKey{}, sink), sink
}

// notePlacement records what a finished runner's worker said about itself.
//
// Deferred rather than called after the runner is built, because a session
// that has not been asked to run anything has not dialed and has nothing to
// say — and because BytesSent is only whole once the step is done with it.
func notePlacement(ctx context.Context, runner shell.Runner) {
	sink, _ := ctx.Value(placementSinkKey{}).(*placementSink)
	if sink == nil {
		return
	}

	placement, ok := venue.PlacementOf(runner)
	if !ok {
		return
	}

	sink.note(placement)
}

// placedStep names the three strings that identify a placement row, as fields so two of them cannot be swapped at a call site.
type placedStep struct{ name, slot, node string }

// recordPlacement persists where a step ran, if it ran anywhere but here.
//
// Best-effort, like the agent's usage row and for the same reason: a
// bookkeeping write must never turn a step that did its work into a failed
// one. For a step that HAS a node, called only after that node is recorded:
// the foreign key that lets retention reap the two together also means the
// row cannot be written first.
//
// slot is what identifies the row within the run: a plan step's node hash, or
// a hook's scope label. node is that hash again when the step HAS one and
// EMPTY when it does not — a hook is deliberately not merkle-hashed, so it has
// no node for retention to reap the row alongside, and cascades off its run
// instead.
func recordPlacement(ctx context.Context, runner stepRunner, sink *placementSink, index int, step placedStep) {
	if runner.st == nil {
		return
	}

	placement, ok := sink.taken()
	if !ok {
		return
	}

	runID := events.RunID(ctx)
	if runID == "" {
		return
	}

	var instance *string
	if placement.Instance != "" {
		instance = &placement.Instance
	}

	// Detached: the likeliest reason a placed step is finishing is that it was
	// cancelled or timed out, and those are precisely the runs whose machine
	// somebody wants named.
	err := runner.st.RecordPlacement(context.WithoutCancel(ctx), store.Placement{
		RunID:         runID,
		StepIndex:     index,
		StepName:      step.name,
		JobName:       runner.jobName,
		Slot:          step.slot,
		NodeHash:      step.node,
		Tag:           placement.Tag,
		Address:       placement.Address,
		InstanceID:    instance,
		GOOS:          placement.GOOS,
		GOARCH:        placement.GOARCH,
		Workdir:       placement.Workdir,
		UID:           placement.UID,
		GID:           placement.GID,
		Image:         placement.Image,
		ImageDigest:   placement.ImageDigest,
		BytesSent:     placement.BytesSent,
		BytesReceived: placement.BytesReceived,
		VolumeBytes:   placement.VolumeBytes,
	})
	if err != nil {
		slog.WarnContext(ctx, "job.placement_unrecorded", "job", runner.jobName, "step", step.name, "error", err)
	}
}
