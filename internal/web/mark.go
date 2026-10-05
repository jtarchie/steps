package web

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/store"
)

// disc is the status a mark's disc is colored by, ordered by how loudly it
// claims a reader: a scope folding several statuses keeps the greatest.
type disc int

const (
	discNone disc = iota
	discQueued
	discAborted
	discPassed
	discPaused
	discFailed
	discErrored
)

// mark is one scope's status reduced to what a 16px icon can carry: the
// disc, whether anything is running, and how much waits on a person. Every
// surface that draws status at a glance — favicon, title, switcher — draws
// one of these, so they cannot disagree about a scope.
type mark struct {
	Disc disc
	// Ring is a run actually running. Queued work does not light it: a queue
	// a paused pipeline or a serial job is holding would look busy.
	Ring bool
	// Needs is what waits on a person, which outranks every disc in the icon.
	Needs int
}

// fold widens a mark over another scope inside it.
func (m mark) fold(other mark) mark {
	m.Disc = max(m.Disc, other.Disc)
	m.Ring = m.Ring || other.Ring
	m.Needs += other.Needs

	return m
}

var discGlyphs = map[disc]string{
	discQueued:  "○",
	discAborted: "■",
	discPassed:  "✓",
	discPaused:  "⏸",
	discFailed:  "✗",
	discErrored: "!",
}

var discWords = map[disc]string{
	discQueued:  "queued",
	discAborted: "aborted",
	discPassed:  "passed",
	discPaused:  "paused",
	discFailed:  "failed",
	discErrored: "errored",
}

// DiscGlyph and Word are the disc alone, for markup that colors the disc and
// the ring separately.
func (m mark) DiscGlyph() string { return discGlyphs[m.Disc] }

func (m mark) Word() string { return discWords[m.Disc] }

// Words is the mark said aloud, for a reader who cannot see the glyphs.
func (m mark) Words() string {
	words := m.Word()
	if m.Ring {
		if words != "" {
			words += ", "
		}

		words += "running"
	}

	return words
}

// Glyph is the mark as text: the disc's glyph, then ◐ when something runs.
func (m mark) Glyph() string {
	glyph := discGlyphs[m.Disc]
	if m.Ring {
		glyph += "◐"
	}

	return glyph
}

// TitlePrefix leads the document title: "(n) " for what waits on a person,
// then the glyph — the part of a tab's title the strip still shows.
func (m mark) TitlePrefix() string {
	prefix := ""
	if m.Needs > 0 {
		prefix = "(" + strconv.Itoa(m.Needs) + ") "
	}

	if glyph := m.Glyph(); glyph != "" {
		prefix += glyph + " "
	}

	return prefix
}

func discOf(status string) disc {
	switch statusWord(status) {
	case "passed":
		return discPassed
	case "failed":
		return discFailed
	case "errored":
		return discErrored
	case "aborted":
		return discAborted
	case "queued":
		return discQueued
	default:
		return discNone
	}
}

func finished(status string) bool {
	word := statusWord(status)

	return word != "running" && word != "queued"
}

// runMark is a run page's mark: that run, and nothing else.
func runMark(run store.RunRow) mark {
	return mark{Disc: discOf(run.Status), Ring: statusWord(run.Status) == "running"}
}

// inFlight counts the queue's open rows, the same reading the root's queued
// column takes; zero when the store cannot say, since a missing count is
// better than a page that fails to draw.
func inFlight(ctx context.Context, target *Pipeline) int {
	if target.Store == nil {
		return 0
	}

	queue, err := target.Store.ListTriggerQueue(ctx, overviewLimit)
	if err != nil {
		return 0
	}

	return len(pendingQueue(queue))
}

// runTitle is a run page's title, which the stream's done frame repeats.
func runTitle(run store.RunRow) string {
	return run.JobName + " #" + shortID(run.ID)
}

// pipelineMark folds every job the pipeline declares by its latest FINISHED
// run — Concourse's dashboard rule — so a red job being rebuilt stays red
// and wears the ring, rather than reading as merely busy. A job no longer in
// the config is not folded: its last failure would otherwise be permanent.
func pipelineMark(ctx context.Context, target *Pipeline, needs int) mark {
	result := mark{Needs: needs}
	if paused(ctx, target) {
		result.Disc = discPaused
	}

	if target.Store == nil {
		return result
	}

	latest, err := target.Store.LatestRunByJob(ctx)
	if err != nil {
		return result
	}

	for _, job := range target.Config().Jobs {
		run, ok := latest[job.Name]
		if !ok {
			continue
		}

		if finished(run.Status) {
			result.Disc = max(result.Disc, discOf(run.Status))

			continue
		}

		result.Ring = result.Ring || statusWord(run.Status) == "running"
		result.Disc = max(result.Disc, lastFinishedDisc(ctx, target, job.Name))
	}

	return result
}

