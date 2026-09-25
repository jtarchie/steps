package pipeline

// Rerunning one build: fly rerun-build (#146). A new run, the whole plan from
// the top, against exactly the versions the named build was created with.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// rerunState names the build a run re-runs.
type rerunState struct {
	of    string
	build int
}

type rerunKey struct{}

func rerunFrom(ctx context.Context) *rerunState {
	state, _ := ctx.Value(rerunKey{}).(*rerunState)

	return state
}

// PrepareRerun points the run RunJob is about to start at one build of a
// recorded run, and reports that run's job. A rerun of a rerun reruns the
// original, as Concourse's RerunBuild does, so a chain of retries never
// drifts from the versions the first build was created with.
func PrepareRerun(ctx context.Context, st runLookup, runID string, build int) (context.Context, string, error) {
	run, err := findRun(ctx, st, runID)
	if err != nil {
		return ctx, "", err
	}

	if run.Rerun() {
		runID, build = run.RerunOf, run.RerunOfBuild
	}

	return context.WithValue(ctx, rerunKey{}, &rerunState{of: runID, build: build}), run.JobName, nil
}

// rerunSkipsCache: a rerun is the whole plan from the top, as Concourse's is.
// Concourse has no cache to skip, and a rerun honouring this one would do
// nothing at all for a build that passed. Safe since #145, because skipping
// the cache no longer re-opens taken versions, and a rerun takes none.
func rerunSkipsCache(ctx context.Context, skipCache bool) bool {
	return skipCache || rerunFrom(ctx) != nil
}

// rerunInputSets replaces the freshly resolved sets with the one build being
// re-run, bound to every version it recorded. Nothing that arrived since joins
// it: Concourse's AdoptRerunInputsAndPipes copies the original's input rows and
// never runs the input algorithm, so passed: is not asked again either.
//
// A version history no longer holds refuses the rerun before anything runs,
// in Concourse's words. A build that recorded nothing is refused unless the
// plan has no get at all, in which case there is nothing to bind.
func rerunInputSets(ctx context.Context, st store.Versions, resolution setResolution, history *resourceHistory) (setResolution, error) {
	rerun := rerunFrom(ctx)
	if rerun == nil {
		return resolution, nil
	}

	inputs, err := st.RunInputs(ctx, rerun.of)
	if err != nil {
		return setResolution{}, fmt.Errorf("could not read what run %q was created with: %w", rerun.of, err)
	}

	buildID := fmt.Sprintf("%s#%d", rerun.of, rerun.build)
	set := merkle.InputSet{}

	for _, input := range inputs {
		if input.BuildID != buildID {
			continue
		}

		if !history.holds(input.Resource, input.Version) {
			return setResolution{}, fmt.Errorf("cannot rerun build #%d of run %q: chosen version of input %s not available", rerun.build, rerun.of, input.Input)
		}

		version, err := store.DecodeVersion(input.Version)
		if err != nil {
			return setResolution{}, fmt.Errorf("cannot rerun build #%d of run %q: its recorded %s version: %w", rerun.build, rerun.of, input.Input, err)
		}

		set[input.Input] = version
	}

	if len(set) == 0 {
		if len(resolution.resources) == 0 {
			return resolution, nil
		}

		return setResolution{}, fmt.Errorf("cannot rerun build #%d of run %q: it recorded no input versions", rerun.build, rerun.of)
	}

	resolution.sets = []merkle.InputSet{set}
	resolution.rerun = true

	return resolution, nil
}
