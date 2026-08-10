package pipeline

// Replay: re-run one step of a recorded run against the state that run had
// reached, instead of paying for the whole plan to get back there.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// PrepareReplay forks a recorded run so this invocation re-executes it from
// one step onward, and reports the workspace to run in.
//
// Why this is not the merkle cache, and not resume either. The cache asks "has
// this content succeeded before", which is never true for an agent step. Resume
// asks "did THIS run already do this step", and only ever continues from where
// a run FAILED. Replay asks a third thing — "run this step again against the
// state the run had reached when it got here" — which is the question you have
// while tuning a prompt, and the one that costs a full plan to answer today.
//
// It does not consult the cache at all. State is restored from three places,
// none of which is content-addressed: the source run's workspace (every
// artifact a get fetched and every file a task wrote is already on disk), its
// recorded run_context, and its step record. A step before the replay point
// does not re-execute because the run record says it ran, not because a hash
// matched — which is why this works even though agent steps are unskippable.
//
// It FORKS rather than re-entering the source run. History stays immutable, so
// the run being compared against is still there — and two prompt variants
// become two runs, side by side, which is the entire point of being able to do
// this cheaply.
func PrepareReplay(
	ctx context.Context, st *store.Store, provider workspace.Provider,
	sourceRunID, fromStep string, job *config.Job,
) (context.Context, string, error) {
	source, err := st.FindRun(ctx, sourceRunID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // FindRun already names the run
	}

	if source.Workspace == "" {
		return ctx, "", fmt.Errorf("run %q recorded no workspace, so there is nothing to replay from", sourceRunID)
	}

	_, err = os.Stat(source.Workspace)
	if err != nil {
		// The most likely reason by far, and the one worth naming: a replay is
		// only as good as the tree it forks, and nothing keeps that tree
		// unless the run was told to.
		return ctx, "", fmt.Errorf(
			"run %q left no workspace at %s — a replay forks the files that run produced, so it needs one kept (--keep-workspace)",
			sourceRunID, source.Workspace)
	}

	from, err := replayIndex(job, fromStep)
	if err != nil {
		return ctx, "", err
	}

	done, err := replayDoneSteps(ctx, st, sourceRunID, job, from)
	if err != nil {
		return ctx, "", err
	}

	replayID := NewRunID()

	dir, err := forkWorkspace(ctx, provider, source.Workspace, replayID)
	if err != nil {
		return ctx, "", err
	}

	err = st.CopyRunContext(ctx, sourceRunID, replayID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // CopyRunContext already names the run
	}

	err = st.StartRun(ctx, replayID, job.Name, dir)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // StartRun already names the run
	}

	err = st.RecordRunParent(ctx, replayID, sourceRunID)
	if err != nil {
		return ctx, "", err //nolint:wrapcheck // RecordRunParent already names the run
	}

	fmt.Printf("replay: %s from %q (forked from %s, %d step(s) restored)\n", replayID, fromStep, sourceRunID, len(done))
	slog.Info("run.replay", "run", replayID, "parent", sourceRunID, "job", job.Name, "from", fromStep, "restored_steps", len(done))

	return withResume(ctx, &resumeState{id: replayID, done: done, resuming: true}), dir, nil
}

// replayIndex resolves --from to a plan position.
//
// By NAME, not by index, and against the CURRENT plan: the pipeline file has
// almost certainly changed since the source run — that is why someone is
// replaying — so a recorded index would point at whatever now sits there.
func replayIndex(job *config.Job, fromStep string) (int, error) {
	var names []string

	for i := range job.Plan {
		name := executedStepName(job.Plan[i])
		if name == fromStep {
			return i, nil
		}

		if name != "" {
			names = append(names, name)
		}
	}

	return 0, fmt.Errorf("job %q has no step named %q to replay from (it has: %s)",
		job.Name, fromStep, strings.Join(names, ", "))
}

// replayDoneSteps marks every step before the replay point as already
// finished, so the plan walker skips straight to it.
//
// Each one is checked against what the source run actually recorded. A step the
// source never completed cannot be restored — its outputs are not in the forked
// workspace and its facts are not in the copied context — so replaying past it
// would run the target step against state that never existed. Refusing names
// the step rather than producing a confidently wrong run.
func replayDoneSteps(
	ctx context.Context, st *store.Store, sourceRunID string, job *config.Job, from int,
) (map[int]string, error) {
	recorded, err := st.CompletedRunSteps(ctx, sourceRunID)
	if err != nil {
		return nil, err //nolint:wrapcheck // CompletedRunSteps already names the run
	}

	done := make(map[int]string, from)

	for i := range from {
		name := executedStepName(job.Plan[i])

		_, ok := recorded[i]
		if !ok {
			return nil, fmt.Errorf(
				"run %q never completed step %d (%q), so a replay from a later step would run against state that run never reached",
				sourceRunID, i, name)
		}

		done[i] = name
	}

	return done, nil
}

// forkWorkspace copies the source run's tree so the replay cannot disturb it.
//
// A copy rather than reuse, because the source run is the thing being compared
// against: a replay that edited it in place would destroy the baseline in the
// act of measuring against it.
func forkWorkspace(ctx context.Context, provider workspace.Provider, source, replayID string) (string, error) {
	resumable, ok := provider.(workspace.Resumable)
	if !ok {
		return "", errors.New("--replay is not supported by this workspace provider")
	}

	dir := filepath.Join(filepath.Dir(source), "replay-"+replayID)

	err := workspace.CopyTree(ctx, source, dir)
	if err != nil {
		return "", fmt.Errorf("could not fork the workspace of the replayed run: %w", err)
	}

	resumable.Reuse(dir)

	return dir, nil
}
