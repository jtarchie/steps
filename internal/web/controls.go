package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// forceAll is the one value of the trigger form's force field that forces.
// Run pages rendered before #147 posted force=1 from a button that read
// "Re-run", and a tab left open through the deploy must not keep forcing.
const forceAll = "all"

// handleTrigger queues a person's trigger of a job. force=all ignores the
// merkle cache, but no page sends it any more: Retry is how a page runs
// unchanged steps again. It does not re-take versions a `version: every` get
// already built (#145): those are effects, and replaying a resource's history
// is not what anyone asked for.
func (s *Server) handleTrigger(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)
	name := c.Param("job")

	_, err = pipeline.Config().FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	// Refused rather than queued: a pause that let the button enqueue would fill the queue with work nobody intends to run, and say nothing about why nothing happened.
	ctx := c.Request().Context()

	stopped, err := pipeline.Store.Paused(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if stopped {
		return pausedRefusal(ctx, pipeline)
	}

	force := c.FormValue("force") == forceAll

	reason := "trigger (web)"
	if force {
		reason = "trigger, no cache (web)"
	}

	// Stamped BEFORE enqueueing: the run this click causes must start at or
	// after this instant, and a stamp taken afterwards could miss a run the
	// drainer started in between.
	since := time.Now().UTC()

	_, err = s.runner.Enqueue(ctx, pipeline.Pipeline, name, reason, force)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// Follow, do not return to the list. The natural loop is trigger then
	// watch; sending the browser back to a table it must poll by hand leaves
	// the person to do the waiting the UI is for.
	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s/follow?since=%d",
		pipeline.Slug, name, since.UnixMilli()))
}

// handleRerun is Retry: fly rerun-build (#146), a recorded run against the
// versions it was created with, as a new run. Refused where a
// trigger is — a read-only server, a paused pipeline, a job the config dropped
// — and for a run still going, which the page offers Abort for instead.
func (s *Server) handleRerun(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	run, ok, err := pipeline.Store.FindRunRow(ctx, c.Param("run"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	err = refuseRerun(ctx, pipeline, run)
	if err != nil {
		return err
	}

	since := time.Now().UTC()

	err = s.runner.EnqueueRerun(ctx, pipeline.Pipeline, run.JobName, run.ID)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s/follow?since=%d",
		pipeline.Slug, run.JobName, since.UnixMilli()))
}

// refuseRerun is why a run cannot be retried right now, if it cannot.
func refuseRerun(ctx context.Context, pipeline *Pipeline, run store.RunRow) error {
	_, err := pipeline.Config().FindJob(run.JobName)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", run.JobName))
	}

	if run.Status == store.RunRunning {
		return echo.NewHTTPError(http.StatusConflict, "this run is still running; abort it first")
	}

	stopped, err := pipeline.Store.Paused(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if stopped {
		return pausedRefusal(ctx, pipeline)
	}

	return nil
}

// pausedRefusal names which pause to lift: telling somebody to unpause a pipeline that is not itself paused sends them to a switch that does nothing.
func pausedRefusal(ctx context.Context, pipeline *Pipeline) error {
	all, err := pipeline.Store.AllPaused(ctx)
	if err == nil && all {
		return echo.NewHTTPError(http.StatusConflict, "everything is paused; resume it from the overview")
	}

	return echo.NewHTTPError(http.StatusConflict,
		"this pipeline is paused; unpause it with steps pipeline unpause -p "+pipeline.Slug)
}

// handleRelease clears the breaker holding a job, so new versions trigger it again.
func (s *Server) handleRelease(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)

	name := c.Param("job")

	err = pipeline.Store.ResetJobFailures(c.Request().Context(), name)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/jobs/"+name+"/detail")
}

// handlePause throws the pipeline-level breaker from the browser.
func (s *Server) handlePause(c *echo.Context) error { return s.setPausedFromPage(c, true) }

// handleUnpause releases it.
func (s *Server) handleUnpause(c *echo.Context) error { return s.setPausedFromPage(c, false) }

