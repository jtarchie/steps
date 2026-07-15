// Package template renders Go templates against source/version data.
package template

import (
	"fmt"
	"log/slog"
	"strings"
	"text/template"
)

// funcMap is the set of helper functions available in every rendered
// template. shellquote is the important one: values interpolated into a
// command string that will run via `sh -c` (a custom tool's run:, a resource
// command) must be quoted, or shell metacharacters in the value — backticks,
// $(...), quotes, ; | & — are interpreted by the shell instead of passed
// through literally. This matters most for values an LLM or an untrusted PR
// produces (e.g. a review body), which can't be assumed shell-safe.
//
//nolint:gochecknoglobals // compiled once, read-only
var funcMap = template.FuncMap{"shellquote": shellQuote}

// shellQuote renders v as a single, safely-quoted POSIX shell word: it wraps
// the value in single quotes (inside which the shell interprets nothing) and
// escapes any embedded single quote as '\”. An empty value becomes ”.
func shellQuote(v any) string {
	return "'" + strings.ReplaceAll(fmt.Sprint(v), "'", `'\''`) + "'"
}

// Render renders a Go template against data (typically
// map[string]any{"source": ..., "version": ...}).
func Render(tmpl string, data map[string]any) (string, error) {
	slog.Debug("template.render", "template", tmpl, "data", data)

	t, err := template.New("step").Funcs(funcMap).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("could not parse template %q: %w", tmpl, err)
	}

	var out strings.Builder

	err = t.Execute(&out, data)
	if err != nil {
		return "", fmt.Errorf("could not render template %q: %w", tmpl, err)
	}

	rendered := out.String()

	slog.Debug("template.rendered", "template", tmpl, "rendered", rendered)

	return rendered, nil
}
