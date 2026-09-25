package config

// A step's when: guard — the shell command whose exit code decides whether
// the step runs at all.

import (
	"fmt"
	"slices"
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
	// Inputs are artifacts only the guard sees, on top of the step's own
	// inputs; the step never does. Keyed by name, never by content: the
	// guard's outcome is already a run-time fact the planner cannot know.
	Inputs []string
}

// UnmarshalYAML decodes a WhenSpec from either a scalar (the command) or a
// mapping ({run, inputs}) YAML node.
func (w *WhenSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind { //nolint:exhaustive // yaml.Node.Kind covers document/alias kinds that can't appear here
	case yaml.ScalarNode:
		return value.Decode(&w.Run) //nolint:wrapcheck // yaml.v3 error is already descriptive
	case yaml.MappingNode:
		err := rejectUnknownKeys(value, "step when", "run", "inputs")
		if err != nil {
			return err
		}

		// Checked before Decode so `inputs: all` says what is wrong rather
		// than surfacing yaml.v3's "cannot unmarshal !!str".
		for i := 0; i+1 < len(value.Content); i += 2 {
			if value.Content[i].Value == "inputs" && value.Content[i+1].Kind != yaml.SequenceNode {
				return fmt.Errorf("step when at line %d: inputs must be a list of artifact names", value.Content[i+1].Line)
			}
		}

		var m struct {
			Run    string   `yaml:"run"`
			Inputs []string `yaml:"inputs"`
		}

		err = value.Decode(&m)
		if err != nil {
			return fmt.Errorf("step when: %w", err)
		}

		w.Run = m.Run
		w.Inputs = m.Inputs

		return nil
	default:
		return fmt.Errorf("step when at line %d must be a command string or a {run, inputs} mapping", value.Line)
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
				return fmt.Errorf("%s: when requires a command (run:)", label)
			}

			return c.validateGuardInputs(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateGuardInputs holds a guard's own inputs to the artifact-name rules
// and refuses one the step already sees. Refused rather than deduplicated: with
// input_mapping renaming src onto repo, a dedup would quietly show the guard
// repo's bytes under src/.
func (c *Config) validateGuardInputs(label string, step *Step) error {
	if step.When == nil || len(step.When.Inputs) == 0 {
		return nil
	}

	err := validateArtifactNames(label+" when", step.When.Inputs, nil)
	if err != nil {
		return err
	}

	own, all := guardViewNames(c, *step)
	if all {
		return fmt.Errorf("%s: when inputs are redundant beside inputs: all — the guard already sees every artifact", label)
	}

	for _, name := range step.When.Inputs {
		if slices.Contains(own, name) {
			return fmt.Errorf("%s: when input %q is already an input of the step — the guard sees every input the step does", label, name)
		}
	}

	return nil
}

// guardViewNames is the step's own input names as its runner resolves them,
// which is the view its guard starts from.
func guardViewNames(c *Config, step Step) (names []string, all bool) {
	kind, _ := step.Kind()

	switch kind { //nolint:exhaustive // every other kind has no input view of its own
	case StepKindTask:
		rt, err := c.ResolveTask(step)
		if err != nil {
			return nil, false // reported by the validator that owns tasks:
		}

		return rt.Inputs, false
	case StepKindAgent, StepKindPut, StepKindLoadVar:
		return step.InputNames(), step.InputsAll()
	case StepKindTry:
		return guardViewNames(c, *step.Try)
	default:
		return nil, false
	}
}
