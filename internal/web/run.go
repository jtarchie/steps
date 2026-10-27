package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// turnRoute is the pattern handleTurn is registered under; handleError keys
// on it to answer with a line of text rather than a page.
const turnRoute = "/p/:pipeline/runs/:run/turns/:seq"

const eventsRoute = "/p/:pipeline/runs/:run/events"

// handleTurn renders one turn's body, which the run page draws only as a
// summary until it is opened (lazyValue, messageValue). htmx gets the <pre>'s
// inner HTML; the link a reader follows with JavaScript off gets a styled page.
func (s *Server) handleTurn(c *echo.Context) error {
	// One URL, two bodies: a cache must not hand the link the fragment.
	c.Response().Header().Add("Vary", "HX-Request")

	gone := echo.NewHTTPError(http.StatusNotFound, "this turn is no longer recorded")

	seq, err := strconv.ParseInt(c.Param("seq"), 10, 64)
	if err != nil || seq < 1 {
		return gone
	}

	rows, err := pipelineOf(c).Store.RunEvents(c.Request().Context(), c.Param("run"), seq-1, 1)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// Seq is global, so the next row of THIS run may be a later one.
	if len(rows) == 0 || rows[0].Seq != seq {
		return gone
	}

	var body string

	// Only what the page draws lazily is served here.
	switch rows[0].Type {
	case events.TypeAgentCall, events.TypeAgentResult:
		body = string(jsonValue(rows[0].Detail).HTML)
	case events.TypeAgentSystem, events.TypeAgentUser:
		body = string(highlightMessage(rows[0].Text))
	default:
		return gone
	}

	if c.Request().Header.Get("HX-Request") == "" {
		body = `<!doctype html><meta charset="utf-8"><link rel="stylesheet" href="/static/app.css"><pre class="json">` + body + `</pre>`
	}

	return c.HTML(http.StatusOK, body) //nolint:wrapcheck // a write error surfaces through the shared error handler
}

