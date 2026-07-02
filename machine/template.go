package machine

import (
	"bytes"
	"fmt"
	"text/template"
)

// ParseTemplate validates template syntax at load time.
func ParseTemplate(name, src string) (*template.Template, error) {
	return template.New(name).Option("missingkey=zero").Parse(src)
}

// RenderTemplate renders a prompt/input template. Data always carries "ctx"
// (run context) and may carry history projections under their `as` names.
func RenderTemplate(name, src string, data map[string]any) (string, error) {
	t, err := ParseTemplate(name, src)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template %s: %w", name, err)
	}
	return buf.String(), nil
}
