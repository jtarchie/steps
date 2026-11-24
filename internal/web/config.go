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
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/alecthomas/chroma/v3"
	"github.com/alecthomas/chroma/v3/lexers"
	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/config"
)

// handleConfig renders one recorded configuration.
func (s *Server) handleConfig(c *echo.Context) error {
	pipeline := pipelineOf(c)
	sha := c.Param("sha")

	revision, found, err := pipeline.Store.FindRevision(c.Request().Context(), sha)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no configuration with that hash has run here")
	}

	type include struct {
		Path  string
		Body  template.HTML
		Lines string
	}

	includes := make([]include, 0, len(revision.Includes))
	for _, path := range slices.Sorted(maps.Keys(revision.Includes)) {
		includes = append(includes, include{Path: path, Body: highlightCode(revision.Includes[path], path), Lines: lineNumbers(revision.Includes[path])})
	}

	loc := s.renderer.localeOf(c)

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "config", map[string]any{
		"Nav":      s.nav(c),
		"SHA":      revision.SHA,
		"Title":    loc.text("config.crumb", shortID(revision.SHA)),
		"Body":     highlightConfig(revision.Source),
		"Lines":    lineNumbers(revision.Source),
		"Includes": includes,
		"Crumbs":   []crumb{{Label: loc.text("tab.jobs"), URL: "/p/" + pipeline.Slug}, {Label: loc.text("config.crumb", shortID(revision.SHA))}},
	})
}

// configSections names the id prefix of each top-level list whose entries are landing places. No prefix is another's followed by a hyphen, so no name under one kind can spell an id under another.
var configSections = map[string]string{
	"resource_types": "resourcetype",
	"resources":      "resource",
	"agents":         "agent",
	"tasks":          "task",
	"mcp_servers":    "mcp-server",
	"mcp_exports":    "mcp-export",
	"jobs":           "job",
}

// configAnchor is the id of one entry's block on the config page; configURL is the only other place that may spell it.
func configAnchor(kind, name string) string {
	return kind + "-" + name
}

// configURL addresses one entry's block in one recorded configuration.
func configURL(slug, sha, kind, name string) string {
	return "/p/" + slug + "/config/" + sha + "#" + url.PathEscape(configAnchor(kind, name))
}

// configMarks is the ids of the blocks opening on each line of source, and how many close at the end of each.
func configMarks(source string) (opens map[int][]string, closes map[int]int) {
	opens, closes = map[int][]string{}, map[int]int{}

	for _, block := range config.SourceBlocks(source) {
		if kind, ok := configSections[block.Section]; ok {
			opens[block.Start] = append(opens[block.Start], configAnchor(kind, block.Name))
			closes[block.End]++
		}
	}

	return opens, closes
}

// highlightConfig colors a pipeline with the lexer and palette every other code block on this site uses, each named entry wrapped in a block carrying its configAnchor. It adds tags and nothing else: the text of the page stays the recorded source.
//
// Chroma directly, NOT through the markdown renderer. Wrapping the source in
// a synthetic ```yaml fence looked like reuse and was a corruption bug: a line
// in the file that is itself ``` at three spaces of indent or fewer — which a
// block scalar at two spaces produces, and agent prompts are full of — CLOSES
// the fence, and everything after it renders as markdown. Headings, dropped
// tags, half a pipeline. On the one page whose whole promise is that it shows
// what the run was told to do, verbatim.
func highlightConfig(source string) (result template.HTML) {
	defer func() {
		if r := recover(); r != nil {
			//nolint:gosec // G203: escaped, no markup added
			result = template.HTML(template.HTMLEscapeString(source))
		}
	}()

	tokens, err := chroma.Coalesce(lexers.Get("yaml")).Tokenise(nil, source)
	if err != nil {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(source))
	}

	writer := configWriter{line: 1, atStart: true}
	writer.opens, writer.closes = configMarks(source)

	for token := range tokens {
		class := styledClass(docsCodeStyle, token.Type)

		// A token is cut at each newline because a block is whole lines and a block scalar is one token many lines long.
		for piece := range strings.SplitAfterSeq(token.Value, "\n") {
			writer.write(class, piece)
		}
	}

	writer.out.WriteString(strings.Repeat("</span>", writer.depth))

	//nolint:gosec // G203: every piece of source text is escaped; the markup is configWriter's own spans
	return template.HTML(writer.out.String())
}

// configWriter writes a source a line-piece at a time, opening and closing the blocks configMarks placed.
type configWriter struct {
	out     strings.Builder
	opens   map[int][]string
	closes  map[int]int
	line    int
	depth   int
	atStart bool
}

func (w *configWriter) write(class, piece string) {
	if piece == "" {
		return
	}

	if w.atStart {
		for _, id := range w.opens[w.line] {
			w.out.WriteString(`<span class="cfgblock" id="` + template.HTMLEscapeString(id) + `">`)

			w.depth++
		}

		w.atStart = false
	}

	// A block closes BEFORE its last newline: it is an inline-block, and a line break left inside one would put whatever follows on the same line.
	text, newline := strings.CutSuffix(piece, "\n")

	if class != "" && text != "" {
		w.out.WriteString(`<span class="hl-` + class + `">` + template.HTMLEscapeString(text) + `</span>`)
	} else {
		w.out.WriteString(template.HTMLEscapeString(text))
	}

	if !newline {
		return
	}

	for range min(w.closes[w.line], w.depth) {
		w.out.WriteString("</span>")

		w.depth--
	}

	w.out.WriteString("\n")

	w.line++
	w.atStart = true
}

// lineNumbers is the gutter for source: one number per line, a final newline ending the last line rather than opening another.
func lineNumbers(source string) string {
	count := strings.Count(strings.TrimSuffix(source, "\n"), "\n") + 1

	var out strings.Builder

	for n := 1; n <= count; n++ {
		if n > 1 {
			out.WriteByte('\n')
		}

		out.WriteString(strconv.Itoa(n))
	}

	return out.String()
}
