package pipeline

// Resumable runs: continue a failed job from the step that failed, rather than
// from the beginning.

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// resumeState is what a run needs to know to skip what a previous attempt
// already did.
type resumeState struct {
	// id identifies this run, printed on failure so it can be resumed.
	id string
	// done names the steps a previous attempt of this run already
	// completed, by the build they ran in and their index within it.
	done map[doneKey]string
	// resuming is true when this run continues a previous one.
	resuming bool
	// nextStepID mints display-tree ids for this run (see steptree.go).
	// Atomic because a fan-out block starts its cells concurrently.
	nextStepID atomic.Int64
}

type resumeKey struct{}

// doneKey is a step's position: an index is relative to the walk it ran in,
// and every triggered build's remainder counts from 0, so it names a step only
// together with its build.
type doneKey struct {
	build string
	index int
}

// foldRunSteps indexes a run's recorded steps for alreadyDone.
func foldRunSteps(steps []store.RunStep) map[doneKey]string {
	done := make(map[doneKey]string, len(steps))
	for _, step := range steps {
		done[doneKey{step.BuildID, step.Index}] = step.Name
	}

	return done
}

func withResume(ctx context.Context, state *resumeState) context.Context {
	return context.WithValue(ctx, resumeKey{}, state)
}

func resumeFrom(ctx context.Context) *resumeState {
	state, _ := ctx.Value(resumeKey{}).(*resumeState)

	return state
}

// alreadyDone reports whether a previous attempt of this run finished a step
// of one build.
func (r *resumeState) alreadyDone(build string, index int) (string, bool) {
	if r == nil || !r.resuming {
		return "", false
	}

	name, ok := r.done[doneKey{build, index}]

	return name, ok
}

// runIDChars is how much of a crypto/rand base32 string a run id keeps.
//
// It was 8, which is 40 bits, under a comment claiming runs "never" collide —
// a probability argument stated as a guarantee. At 40 bits the birthday bound
// is about 1% at 149,000 retained runs and 12% at 525,600, which is a
// one-minute poll for a year, and `run_history: 0` (no limit) is a documented
// setting. 16 chars is 80 bits, where the same numbers are unreachable.
//
// Widening is not what makes this safe — StartRun refusing an id some run
// already holds is. This only makes the refusal something nobody ever sees.
const runIDChars = 16

// NewRunID mints an identifier for a run.
//
// Random rather than sequential so two runs of the same job — including
// concurrent ones under `steps web` — do not have to coordinate to differ.
// A collision is not prevented here and is not claimed to be: it is refused
// by StartRun, which inserts rather than upserts precisely so that an
// improbable event is an error instead of a silent takeover.
func NewRunID() string {
	return rand.Text()[:runIDChars]
}

// WithNewRun fixes the id RunJob gives the run it starts, so a caller can address that run — to abort it — before it exists.
func WithNewRun(ctx context.Context, id string) context.Context {
	return withResume(ctx, &resumeState{id: id, done: map[doneKey]string{}})
}

// runLookup is a run read that can also name the pipeline it read: Meta beside
// Runs, because a run id is globally unique while the lookup is scoped, so the
// pipeline is the fact that explains a miss ("no run X in pipeline Y" versus
// "no run X anywhere", which are different bugs to chase).
type runLookup interface {
	store.Meta
	store.Runs
}

// PrepareResume loads a previous run so this one can continue it, and reports
// the workspace to reuse.
//
// Resuming is NOT the merkle cache. The cache asks "has this content succeeded
// before", which is deliberately never true for a put or an agent — those have
// side effects and are non-deterministic. Resuming asks something narrower and
// answerable for exactly those steps: "did THIS run already do this one". That
// distinction is the whole feature. An agent step is not repeatable, so
// re-running it does not reproduce the reviewed output — it produces a
// different one, which makes a restart lossy as well as expensive.
func PrepareResume(ctx context.Context, st runLookup, runID string) (context.Context, string, error) {
	run, err := findRun(ctx, st, runID)
	if err != nil {
		return ctx, "", err
	}

	done, err := st.CompletedRunSteps(ctx, runID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // CompletedRunSteps already names the run
	}

	slog.Info("run.resume", "run", runID, "job", run.JobName, "completed_steps", len(done))

	return withResume(ctx, &resumeState{id: runID, done: foldRunSteps(done), resuming: true}), run.Workspace, nil
}

// ResumeJobName is the job a recorded run belongs to, so `--resume` alone
// selects the right one.
func ResumeJobName(ctx context.Context, st runLookup, runID string) (string, error) {
	run, err := findRun(ctx, st, runID)
	if err != nil {
		return "", err
	}

	return run.JobName, nil
}

