package web

import (
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
)

// barView is what the page's own job or run can have done to it, drawn in the
// action bar beside the pipeline's pause. A page that sets none gets the pause
// alone.
type barView struct {
	// Job is the job Trigger starts a new run of; empty offers no Trigger.
	Job  string
	Held bool
	// RunID is the run Retry re-runs; Retry is false when
	// it cannot be: the run is live, or its job is gone.
	RunID string
	Retry bool
	// ResumeFrom is the step Resume picks the run up at, empty when no resume is offered; ResumeKept is how many finished steps it keeps.
	ResumeFrom string
	ResumeKept int
	// Abort is where the Abort button posts; empty when nothing here can be stopped.
	Abort string
}

// runBar is a run page's bar. Abort while the run is live and Retry once it is
// not, in one slot, as on Concourse's build page.
func runBar(pipeline *Pipeline, run store.RunRow, jobDeclared bool, resume scheduler.ResumeAt) barView {
	bar := barView{RunID: run.ID}

	if jobDeclared {
		bar.Job = run.JobName
	}

	if run.Status == store.RunRunning {
		bar.Abort = "/p/" + pipeline.Slug + "/runs/" + run.ID + "/abort"

		return bar
	}

	if jobDeclared {
		bar.Retry = true
		bar.ResumeFrom, bar.ResumeKept = resume.From, resume.Kept
	}

	return bar
}
