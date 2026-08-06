package pipeline

// Resumable runs: continue a failed job from the step that failed, rather than
// from the beginning.

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"

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

// NewRunID mints an identifier for a run. Random rather than sequential so two
// runs of the same job — including concurrent ones under `steps watch` — never
// collide on it.
func NewRunID() string {
	return rand.Text()[:8]
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
