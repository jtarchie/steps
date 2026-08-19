package web

// Templates, and the small formatting decisions they share.

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jtarchie/steps/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// renderer is echo's template bridge.
//
// One parsed set PER PAGE, rather than one set for everything: each page file
// defines a template literally named "page", which the layout invokes. Go
// templates can only invoke a name known at parse time, so a single shared
// set would leave every page fighting over that one name and the last file
// parsed would win — silently, and identically on every route.
type renderer struct{ pages map[string]*template.Template }

func newRenderer() (*renderer, error) {
	layout, err := assets.ReadFile("templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: could not read layout: %w", err)
	}

	files, err := assets.ReadDir("templates")
	if err != nil {
		return nil, fmt.Errorf("web: could not list templates: %w", err)
	}

	pages := map[string]*template.Template{}

	for _, file := range files {
		name := strings.TrimSuffix(file.Name(), ".html")
		if name == "layout" {
			continue
		}

		body, readErr := assets.ReadFile("templates/" + file.Name())
		if readErr != nil {
			return nil, fmt.Errorf("web: could not read %q: %w", file.Name(), readErr)
		}

		tmpl, parseErr := template.New(name).Funcs(templateFuncs()).Parse(string(layout))
		if parseErr != nil {
			return nil, fmt.Errorf("web: could not parse layout for %q: %w", name, parseErr)
		}

		tmpl, parseErr = tmpl.Parse(string(body))
		if parseErr != nil {
			return nil, fmt.Errorf("web: could not parse %q: %w", file.Name(), parseErr)
		}

		pages[name] = tmpl
	}

	return &renderer{pages: pages}, nil
}

// Render executes the named page inside the layout.
func (r *renderer) Render(w io.Writer, name string, data any, _ echo.Context) error {
	tmpl, ok := r.pages[name]
	if !ok {
		return fmt.Errorf("web: no template named %q", name)
	}

	values, isMap := data.(map[string]any)
	if !isMap {
		values = map[string]any{"Data": data}
	}

	values["Page"] = name

	// Title and TitleMark are optional per page, but the layout always reads
	// them — and `favicon` takes a string, so a missing key would reach it as
	// a nil interface and fail the render rather than degrade.
	if _, ok := values["Title"]; !ok {
		values["Title"] = ""
	}

	if _, ok := values["TitleMark"]; !ok {
		values["TitleMark"] = ""
	}

	err := tmpl.ExecuteTemplate(w, "layout", values)
	if err != nil {
		return fmt.Errorf("web: could not render %q: %w", name, err)
	}

	return nil
}

// handleCSS serves the stylesheet from the embedded assets.
func (s *Server) handleCSS(c echo.Context) error {
	data, err := assets.ReadFile("static/app.css")
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	//nolint:wrapcheck // echo's blob error is returned verbatim
	return c.Blob(http.StatusOK, "text/css; charset=utf-8", data)
}

// templateFuncs are the formatting decisions the templates share. They live
// here rather than in the templates because a duration rendered two ways on
// two pages is a bug that only ever gets noticed by the person comparing them.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"duration":   formatDuration,
		"ago":        formatAgo,
		"agoTag":     agoTag,
		"elapsedTag": elapsedTag,
		"stamp":      formatStamp,
		"short":      shortID,
		"statusWord": statusWord,
		// JSON is parsed and highlighted rather than re-indented — see
		// jsonview.go. jsonValue folds a bulky payload behind a summary for a
		// transcript row; jsonPre is the same rendering for a page that gives
		// it a <pre> of its own.
		"jsonValue": jsonValue,
		"jsonPre":   jsonPre,
		"jsonLine":  jsonLine,
		"prose":     renderProse,
		"thousands": thousands,
		"lower":     strings.ToLower,
		"trimMD":    func(name string) string { return strings.TrimSuffix(name, ".md") },
		"sparkline": sparkline,
		// The sparkline's geometry: bars are 6 wide on an 8-unit pitch, drawn
		// from a 16-high baseline. Arithmetic in the template rather than
		// pre-baked coordinates in the model, so the chart stays a
		// presentation detail.
		"mul2":    func(n int) int { return n * 8 },
		"sub16":   func(n int) int { return 16 - n },
		"slug":    slugify,
		"mark":    statusMark,
		"favicon": faviconFor,
		"rfc3339": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	}
}

