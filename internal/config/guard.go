package config

// A step's when: guard — the shell command whose exit code decides whether
// the step runs at all.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// WhenSpec is a step's when: guard — an explicit shell command whose EXIT
// CODE decides whether the step runs at all: 0 runs it, nonzero skips it. A
// nonzero exit is a legitimate "false" (a `grep -q` that finds nothing), never
// a failure; only a runner-level error (the command could not be started at
// all — a bad image, a docker daemon that isn't up) fails the step, so an
// infrastructure problem is never silently read as "skip".
//
// A skipped step behaves exactly like a merkle-cached skip: it fires no hooks,
// records no node or job_run, and does not appear in a job's assert.execution
// log. The guard runs in the same view the step itself would get — under the
// step's resolved image, in a directory materialized from the step's declared
// inputs — so it can read what the step reads.
//
// It implements yaml.Unmarshaler for the same scalar-or-mapping reason
// FixSpec does: `when: test -f x` is the common case, `when: {run: ...}` the
// explicit one.
type WhenSpec struct {
	Run string
}

// UnmarshalYAML decodes a WhenSpec from either a scalar (the command) or a
// mapping ({run}) YAML node.
func (w *WhenSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&w.Run) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		var m struct {
			Run string `yaml:"run"`
		}

		err := value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step when: %w", err)
		}

		w.Run = m.Run

		return nil
	default:
		return fmt.Errorf("step when at line %d must be a command string or a {run} mapping", value.Line)
	}
}

// validateStepGuards rejects a when: guard on a get step (a get fans the
// remainder of the plan out per version, so gating one has no coherent
// meaning — the same reasoning that rejects image:/assert: there) and an
// empty guard command (which would otherwise run `sh -c ""`, exit 0, and
// silently mean "always run").
func (c *Config) validateStepGuards() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.When == nil {
				return nil
			}

			if step.Get != "" {
				return fmt.Errorf("%s (get %q): when is not valid on get steps", label, step.Get)
			}

			if strings.TrimSpace(step.When.Run) == "" {
				return fmt.Errorf("%s: when requires a command", label)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
