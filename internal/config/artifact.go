package config

// Artifact names: the pattern every input/output/resource name must match,
// and the load-time checks over a step's inputs:/outputs: declarations and
// input_mapping:/output_mapping: rewrites.

import (
	"fmt"
	"regexp"
)

// artifactNamePattern constrains input/output/resource names used to build
// filesystem paths: this is load-bearing, not cosmetic — it rules out `..`
// segments and separators, so a name can never escape the directory it's
// joined under, and keeps the workspace copy/btrfs backends' shelled-out
// argv construction free of characters that would need escaping.
var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`) //nolint:gochecknoglobals // static, read-only

// ValidateArtifactName checks name against artifactNamePattern. Used both at
// config-load time (validateArtifactNames) and by internal/workspace at
// runtime, when materializing a step's directories.
func ValidateArtifactName(name string) error {
	if !artifactNamePattern.MatchString(name) {
		return fmt.Errorf("invalid artifact name %q: must match %s", name, artifactNamePattern.String())
	}

	// Reserved: handoff notes are rendered under this directory in the shared
	// build root (see HandoffNoteDir), so an artifact of the same name would
	// materialize over them.
	if name == HandoffNoteDir {
		return fmt.Errorf("invalid artifact name %q: reserved for handoff notes", name)
	}

	return nil
}

func (c *Config) validateArtifactDecls() error {
	for i := range c.Tasks {
		task := c.Tasks[i]

		err := validateArtifactNames(fmt.Sprintf("task %q", task.Name), task.Inputs.names(), task.Outputs)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return c.validateStepArtifactDecls(label, *step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateStepArtifactDecls(label string, step Step) error {
	err := c.validateMappingPlacement(label, step)
	if err != nil {
		return err
	}

	switch {
	case step.Get != "":
		if step.InputsDeclared() || step.Outputs != nil {
			return fmt.Errorf("%s (get %q): inputs/outputs are not valid on get steps", label, step.Get)
		}

		return nil
	case step.Put != "":
		if step.Outputs != nil {
			return fmt.Errorf("%s (put %q): outputs are not valid on put steps", label, step.Put)
		}

		// inputs: all is a put-only escape hatch; it names no artifacts, so
		// skip name validation for it.
		if step.InputsAll() {
			return nil
		}

		return validateArtifactNames(fmt.Sprintf("%s (put %q)", label, step.Put), step.InputNames(), nil)
	default:
		if step.InputsAll() {
			return fmt.Errorf("%s: inputs: all is only valid on put steps", label)
		}

		return validateArtifactNames(label, step.InputNames(), step.Outputs)
	}
}

// validateMappingPlacement enforces that input_mapping/output_mapping — which
// physically rename a materialized directory — appear only on task steps and
// only under a workspace: block.
func (c *Config) validateMappingPlacement(label string, step Step) error {
	if len(step.InputMapping) == 0 && len(step.OutputMapping) == 0 {
		return nil
	}

	if step.Task == "" {
		return fmt.Errorf("%s: input_mapping/output_mapping are only valid on task steps", label)
	}

	if c.Workspace == nil {
		return fmt.Errorf("%s: input_mapping/output_mapping require a top-level workspace: block", label)
	}

	return nil
}

// validateArtifactMappings enforces that a task step's input_mapping/
// output_mapping keys are a subset of the resolved task's declared inputs/
// outputs — a mapping key that names no declared input/output is a typo that
// would otherwise silently do nothing. Placement rules (task-step-only,
// workspace-only) are enforced in validateStepArtifactDecls.
func (c *Config) validateArtifactMappings() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.InputMapping) == 0 && len(step.OutputMapping) == 0 {
				return nil
			}

			rt, err := c.ResolveTask(*step)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}

			err = checkMappingKeys(label, "input_mapping", step.InputMapping, rt.Inputs)
			if err != nil {
				return err
			}

			return checkMappingKeys(label, "output_mapping", step.OutputMapping, rt.Outputs)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkMappingKeys rejects a mapping key that names no declared artifact.
func checkMappingKeys(label, field string, mapping map[string]string, declared []string) error {
	if len(mapping) == 0 {
		return nil
	}

	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	for key := range mapping {
		if !declaredSet[key] {
			return fmt.Errorf("%s: %s key %q is not a declared %s", label, field, key,
				map[string]string{"input_mapping": "input", "output_mapping": "output"}[field])
		}
	}

	return nil
}

// validateArtifactNames checks every name in inputs/outputs against
// artifactNamePattern (see workspace.go) and rejects duplicates within a
// list or a name appearing in both — in-place propagation (an output
// shadowing one of the same step's own inputs) isn't supported.
func validateArtifactNames(context string, inputs, outputs []string) error {
	seen := map[string]string{}

	check := func(names []string, kind string) error {
		for _, name := range names {
			err := ValidateArtifactName(name)
			if err != nil {
				return fmt.Errorf("%s: %w", context, err)
			}

			prevKind, ok := seen[name]
			if ok {
				if prevKind == kind {
					return fmt.Errorf("%s: duplicate %s %q", context, kind, name)
				}

				return fmt.Errorf("%s: %q cannot be both an input and an output of the same step", context, name)
			}

			seen[name] = kind
		}

		return nil
	}

	err := check(inputs, "input")
	if err != nil {
		return err
	}

	return check(outputs, "output")
}

// StableStrings returns a non-nil copy of names, so json.Marshal always
// encodes it as [] rather than null — a nil inputs/outputs list and an
// explicit inputs: [] must hash identically, since they mean the same thing
// (no artifacts). Used by merkle/agent content-hashing when folding in a
// step's Inputs/Outputs.
func StableStrings(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)

	return out
}