// reportResumable prints how to continue a failed run, and where its files
// are.
//
// Both halves matter. Without the id there is nothing to resume; without the
// directory an operator cannot see the work that survived — and the files a
// step had just written when it failed are the most useful thing to look at.
func reportResumable(ctx context.Context, runID string, bw workspace.BuildWorkspace) {
	notef(ctx, "run: %s  (resume with: steps run <pipeline> --resume %s)", runID, runID)

	if rooted, ok := bw.(workspace.RootedBuild); ok {
		notef(ctx, "run: %s  workspace kept at %s", runID, rooted.Root())
	}
}

// forceKey types the context value carrying --force.
type forceKey struct{}

// withForce records that this run was asked to re-run everything.
//
// RunJob's skipCache only bypasses the chain-skip planner, which is enough for
// every step that consults `skippable`. An across: cell does not — it asks the
// store about its own node hash directly, which is what gives a matrix
// per-cell caching — so it needs the flag itself, or `--force` and `steps
// test` would print `skip: <cell> (unchanged)` for every cell and evaluate
// none of their asserts.
func withForce(ctx context.Context, force bool) context.Context {
	if !force {
		return ctx
	}

	return context.WithValue(ctx, forceKey{}, true)
}

// forced reports whether this run must re-run everything.
func forced(ctx context.Context) bool {
	force, _ := ctx.Value(forceKey{}).(bool)

	return force
}

// reopenTakenKey types the context value carrying "re-open taken versions".
type reopenTakenKey struct{}

// WithTakenVersionsReopened makes a run's `version: every` cursor stop
// filtering, so versions an earlier run already took are fanned out again.
// Only `steps test` asks: its execution assertions must hold on every rerun
// against one state file. --force does not imply it (#145) — replaying a
// resource's history repeats effects the cache never skips.
func WithTakenVersionsReopened(ctx context.Context) context.Context {
	return context.WithValue(ctx, reopenTakenKey{}, true)
}

func takenVersionsReopened(ctx context.Context) bool {
	reopened, _ := ctx.Value(reopenTakenKey{}).(bool)

	return reopened
}

// recordRunIdentity writes the row every event, history entry and resume of
// this run keys on — minting it, or putting an existing one back in flight.
//
// The two are different acts and are no longer one upsert. The error is
// RETURNED rather than logged, which is the point: a mint that collides with
// an id some run already holds used to take that row over silently, and a
// bookkeeping write nobody checks is exactly how that stayed invisible. A run
// that cannot establish its own identity has nowhere to record what it does,
// so there is nothing useful for it to go on and do.
//
// The revision comes from the CONFIG this run was handed, never from the store
// handle: this write happens long after the caller took that config — past
// placement, leases, image pulls and preflight — and a daemon that reloaded
// in between would otherwise stamp a configuration this run never executed.
//
// It is re-interned here rather than trusted to still be in the table, and for
// the same window: a reload adopting a newer configuration sweeps every
// revision no run references yet, which is exactly what this one is until the
// line below runs. Without this the row resolves to NULL and the run that was
// executing across the edit — the one whose configuration anybody would want —
// is the only one that records none. The upsert is idempotent, so the common
// case pays one statement against a row that is already there.
func recordRunIdentity(
	ctx context.Context, st store.Store, resume *resumeState, jobName, workspaceRoot string, revision config.Revision,
) error {
	if revision.Recorded() {
		err := st.RecordRevision(ctx, revision.SHA, revision.Source, revision.Includes)
		if err != nil {
			return fmt.Errorf("job %q: %w", jobName, err)
		}
	}

	if resume.resuming {
		err := st.ResumeRun(ctx, resume.id, workspaceRoot, revision.SHA)
		if err != nil {
			return fmt.Errorf("job %q: %w", jobName, err)
		}

		return nil
	}

	err := st.StartRun(ctx, resume.id, jobName, workspaceRoot, revision.SHA)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	if rerun := rerunFrom(ctx); rerun != nil {
		err = st.RecordRunRerun(ctx, resume.id, rerun.of, rerun.build)
		if err != nil {
			return fmt.Errorf("job %q: %w", jobName, err)
		}
	}

	return nil
}

