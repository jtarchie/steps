package web

// `steps run --target` (#283): a one-off handed to the daemon, and the reads a terminal follows any run through — this daemon's or one on another host — without the state file.

import (
	"net/http"
	"strconv"

	"github.com/labstack/echo/v5"
)

// oneOffPath is outside /api/pipelines/:pipeline because the daemon names the pipeline, not the sender: a name the sender chose could be one already served, and a one-off must never replace it.
const oneOffPath = "/api/one-offs"

// maxAPIRuns bounds a run listing; a follower asks for one.
const maxAPIRuns = 50

func (s *Server) handleAPIOneOff(c *echo.Context) error {
	manager := s.held()
	if manager == nil {
		return echo.NewHTTPError(http.StatusForbidden, "this server is read-only")
	}

	var req OneOffRequest

	err := c.Bind(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "could not read the uploaded configuration: "+err.Error())
	}

	if req.Source == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "the uploaded configuration is empty")
	}

	result, err := manager.OneOff(c.Request().Context(), req)
	if err != nil {
		return setError(err)
	}

	//nolint:wrapcheck // echo writes the response; an encoding failure is reported verbatim
	return c.JSON(http.StatusCreated, result)
}

// handleAPIRuns is the newest runs, of ?job= when given.
func (s *Server) handleAPIRuns(c *echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil || limit < 1 || limit > maxAPIRuns {
		limit = 1
	}

	runs, err := target.Store.ListRuns(c.Request().Context(), c.QueryParam("job"), limit)
	if err != nil {
		return echoError(err)
	}

	//nolint:wrapcheck // as above
	return c.JSON(http.StatusOK, runs)
}

func (s *Server) handleAPIRun(c *echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	run, found, err := target.Store.FindRunRow(c.Request().Context(), c.Param("run"))
	if err != nil {
		return echoError(err)
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	//nolint:wrapcheck // as above
	return c.JSON(http.StatusOK, run)
}

// handleAPIRunEvents is one page of a run's events after ?after=, as recorded. Paged requests rather than a stream: each page passes the door again, so a revoked reader stops at the next poll with nothing to re-check mid-stream, and the follower's own end-of-run logic is the one `steps runs follow` already runs against a local file.
func (s *Server) handleAPIRunEvents(c *echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return s.notServed(c, c.Param("pipeline"))
	}

	after, _ := strconv.ParseInt(c.QueryParam("after"), 10, 64)

	limit, err := strconv.Atoi(c.QueryParam("limit"))
	if err != nil || limit < 1 || limit > liveBatch {
		limit = liveBatch
	}

	rows, err := target.Store.RunEvents(c.Request().Context(), c.Param("run"), after, limit)
	if err != nil {
		return echoError(err)
	}

	//nolint:wrapcheck // as above
	return c.JSON(http.StatusOK, rows)
}
