package web

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// The root, when this process serves more than one pipeline.
//
// Everything else in this package is scoped to a pipeline by a route
// parameter, and deliberately: `/p/<slug>/...` is the whole UI, and a
// `Pipeline` handle is what a handler reads through. The overview is the one
// view above that, so it is the one place that reads through store.Reader —
// which crosses pipelines by construction and therefore has to name them.
//
// It exists because `--state shared.db` made "what does this file hold" a
// real question, and because the previous root answered a question nobody
// asked: it redirected to whichever slug sorted first, silently picking one
// of several.

// overviewLimit bounds the global feed. Smaller than historyLimit because
// this is the glance-at-it page: an operator who wants a pipeline's whole
// history opens that pipeline.
const overviewLimit = 50

// overviewRun is one row of the global feed: a run, plus the slug that makes
// it reachable.
type overviewRun struct {
	store.RunRow

	Pipeline string
}

// overviewPipeline is one served pipeline, with what the state file records
// about it alongside what this process knows.
type overviewPipeline struct {
	Slug string
	Path string
	Jobs int
}

// handleIndex answers the bare root, and its answer depends on how many
// pipelines this process serves.
//
// One — the overwhelmingly common case — redirects straight through to that
// pipeline's board, so nobody pays a click for a list of one. Several render
// the overview, because with several there is no defensible pipeline to pick.
func (s *Server) handleIndex(c echo.Context) error {
	if len(s.pipelines) == 1 {
		//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
		return c.Redirect(http.StatusFound, "/p/"+s.pipelines[0].Slug)
	}

	runs, err := s.recentRunsAcross(c.Request().Context(), overviewLimit)
	if err != nil {
		return err
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "overview", map[string]any{
		"Nav":       s.globalNav(c),
		"Pipelines": s.overviewPipelines(),
		"Runs":      runs,
	})
}

// overviewPipelines describes what this process serves, sorted by slug.
func (s *Server) overviewPipelines() []overviewPipeline {
	out := make([]overviewPipeline, 0, len(s.pipelines))
	for _, pipeline := range s.pipelines {
		out = append(out, overviewPipeline{
			Slug: pipeline.Slug,
			Path: pipeline.Path,
			Jobs: len(pipeline.Config().Jobs),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })

	return out
}

// recentRunsAcross reads the newest runs of every SERVED pipeline, newest
// first.
//
// Grouped by state FILE rather than queried per pipeline, because served
// pipelines need not share one: `steps web app.yml infra.yml` gives each its
// own `.steps/<name>.db` unless --state says otherwise. Within a file, one
// ordered query does the interleaving; across files there is nothing to do
// but merge, and each group returns its own top `limit` so the merge cannot
// be short of rows it should have had.
//
// Only pipelines this process serves are named, so a file shared with a
// pipeline nobody here loaded contributes nothing — a row with no route
// behind it is worse than a missing one, and the feed is bounded, so it would
// crowd out rows that do have one.
func (s *Server) recentRunsAcross(ctx context.Context, limit int) ([]overviewRun, error) {
	var (
		handles = map[string]*store.Store{}
		names   = map[string][]string{}
		// The store scopes itself by pipeline NAME and the URL is built from
		// the slug. They are the same string today — both come from
		// resolvePipelineName — but they are assigned in two places, and a
		// feed that assumed it would link every row somewhere wrong the day
		// that stopped being true.
		slugByName = map[string]string{}
	)

	for _, pipeline := range s.pipelines {
		path := pipeline.Store.Path()
		name := pipeline.Store.Pipeline()

		handles[path] = pipeline.Store
		names[path] = append(names[path], name)
		slugByName[name] = pipeline.Slug
	}

	var runs []overviewRun

	for path, handle := range handles {
		rows, err := handle.Reader().RecentRuns(ctx, names[path], limit)
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}

		for _, row := range rows {
			runs = append(runs, overviewRun{RunRow: row.RunRow, Pipeline: slugByName[row.Pipeline]})
		}
	}

	sort.SliceStable(runs, func(i, j int) bool {
		return runs[i].StartedAt.After(runs[j].StartedAt)
	})

	if len(runs) > limit {
		runs = runs[:limit]
	}

	return runs, nil
}

