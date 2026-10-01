package pipeline

// One run is one build. A `version: every` job with several versions waiting
// builds the oldest and queues itself again, which is Concourse's scheduler:
// one build per job per pass, the next pass taking the next version. Holding
// every version's build in one run made each thing that counts builds —
// max_in_flight, serial:, the job's history, a retry — count the wrong thing.

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// cursorLocks serializes, per pipeline and job, the read of what a job has
// taken and the take itself. Two claims of one job under max_in_flight > 1
// otherwise both resolve the same oldest version before either records it.
// In-process is enough: one daemon per state database is the deployment rule.
var cursorLocks sync.Map //nolint:gochecknoglobals // per-process lock table; see above

// resolvedSets runs inside that critical section, after a run has resolved
// its sets and before it takes one. A test seam: a run that could be joined
// there by a sibling is the race the lock exists to close.
var resolvedSets = func() {} //nolint:gochecknoglobals // test seam; see above

func lockJobCursor(pipelineName, jobName string) func() {
	mu, _ := cursorLocks.LoadOrStore(pipelineName+"\x00"+jobName, &sync.Mutex{})
	lock, _ := mu.(*sync.Mutex)
	lock.Lock()

	return lock.Unlock
}

type backlogKey struct{}

// WithBacklog is told, as a run takes its version, how many input sets are
// still waiting behind it. The daemon queues the job again; a caller that
// installs nothing (`steps run`) builds one version per invocation, as
// `fly trigger-job` does. Told at the take rather than at the finish, so the
// next build can start beside this one when max_in_flight allows it.
func WithBacklog(ctx context.Context, waiting func(ctx context.Context, sets int)) context.Context {
	return context.WithValue(ctx, backlogKey{}, waiting)
}

type setIndexKey struct{}

// withSetIndex makes a run build the index-th waiting set instead of the
// oldest. Only `steps test` asks, through BuildEveryVersion: it re-opens taken
// versions, so every run resolves the same sets and only the index moves.
func withSetIndex(ctx context.Context, index int) context.Context {
	return context.WithValue(ctx, setIndexKey{}, index)
}

// oneBuild narrows a fresh resolution to the set this run builds, reporting
// how many wait behind it. A resumed or rerun resolution is left alone: its
// sets are the record of a run already created.
func oneBuild(ctx context.Context, resolution setResolution) (setResolution, int) {
	index, _ := ctx.Value(setIndexKey{}).(int)

	if index >= len(resolution.sets) {
		resolution.sets = nil

		return resolution, 0
	}

	waiting := len(resolution.sets) - index - 1
	resolution.sets = resolution.sets[index : index+1]

	return resolution, waiting
}

// takeBuild records what this run's build was created with — EVERY get's
// version, fixed gets and pins included, which is what a resume or a rerun
// rebuilds it against (fly rerun-build's build_resource_config_version_inputs)
// — and advances each fanning get's cursor past it.
//
// Taken as the build is CREATED, whatever it then does: NextEveryVersion reads
// the versions a build was created with and filters on status nowhere, so a
// version a failed build took stays taken (concourse/concourse#413). Taking
// only on success was tried and reverted — a version that fails forever then
// re-runs forever, on every trigger, with an agent's bill attached.
//
// A resumed run takes nothing (it took when it was created), nor does a rerun
// (the original did). A pinned run records and does not take: the cursor is a
// high-water mark over discovery order, and a pin resolved outside history is
// minted at the TOP order, so taking it would leap the mark over every
// unbuilt version below it.
func takeBuild(
	ctx context.Context, st store.Store, jobName string, cursor *versionCursor,
	resolution setResolution, pinnedRun bool, waiting int,
) {
	if resolution.recorded || len(resolution.sets) == 0 {
		return
	}

	set := resolution.sets[0]

	for input, version := range set {
		recordRunInput(ctx, st, input, resolution.resources[input], version)
	}

	if pinnedRun || resolution.rerun {
		return
	}

	for _, every := range resolution.everyInputs {
		if version := set[every.input]; version != nil {
			cursor.take(ctx, st, jobName, every.resource, version)
		}
	}

	if next, ok := ctx.Value(backlogKey{}).(func(context.Context, int)); ok && waiting > 0 {
		next(context.WithoutCancel(ctx), waiting)
	}
}

// BuildEveryVersion runs job once per waiting input set, each its own run,
// and judges the job's assert: over all of them — which is the only shape a
// fixture can describe, since it cannot know how a backlog is split into
// runs. For `steps test`, whose cursor never filters, so walking the backlog
// one claim at a time would build the oldest version forever.
func BuildEveryVersion(ctx context.Context, job *config.Job, run func(context.Context) error) error {
	builds := &jobBuilds{}
	ctx = context.WithValue(ctx, jobBuildsKey{}, builds)

	var errs []error

	for index := 0; ; index++ {
		waiting := 0

		runCtx := WithBacklog(withSetIndex(ctx, index), func(_ context.Context, sets int) { waiting = sets })

		err := run(runCtx)
		if err != nil {
			errs = append(errs, err)
		}

		if waiting == 0 {
			break
		}
	}

	return checkJobAssert(job, &builds.log, errors.Join(errs...))
}

type jobBuildsKey struct{}

// jobBuilds is what BuildEveryVersion collects across a job's runs.
type jobBuilds struct {
	log execLog
}

// deferJobAssert hands a run's execution log to the BuildEveryVersion
// judging it, reporting whether one is: the run then carries its plan's own
// outcome rather than an assertion over a fraction of what the job did.
func deferJobAssert(ctx context.Context, log *execLog) bool {
	builds, ok := ctx.Value(jobBuildsKey{}).(*jobBuilds)
	if !ok {
		return false
	}

	builds.log.names = append(builds.log.names, log.snapshot()...)

	return true
}

// recordRunInput files the version this build was created with, so --resume
// can reach it after the cursor has moved past it.
//
// Best-effort, and detached for the same reason take is: a build already under
// way must not fail over bookkeeping, and the cost of a lost row is a resume
// that cannot re-open one version.
func recordRunInput(ctx context.Context, st store.Versions, inputName, resourceName string, version map[string]any) {
	key, ok := encodeVersion(version)
	if !ok {
		return
	}

	err := st.RecordRunInput(context.WithoutCancel(ctx), events.RunID(ctx), inputName, resourceName, key)
	if err != nil {
		slog.WarnContext(ctx, "job.run_input_unrecorded", "get", inputName, "resource", resourceName, "error", err)
	}
}
