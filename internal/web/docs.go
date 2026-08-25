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

	"github.com/alecthomas/chroma/v2"
	"github.com/labstack/echo/v4"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"

	"github.com/jtarchie/steps/docs"
)

// docsCodeStyle colors fenced code with the UI's own palette (app.css's
// :root) instead of a stock chroma theme. The stock themes break the one
// rule this UI's design is built on — ANSI-16 semantics, where red means
// failure — by painting every YAML key red on a cool-gray ground. Here keys
// are blue (data), strings green, comments dim, on the panel the rest of
// the UI already uses.
var docsCodeStyle = chroma.MustNewStyle("steps", chroma.StyleEntries{
	chroma.Background:        "#d8d5c9 bg:#171a16", // --fg on --panel
	chroma.Comment:           "italic #83887b",     // --dim
	chroma.Keyword:           "#b98fcc",            // --magenta
	chroma.NameTag:           "#7aa4d9",            // --blue: YAML keys
	chroma.NameAttribute:     "#7aa4d9",
	chroma.NameFunction:      "#6fbcb4", // --cyan
	chroma.LiteralString:     "#84c06d", // --green
	chroma.LiteralNumber:     "#d9a94a", // --yellow
	chroma.KeywordType:       "#6fbcb4",
	chroma.NameConstant:      "#d9a94a",
	chroma.Operator:          "#83887b",
	chroma.Punctuation:       "#83887b",
	chroma.GenericSubheading: "#83887b",
})

// markdown is the shared converter: GFM for the docs' tables, heading ids so
// the pages' #anchor cross-links resolve, and server-side chroma
// highlighting in the UI's own style.
var markdown = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		highlighting.NewHighlighting(highlighting.WithCustomStyle(docsCodeStyle)),
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
)

// githubIDs generates heading ids with docs.Slug — GitHub's algorithm —
// instead of goldmark's default, which folds "_" into "-" and strands every
// hand-written anchor containing a field name like max_visits. One instance
// per page render, so duplicate headings dedupe per page.
type githubIDs struct{ seen map[string]int }

func newGithubIDs() *githubIDs { return &githubIDs{seen: map[string]int{}} }

func (g *githubIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	slug := docs.Slug(string(value))
	if slug == "" {
		slug = "heading"
	}

	if n, dup := g.seen[slug]; dup {
		g.seen[slug] = n + 1
		slug = fmt.Sprintf("%s-%d", slug, n)
	} else {
		g.seen[slug] = 1
	}

	return []byte(slug)
}

func (g *githubIDs) Put(value []byte) { g.seen[string(value)] = 1 }

// handleDocsIndex lands /docs on the index page.
func (s *Server) handleDocsIndex(c echo.Context) error {
	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusFound, "/docs/README.md")
}

// tocEntry is one h2 in the page's table of contents.
type tocEntry struct {
	Text string
	ID   string
}

// handleDocs renders one embedded doc page. The route keeps the .md suffix
// so the pages' own relative links (`[resources.md](resources.md)`) resolve
// with no rewriting.
func (s *Server) handleDocs(c echo.Context) error {
	name := c.Param("page")

	body, err := docs.Page(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such doc page — /docs lists them all")
	}

	var rendered bytes.Buffer

	// A fresh ID generator per render, so duplicate headings dedupe within
	// the page and never across requests.
	ctx := parser.NewContext(parser.WithIDs(newGithubIDs()))

	err = markdown.Convert(body, &rendered, parser.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("web: could not render %s: %w", name, err)
	}

	// The in-page TOC is the h2s — section-level wayfinding for pages that
	// render tens of thousands of pixels tall. Fewer than three sections
	// isn't a page that needs a map.
	var toc []tocEntry

	for _, heading := range docs.Headings(string(body)) {
		if heading.Level == 2 {
			toc = append(toc, tocEntry{Text: heading.Text, ID: heading.ID})
		}
	}

	if len(toc) < 3 {
		toc = nil
	}

	return c.Render(http.StatusOK, "docs", map[string]any{ //nolint:wrapcheck // render errors surface through the shared error handler
		"Nav":    s.globalNav(c),
		"Title":  "docs: " + strings.TrimSuffix(name, ".md"),
		"Groups": docs.Groups(),
		"Name":   name,
		"TOC":    toc,
		//nolint:gosec // the HTML is rendered from this repo's own embedded docs, not user input
		"HTML": template.HTML(rendered.String()),
	})
}