// resumeInputSets replaces a resume's freshly resolved sets with the builds
// the run was created with, each binding every get to the version its record
// holds — fixed gets and pins included. This is fly rerun-build's
// AdoptRerunInputsAndPipes: a rerun copies every row of the original build's
// build_resource_config_version_inputs and starts from them, never from what
// the check reports now. It is also what makes a build's completed steps
// trustworthy — they are filed under "<run>#<set>", and are that build's only
// if set n binds what build n was created with.
//
// A recorded version no longer in the resource's history — version_history:
// pruned it — leaves that build with nothing to rebuild; Concourse marks the
// rerun aborted ("chosen version of input X not available") rather than
// choose another, and so does this, before anything runs. A build that
// completed steps but recorded no inputs is refused the same way: its record
// cannot be placed, so nothing may be skipped on its behalf. A run that
// failed before it created any build has no record and resolves as a new run
// would, taking and recording as it goes.
//
// The read is NOT best-effort, unlike the write that fills the table: a resume
// that cannot tell which versions it is continuing would silently select the
// wrong ones — or none, which is the false green this whole path exists to
// remove.
func resumeInputSets(ctx context.Context, st store.Versions, resolution setResolution, history *resourceHistory) (setResolution, error) {
	state := resumeFrom(ctx)
	if state == nil || !state.resuming {
		return resolution, nil
	}

	inputs, err := st.RunInputs(ctx, state.id)
	if err != nil {
		return setResolution{}, fmt.Errorf("could not read what run %q was created with: %w", state.id, err)
	}

	sets, err := recordedInputSets(ctx, state.id, inputs, history)
	if err != nil {
		return setResolution{}, err
	}

	err = refuseUnrecordedBuilds(state, len(sets))
	if err != nil {
		return setResolution{}, err
	}

	if len(sets) == 0 {
		return resolution, nil
	}

	resolution.sets = sets
	resolution.recorded = true

	return resolution, nil
}

// recordedInputSets rebuilds the run's builds from their records, in build
// order, stopping at the first index nothing recorded.
func recordedInputSets(ctx context.Context, runID string, inputs []store.RunInput, history *resourceHistory) ([]merkle.InputSet, error) {
	recorded := map[string][]store.RunInput{}
	for _, input := range inputs {
		recorded[input.BuildID] = append(recorded[input.BuildID], input)
	}

	var sets []merkle.InputSet

	for i := 0; ; i++ {
		bindings, ok := recorded[buildIDForSet(ctx, i)]
		if !ok {
			return sets, nil
		}

		set, err := recordedInputSet(runID, i, bindings, history)
		if err != nil {
			return nil, err
		}

		sets = append(sets, set)
	}
}

// refuseUnrecordedBuilds refuses a resume when a build completed steps that no
// rebuilt set could place — the steps before the first get are the bare run
// id's and bind nothing, so they are not the question.
func refuseUnrecordedBuilds(state *resumeState, rebuilt int) error {
	for key := range state.done {
		if n, ok := buildIndex(state.id, key.build); ok && n >= rebuilt {
			return fmt.Errorf(
				"cannot resume run %q: build #%d completed steps but what it was created with was not recorded — start a new run",
				state.id, n)
		}
	}

	return nil
}

// recordedInputSet rebuilds one build's set from its record, refusing a
// version the resource's history no longer holds.
func recordedInputSet(runID string, index int, bindings []store.RunInput, history *resourceHistory) (merkle.InputSet, error) {
	set := merkle.InputSet{}

	for _, binding := range bindings {
		if !history.holds(binding.Resource, binding.Version) {
			return nil, fmt.Errorf(
				"cannot resume run %q: build #%d was created with %s %s, which is no longer in the resource's history — start a new run",
				runID, index, binding.Resource, binding.Version)
		}

		version, err := store.DecodeVersion(binding.Version)
		if err != nil {
			return nil, fmt.Errorf("cannot resume run %q: build #%d's recorded %s version: %w", runID, index, binding.Resource, err)
		}

		set[binding.Input] = version
	}

	return set, nil
}

// buildIndex reads the set index out of a build id buildIDForSet named, and
// reports false for the bare run id — the steps before the first get, which
// bind no version and belong to no set.
func buildIndex(runID, buildID string) (int, bool) {
	rest, ok := strings.CutPrefix(buildID, runID+"#")
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(rest)

	return n, err == nil
}

// findRun reads the run a --resume or --replay names, turning "this pipeline
// does not have it" into the error the operator needs to see.
//
// The store reports absence as ok=false rather than an error because most of
// its callers render "no such run" as a page; the three callers here are all
// a command that cannot proceed, and the pipeline is named because run ids are
// globally unique while the lookup is scoped — asking the wrong pipeline of a
// shared state file is the way this fails.
func findRun(ctx context.Context, st runLookup, runID string) (store.RunRow, error) {
	run, ok, err := st.FindRunRow(ctx, runID)
	if err != nil {
		return store.RunRow{}, err //nolint:wrapcheck // the store names the run
	}

	if !ok {
		return store.RunRow{}, fmt.Errorf("no run %q was recorded for pipeline %q", runID, st.Pipeline())
	}

	return run, nil
}