// formatDuration renders a duration the way a terminal would: whole seconds
// below a minute, m/s above, and a dash for nothing.
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}

	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}

	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}

	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60

	if minutes < 60 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}

	return fmt.Sprintf("%dh %02dm", minutes/60, minutes%60)
}

// formatAgo renders how long ago something happened.
func formatAgo(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	elapsed := time.Since(t)
	if elapsed < 0 {
		return "just now"
	}

	return formatDuration(elapsed) + " ago"
}

// formatStamp renders an absolute local timestamp.
func formatStamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}

	return t.Local().Format("2006-01-02 15:04:05")
}

// shortID abbreviates a run id or hash to the length a person actually reads.
func shortID(id string) string {
	const shortLen = 7
	if len(id) <= shortLen {
		return id
	}

	return id[:shortLen]
}

// statusWord maps a stored status to the word the UI shows. The vocabulary
// differs by table (a node says "succeeded", a queue row says "done"), and a
// reader should not have to know which table they are looking at.
func statusWord(status string) string {
	switch status {
	case "succeeded", "done":
		return "passed"
	case "":
		return "running"
	default:
		return status
	}
}

// thousands groups a count the way the CLI's own usage summary does
// ("34,500 tokens"), so the two front ends do not report the same number in
// two spellings.
func thousands(n int) string {
	digits := strconv.Itoa(n)

	// Grouped after the sign rather than by recursing on -n: negating
	// math.MinInt64 yields itself, so that guard never terminated.
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}

	var out strings.Builder

	out.WriteString(sign)

	for i, digit := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out.WriteByte(',')
		}

		out.WriteRune(digit)
	}

	return out.String()
}

// sparkBar is one bar of a duration sparkline.
type sparkBar struct {
	Height int
	Failed bool
	Title  string
}

// sparkline turns a job's recent runs into bars scaled to the slowest, oldest
// first — so the trend reads left to right like every other timeline here.
func sparkline(runs []store.RunRow) []sparkBar {
	const (
		maxBars   = 16
		maxHeight = 14
	)

	if len(runs) > maxBars {
		runs = runs[:maxBars]
	}

	var slowest time.Duration

	for _, run := range runs {
		if d := run.Duration(); d > slowest {
			slowest = d
		}
	}

	bars := make([]sparkBar, 0, len(runs))

	// ListRuns is newest-first; walk backwards so the chart reads oldest to
	// newest.
	for i := len(runs) - 1; i >= 0; i-- {
		run := runs[i]
		height := 1

		if slowest > 0 {
			height = int(float64(run.Duration()) / float64(slowest) * maxHeight)
			if height < 1 {
				height = 1
			}
		}

		bars = append(bars, sparkBar{
			Height: height,
			Failed: run.Status == "failed",
			Title:  fmt.Sprintf("%s — %s", formatDuration(run.Duration()), statusWord(run.Status)),
		})
	}

	return bars
}

// transcriptEvent mirrors internal/agent's persisted transcript shape. It is
// re-declared rather than imported because internal/agent is an execution
// package the web layer must not depend on — and the shape is a stored JSON
// contract, which is exactly the kind of thing two packages may each know.
type transcriptEvent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Name string `json:"name,omitempty"`
	// Args stays raw rather than decoding to a map and marshaling it again.
	// Not because that would restore an order — internal/agent already stored
	// these as a map[string]any (transcript.go), so the model's own ordering
	// was canonicalized away upstream and no consumer can get it back. It
	// stays raw because a second reshape can only lose: a non-object args
	// value would decode to nothing, and the run transcript reads the very
	// same recorded bytes, so both pages show one string rather than two
	// re-encodings of it.
	Args    json.RawMessage   `json:"args,omitempty"`
	Content string            `json:"content,omitempty"`
	Agent   string            `json:"agent,omitempty"`
	Request string            `json:"request,omitempty"`
	Events  []transcriptEvent `json:"events,omitempty"`
}

