package web

// The /p/:pipeline/config/:sha route: the configuration a run executed,
// verbatim.
//
// A run recorded which configuration it was started from before anything
// could show it, and the file on disk is only ever its newest version — so
// the run that broke may have executed something no longer readable
// anywhere. This serves it back from the store.

import (
	"bytes"
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

	body, err := highlightYAML(revision.Source)
	if err != nil {
		return err
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "config", map[string]any{
		"Nav":  s.nav(c),
		"SHA":  revision.SHA,
		"Body": body,
	})
}

// highlightYAML colors a pipeline the way /docs colors its examples, by
// handing the markdown renderer a fenced block.
//
// Reusing that pipeline rather than driving chroma directly: it already
// carries this UI's palette (see docsCodeStyle), and a second highlighter
// configured separately is a second thing to keep in agreement with the
// stylesheet.
func highlightYAML(source string) (template.HTML, error) {
	var rendered bytes.Buffer

	err := markdown.Convert([]byte("```yaml\n"+source+"\n```\n"), &rendered)
	if err != nil {
		return "", fmt.Errorf("web: could not render configuration: %w", err)
	}

	//nolint:gosec // the source is this pipeline's own YAML, rendered as a code block rather than as markup
	return template.HTML(rendered.String()), nil
}
