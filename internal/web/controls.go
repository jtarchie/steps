package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/scheduler"
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

	// Stamped BEFORE enqueueing: the run this click causes must start at or
	// after this instant, and a stamp taken afterwards could miss a run the
	// drainer started in between.
	since := time.Now().UTC()

	err = s.queueTrigger(c.Request().Context(), pipeline, name, "web", c.FormValue("force") == forceAll, pinFrom(c), s.roleOf(c))
	if err != nil {
		return err
	}

	// Follow, do not return to the list. The natural loop is trigger then
	// watch; sending the browser back to a table it must poll by hand leaves
	// the person to do the waiting the UI is for.
	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s/follow?since=%d",
		pipeline.Slug, name, since.UnixMilli()))
}

// pinFromPrefix marks a form field naming one field of the version a trigger is pinned to: the resource page's per-version button sends one per field.
const pinFromPrefix = "pin."

// pinFrom is the version a page's trigger names, or nil for the job's ordinary trigger.
func pinFrom(c *echo.Context) map[string]string {
	form, err := c.FormValues()
	if err != nil {
		return nil
	}

	var pin map[string]string

	for key, values := range form {
		field, found := strings.CutPrefix(key, pinFromPrefix)
		if !found || len(values) == 0 {
			continue
		}

		if pin == nil {
			pin = map[string]string{}
		}

		pin[field] = values[0]
	}

	return pin
}

