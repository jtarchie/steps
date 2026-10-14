package pipeline

import (
	"fmt"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// UnquotedShellValues names every template value headed into `sh -c` without
// shellquote: resource check/in/out and custom tool run: lines. These are
// warnings, not load errors — shellquote is a documented convention, and an
// author may leave a value they wrote themselves (a source:) bare on purpose.
// Here rather than in config for the reason ValidateExpressions gives: config
// cannot reach the template parser.
func UnquotedShellValues(cfg *config.Config) []string {
	warnings := make([]string, 0, len(cfg.ResourceTypes))

	for _, rt := range cfg.ResourceTypes {
		for _, hit := range rsrc.UnquotedRefs(rt) {
			warnings = append(warnings, fmt.Sprintf("resource type %q %s", rt.Name, unquotedAdvice(hit)))
		}
	}

	return append(warnings, unquotedToolValues(cfg)...)
}

func unquotedToolValues(cfg *config.Config) []string {
	var warnings []string

	tools := func(owner string, specs []config.ToolSpec) {
		for _, spec := range specs {
			for _, expr := range agent.UnquotedToolRefs(spec.Run) {
				warnings = append(warnings, fmt.Sprintf("%s tool %q %s", owner, spec.Name, unquotedAdvice("run: "+expr)))
			}
		}
	}

	for _, a := range cfg.Agents {
		tools(fmt.Sprintf("agent %q", a.Name), a.Tools)
	}

	for _, task := range cfg.Tasks {
		if task.Fix != nil {
			tools(fmt.Sprintf("task %q fix", task.Name), task.Fix.Tools)
		}
	}

	for _, job := range cfg.Jobs {
		_ = job.VisitSteps(func(label string, step *config.Step) error {
			tools(label, step.Tools)

			if step.Fix != nil {
				tools(label+" fix", step.Fix.Tools)
			}

			return nil
		})
	}

	return warnings
}

func unquotedAdvice(hit string) string {
	return hit + " reaches the shell unquoted; pipe it through shellquote"
}
