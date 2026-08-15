package pipeline

// File-level checks `steps validate` runs that need more than internal/config
// can reach.

import (
	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// ValidateExpressions type-checks every expr-backed resource type's
// expressions without running any of them.
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
	return rsrc.CompileExprPrograms(cfg) //nolint:wrapcheck // CompileExprPrograms names the resource type and slot
}