// agentDialView is one agent step's effective limits, as the job page shows
// them.
//
// The values are RESOLVED, not the ones written on any single object: a step
// may override its agent, the agent may override the pipeline default, and
// the default is a constant in Go. Answering "why did this step stop at 30
// turns" used to mean cross-referencing all three, which is why the numbers
// that decide it belong on the page that lists the step.
type agentDialView struct {
	// Where is the step this row describes, as VisitSteps labels it — the
	// plan position, and the hook when it is one. Without it two rows for one
	// agent are byte-identical, which defeats the section's own purpose: "why
	// did THIS step stop at N turns" is unanswerable when several rows claim
	// different Ns for the same name, and a hook agent that only runs on
	// failure reads as an ordinary plan step.
	Where string
	Agent string
	// Turns is the tool-calling cap. Zero means the author removed it.
	Turns int
	// ContextBytes caps what a context_paths: file may deliver. Zero means
	// uncapped, which only an explicit 0 produces.
	ContextBytes int
	// Timeout is the per-attempt deadline as written; empty means the
	// built-in default applies.
	Timeout string
	// Broken is why this step's invocation would not resolve, empty for the
	// ordinary case. A row with it carries no numbers, because there are
	// none.
	Broken string
}

// Uncapped reports a dial an author explicitly removed, which the page shows
// as a word rather than as 0 — a zero in a limit column reads as "nothing
// allowed", the opposite of what it means here.
func (a agentDialView) UncappedTurns() bool { return a.Turns == 0 }

// UncappedContext is the same for the context ceiling.
func (a agentDialView) UncappedContext() bool { return a.ContextBytes == 0 }

// UncappedTimeout is the same for the deadline, and it is the one that had to
// be spelled out rather than left to the template.
//
// A deadline is a STRING here, and `timeout: 0` is the documented way to say
// a step has no wall-clock ceiling (agentTimeout returns noAgentDeadline for
// it). Any non-empty string is truthy in a Go template, so the most dangerous
// dial state on the page rendered as the smallest possible number — beside
// two columns that correctly said "uncapped" — and an operator auditing for
// runaway spend would read it as tightly bounded.
func (a agentDialView) UncappedTimeout() bool {
	if a.Timeout == "" {
		return false
	}

	parsed, err := config.ParseTimeout(a.Timeout)

	return err == nil && parsed == 0
}

// agentDials resolves the effective limits of every agent step in a job.
// It covers the agents a step names DIRECTLY. A task's `fix:` agent and a
// step's sub-agent `tools:` grants run real conversations under limits of
// their own and are not listed — Job.AgentNames walks all three, this walks
// one — so the heading says "steps", not "agents".
func agentDials(cfg *config.Config, job config.Job) []agentDialView {
	var dials []agentDialView

	// Through VisitSteps rather than over job.Plan, because a plan is a TREE:
	// do:, in_parallel:, across: and try: all hold steps, and an agent nested
	// in one runs under limits just as worth seeing.
	_ = job.VisitSteps(func(label string, step *config.Step) error {
		if step.Agent == "" {
			return nil
		}

		where := strings.TrimPrefix(label, fmt.Sprintf("job %q ", job.Name))

		ri, err := cfg.ResolveAgentInvocation(*step)
		if err != nil {
			// Shown, not dropped. Not every resolution failure is caught at
			// load — `reasoning_effort:` is validated here and nowhere else,
			// and an endpoint credential is deferred on purpose — so a
			// silently missing row would leave the page confidently listing
			// the job's other agents and looking complete while omitting the
			// one that is broken. That is the failure this section exists to
			// surface, not to hide.
			dials = append(dials, agentDialView{Where: where, Agent: step.Agent, Broken: err.Error()})

			//nolint:nilerr // the error is rendered as the row above; see Broken
			return nil
		}

		dials = append(dials, agentDialView{
			Where:        where,
			Agent:        ri.AgentName,
			Turns:        ri.MaxTurns,
			ContextBytes: ri.MaxContextBytes,
			Timeout:      ri.Timeout,
		})

		return nil
	})

	return dials
}