// handleRun renders a run transcript.
func (s *Server) handleRun(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	run, ok, err := pipeline.Store.FindRunRow(ctx, c.Param("run"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	view, err := s.assembleRun(c, run)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// A failed run opens by answering the question it prompts: what is
	// different about this one. Only computed for a failure — on a green run
	// it is trivia, and it costs a second query.
	if view.Run.Status == store.RunFailed {
		err = s.attachDiff(c, &view)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	// A later pipeline set may have dropped the job; its trigger would be a 404.
	cfg := pipeline.Config()
	_, jobErr := cfg.FindJob(view.Run.JobName)

	strip, err := s.runStrip(ctx, pipeline, view.Run.JobName)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// The job's neighbours, from the configuration alone: the board's view
	// of the same edges, without the board's two store reads.
	var job jobView

	for _, candidate := range buildJobViews(cfg, nil, nil) {
		if candidate.Name == view.Run.JobName {
			job = candidate
		}
	}

	approvals, questions, err := parkedOn(ctx, readsOf(c, pipeline), view.Run)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	nav := s.nav(c)
	nav.Mark = runMark(view.Run)
	nav.MarkURL = "/p/" + pipeline.Slug + "/runs/" + view.Run.ID + "/mark"

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "run", map[string]any{
		"Nav":       nav,
		"Run":       view,
		"Strip":     strip,
		"Job":       job,
		"Approvals": approvals,
		"Questions": questions,
		"Bar":       runBar(pipeline, view.Run, jobErr == nil),
		"Title":     runTitle(view.Run),
		"Crumbs": []crumb{
			{Label: "jobs", URL: "/p/" + pipeline.Slug},
			{Label: view.Run.JobName, URL: "/p/" + pipeline.Slug + "/jobs/" + view.Run.JobName + "/detail"},
			{Label: "#" + shortID(view.Run.ID)},
		},
	})
}

// assembleRun reads a run's events and the nodes they reference, and folds
// them into the view.
func (s *Server) assembleRun(c *echo.Context, run store.RunRow) (runView, error) {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	rows, nodes, err := readBatch(ctx, pipeline.Store, run.ID, 0, runEventLimit)
	if err != nil {
		return runView{}, err
	}

	view := buildRunView(run, rows, nodes)

	// Independent of the usage query below: a countdown on a running step
	// needs no spend history, only the config a running step's deadline is
	// resolved against.
	attachStepDeadlines(&view, pipeline.Config(), run.JobName, run.ConfigSHA)

	// Best-effort: a run page that cannot show spend is worth more than one
	// that 500s over it. A run predating this table simply has none.
	usage, err := pipeline.Store.RunUsage(ctx, run.ID)
	if err == nil {
		view.Usage = usage
		view.Ceilings, view.ConfigDrifted = agentCeilings(pipeline.Config(), run.JobName, run.ConfigSHA)
	}

	// Same terms: a run page that cannot say which machines it used is worth
	// more than one that 500s over it, and a run predating this table has none.
	placements, err := pipeline.Store.RunPlacements(ctx, run.ID)
	if err == nil {
		view.Placements = placements
	}

	return view, nil
}

// attachDiff fills in what changed since the last green run of this job.
func (s *Server) attachDiff(c *echo.Context, view *runView) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	runs, err := pipeline.Store.ListRuns(ctx, view.Run.JobName, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	for _, candidate := range runs {
		if candidate.Status != store.RunSucceeded || !candidate.StartedAt.Before(view.Run.StartedAt) {
			continue
		}

		prior, err := s.priorSteps(c, candidate)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		view.Changed, view.Uncompared = diffAgainst(*view, prior)
		view.ComparedTo = candidate.ID

		// Only when they differ, and only when both are known: a run
		// recorded before revisions existed has no hash, and reporting that
		// as a change would make every such page claim an edit that never
		// happened.
		if candidate.ConfigSHA != "" && view.Run.ConfigSHA != "" && candidate.ConfigSHA != view.Run.ConfigSHA {
			view.ComparedConfig = candidate.ConfigSHA
		}

		return nil
	}

	return nil
}

// priorSteps reads only what a diff compares: the prior run's step names and
// the content hashes beside them.
//
// The full page assembler would answer three more queries — the nodes behind
// those hashes, the run's spend, the machines it used — and diffAgainst
// discards every one of them. buildRunView takes nil results for exactly this
// reason: a hash comes off the event row, and only a step's rendered RESULT
// needs the node.
func (s *Server) priorSteps(c *echo.Context, run store.RunRow) (runView, error) {
	rows, err := pipelineOf(c).Store.RunEvents(c.Request().Context(), run.ID, 0, runEventLimit)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	return buildRunView(run, rows, nil), nil
}

// handleNode renders one merkle node: what its hash is made of, and which
// runs have used it. It is the cache's receipt — the answer to "why was this
// step skipped", which nothing else in the product can show.
func (s *Server) handleNode(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	hash := c.Param("hash")

	byHash, err := pipeline.Store.NodesByHash(ctx, []string{hash})
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	node, ok := byHash[hash]
	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no node with that hash")
	}

	runs, err := pipeline.Store.RunsUsingNode(ctx, hash, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	transcript, hasTranscript, err := pipeline.Store.NodeTranscript(ctx, hash)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// A node knows which job recorded it, but not which run a reader came
	// from — so the trail goes up through the job when there is one.
	crumbs := []crumb{{Label: "jobs", URL: "/p/" + pipeline.Slug}}
	if node.JobName != "" {
		crumbs = append(crumbs, crumb{Label: node.JobName, URL: "/p/" + pipeline.Slug + "/jobs/" + node.JobName + "/detail"})
	}

	crumbs = append(crumbs, crumb{Label: "node " + shortID(hash)})

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "node", map[string]any{
		"Nav":        s.nav(c),
		"Title":      "node " + shortID(hash),
		"Crumbs":     crumbs,
		"Node":       node,
		"Content":    node.Content,
		"Result":     node.Result,
		"Runs":       runs,
		"Transcript": transcriptEvents(transcript, hasTranscript),
	})
}

// parkedOn is what this run is parked on a person for: the attention list already read the pipeline's pending backlog this request, so filtering that costs no query. It filters into fresh slices because the read is shared. Only a running run waits: an abort or a failed sibling leaves its approval pending, and nobody would act on that decision.
func parkedOn(ctx context.Context, reads *pipelineReads, run store.RunRow) ([]store.Approval, []store.Question, error) {
	if run.Status != store.RunRunning {
		return nil, nil, nil
	}

	pendingApprovals, err := reads.PendingApprovals(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("pending approvals: %w", err)
	}

	pendingQuestions, err := reads.PendingQuestions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("pending questions: %w", err)
	}

	var (
		approvals []store.Approval
		questions []store.Question
	)

	for _, a := range pendingApprovals {
		if a.RunID == run.ID {
			approvals = append(approvals, a)
		}
	}

	for _, q := range pendingQuestions {
		if q.RunID == run.ID {
			questions = append(questions, q)
		}
	}

	return approvals, questions, nil
}
