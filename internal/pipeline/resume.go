package pipeline

// Resumable runs: continue a failed job from the step that failed, rather than
// from the beginning.

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// resumeState is what a run needs to know to skip what a previous attempt
// already did.
type resumeState struct {
	// id identifies this run, printed on failure so it can be resumed.
	id string
	// done maps a step index to the name it ran under, for the steps a
	// previous attempt of this run already completed.
	done map[int]string
	// resuming is true when this run continues a previous one.
	resuming bool
	// nextStepID mints display-tree ids for this run (see steptree.go).
	// Atomic because a fan-out block starts its cells concurrently.
	nextStepID atomic.Int64
}

type resumeKey struct{}

func withResume(ctx context.Context, state *resumeState) context.Context {
	return context.WithValue(ctx, resumeKey{}, state)
}

func resumeFrom(ctx context.Context) *resumeState {
	state, _ := ctx.Value(resumeKey{}).(*resumeState)

	return state
}

// alreadyDone reports whether a previous attempt of this run finished a step.
func (r *resumeState) alreadyDone(index int) (string, bool) {
	if r == nil || !r.resuming {
		return "", false
	}

	name, ok := r.done[index]

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
func PrepareResume(ctx context.Context, st *store.Store, runID string) (context.Context, string, error) {
	run, err := st.FindRun(ctx, runID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // FindRun already names the run
	}

	done, err := st.CompletedRunSteps(ctx, runID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // CompletedRunSteps already names the run
	}

	slog.Info("run.resume", "run", runID, "job", run.JobName, "completed_steps", len(done))

	return withResume(ctx, &resumeState{id: runID, done: done, resuming: true}), run.Workspace, nil
}

// ResumeJobName is the job a recorded run belongs to, so `--resume` alone
// selects the right one.
func ResumeJobName(ctx context.Context, st *store.Store, runID string) (string, error) {
	run, err := st.FindRun(ctx, runID)
	if err != nil {
		return "", err //nolint:wrapcheck // FindRun already names the run
	}

	return run.JobName, nil
}

// reportResumable prints how to continue a failed run, and where its files
// are.
//
// Both halves matter. Without the id there is nothing to resume; without the
// directory an operator cannot see the work that survived — and the files a
// step had just written when it failed are the most useful thing to look at.
func reportResumable(runID string, bw workspace.BuildWorkspace) {
	fmt.Printf("run: %s  (resume with: steps run <pipeline> --resume %s)\n", runID, runID)

	if rooted, ok := bw.(workspace.RootedBuild); ok {
		fmt.Printf("run: %s  workspace kept at %s\n", runID, rooted.Root())
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
// configSHA comes from the CONFIG this run was handed, never from the store
// handle: this write happens long after the caller took that config — past
// placement, leases, image pulls and preflight — and a daemon that reloaded
// in between would otherwise stamp a configuration this run never executed.
func recordRunIdentity(
	ctx context.Context, st *store.Store, resume *resumeState, jobName, workspaceRoot, configSHA string,
) error {
	if resume.resuming {
		err := st.ResumeRun(ctx, resume.id, workspaceRoot, configSHA)
		if err != nil {
			return fmt.Errorf("job %q: %w", jobName, err)
		}

		return nil
	}

	err := st.StartRun(ctx, resume.id, jobName, workspaceRoot, configSHA)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}