// ArgsJSON is a call's arguments as they were recorded.
func (e transcriptEvent) ArgsJSON() string { return string(e.Args) }

// transcriptEvents decodes a stored transcript, yielding nil when there is
// none or it will not parse.
func transcriptEvents(raw string, ok bool) []transcriptEvent {
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}

	var decoded []transcriptEvent

	err := json.Unmarshal([]byte(raw), &decoded)
	if err != nil {
		return nil
	}

	return decoded
}

// statusMark is the one-glyph status a browser tab can carry. It prefixes the
// document title and selects the favicon, so a run left in a background tab
// reports its outcome without being reopened — the single most common way a
// CI page is actually used.
func statusMark(status string) string {
	switch statusWord(status) {
	case "passed":
		return "✓"
	case "failed", "errored", "aborted":
		return "✗"
	case "running":
		return "◐"
	default:
		return ""
	}
}

// faviconDots maps a status mark to a self-contained SVG data URI. Inline
// rather than files: three flat discs cost less as data URIs than as three
// more embedded assets and three more requests, and the page is already
// committed to shipping its own chrome.
var faviconDots = map[string]string{
	"✓": faviconSVG("%2384c06d"),
	"✗": faviconSVG("%23e0645a"),
	"◐": faviconSVG("%23d9a94a"),
	"":  faviconSVG("%2383887b"),
}

// faviconSVG builds a filled-circle favicon in the given (URL-escaped) color.
func faviconSVG(color string) string {
	return "data:image/svg+xml," +
		"%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 16 16'%3E" +
		"%3Ccircle cx='8' cy='8' r='6' fill='" + color + "'/%3E%3C/svg%3E"
}

// faviconFor resolves a status mark to its icon, falling back to the neutral
// dot for pages that carry no status at all.
func faviconFor(marker string) template.URL {
	icon, ok := faviconDots[marker]
	if !ok {
		icon = faviconDots[""]
	}

	//nolint:gosec // G203: the value is one of four constants built above, never input
	return template.URL(icon)
}

// slugify renders a step name as a URL fragment: lowercase, with every run of
// non-alphanumerics collapsed to a single dash. An across: cell named
// review[security] becomes review-security, so a step is linkable by name
// rather than by position alone.
func slugify(name string) string {
	var out strings.Builder

	dashed := false

	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)

			dashed = false

			continue
		}

		if !dashed && out.Len() > 0 {
			out.WriteByte('-')

			dashed = true
		}
	}

	return strings.TrimSuffix(out.String(), "-")
}

// agoTag renders a relative timestamp as a <time> element carrying the
// absolute instant, so the shared ticker can keep re-rendering it. Without
// it, "4s ago" is baked at render time and is a lie by the time anyone reads
// it — most visibly on a page that never reloads.
func agoTag(t time.Time) template.HTML {
	if t.IsZero() {
		return `<span class="dim">—</span>`
	}

	//nolint:gosec // G203: both interpolations are machine-formatted, not input
	return template.HTML(fmt.Sprintf(`<time data-ago=%q>%s</time>`,
		t.UTC().Format(time.RFC3339Nano), template.HTMLEscapeString(formatAgo(t))))
}

// elapsedTag renders a duration that is still accumulating: a finished run
// gets a fixed value, a running one gets the start instant for the ticker to
// count from.
func elapsedTag(run store.RunRow) template.HTML {
	if !run.FinishedAt.IsZero() || run.StartedAt.IsZero() {
		return template.HTML(template.HTMLEscapeString(formatDuration(run.Duration()))) //nolint:gosec // G203: escaped above
	}

	//nolint:gosec // G203: both interpolations are machine-formatted, not input
	return template.HTML(fmt.Sprintf(`<time data-elapsed-since=%q>%s</time>`,
		run.StartedAt.UTC().Format(time.RFC3339Nano), template.HTMLEscapeString(formatDuration(run.Duration()))))
}