// queueTrigger is a person's trigger of a job, from the page or from `steps runs trigger`; from says which. A pin makes it a row of its own, built against the version it names.
func (s *Server) queueTrigger(ctx context.Context, pipeline *Pipeline, name, from string, force bool, pin map[string]string, role Role) error {
	job, err := pipeline.Config().FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	// Refused rather than queued: a pause that let the button enqueue would fill the queue with work nobody intends to run, and say nothing about why nothing happened.
	stopped, err := pipeline.Store.Paused(ctx)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if stopped {
		return pausedRefusal(ctx, pipeline)
	}

	if len(pin) > 0 {
		return s.queuePinnedTrigger(ctx, pipeline, job, name, from, force, pin, role)
	}

	reason := "trigger (" + from + ")"
	if force {
		reason = "trigger, no cache (" + from + ")"
	}

	err = s.runner.Enqueue(ctx, pipeline.Pipeline, name, reason, force)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// queuePinnedTrigger is queueTrigger's pinned row: refused with force, and held to the job's passed: gates below admin.
func (s *Server) queuePinnedTrigger(ctx context.Context, pipeline *Pipeline, job *config.Job, name, from string, force bool, pin map[string]string, role Role) error {
	// ponytail: a pinned trigger cannot also skip the cache; the force flag is per job in memory and a pinned row would spend another request's. Carry force on the row if anyone asks.
	if force {
		return echo.NewHTTPError(http.StatusBadRequest, "a pinned trigger cannot also skip the cache")
	}

	if role < RoleAdmin {
		err := refuseUnpassedPin(ctx, pipeline, job, pin)
		if err != nil {
			return err
		}
	}

	err := s.runner.EnqueuePinned(ctx, pipeline.Pipeline, name, "trigger, pinned ("+from+")", pin)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// refuseUnpassedPin holds a non-admin's pin to the job's passed: gates. A pin no green version matches is resolved from the check instead, so it would build what upstream never passed — which Concourse does not let anyone do, and which here only an admin may.
// ponytail: each constrained resource is judged alone; a fan-in whose pinned versions passed upstream in different builds gets through. Judge the set per upstream job (HasPassedVersionSet) if that matters.
func refuseUnpassedPin(ctx context.Context, pipeline *Pipeline, job *config.Job, pin map[string]string) error {
	constraints := job.PassedConstraints()

	for _, resource := range slices.Sorted(maps.Keys(constraints)) {
		upstream := constraints[resource]

		green, err := pipeline.Store.GreenVersions(ctx, resource, upstream)
		if err != nil {
			return fmt.Errorf("web: %w", err)
		}

		if !slices.ContainsFunc(green, func(version map[string]any) bool { return pinMatches(version, pin) }) {
			return echo.NewHTTPError(http.StatusForbidden, fmt.Sprintf(
				"no version of %s matching that pin has passed %v; pinning past a passed: gate needs the admin role (an operator may pin a version that passed, or trigger without a pin)",
				resource, upstream))
		}
	}

	return nil
}

// pinMatches is resource.SelectVersion's matching, which depguard keeps out of this package: every pinned field present, compared as text.
func pinMatches(version map[string]any, pin map[string]string) bool {
	for key, want := range pin {
		got, ok := version[key]
		if !ok || fmt.Sprint(got) != want {
			return false
		}
	}

	return true
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

	since := time.Now().UTC()

	job, err := s.queueRerun(c.Request().Context(), pipeline, c.Param("run"), "web")
	if err != nil {
		return err
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/p/%s/jobs/%s/follow?since=%d",
		pipeline.Slug, job, since.UnixMilli()))
}

// queueRerun queues a retry of a recorded run and names the job it belongs to.
func (s *Server) queueRerun(ctx context.Context, pipeline *Pipeline, runID, from string) (string, error) {
	run, ok, err := pipeline.Store.FindRunRow(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("web: %w", err)
	}

	if !ok {
		return "", echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	err = refuseRerun(ctx, pipeline, run)
	if err != nil {
		return "", err
	}

	err = s.runner.EnqueueRerun(ctx, pipeline.Pipeline, run.JobName, "retry ("+from+")", run.ID)
	if err != nil {
		return "", fmt.Errorf("web: %w", err)
	}

	return run.JobName, nil
}

// handleResume continues a failed run from the step that failed, in its own tree, and lands on that run's page: it is the same run, so the follow page, which waits for a NEW one, would wait forever.
func (s *Server) handleResume(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	pipeline := pipelineOf(c)

	run, err := s.queueResume(c.Request().Context(), pipeline, c.Param("run"), "web")
	if err != nil {
		return err
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+pipeline.Slug+"/runs/"+run)
}

func (s *Server) handleAPIResume(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	_, err = s.queueResume(c.Request().Context(), target, c.Param("run"), "cli")
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusAccepted) //nolint:wrapcheck // echo's own response error
}

// queueResume is queueRerun's refusals plus the runner's: a run that cannot be resumed is a 409 naming Retry, never a silent build from the top.
func (s *Server) queueResume(ctx context.Context, pipeline *Pipeline, runID, from string) (string, error) {
	run, ok, err := pipeline.Store.FindRunRow(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("web: %w", err)
	}

	if !ok {
		return "", echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	err = refuseRerun(ctx, pipeline, run)
	if err != nil {
		return "", err
	}

	err = s.runner.EnqueueResume(ctx, pipeline.Pipeline, run.JobName, "resume ("+from+")", run.ID)
	if err != nil {
		if errors.Is(err, scheduler.ErrNotResumable) {
			return "", echo.NewHTTPError(http.StatusConflict, err.Error())
		}

		return "", fmt.Errorf("web: %w", err)
	}

	return run.ID, nil
}

// TriggerRequest is what `steps runs trigger` sends: nothing, for the job's ordinary trigger, or the version fields to pin it to.
type TriggerRequest struct {
	Pin   map[string]string `json:"pin,omitempty"`
	Force bool              `json:"force,omitempty"`
}

// handleAPITrigger answers 202: the run is queued, not started.
func (s *Server) handleAPITrigger(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	var req TriggerRequest

	// An empty body is the ordinary trigger, so only a body that is there has to parse. A field this daemon does not know is refused: read as absent, a misspelled or newer-client pin would build the newest version instead.
	decoder := json.NewDecoder(c.Request().Body)
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&req)
	if err != nil && !errors.Is(err, io.EOF) {
		return echo.NewHTTPError(http.StatusBadRequest, "the request body is not a trigger request: "+err.Error())
	}

	err = s.queueTrigger(c.Request().Context(), target, c.Param("job"), "cli", req.Force, req.Pin, s.roleOf(c))
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusAccepted) //nolint:wrapcheck // echo's own response error
}

func (s *Server) handleAPIRerun(c *echo.Context) error {
	err := s.requireControls()
	if err != nil {
		return err
	}

	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	_, err = s.queueRerun(c.Request().Context(), target, c.Param("run"), "cli")
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusAccepted) //nolint:wrapcheck // echo's own response error
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
