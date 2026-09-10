package web

// Two routes per verb because a browser is sent back to a page and a terminal wants a status; the refusals are one function each so the two cannot drift.

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v4"
)

// abortRun is nil when the run was told to stop.
func (s *Server) abortRun(ctx context.Context, target *Pipeline, runID string) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	if s.runner.Abort(target, runID) {
		return nil
	}

	run, found, err := target.Store.FindRunRow(ctx, runID)
	if err != nil {
		return echoError(err)
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	// Started by a `steps run` against this file, or by a daemon that died mid-build — either way there is no context here to cancel.
	if run.Status == "running" {
		return echo.NewHTTPError(http.StatusConflict, "this run is not running on this daemon")
	}

	return echo.NewHTTPError(http.StatusConflict, "this run already "+statusWord(run.Status))
}

// abortQueued is nil when the job's queued run was dropped.
func (s *Server) abortQueued(ctx context.Context, target *Pipeline, jobName string) error {
	if s.runner == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	dropped, err := s.runner.AbortQueued(ctx, target, jobName)
	if err != nil {
		return echoError(err)
	}

	if !dropped {
		return echo.NewHTTPError(http.StatusConflict, "nothing is queued for "+jobName)
	}

	return nil
}

func (s *Server) handleAbortRun(c echo.Context) error {
	target := pipelineOf(c)

	err := s.abortRun(c.Request().Context(), target, c.Param("run"))
	if err != nil {
		return err
	}

	// Back to the run, which reads aborted once its hooks have finished — the page's closing reload shows it.
	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, "/p/"+target.Slug+"/runs/"+c.Param("run"))
}

func (s *Server) handleAbortQueued(c echo.Context) error {
	target := pipelineOf(c)

	err := s.abortQueued(c.Request().Context(), target, c.Param("job"))
	if err != nil {
		return err
	}

	//nolint:wrapcheck // as above
	return c.Redirect(http.StatusSeeOther, "/p/"+target.Slug+"/jobs/"+c.Param("job"))
}

// handleAPIAbortRun answers 202, not 204: the run is still unwinding through its hooks when this returns.
func (s *Server) handleAPIAbortRun(c echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
	}

	err := s.abortRun(c.Request().Context(), target, c.Param("run"))
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusAccepted) //nolint:wrapcheck // as above
}

func (s *Server) handleAPIAbortQueued(c echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
	}

	err := s.abortQueued(c.Request().Context(), target, c.Param("job"))
	if err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent) //nolint:wrapcheck // as above
}
