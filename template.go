package main

import (
	"fmt"
	"strings"
	"text/template"
)

// Render renders a Go template against data (typically
// map[string]any{"source": ..., "version": ...}).
func Render(tmpl string, data map[string]any) (string, error) {
	t, err := template.New("step").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("could not parse template %q: %w", tmpl, err)
	}

	var out strings.Builder

	err = t.Execute(&out, data)
	if err != nil {
		return "", fmt.Errorf("could not render template %q: %w", tmpl, err)
	}

	return out.String(), nil
}
