package web

// Templates, and the small formatting decisions they share.

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
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
		"stamp":      formatStamp,
		"short":      shortID,
		"statusWord": statusWord,
		"prettyJSON": prettyJSON,
		"trim":       strings.TrimSpace,
		"firstLine":  firstLine,
		"sparkline":  sparkline,
		// The sparkline's geometry: bars are 6 wide on an 8-unit pitch, drawn
		// from a 16-high baseline. Arithmetic in the template rather than
		// pre-baked coordinates in the model, so the chart stays a
		// presentation detail.
		"mul2":  func(n int) int { return n * 8 },
		"sub16": func(n int) int { return 16 - n },
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

// prettyJSON re-indents stored JSON for display, returning the input
// unchanged when it is not JSON — a content map that will not parse is still
// worth showing.
func prettyJSON(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}

	var value any

	err := json.Unmarshal([]byte(raw), &value)
	if err != nil {
		return raw
	}

	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return raw
	}

	return string(out)
}

// firstLine is the first line of a multi-line string, for a summary cell.
func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")

	return line
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
	Type    string            `json:"type"`
	Text    string            `json:"text,omitempty"`
	Name    string            `json:"name,omitempty"`
	Args    map[string]any    `json:"args,omitempty"`
	Content string            `json:"content,omitempty"`
	Agent   string            `json:"agent,omitempty"`
	Request string            `json:"request,omitempty"`
	Events  []transcriptEvent `json:"events,omitempty"`
}

// ArgsJSON renders a call's arguments for display.
func (e transcriptEvent) ArgsJSON() string {
	if len(e.Args) == 0 {
		return ""
	}

	data, err := json.Marshal(e.Args)
	if err != nil {
		return ""
	}

	return string(data)
}

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
