package resource

import (
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/template"
)

// UnquotedRefs reports each check/in/out action of a shell-backed type that
// prints .source, .version or .params without shellquote, as "<stage>:
// <action>". The roots are the keys the shell backend renders with.
func UnquotedRefs(rt config.ResourceType) []string {
	var found []string

	for _, stage := range []struct{ name, tmpl string }{
		{"check", rt.Config.Check}, {"in", rt.Config.In}, {"out", rt.Config.Out},
	} {
		for _, expr := range template.UnquotedRefs(stage.tmpl, "source", "version", "params") {
			found = append(found, fmt.Sprintf("%s: %s", stage.name, expr))
		}
	}

	return found
}
