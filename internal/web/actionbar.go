package web

import "github.com/jtarchie/steps/internal/store"

// barView is what the page's own job or run can have done to it, drawn in the
// action bar beside the pipeline's pause. A page that sets none gets the pause
// alone.
type barView struct {
	// Job is the job Trigger starts a new run of; empty offers no Trigger.
	Job  string
	Held bool
	// RunID is the run Retry re-runs, every build of it; Retry is false when
	// it cannot be: the run is live, or its job is gone.
	RunID string
	Retry bool
	// Abort is where the Abort button posts; empty when nothing here can be stopped.
	Abort string
}

// runBar is a run page's bar. Abort while the run is live and Retry once it is
// not, in one slot, as on Concourse's build page.
func runBar(pipeline *Pipeline, run store.RunRow, jobDeclared bool) barView {
	bar := barView{RunID: run.ID}

	if jobDeclared {
		bar.Job = run.JobName
	}

	if run.Status == "running" {
		bar.Abort = "/p/" + pipeline.Slug + "/runs/" + run.ID + "/abort"

		return bar
	}

	bar.Retry = jobDeclared

	return bar
}
