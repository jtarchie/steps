package web

import (
	"sort"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// jobView is a job as the board and the job page show it.
type jobView struct {
	Name   string
	Latest store.RunRow
	HasRun bool
	// Held is the circuit breaker's state (max_consecutive_failures), never a person's pause.
	Held     bool
	Failures int
	HeldAt   time.Time
	// Upstream and Downstream are the passed: constraint graph, per resource.
	Upstream   []edgeView
	Downstream []edgeView
	// Finished is the latest run that is over, when Latest is still going:
	// what the graph colors a busy job by.
	Finished    store.RunRow
	HasFinished bool
	// Inputs and Outputs are every resource the job gets and puts, hooks
	// included: what the graph draws on its left and right.
	Inputs  []config.JobInput
	Outputs []string
	// Queued is the trigger that has not become a run yet; nil when there is none.
	Queued *queuedJob
}

// queuedJob is a job's queued run: when it was enqueued, for the follow page's since=, and the first check holding it back (empty once a worker has claimed it).
type queuedJob struct {
	Since     int64
	WaitingOn string
}

// edgeView is one passed: dependency: a resource that must be green in some
// other job. The resource is the edge's identity, because that is what the
// constraint is actually about — a job does not depend on a job, a get
// depends on a version having passed somewhere.
type edgeView struct {
	Resource string
	Job      string
}

// buildJobViews assembles the board: every configured job, its latest run,
// its breaker state, and the dependency edges in both directions.
func buildJobViews(cfg *config.Config, latest map[string]store.RunRow, paused []store.HeldJob) []jobView {
	pausedBy := map[string]store.HeldJob{}
	for _, job := range paused {
		pausedBy[job.Name] = job
	}

	views := make([]jobView, 0, len(cfg.Jobs))

	for _, job := range cfg.Jobs {
		views = append(views, buildJobView(job, latest, pausedBy))
	}

	return linkDownstream(views)
}

// buildJobView assembles one job's row: its latest run, its breaker state, and
// the passed: constraints it declares.
func buildJobView(job config.Job, latest map[string]store.RunRow, pausedBy map[string]store.HeldJob) jobView {
	view := jobView{Name: job.Name, Inputs: job.Inputs(), Outputs: job.Outputs()}

	if run, ok := latest[job.Name]; ok {
		view.Latest, view.HasRun = run, true
	}

	if breaker, ok := pausedBy[job.Name]; ok {
		view.Held, view.Failures, view.HeldAt = true, breaker.Consecutive, breaker.HeldAt
	}

	for resource, upstream := range job.PassedConstraints() {
		for _, name := range upstream {
			view.Upstream = append(view.Upstream, edgeView{Resource: resource, Job: name})
		}
	}

	sortEdges(view.Upstream)

	return view
}

// linkDownstream fills in each job's downstream edges by reading every
// upstream edge backwards. Derived rather than stored, so the two directions
// cannot disagree.
func linkDownstream(views []jobView) []jobView {
	byName := map[string]int{}
	for i, view := range views {
		byName[view.Name] = i
	}

	for _, view := range views {
		for _, edge := range view.Upstream {
			position, ok := byName[edge.Job]
			if !ok {
				continue
			}

			views[position].Downstream = append(views[position].Downstream,
				edgeView{Resource: edge.Resource, Job: view.Name})
		}
	}

	for i := range views {
		sortEdges(views[i].Downstream)
	}

	return views
}

func sortEdges(edges []edgeView) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].Job == edges[j].Job {
			return edges[i].Resource < edges[j].Resource
		}

		return edges[i].Job < edges[j].Job
	})
}
