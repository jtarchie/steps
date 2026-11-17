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

	warnings = append(warnings, unquotedToolValues(cfg)...)

	return append(warnings, factsInShell(cfg)...)
}

// factsInShell names each get's fact pasted into a run:, which is sh -c. A
// fact is text a resource reported — a pull request's title is whoever opened
// it — and ((.:name)) has no shellquote, so the safe spelling is the get's own
// files, quoted. A load_var: value is the pipeline's own and is not named.
func factsInShell(cfg *config.Config) []string {
	var warnings []string

	for _, job := range cfg.Jobs {
		gets := map[string]bool{}

		_ = job.VisitSteps(func(_ string, step *config.Step) error {
			if step.Get != "" {
				gets[step.Get] = true
			}

			return nil
		})

		_ = job.VisitSteps(func(label string, step *config.Step) error {
			for _, ref := range config.LocalRefs(step.Run) {
				if gets[ref.Root] {
					warnings = append(warnings, fmt.Sprintf(
						"%s run: %s is a fact get %q reported, pasted into sh -c as written: whoever wrote it (a pull request's author, say) writes shell. Read the get's own files, quoted (\"$(cat %s/…)\"), or hand it to a put's params:",
						label, ref, ref.Root, ref.Root))
				}
			}

			return nil
		})
	}

	return warnings
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
