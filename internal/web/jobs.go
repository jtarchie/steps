package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// handleJobs renders the board: every job, its latest run, the trigger queue.
func (s *Server) handleJobs(c *echo.Context) error {
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

	views := buildJobViews(pipeline.Config(), latest, paused)
	withLastFinished(ctx, pipeline, views)

	failing, err := pipeline.Store.CheckErrors(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	rows, err := s.markQueued(ctx, pipeline, queue, views)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "jobs", map[string]any{
		"Nav":   s.nav(c),
		"Title": c.Param("pipeline"),
		"Jobs":  views,
		"Graph": buildGraph(views, failingByName(failing)),
		"Queue": rows,
		"SHA":   pipeline.Config().Revision.SHA,
	})
}

// markQueued fills in each job's queued run and what holds it back, and the queue table's rows with the same reasons, asked once per job.
func (s *Server) markQueued(ctx context.Context, pipeline *Pipeline, queue []store.QueueRow, views []jobView) ([]queueRowView, error) {
	reasons := map[string]string{}

	reason := func(name string) (string, error) {
		if text, seen := reasons[name]; seen {
			return text, nil
		}

		text, err := waitingOn(ctx, pipeline, s.capacity, queue, name)
		reasons[name] = text

		return text, err
	}

	for i := range views {
		row, queued, err := queuedRow(ctx, pipeline, queue, views[i].Name)
		if err != nil {
			return nil, err
		}

		if !queued {
			continue
		}

		next := &queuedJob{Since: row.EnqueuedAt.UnixMilli()}

		// A claimed row with no run yet is starting, and every limit has already let it through.
		if row.Status == "pending" {
			next.WaitingOn, err = reason(views[i].Name)
			if err != nil {
				return nil, err
			}
		}

		views[i].Queued = next
	}

	outstanding := pendingQueue(queue)
	rows := make([]queueRowView, 0, len(outstanding))

	for _, row := range outstanding {
		view := queueRowView{QueueRow: row}

		if row.Status == "pending" {
			waiting, err := reason(row.JobName)
			if err != nil {
				return nil, err
			}

			view.WaitingOn = waiting
		}

		rows = append(rows, view)
	}

	return rows, nil
}

// queueRowView is an outstanding queue row and, while it is pending, what holds it back.
type queueRowView struct {
	store.QueueRow
	WaitingOn string
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

// handleJobRedirect sends a job's address to what a reader came for: its
// latest run; failing that, the follow page for the trigger it is waiting
// on, so the reader lands on exactly that trigger's run; failing that, the
// detail page. 302 and never 301: a browser may cache a permanent redirect,
// and "latest" changes with every run.
func (s *Server) handleJobRedirect(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	_, err := pipeline.Config().FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	base := "/p/" + pipeline.Slug + "/jobs/" + name

	strip, err := s.runStrip(ctx, pipeline, name)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if len(strip.Runs) > 0 {
		//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
		return c.Redirect(http.StatusFound, "/p/"+pipeline.Slug+"/runs/"+strip.Runs[0].ID)
	}

	if strip.Queued {
		//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
		return c.Redirect(http.StatusFound, fmt.Sprintf("%s/follow?since=%d", base, strip.QueuedSince))
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusFound, base+"/detail")
}

// handleJobDetail renders one job: its dependencies, its runs, its green versions.
func (s *Server) handleJobDetail(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	// One read for the whole request. A reload swaps this pointer under a
	// handler mid-render, and three reads is three configurations: the 404
	// guard would pass against one while the view came up empty from another
	// and the agent dials were resolved out of a third.
	cfg := pipeline.Config()

	job, err := cfg.FindJob(name)
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

	for _, candidate := range buildJobViews(cfg, latest, paused) {
		if candidate.Name == job.Name {
			view = candidate
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "job", map[string]any{
		"Nav":      s.nav(c),
		"Title":    job.Name,
		"Crumbs":   []crumb{{Label: "jobs", URL: "/p/" + pipeline.Slug}, {Label: job.Name, URL: "/p/" + pipeline.Slug + "/jobs/" + job.Name}, {Label: "detail"}},
		"Job":      view,
		"Bar":      barView{Job: job.Name, Held: view.Held},
		"Runs":     runs,
		"Versions": versions,
		"Spark":    sparkline(runs),
		"Dials":    agentDials(cfg, *job),
	})
}

// handleRunHistory renders the pipeline-wide run list, newest first — the
// cross-job history that otherwise only exists per job, or on the
// multi-pipeline overview a single-pipeline deployment never sees.
func (s *Server) handleRunHistory(c *echo.Context) error {
	pipeline := pipelineOf(c)

	runs, err := pipeline.Store.ListRuns(c.Request().Context(), "", historyLimit)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "runs", map[string]any{
		"Nav":   s.nav(c),
		"Title": "runs",
		"Runs":  runs,
	})
}

// stripLimit bounds the run strip. Enough to see a job's recent shape;
// the detail page carries the history proper.
const stripLimit = 20

// stripChip is one run on the strip: the row, plus the duration as text
// because a title attribute cannot hold the ticking <time> elapsedTag draws.
type stripChip struct {
	store.RunRow
	Duration string
}

// runStripView is a job's recent runs, newest first, with the trigger that
// has not become a run yet ahead of them — the newest thing about the job.
type runStripView struct {
	Runs        []stripChip
	QueuedSince int64
	Queued      bool
}

// runStrip reads what the strip shows. A claimed row is "running" to the
// queue for the whole build, and a pending one may already have been
// overtaken — so a row counts as queued only while no run has started since
// it was enqueued, which is the same question the follow page asks.
func (s *Server) runStrip(ctx context.Context, pipeline *Pipeline, jobName string) (runStripView, error) {
	runs, err := pipeline.Store.ListRuns(ctx, jobName, stripLimit)
	if err != nil {
		return runStripView{}, err //nolint:wrapcheck // the caller wraps with its own context
	}

	view := runStripView{Runs: make([]stripChip, 0, len(runs))}
	for _, run := range runs {
		view.Runs = append(view.Runs, stripChip{RunRow: run, Duration: formatDuration(run.Duration())})
	}

	queue, err := pipeline.Store.ListTriggerQueue(ctx, 25)
	if err != nil {
		return runStripView{}, err //nolint:wrapcheck // the caller wraps with its own context
	}

	row, queued, err := queuedRow(ctx, pipeline, queue, jobName)
	if err != nil {
		return runStripView{}, err
	}

	if queued {
		view.Queued = true
		view.QueuedSince = row.EnqueuedAt.UnixMilli()
	}

	return view, nil
}

// queuedRow is the job's newest outstanding queue row, while no run has started since it was enqueued.
func queuedRow(ctx context.Context, pipeline *Pipeline, queue []store.QueueRow, jobName string) (store.QueueRow, bool, error) {
	for _, row := range queue {
		if row.JobName != jobName || (row.Status != "pending" && row.Status != "running") {
			continue
		}

		_, served, err := pipeline.Store.FirstRunSince(ctx, jobName, row.EnqueuedAt)
		if err != nil {
			return store.QueueRow{}, false, err //nolint:wrapcheck // the caller wraps with its own context
		}

		return row, !served, nil
	}

	return store.QueueRow{}, false, nil
}
