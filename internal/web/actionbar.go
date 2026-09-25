package web

import (
	"context"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/store"
)

// barView is what the page's own job or run can have done to it, drawn in the
// action bar beside the pipeline's pause. A page that sets none gets the pause
// alone.
type barView struct {
	// Job is the job Trigger starts a new run of; empty offers no Trigger.
	Job     string
	NoCache bool
	Held    bool
	// RunID is the run Retry re-runs; Retry is false when it cannot be: the
	// run is live, holds several builds (each is retried from its own row), or
	// its job is gone.
	RunID string
	Retry bool
	// Abort is where the Abort button posts; empty when nothing here can be stopped.
	Abort string
}

// runBar is a run page's bar. Abort while the run is live and Retry once it is
// not, in one slot, as on Concourse's build page.
func runBar(ctx context.Context, pipeline *Pipeline, run store.RunRow, jobDeclared bool) barView {
	bar := barView{RunID: run.ID}

	if jobDeclared {
		bar.Job = run.JobName
	}

	if run.Status == "running" {
		bar.Abort = "/p/" + pipeline.Slug + "/runs/" + run.ID + "/abort"

		return bar
	}

	bar.Retry = jobDeclared && builds(ctx, pipeline, run.ID) <= 1

	return bar
}

// builds counts the triggered builds a run recorded inputs for; a run with no
// get recorded none and is one build.
func builds(ctx context.Context, pipeline *Pipeline, runID string) int {
	inputs, err := pipeline.Store.RunInputs(ctx, runID)
	if err != nil {
		return 0
	}

	seen := map[string]bool{}

	for _, input := range inputs {
		if strings.HasPrefix(input.BuildID, runID+"#") {
			seen[input.BuildID] = true
		}
	}

	return len(seen)
}

// buildRetries maps each build's row of a finished fan-out run to its build
// index, so the row can offer Retry of itself: Retry re-runs one build, and a
// run of several has no single build for the bar to name. The fan-out get's
// rows are the builds, one per set, in set order.
//
// Only the page asks: a live run offers Abort, not Retry, so the stream that
// draws a live run never needs it.
func buildRetries(ctx context.Context, pipeline *Pipeline, view runView, jobDeclared bool) map[int64]string {
	count := builds(ctx, pipeline, view.Run.ID)
	if !jobDeclared || view.Run.Status == "running" || count <= 1 {
		return nil
	}

	byIndex := map[int][]int64{}

	for _, root := range view.Roots {
		if root.Kind == "get" {
			byIndex[root.Index] = append(byIndex[root.Index], root.ID)
		}
	}

	for _, rows := range byIndex {
		if len(rows) != count {
			continue
		}

		retries := make(map[int64]string, count)
		for build, id := range rows {
			retries[id] = strconv.Itoa(build)
		}

		return retries
	}

	return nil
}
