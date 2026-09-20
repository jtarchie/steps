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

	"github.com/alecthomas/chroma/v3"
	"github.com/labstack/echo/v5"
	highlighting "github.com/yuin/goldmark-highlighting/v3"
	"github.com/yuin/goldmark/v2/ast"
	"github.com/yuin/goldmark/v2/extension"
	"github.com/yuin/goldmark/v2/parser"
	"github.com/yuin/goldmark/v2/renderer/html"

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
	// The rest of Generic*, added for diff and markdown detection
	// (internal/web/detect.go): without these a highlighted diff or heading
	// rendered flat, one colour, which made the single most valuable
	// detection buy nothing.
	chroma.GenericDeleted:  "#e0645a",      // --red: a removed line — the ANSI-16 reading this whole UI is built on
	chroma.GenericInserted: "#84c06d",      // --green: an added line
	chroma.GenericHeading:  "bold #7aa4d9", // --blue: `diff --git`/`Index:` lines, a markdown h1
	chroma.GenericStrong:   "bold",         // markdown **bold**, a diff's ! line
	chroma.GenericEmph:     "italic",       // markdown _emph_
})

// docsParser and docsRenderer are the shared converter: GFM for the docs' tables, heading ids so the pages' #anchor cross-links resolve, and server-side chroma highlighting in the UI's own style.
//
// Two values rather than one, because goldmark has no combined Markdown type any more — an extension declares its parser half and its renderer half separately.
//
// The highlighter has a parser half too and it is deliberately NOT here: all it does is read `{...}` attributes off a fence info line, which no page in the corpus writes, so it would be a walk of every page AST per render that no test could break.
var docsParser = parser.New(
	parser.WithExtensions(extension.GFMParser),
	parser.WithAutoHeadingID(),
)

var docsRenderer = html.New(
	html.WithExtensions(
		extension.GFMHTMLRenderer,
		highlighting.NewHTMLRenderer(highlighting.WithCustomStyle(docsCodeStyle)),
	),
)

// githubIDs generates heading ids with docs.Slug — GitHub's algorithm —
// instead of goldmark's default, which folds "_" into "-" and strands every
// hand-written anchor containing a field name like max_visits.
//
// Only the BASE id: goldmark's own parser.IDs appends the -1, -2 suffix for a repeated heading, which is what this type used to carry a map to do and is the same numbering either way. Uniqueness is per parse context, so it is per page render — see handleDocs.
type githubIDs struct{}

func (githubIDs) Generate(value []byte, _ ast.NodeKind) []byte {
	slug := docs.Slug(string(value))
	if slug == "" {
		slug = "heading"
	}

	return []byte(slug)
}

// renderDocsMarkdown converts one page. A function rather than three lines inside the handler because the anchors and the highlighting are the whole contract of this file, and a test that spells the wiring out a second time proves only that goldmark works.
//
// A fresh context per render, which is what carries the set of ids already taken: duplicate headings dedupe within the page and never across requests.
func renderDocsMarkdown(body []byte) (string, error) {
	var rendered bytes.Buffer

	ctx := parser.NewContext(parser.WithIDGenerator(githubIDs{}))

	err := docsRenderer.Render(&rendered, body, docsParser.Parse(body, parser.WithContext(ctx)))
	if err != nil {
		return "", fmt.Errorf("rendering markdown: %w", err)
	}

	return rendered.String(), nil
}

// handleDocsIndex lands /docs on the index page.
func (s *Server) handleDocsIndex(c *echo.Context) error {
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
func (s *Server) handleDocs(c *echo.Context) error {
	name := c.Param("page")

	body, err := docs.Page(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "no such doc page — /docs lists them all")
	}

	rendered, err := renderDocsMarkdown(body)
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
		"HTML": template.HTML(rendered),
	})
}