// The browser's half of `steps pipeline pause`, refused under --read-only
// like every other control and same-origin-checked like every other POST.
//
// It is a second route rather than a call into /api because that group
// refuses anything browser-shaped on purpose — a rebinding page must not
// reach the verbs that set, destroy or rename. Pause is not in that class:
// this UI already offers Trigger, which runs arbitrary commands, so a page
// that can press this one gains nothing it did not already have.
func (s *Server) setPausedFromPage(c *echo.Context, pause bool) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)
	ctx := c.Request().Context()

	if pause {
		err = pipeline.Store.Pause(ctx)
	} else {
		err = pipeline.Store.Unpause(ctx)
	}

	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, returnTo(c, "/p/"+pipeline.Slug))
}

// Paths of the daemon-wide pause, which belongs to no pipeline.
const (
	pauseAllPath   = "/pause-all"
	unpauseAllPath = "/unpause-all"
)

func (s *Server) handlePauseAll(c *echo.Context) error { return s.setAllPausedFromPage(c, true) }

func (s *Server) handleUnpauseAll(c *echo.Context) error { return s.setAllPausedFromPage(c, false) }

// Admin, because it stops pipelines whose own access rules may not let this person operate them. Written to every database a served pipeline lives in, since they need not share one, and recorded against every served pipeline, since the log is written through a pipeline's handle and each of them was stopped.
func (s *Server) setAllPausedFromPage(c *echo.Context, pause bool) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	ctx := c.Request().Context()

	for _, handle := range s.databases() {
		if pause {
			err = handle.PauseAll(ctx)
		} else {
			err = handle.UnpauseAll(ctx)
		}

		if err != nil {
			return fmt.Errorf("web: %w", err)
		}
	}

	route := activityRoute{action: verb("unpause all"), target: nothing}
	if pause {
		route.action = verb("pause all")
	}

	for _, pipeline := range s.Served() {
		s.writeActivity(c, pipeline, route)
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, returnTo(c, "/"))
}

// databases is one handle per database the served pipelines live in.
func (s *Server) databases() []store.Store {
	seen := map[string]bool{}

	var handles []store.Store

	for _, pipeline := range s.Served() {
		if pipeline.Store == nil || seen[pipeline.Store.Description()] {
			continue
		}

		seen[pipeline.Store.Description()] = true
		handles = append(handles, pipeline.Store)
	}

	return handles
}

// allPaused is whether any database served from is under PauseAll; false when none can say, as a missing banner beats a page that will not draw.
func (s *Server) allPaused(ctx context.Context) bool {
	for _, handle := range s.databases() {
		paused, err := handle.AllPaused(ctx)
		if err == nil && paused {
			return true
		}
	}

	return false
}

// returnTo is where a control sends the reader back to.
//
// The banner carrying these buttons is on EVERY page of a pipeline and on the
// overview above them, so a fixed destination would move somebody off the run
// they were reading to unpause it. The form says where it came from, and this
// is the one thing that value may be: a path on this server. A value starting
// "//" is a URL with a host (browsers read //evil.tld as absolute) and so is one starting "/\", since a browser folds that backslash to a slash before it reads the authority — which is how a redirect to a page's own referrer becomes a redirect off it.
func returnTo(c *echo.Context, fallback string) string {
	to := c.FormValue("return")
	if isLocalPath(to) {
		return to
	}

	return fallback
}

// handleHook is a delivery to a webhook resource. It has no runner guard: it authenticates with the sender's signature, not with this server's absent authentication, so --read-only does not withhold it.
func (s *Server) handleHook(c *echo.Context) error {
	target := pipelineOf(c)
	if target.Hooks == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no webhook resources in this pipeline")
	}

	target.Hooks(c.Response(), c.Request(), c.Param("resource"))

	return nil
}

// requireControls refuses a state-changing route on a read-only server.
func (s *Server) requireControls() error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	return nil
}
