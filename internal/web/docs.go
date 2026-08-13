package web

// The /docs routes serve the embedded documentation (package docs — the same
// pages `steps docs` renders in a terminal) as HTML. Markdown is converted
// server-side with goldmark; there is no client-side renderer to ship, and
// the pages stay readable with curl.

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/jtarchie/steps/docs"
)

// markdown is the shared converter: GFM for the docs' tables, auto heading
// ids so the pages' own #anchor cross-links resolve, and server-side chroma
// highlighting in a style that sits on the UI's warm-dark terminal ground.
var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(highlighting.WithStyle("gruvbox")),
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// handleDocsIndex lands /docs on the index page.
func (s *Server) handleDocsIndex(c echo.Context) error {
	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusFound, "/docs/README.md")
}

// handleDocs renders one embedded doc page. The route keeps the .md suffix
// so the pages' own relative links (`[resources.md](resources.md)`) resolve
// with no rewriting.
func (s *Server) handleDocs(c echo.Context) error {
	name := c.Param("page")

	body, err := docs.Page(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such doc page")
	}

	var rendered bytes.Buffer

	err = markdown.Convert(body, &rendered)
	if err != nil {
		return fmt.Errorf("web: could not render %s: %w", name, err)
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "docs", map[string]any{
		"Nav":   s.nav(c),
		"Title": "docs: " + strings.TrimSuffix(name, ".md"),
		"Pages": docs.Pages(),
		"Name":  name,
		//nolint:gosec // the HTML is rendered from this repo's own embedded docs, not user input
		"HTML": template.HTML(rendered.String()),
	})
}
