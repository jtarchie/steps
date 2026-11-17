package web

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/jtarchie/steps/internal/runview"
	"github.com/labstack/echo/v5"
)

// keptOutputRoute names a run, a step and its n-th kept output; handleError answers its failures in plain text.
const keptOutputRoute = "/p/:pipeline/runs/:run/steps/:step/output/:n"

// keptOutputN is which stored output holds output i of step whole, false when that output was not elided.
func keptOutputN(step *runview.Step, i int) (int, bool) {
	if i >= len(step.KeptOutputs) {
		return 0, false
	}

	n, err := strconv.Atoi(step.KeptOutputs[i])

	return n, err == nil && n >= 0
}

// keptOutputURL links output i of step to its stored whole, or is "" when that output was not elided.
func keptOutputURL(slug, runID string, step *runview.Step, i int) string {
	n, kept := keptOutputN(step, i)
	if !kept {
		return ""
	}

	return fmt.Sprintf("/p/%s/runs/%s/steps/%d/output/%d", slug, runID, step.ID, n)
}

// handleKeptOutput serves a kept output as text, after the same pipeline checks as the run page and the same run lookup.
func (s *Server) handleKeptOutput(c *echo.Context) error {
	pipeline := pipelineOf(c)

	run, ok, err := pipeline.Store.FindRunRow(c.Request().Context(), c.Param("run"))
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	gone := echo.NewHTTPError(http.StatusNotFound, "the full output is no longer stored")

	step, stepErr := strconv.ParseInt(c.Param("step"), 10, 64)
	n, nErr := strconv.Atoi(c.Param("n"))

	if stepErr != nil || nErr != nil {
		return gone
	}

	first := true

	found, err := pipeline.Store.StepOutput(c.Request().Context(), run.ID, step, n, func(chunk []byte) error {
		if first {
			first = false

			c.Response().Header().Set(echo.HeaderContentType, "text/plain; charset=utf-8")
			c.Response().WriteHeader(http.StatusOK)
		}

		_, err := c.Response().Write(chunk)

		return err //nolint:wrapcheck // a write error surfaces through the shared error handler
	})
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !found {
		return gone
	}

	return nil
}
