package web

// The /p/:pipeline/config/:sha route: the configuration a run executed,
// verbatim.
//
// A run recorded which configuration it was started from before anything
// could show it, and the file on disk is only ever its newest version — so
// the run that broke may have executed something no longer readable
// anywhere. This serves it back from the store.

import (
	"context"
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
	"github.com/jtarchie/steps/internal/runview"
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
		ID    string
		Body  template.HTML
		Lines string
	}

	includes := make([]include, 0, len(revision.Includes))
	for _, path := range slices.Sorted(maps.Keys(revision.Includes)) {
		includes = append(includes, include{Path: path, ID: includeAnchor(path), Body: highlightCode(revision.Includes[path], path), Lines: lineNumbers(revision.Includes[path])})
	}

	loc := s.renderer.localeOf(c)

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "config", map[string]any{
		"Nav":      s.nav(c),
		"SHA":      revision.SHA,
		"Title":    loc.text("config.crumb", shortID(revision.SHA)),
		"Body":     highlightConfig(revision.Source, configLinker{slug: pipeline.Slug, includes: revision.Includes, label: loc.text}),
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

// configStepAnchor is the id of one step's block inside its job's: path is config.SourceBlock's, the keys that reach the step.
func configStepAnchor(job, path string) string {
	return configAnchor("job", job) + ":" + path
}

// configStepURL addresses one step's block in one recorded configuration.
func configStepURL(slug, sha, job, path string) string {
	return "/p/" + slug + "/config/" + sha + "#" + url.PathEscape(configStepAnchor(job, path))
}

// includeAnchor is the id of one include file's heading under the pipeline's source.
func includeAnchor(path string) string {
	return "include-" + path
}

// configLinker says where a name in one pipeline's configuration leads. The zero value links only within the page and labels nothing.
type configLinker struct {
	slug     string
	includes map[string]string
	label    func(key string, args ...any) string
}

// selfPages is the page each entry with one lives at, under /p/<slug>.
var selfPages = map[string]string{"jobs": "/jobs/", "resources": "/resources/", "mcp_exports": "/mcp/exports/"}

// anchor is the opening tag of ref's link, or "" for a reference with nowhere to lead.
func (l configLinker) anchor(ref config.SourceRef) string {
	var href, class, key string

	switch ref.Kind {
	case config.RefEntry:
		href, class, key = "#"+url.PathEscape(configAnchor(configSections[ref.Section], ref.Name)), "cfgref", "config.ref.def"
	case config.RefSelf:
		if l.slug == "" {
			return ""
		}

		href, class, key = "/p/"+l.slug+selfPages[ref.Section]+url.PathEscape(ref.Name), "cfgref out", "config.ref.page"
	case config.RefInclude:
		if _, shown := l.includes[ref.Name]; !shown {
			return ""
		}

		href, class, key = "#"+url.PathEscape(includeAnchor(ref.Name)), "cfgref", "config.ref.include"
	}

	label := ""
	if l.label != nil {
		label = ` aria-label="` + template.HTMLEscapeString(l.label(key, ref.Name)) + `"`
	}

	return `<a class="` + class + `" href="` + template.HTMLEscapeString(href) + `"` + label + `>`
}

// highlightConfig colors a pipeline with the lexer and palette every other code block on this site uses, each named entry and each step wrapped in a block carrying its anchor, and each name that refers to one made a link. It adds tags and nothing else: the text of the page stays the recorded source.
//
// Chroma directly, NOT through the markdown renderer. Wrapping the source in
// a synthetic ```yaml fence looked like reuse and was a corruption bug: a line
// in the file that is itself ``` at three spaces of indent or fewer — which a
// block scalar at two spaces produces, and agent prompts are full of — CLOSES
// the fence, and everything after it renders as markdown. Headings, dropped
// tags, half a pipeline. On the one page whose whole promise is that it shows
// what the run was told to do, verbatim.
func highlightConfig(source string, links configLinker) (result template.HTML) {
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

	writer := newConfigWriter(config.MapSource(source), links)

	for token := range tokens {
		class := styledClass(docsCodeStyle, token.Type)

		// A token is cut at each newline because a block is whole lines and a block scalar is one token many lines long.
		for piece := range strings.SplitAfterSeq(token.Value, "\n") {
			writer.write(class, piece)
		}
	}

	writer.out.WriteString(strings.Repeat("</span>", writer.depth))

	//nolint:gosec // G203: every piece of source text is escaped; the markup is configWriter's own spans and links
	return template.HTML(writer.out.String())
}

// configWriter writes a source a line-piece at a time, opening and closing the blocks and linking the references a config.SourceMap placed.
type configWriter struct {
	out   strings.Builder
	links configLinker
	// opens is the ids of the blocks starting on each line, outermost first; closes is how many end with it; refs is the references on it, left to right.
	opens  map[int][]string
	closes map[int]int
	refs   map[int][]config.SourceRef
	line   int
	// col is how many bytes of the current line are written.
	col     int
	depth   int
	atStart bool
}

func newConfigWriter(source config.SourceMap, links configLinker) *configWriter {
	writer := &configWriter{links: links, opens: map[int][]string{}, closes: map[int]int{}, refs: map[int][]config.SourceRef{}, line: 1, atStart: true}

	for _, block := range source.Blocks {
		kind, ok := configSections[block.Section]
		if !ok {
			continue
		}

		id := configAnchor(kind, block.Name)
		if block.Path != "" {
			id = configStepAnchor(block.Name, block.Path)
		}

		writer.opens[block.Start] = append(writer.opens[block.Start], id)
		writer.closes[block.End]++
	}

	for _, ref := range source.Refs {
		writer.refs[ref.Line] = append(writer.refs[ref.Line], ref)
	}

	return writer
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
		w.out.WriteString(`<span class="hl-` + class + `">`)
		w.text(text)
		w.out.WriteString(`</span>`)
	} else {
		w.text(text)
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
	w.col = 0
	w.atStart = true
}

// text writes one run of a line, a link around each reference that lies wholly inside it: a reference the lexer cut in two is left as text.
func (w *configWriter) text(text string) {
	end := w.col + len(text)

	for _, ref := range w.refs[w.line] {
		if ref.Start < w.col || ref.End > end {
			continue
		}

		open := w.links.anchor(ref)
		if open == "" {
			continue
		}

		w.out.WriteString(template.HTMLEscapeString(text[:ref.Start-w.col]) + open + template.HTMLEscapeString(text[ref.Start-w.col:ref.End-w.col]) + "</a>")

		text, w.col = text[ref.End-w.col:], ref.End
	}

	w.out.WriteString(template.HTMLEscapeString(text))

	w.col = end
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

// attachStepConfig gives each of a run's rows the place its step sits in the configuration the run executed.
//
// A row is placed by position where the events carry one, and that is the top of the plan only: a get publishes the rest of the plan as its children, their indexes counted from the step after it, so a plan-level row's position is its index past every get above it. A row inside a block carries its block's index and no position of its own, so it is placed as the ONE step of its kind and name beneath wherever its block landed, and at the block when there is none or more than one: a link to the enclosing step is right, and a guess between twins is not. A hook's row has no plan index and names its hook instead (internal/pipeline's hookRowName: "on_failure · task tidy"), which is the key it sits under in whatever it hangs off — the step it landed on, or the job.
func (s *Server) attachStepConfig(ctx context.Context, view *runView, pipeline *Pipeline) {
	run := view.Run
	if run.ConfigSHA == "" {
		return
	}

	placer := stepPlacer{urls: map[int64]string{}, slug: pipeline.Slug, sha: run.ConfigSHA, job: run.JobName}

	for _, block := range s.revisionBlocks(ctx, pipeline, run.ConfigSHA) {
		if block.Section == "jobs" && block.Name == run.JobName && block.Path != "" {
			placer.steps = append(placer.steps, block)
		}
	}

	for _, step := range view.Steps {
		if step.ParentID == 0 {
			placer.place(step, "", 0, true)
		}
	}

	view.StepConfig = placer.urls
}

type stepPlacer struct {
	steps          []config.SourceBlock
	urls           map[int64]string
	slug, sha, job string
}

// place records step's URL and places its children. scope is the path of the block step sits in, and inPlan says step is a plan-level row, whose position is base plus its index.
func (p *stepPlacer) place(step *runview.Step, scope string, base int, inPlan bool) {
	path := p.within(step, scope)
	if inPlan {
		path = p.exact("plan." + strconv.Itoa(base+step.Index))
	}

	if hook, _, _ := strings.Cut(step.Name, " · "); step.Kind == "hook" {
		path = p.exact(strings.TrimPrefix(scope+"."+hook, "."))
	}

	if path != "" {
		p.urls[step.ID] = configStepURL(p.slug, p.sha, p.job, path)
	}

	for _, child := range step.Children {
		switch {
		case step.Kind != "get":
			p.place(child, path, 0, false)
		case inPlan:
			p.place(child, "", base+step.Index+1, true)
		default:
			p.place(child, scope, 0, false)
		}
	}
}

// named reports whether a run would publish block's row under name: its own, or for a try: — which has none — that of the step it wraps.
func (p *stepPlacer) named(block config.SourceBlock, name string) bool {
	if block.StepKind != "try" {
		return block.StepName == name
	}

	for _, wrapped := range p.steps {
		if wrapped.Path == block.Path+".try" {
			return wrapped.StepName == name
		}
	}

	return false
}

// exact is path when the job has a step there.
func (p *stepPlacer) exact(path string) string {
	for _, block := range p.steps {
		if block.Path == path {
			return path
		}
	}

	return ""
}

// within is the one step of step's kind and name under scope, or scope itself.
func (p *stepPlacer) within(step *runview.Step, scope string) string {
	found, matches := scope, 0

	for _, block := range p.steps {
		if scope != "" && strings.HasPrefix(block.Path, scope+".") && block.StepKind == step.Kind && p.named(block, step.Name) {
			found = block.Path
			matches++
		}
	}

	if matches != 1 {
		return scope
	}

	return found
}

// revisionBlocks is the landing places of one recorded configuration, read and parsed once: a revision is its hash, and the live stream asks on every flush.
func (s *Server) revisionBlocks(ctx context.Context, pipeline *Pipeline, sha string) []config.SourceBlock {
	key := pipeline.Slug + "\x00" + sha

	if cached, ok := s.stepBlocks.Load(key); ok {
		blocks, _ := cached.([]config.SourceBlock)

		return blocks
	}

	// Best-effort, like the spend beside it: a run page without these links is worth more than one that fails over them.
	revision, found, err := pipeline.Store.FindRevision(ctx, sha)
	if err != nil || !found {
		return nil
	}

	blocks := config.MapSource(revision.Source).Blocks
	// ponytail: never evicted, one small slice per revision a run page has been opened for; bound it (an LRU) if a daemon ever serves thousands of revisions.
	s.stepBlocks.Store(key, blocks)

	return blocks
}