// lastFinishedRuns bounds the look-back behind a job's in-flight runs.
// ponytail: a job with more runs in flight than this reads as never having
// finished until one does; max_in_flight that high is not a shape seen yet.
const lastFinishedRuns = 5

// lastFinishedDisc is asked only of a job with a run in flight, so the read
// costs one query per BUSY job, not per job.
func lastFinishedDisc(ctx context.Context, target *Pipeline, job string) disc {
	runs, err := target.Store.ListRuns(ctx, job, lastFinishedRuns)
	if err != nil {
		return discNone
	}

	for _, run := range runs {
		if finished(run.Status) {
			return discOf(run.Status)
		}
	}

	return discNone
}

const (
	colorBg     = "#111310"
	colorDim    = "#83887b"
	colorFaint  = "#565b50"
	colorGreen  = "#84c06d"
	colorRed    = "#e0645a"
	colorYellow = "#d9a94a"
	colorBlue   = "#7aa4d9"
)

var discColors = map[disc]string{
	discNone:    colorDim,
	discQueued:  colorFaint,
	discAborted: colorDim,
	discPassed:  colorGreen,
	discPaused:  colorBlue,
	discFailed:  colorRed,
	discErrored: colorRed,
}

// svgEscaper writes the URI already in html/template's normalized form, so
// the favicon's href and #mark's data-icon render as the same string and
// app.js can tell an unchanged icon from a changed one.
var svgEscaper = strings.NewReplacer("%", "%25", "#", "%23", "<", "%3C", ">", "%3E", " ", "%20", "'", "%27")

// Favicon draws the mark as an SVG data URI. A disc is as large as the icon
// allows; a running ring is drawn outside it with a gap, so it reads over
// any disc color including the yellow one; a needs-you count turns the disc
// yellow and writes the number on it — the one number worth squinting at.
func (m mark) Favicon() template.URL {
	radius, fill := "7", discColors[m.Disc]
	if m.Ring {
		radius = "5"
	}

	var svg strings.Builder

	svg.WriteString("<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'>")

	if m.Ring {
		svg.WriteString("<circle cx='8' cy='8' r='7.25' fill='none' stroke='" + colorYellow + "' stroke-width='1.5'/>")
	}

	if m.Needs > 0 {
		fill = colorYellow
	}

	svg.WriteString("<circle cx='8' cy='8' r='" + radius + "' fill='" + fill + "'/>")

	if m.Needs > 0 {
		label, size := strconv.Itoa(m.Needs), "9"
		if m.Needs > 9 {
			label, size = "9+", "6.5"
		}

		if m.Ring {
			size = "7"
			if m.Needs > 9 {
				size = "5"
			}
		}

		svg.WriteString("<text x='8' y='8' text-anchor='middle' dominant-baseline='central' font-family='sans-serif' font-weight='700' font-size='" +
			size + "' fill='" + colorBg + "'>" + label + "</text>")
	}

	svg.WriteString("</svg>")

	//nolint:gosec // G203: every input is a constant above or a formatted int, never request data
	return template.URL("data:image/svg+xml," + svgEscaper.Replace(svg.String()))
}

// runMarkRoute is a run's mark route; handleError keys on it and on the
// other two, which answer a hidden tab and never a reader.
const runMarkRoute = "/p/:pipeline/runs/:run/mark"

func isMarkRoute(path string) bool {
	return path == "/mark" || path == "/p/:pipeline/mark" || path == runMarkRoute
}

// handleMark answers a hidden tab with its scope's #mark alone: the root's
// for every pipeline, a pipeline page's for that pipeline — the same scope
// nav already resolved for the page.
func (s *Server) handleMark(c *echo.Context) error {
	return s.writeMark(c, s.nav(c))
}

func (s *Server) handleRunMark(c *echo.Context) error {
	pipeline := pipelineOf(c)

	run, ok, err := pipeline.Store.FindRunRow(c.Request().Context(), c.Param("run"))
	if err != nil {
		return fmt.Errorf("web: run mark: %w", err)
	}

	if !ok {
		return echo.NewHTTPError(http.StatusNotFound, "no such run")
	}

	nav := s.nav(c)
	nav.Mark = runMark(run)
	nav.MarkURL = "/p/" + pipeline.Slug + "/runs/" + run.ID + "/mark"

	return s.writeMark(c, nav)
}

func (s *Server) writeMark(c *echo.Context, nav navData) error {
	tmpl, ok := s.renderer.pages["empty"]
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "no layout to draw a mark from")
	}

	var out strings.Builder

	err := tmpl.ExecuteTemplate(&out, "markcarrier", nav)
	if err != nil {
		return fmt.Errorf("web: render mark: %w", err)
	}

	c.Response().Header().Set("Cache-Control", "no-store")

	//nolint:wrapcheck // echo's write error is returned verbatim by every handler here
	return c.HTML(http.StatusOK, out.String())
}
