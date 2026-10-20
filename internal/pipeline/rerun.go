package pipeline

// Rerunning a run: fly rerun-build (#146). A new run, the whole plan from the
// top, against exactly the versions the named run was created with.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
)

// PrepareRerun resolves which recorded run a new one re-runs — Run.RerunOf
// — and reports that run's job. A rerun of a rerun reruns the original, as
// Concourse's RerunBuild does, so a chain of retries never drifts from the
// versions the first build was created with.
func PrepareRerun(ctx context.Context, st runLookup, runID string) (string, string, error) {
	run, err := findRun(ctx, st, runID)
	if err != nil {
		return "", "", err
	}

	if run.Rerun() {
		runID = run.RerunOf
	}

	return runID, run.JobName, nil
}

// rerunSkipsCache: a rerun is the whole plan from the top, as Concourse's is.
// Concourse has no cache to skip, and a rerun honouring this one would do
// nothing at all for a build that passed. Safe since #145, because skipping
// the cache no longer re-opens taken versions, and a rerun takes none.
func rerunSkipsCache(opts Run) bool {
	return opts.Force || opts.RerunOf != ""
}

func rerunOf(ctx context.Context) string {
	if resume := resumeFrom(ctx); resume != nil {
		return resume.rerunOf
	}

	return ""
}

// rerunInputSets replaces the freshly resolved set with the one the re-run
// run recorded: every version it was created with. Nothing that arrived since
// joins it: Concourse's AdoptRerunInputsAndPipes copies the original's input
// rows and never runs the input algorithm, so passed: is not asked again
// either.
//
// A version history no longer holds refuses the rerun before anything runs,
// in Concourse's words.
//
// A run that recorded nothing never resolved: preflight refused it before
// planning. Its retry is the fresh resolution it never reached — and it TAKES
// what it builds, as the refused run would have, because a rerun's usual
// take-nothing would leave the version looking unbuilt and the next ordinary
// run would build it a second time. What survives of the rerun is the link
// (rerun_of) and the cache skip. Refusing it ("recorded no input versions")
// was tried first, and it refused exactly the run a person retries after
// fixing the configuration.
func rerunInputSets(ctx context.Context, st store.Versions, resolution setResolution, history *resourceHistory) (setResolution, error) {
	of := rerunOf(ctx)
	if of == "" {
		return resolution, nil
	}

	inputs, err := st.RunInputs(ctx, of)
	if err != nil {
		return setResolution{}, fmt.Errorf("could not read what run %q was created with: %w", of, err)
	}

	if len(inputs) == 0 {
		return resolution, nil
	}

	set := merkle.InputSet{}

	for _, input := range inputs {
		if !history.holds(input.Resource, input.Version) {
			return setResolution{}, fmt.Errorf("cannot rerun run %q: chosen version of input %s not available", of, input.Input)
		}

		version, err := store.DecodeVersion(input.Version)
		if err != nil {
			return setResolution{}, fmt.Errorf("cannot rerun run %q: its recorded %s version: %w", of, input.Input, err)
		}

		set[input.Input] = version
	}

	resolution.sets = []merkle.InputSet{set}
	resolution.rerun = true

	return resolution, nil
}
