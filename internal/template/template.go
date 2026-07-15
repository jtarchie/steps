// Package template renders Go templates against source/version data.
package template

import (
	"fmt"
	"log/slog"
	"strings"
	"text/template"

	"github.com/frioux/leatherman/pkg/shellquote"
	sprig "github.com/go-task/slim-sprig/v3"
)

// funcMap is the set of helper functions available in every rendered
// template: slim-sprig's library (string/list/default/date helpers, with no
// external dependencies) plus our own shellquote.
//
// shellquote is the important local one: values interpolated into a command
// string that will run via `sh -c` (a custom tool's run:, a resource command)
// must be quoted, or shell metacharacters in the value — backticks, $(...),
// quotes, ; | & — are interpreted by the shell instead of passed through
// literally. This matters most for values an LLM or an untrusted PR produces
// (e.g. a review body), which can't be assumed shell-safe. Note sprig's own
// quote/squote just wrap in double/single quotes without escaping, so they
// are NOT shell-safe — use shellquote for anything headed into sh -c.
//
//nolint:gochecknoglobals // compiled once, read-only
var funcMap = newFuncMap()

// newFuncMap builds funcMap: slim-sprig's text/template functions, with our
// shellquote added (and taking precedence on any name clash).
func newFuncMap() template.FuncMap {
	fm := sprig.TxtFuncMap()
	fm["shellquote"] = shellQuote

	return fm
}

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
