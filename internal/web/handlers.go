package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/store"
)

// handleIndex sends the bare root at the first pipeline's board. With one
// pipeline loaded — the common case — the redirect is invisible.
func (s *Server) handleIndex(c echo.Context) error {
	slugs := make([]string, 0, len(s.bySlug))
	for slug := range s.bySlug {
		slugs = append(slugs, slug)
	}

	sort.Strings(slugs)

	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusFound, "/p/"+slugs[0])
}

// handleJobs renders the board: every job, its latest run, the trigger queue.
func (s *Server) handleJobs(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	latest, err := pipeline.Store.LatestRunByJob(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	queue, err := pipeline.Store.ListTriggerQueue(ctx, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "jobs", map[string]any{
		"Nav":   s.nav(c),
		"Jobs":  buildJobViews(pipeline.Cfg, latest, paused),
		"Queue": pendingQueue(queue),
	})
}

// pendingQueue filters the queue to what is still outstanding — a finished
// row is history, and history is what the run list is for.
func pendingQueue(rows []store.QueueRow) []store.QueueRow {
	var out []store.QueueRow

	for _, row := range rows {
		if row.Status == "pending" || row.Status == "running" {
			out = append(out, row)
		}
	}

	return out
}

// handleJob renders one job: its dependencies, its runs, its green versions.
func (s *Server) handleJob(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	job, err := pipeline.Cfg.FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	latest, err := pipeline.Store.LatestRunByJob(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	runs, err := pipeline.Store.ListRuns(ctx, job.Name, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	versions, err := pipeline.Store.PassedVersions(ctx, job.Name, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// The job's own view carries both edge directions; find it rather than
	// recomputing, so the board and this page can never disagree.
	var view jobView

	for _, candidate := range buildJobViews(pipeline.Cfg, latest, paused) {
		if candidate.Name == job.Name {
			view = candidate
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "job", map[string]any{
		"Nav":      s.nav(c),
		"Job":      view,
		"Runs":     runs,
		"Versions": versions,
		"Spark":    sparkline(runs),
	})
}

// handleRun renders a run transcript.
func (s *Server) handleRun(c echo.Context) error {
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
	if view.Run.Status == "failed" {
		err = s.attachDiff(c, &view)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "run", map[string]any{
		"Nav": s.nav(c),
		"Run": view,
	})
}

// assembleRun reads a run's events and the nodes they reference, and folds
// them into the view.
func (s *Server) assembleRun(c echo.Context, run store.RunRow) (runView, error) {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	rows, err := pipeline.Store.RunEvents(ctx, run.ID, 0, 5000)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	hashes := make([]string, 0, len(rows))

	for _, row := range rows {
		if row.Hash != "" {
			hashes = append(hashes, row.Hash)
		}
	}

	nodes, err := pipeline.Store.NodesByHash(ctx, hashes)
	if err != nil {
		return runView{}, fmt.Errorf("web: %w", err)
	}

	return buildRunView(run, rows, nodes), nil
}

// attachDiff fills in what changed since the last green run of this job.
func (s *Server) attachDiff(c echo.Context, view *runView) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	runs, err := pipeline.Store.ListRuns(ctx, view.Run.JobName, historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	for _, candidate := range runs {
		if candidate.Status != "succeeded" || !candidate.StartedAt.Before(view.Run.StartedAt) {
			continue
		}

		prior, err := s.assembleRun(c, candidate)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		view.Changed = diffAgainst(*view, prior)
		view.ComparedTo = candidate.ID

		return nil
	}

	return nil
}

// handleNode renders one merkle node: what its hash is made of, and which
// runs have used it. It is the cache's receipt — the answer to "why was this
// step skipped", which nothing else in the product can show.
func (s *Server) handleNode(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	hash := c.Param("hash")

	node, ok, err := pipeline.Store.FindNode(ctx, hash)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

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

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "node", map[string]any{
		"Nav":        s.nav(c),
		"Node":       node,
		"Content":    prettyJSON(node.Content),
		"Result":     prettyJSON(node.Result),
		"Runs":       runs,
		"Transcript": transcriptEvents(transcript, hasTranscript),
	})
}

// handleApprovals lists decisions, pending first.
func (s *Server) handleApprovals(c echo.Context) error {
	pipeline := pipelineOf(c)

	approvals, err := pipeline.Store.AllApprovals(c.Request().Context(), historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "approvals", map[string]any{
		"Nav":       s.nav(c),
		"Approvals": approvals,
	})
}

// handleResources shows what the watcher has seen, and the breaker state that
// stops it acting on what it sees.
func (s *Server) handleResources(c echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	checked, err := pipeline.Store.CheckedResources(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := pipeline.Store.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "resources", map[string]any{
		"Nav":       s.nav(c),
		"Resources": pipeline.Cfg.Resources,
		"Checked":   checkedByName(checked),
		"Paused":    paused,
	})
}

func checkedByName(rows []store.CheckedResource) map[string]store.CheckedResource {
	byName := map[string]store.CheckedResource{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	return byName
}

// handleTrigger queues a job. force re-runs everything, ignoring the merkle
// cache — without it a re-run of an unchanged pipeline correctly does almost
// nothing, which is never what someone pressing "re-run" meant.
func (s *Server) handleTrigger(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)
	name := c.Param("job")

	_, err := pipeline.Cfg.FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	force := c.FormValue("force") != ""

	reason := "manual (web)"
	if force {
		reason = "manual re-run, forced (web)"
	}

	_, err = s.runner.Enqueue(c.Request().Context(), pipeline, name, reason, force)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s", pipeline.Slug, name))
}

// handleDecideApproval records a human decision, through the same row the
// CLI's approve/reject write.
func (s *Server) handleDecideApproval(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "approval id must be a number")
	}

	status := "approved"
	if c.FormValue("decision") == "reject" {
		status = "rejected"
	}

	err = pipeline.Store.DecideApproval(c.Request().Context(), id, status, "web", c.FormValue("reason"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/approvals")
}

// handleResumeBreaker puts a paused job back in the watch rotation.
func (s *Server) handleResumeBreaker(c echo.Context) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	pipeline := pipelineOf(c)

	err := pipeline.Store.ResetJobFailures(c.Request().Context(), c.Param("job"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/resources")
}

// handleSearch backs the jump palette: jobs, recent runs, and pipelines,
// filtered by substring. JSON rather than a page — it is the one place the UI
// is a control instead of a document.
func (s *Server) handleSearch(c echo.Context) error {
	pipeline := pipelineOf(c)
	query := strings.ToLower(c.QueryParam("q"))

	type hit struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		Hint string `json:"hint"`
		URL  string `json:"url"`
	}

	var hits []hit

	add := func(candidate hit) {
		if len(hits) >= 20 {
			return
		}

		if query != "" && !strings.Contains(strings.ToLower(candidate.Kind+" "+candidate.Name+" "+candidate.Hint), query) {
			return
		}

		hits = append(hits, candidate)
	}

	for _, other := range s.pipelines {
		add(hit{Kind: "pipeline", Name: other.Slug, Hint: other.Path, URL: "/p/" + other.Slug})
	}

	for _, job := range pipeline.Cfg.Jobs {
		add(hit{Kind: "job", Name: job.Name, URL: fmt.Sprintf("/p/%s/jobs/%s", pipeline.Slug, job.Name)})
	}

	runs, err := pipeline.Store.ListRuns(c.Request().Context(), "", 50)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	for _, run := range runs {
		add(hit{
			Kind: "run",
			Name: run.JobName + " " + shortID(run.ID),
			Hint: run.Status,
			URL:  fmt.Sprintf("/p/%s/runs/%s", pipeline.Slug, run.ID),
		})
	}

	//nolint:wrapcheck // echo's JSON error is returned verbatim
	return c.JSON(http.StatusOK, hits)
}
