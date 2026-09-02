package web

// The /p/:pipeline/config/:sha route: the configuration a run executed,
// verbatim.
//
// A run recorded which configuration it was started from before anything
// could show it, and the file on disk is only ever its newest version — so
// the run that broke may have executed something no longer readable
// anywhere. This serves it back from the store.

import (
	"fmt"
	"html/template"
	"net/http"

	"github.com/labstack/echo/v4"
)

// handleConfig renders one recorded configuration.
func (s *Server) handleConfig(c echo.Context) error {
	pipeline := pipelineOf(c)
	sha := c.Param("sha")

	revision, found, err := pipeline.Store.FindRevision(c.Request().Context(), sha)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no configuration with that hash has run here")
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "config", map[string]any{
		"Nav":    s.nav(c),
		"SHA":    revision.SHA,
		"Title":  "config " + shortID(revision.SHA),
		"Body":   highlightYAML(revision.Source),
		"Crumbs": []crumb{{Label: "jobs", URL: "/p/" + pipeline.Slug}, {Label: "config " + shortID(revision.SHA)}},
	})
}

// highlightYAML colors a pipeline with the same lexer and palette every other
// code block on this site uses.
//
// Chroma directly, NOT through the markdown renderer. Wrapping the source in
// a synthetic ```yaml fence looked like reuse and was a corruption bug: a line
// in the file that is itself ``` at three spaces of indent or fewer — which a
// block scalar at two spaces produces, and agent prompts are full of — CLOSES
// the fence, and everything after it renders as markdown. Headings, dropped
// tags, half a pipeline. On the one page whose whole promise is that it shows
// what the run was told to do, verbatim.
func highlightYAML(source string) template.HTML {
	return highlightCode(source, "yaml")
}
