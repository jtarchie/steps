package pipeline

// Rerunning a run: fly rerun-build (#146). A new run, the whole plan from the
// top, against exactly the versions the named run's builds were created with —
// every build, or one of them.

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// allBuilds is a rerun of every build of a run, rather than one.
const allBuilds = -1

// rerunState names the run, and the build of it or allBuilds, a run re-runs.
type rerunState struct {
	of    string
	build int
}

type rerunKey struct{}

func rerunFrom(ctx context.Context) *rerunState {
	state, _ := ctx.Value(rerunKey{}).(*rerunState)

	return state
}

// PrepareRerun points the run RunJob is about to start at a recorded run —
// one build of it, or every build when build is negative — and reports that
// run's job. A rerun of a rerun reruns the
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

	if build < 0 {
		build = allBuilds
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

// rerunInputSets replaces the freshly resolved sets with the builds being
// re-run, each bound to every version it recorded, in build order. Nothing that
// arrived since joins them: Concourse's AdoptRerunInputsAndPipes copies the
// original's input rows and never runs the input algorithm, so passed: is not
// asked again either.
//
// A version history no longer holds refuses the rerun before anything runs,
// in Concourse's words. A run that recorded nothing is refused unless the plan
// has no get at all, in which case there is nothing to bind.
func rerunInputSets(ctx context.Context, st store.Versions, resolution setResolution, history *resourceHistory) (setResolution, error) {
	rerun := rerunFrom(ctx)
	if rerun == nil {
		return resolution, nil
	}

	inputs, err := st.RunInputs(ctx, rerun.of)
	if err != nil {
		return setResolution{}, fmt.Errorf("could not read what run %q was created with: %w", rerun.of, err)
	}

	byBuild, err := rerunBuilds(rerun, inputs, history)
	if err != nil {
		return setResolution{}, err
	}

	if len(byBuild) == 0 {
		if len(resolution.resources) == 0 {
			return resolution, nil
		}

		return setResolution{}, fmt.Errorf("cannot rerun run %q: it recorded no input versions for that build", rerun.of)
	}

	builds := slices.Sorted(maps.Keys(byBuild))
	sets := make([]merkle.InputSet, 0, len(builds))

	for _, build := range builds {
		sets = append(sets, byBuild[build])
	}

	resolution.sets = sets
	resolution.rerun = true

	return resolution, nil
}

// rerunBuilds is the recorded input set of each build being re-run, by build index.
func rerunBuilds(rerun *rerunState, inputs []store.RunInput, history *resourceHistory) (map[int]merkle.InputSet, error) {
	byBuild := map[int]merkle.InputSet{}

	for _, input := range inputs {
		build, ok := buildIndex(rerun.of, input.BuildID)
		if !ok || (rerun.build != allBuilds && build != rerun.build) {
			continue
		}

		if !history.holds(input.Resource, input.Version) {
			return nil, fmt.Errorf("cannot rerun build #%d of run %q: chosen version of input %s not available", build, rerun.of, input.Input)
		}

		version, err := store.DecodeVersion(input.Version)
		if err != nil {
			return nil, fmt.Errorf("cannot rerun build #%d of run %q: its recorded %s version: %w", build, rerun.of, input.Input, err)
		}

		if byBuild[build] == nil {
			byBuild[build] = merkle.InputSet{}
		}

		byBuild[build][input.Input] = version
	}

	return byBuild, nil
}
