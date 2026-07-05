package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

// webTemplates is the parsed page set, with the formatting helpers the
// templates reference. Parsed once at startup; a parse error is a programmer
// bug in an embedded file, so panic.
var webTemplates = template.Must(template.New("").Funcs(template.FuncMap{
	"tokens": formatCount,
	"cost":   func(c float64) string { return fmt.Sprintf("$%.4f", c) },
	"ago":    humanizeSince,
	"json": func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(raw)
	},
}).ParseFS(templateFS, "templates/*.html"))

// humanizeSince renders a timestamp as a compact relative age.
func humanizeSince(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return t.Format("2006-01-02 15:04")
	}
}
