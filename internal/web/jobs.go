package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// handleJobs renders the board: every job, its latest run, the trigger queue.
func (s *Server) handleJobs(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	reads := readsOf(c, pipeline)

	latest, err := reads.LatestRunByJob(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	paused, err := reads.HeldJobs(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	queue, err := reads.TriggerQueue(ctx, 25)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	views := buildJobViews(pipeline.Config(), latest, paused)
	withLastFinished(ctx, reads, views)

	failing, err := reads.CheckErrors(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	rows, err := s.markQueued(ctx, s.renderer.localeOf(c), reads, queue, views)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "jobs", map[string]any{
		"Nav":      s.nav(c),
		"Title":    c.Param("pipeline"),
		"Jobs":     views,
		"Graph":    buildGraphIn(s.renderer.localeOf(c), views, failingByName(failing)),
		"Queue":    rows,
		"SHA":      pipeline.Config().Revision.SHA,
		"Unfenced": pipeline.Unfenced(),
	})
}

// markQueued fills in each job's queued run and what holds it back, and the queue table's rows with the same reasons, asked once per job.
func (s *Server) markQueued(ctx context.Context, loc *locale, reads *pipelineReads, queue []store.QueueRow, views []jobView) ([]queueRowView, error) {
	pipeline := reads.target

	reasons := map[string]string{}

	reason := func(name string) (string, error) {
		if text, seen := reasons[name]; seen {
			return text, nil
		}

		text, err := waitingOnIn(ctx, loc, reads, s.capacity, queue, name)
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
		if row.Status == store.QueuePending {
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

		if row.Status == store.QueuePending {
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
		if row.Status == store.QueuePending || row.Status == store.QueueRunning {
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

	paused, err := pipeline.Store.HeldJobs(ctx)
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
		"Crumbs":   []crumb{{Label: s.renderer.localeOf(c).text("tab.jobs"), URL: "/p/" + pipeline.Slug}, {Label: job.Name, URL: "/p/" + pipeline.Slug + "/jobs/" + job.Name}, {Label: s.renderer.localeOf(c).text("job.crumb.detail")}},
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
// runsPage is how many runs one page of the history shows. Smaller than historyLimit because there is now an "older" link: the page no longer has to hold everything it can reach.
const runsPage = 50

// handleRunHistory is every run of the pipeline, newest first, narrowed by ?job= and ?q= and paged by ?before=<run id>.
func (s *Server) handleRunHistory(c *echo.Context) error {
	pipeline := pipelineOf(c)
	loc := s.renderer.localeOf(c)
	query := c.Request().URL.Query()
	base := "/p/" + pipeline.Slug + "/runs"

	find := store.RunQuery{
		Job:    query.Get("job"),
		Text:   strings.TrimSpace(query.Get("q")),
		Before: query.Get("before"),
		Limit:  runsPage + 1,
	}
	find.Statuses = statusesNamed(loc, find.Text)

	runs, err := pipeline.Store.FindRuns(c.Request().Context(), find)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	older := ""
	if len(runs) > runsPage {
		runs = runs[:runsPage]
		older = pathQuery(base, query, "before", runs[len(runs)-1].ID)
	}

	var filters []activityFilter

	for _, f := range []struct{ key, label, value string }{
		{"job", "job", find.Job},
		{"q", "text", find.Text},
	} {
		if f.value != "" {
			filters = append(filters, activityFilter{Label: f.label, Value: f.value, Remove: pathQuery(base, query, f.key, "")})
		}
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "runs", map[string]any{
		"Nav":     s.nav(c),
		"Title":   loc.text("tab.runs"),
		"Runs":    runs,
		"Jobs":    pipeline.Config().JobNames(),
		"Job":     find.Job,
		"Text":    find.Text,
		"Filters": filters,
		"Older":   older,
		"Paged":   find.Before != "",
		"Base":    base,
		"Query":   query,
	})
}

// statusesNamed is every run status whose word, as this reader sees it or in English, holds text: the page says "passed" where the store says "succeeded", and a reader types what they read.
func statusesNamed(loc *locale, text string) []store.RunStatus {
	if text == "" {
		return nil
	}

	needle := strings.ToLower(text)

	var named []store.RunStatus

	for _, status := range []store.RunStatus{store.RunRunning, store.RunSucceeded, store.RunFailed, store.RunAborted} {
		word := statusWord(status)
		if strings.Contains(strings.ToLower(loc.word("status", word)), needle) || strings.Contains(word, needle) {
			named = append(named, status)
		}
	}

	return named
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
		if row.JobName != jobName || (row.Status != store.QueuePending && row.Status != store.QueueRunning) {
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

// waitingOnIn is follow.go's waitingOn in loc's words.
func waitingOnIn(ctx context.Context, loc *locale, reads *pipelineReads, capacity Capacity, queue []store.QueueRow, name string) (string, error) {
	_, err := reads.target.Config().FindJob(name)
	if err != nil {
		return "", nil //nolint:nilerr // a dropped job is a row with no reason, not a broken page
	}

	_, inFlight, _ := queuedState(queue, name, time.Time{})

	checks, err := followChecksIn(ctx, loc, reads, capacity, name, false, inFlight)
	if err != nil {
		return "", err
	}

	for _, check := range checks {
		if !check.Clear {
			return check.Text, nil
		}
	}

	return "", nil
}
