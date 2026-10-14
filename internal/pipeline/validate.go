package pipeline

// File-level checks `steps validate` runs that need more than internal/config
// can reach.

import (
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/exprlang"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// ValidateExpressions type-checks every expr-backed resource type's
// expressions, and compiles every webhook and cron resource, without running
// any of them.
//
// It is a pass-through to resource.CompileExprPrograms, and exists only
// because of who may call whom: main wires up config/store/workspace and
// hands off to this package, and deliberately does not reach past it into the
// packages this one orchestrates (depguard's Main rule). So the check lives
// where the caller already is.
//
// Why it is not a load error, which is where it belongs on first reading:
// internal/config imports nothing internal and no third-party code but the
// YAML parser, which is what lets every other package share those types
// without inheriting an expression engine. Keeping that seam costs one
// validate-time call; giving it up would cost the shape of the whole
// dependency graph.
func ValidateExpressions(cfg *config.Config) error {
	return errors.Join(rsrc.CompileExprPrograms(cfg), rsrc.CompileWebhooks(cfg), rsrc.CompileCrons(cfg))
}

// ValidateExports refuses an export that cannot be served as written. config cannot reach the template parser or the expression engine, so what an export's commands do with a caller's values, and whether a rule's expressions compile, is checked here, and again where the export is built.
func ValidateExports(cfg *config.Config) error {
	errs := []error{validateRuleExpressions(cfg)}

	for _, export := range cfg.MCPExports {
		err := agent.CheckExportedCommands(export)
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp_exports %q: %w", export.Name, err))
		}
	}

	return errors.Join(errs...)
}

func validateRuleExpressions(cfg *config.Config) error {
	var errs []error

	for _, rule := range cfg.Rules {
		if rule.When != "" {
			_, err := exprlang.CompileWhen(rule.When)
			if err != nil {
				errs = append(errs, fmt.Errorf("rules %q: when: %w", rule.Name, err))
			}
		}

		if rule.Accept != "" {
			_, err := exprlang.CompileAccept(rule.Accept)
			if err != nil {
				errs = append(errs, fmt.Errorf("rules %q: accept: %w", rule.Name, err))
			}
		}
	}

	return errors.Join(errs...)
}
