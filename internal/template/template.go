// Package template renders Go templates against source/version data.
package template

import (
	"fmt"
	"log/slog"
	"strings"
	"text/template"
)

// Render renders a Go template against data (typically
// map[string]any{"source": ..., "version": ...}).
func Render(tmpl string, data map[string]any) (string, error) {
	slog.Debug("template.render", "template", tmpl, "data", data)

	t, err := template.New("step").Option("missingkey=error").Parse(tmpl)
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
