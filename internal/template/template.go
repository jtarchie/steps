// Package template renders Go templates against source/version data.
package template

import (
	"fmt"
	"log/slog"
	"strings"
	"text/template"

	"github.com/frioux/leatherman/pkg/shellquote"
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

// shellQuote renders v as a single, safely-quoted POSIX shell word, via
// leatherman's shellquote.Quote — which quotes only when the value contains
// characters the shell would otherwise interpret (a plain "approve" stays
// bare; a review body with backticks / $(...) / quotes is single-quoted).
// Returns Quote's error (e.g. a null byte in the value) so a bad value fails
// the render rather than producing something unsafe.
func shellQuote(v any) (string, error) {
	quoted, err := shellquote.Quote([]string{fmt.Sprint(v)})
	if err != nil {
		return "", fmt.Errorf("shellquote: %w", err)
	}

	return quoted, nil
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
