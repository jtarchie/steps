package config

// Artifact names: the pattern every input/output/resource name must match,
// and the load-time checks over a step's inputs:/outputs: declarations and
// input_mapping:/output_mapping: rewrites.

import (
	"fmt"
	"regexp"
	"strings"
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

	// Reserved: a task reading context: from: is handed each demanded step's
	// outcome as a file under this directory (see UpstreamDir).
	if name == UpstreamDir {
		return fmt.Errorf("invalid artifact name %q: reserved for delivered step outcomes", name)
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

	//kindswitch:ignore Task, Agent and a try: wrapper all declare inputs/outputs the same way, which is what default: validates
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
// physically rename a materialized directory — appear only on task steps.
func (c *Config) validateMappingPlacement(label string, step Step) error {
	if len(step.InputMapping) == 0 && len(step.OutputMapping) == 0 {
		return nil
	}

	if step.Task == "" {
		return fmt.Errorf("%s: input_mapping/output_mapping are only valid on task steps", label)
	}

	return nil
}

// ValidateArtifactPath accepts what a capture may materialize to: an artifact
// name, optionally followed by /-separated segments inside it — a collecting
// matrix captures a cell's output at findings/alpha/fast (see
// CollectedOutputMapping). The first segment is a full artifact name,
// reservations included; later segments need only the charset, since they
// name directories inside an artifact rather than artifacts. Every segment
// matching the pattern is what makes the path safe to join: no segment can be
// empty, "..", or absolute.
//
// USER-authored mapping values never reach this looser rule — they are pinned
// to ValidateArtifactName at load (see validateArtifactMappings), so the only
// slash-carrying paths are machine-composed from already-validated parts.
func ValidateArtifactPath(artifactPath string) error {
	segments := strings.Split(artifactPath, "/")

	err := ValidateArtifactName(segments[0])
	if err != nil {
		return err
	}

	for _, segment := range segments[1:] {
		if !artifactNamePattern.MatchString(segment) {
			return fmt.Errorf("invalid artifact path %q: segment %q must match %s", artifactPath, segment, artifactNamePattern.String())
		}
	}

	return nil
}

// validateArtifactMappings enforces that a task step's input_mapping/
// output_mapping keys are a subset of the resolved task's declared inputs/
// outputs — a mapping key that names no declared input/output is a typo that
// would otherwise silently do nothing — and that every mapping VALUE is a
// plain artifact name. The value check used to live only at run time
// (materializeSpace); it has to stay strict here because the runtime seam now
// accepts machine-composed paths with slashes (ValidateArtifactPath), and a
// user-written mapping must not ride that loosening into another artifact's
// directory. Placement rules (task-step-only, workspace-only) are enforced in
// validateStepArtifactDecls.
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

// checkMappingKeys rejects a mapping key that names no declared artifact, and
// a mapping value that is not a plain artifact name — see
// validateArtifactMappings for why the value check is load-bearing here.
func checkMappingKeys(label, field string, mapping map[string]string, declared []string) error {
	if len(mapping) == 0 {
		return nil
	}

	declaredSet := make(map[string]bool, len(declared))
	for _, name := range declared {
		declaredSet[name] = true
	}

	for key, value := range mapping {
		if !declaredSet[key] {
			return fmt.Errorf("%s: %s key %q is not a declared %s", label, field, key,
				map[string]string{"input_mapping": "input", "output_mapping": "output"}[field])
		}

		err := ValidateArtifactName(value)
		if err != nil {
			return fmt.Errorf("%s: %s value for %q: %w", label, field, key, err)
		}
	}

	return nil
}

// validateArtifactNames checks every name in inputs/outputs against
// artifactNamePattern (see workspace.go) and rejects duplicates within a
// list. A name appearing in BOTH lists is legal and means read-modify-write:
// the step's directory is materialized from the artifact's current content
// (as an input) and captured back over it (as an output) — the way a revise
// loop carries state between visits now that every step's view is only its
// declarations.
func validateArtifactNames(context string, inputs, outputs []string) error {
	check := func(names []string, kind string) error {
		seen := map[string]bool{}

		for _, name := range names {
			err := ValidateArtifactName(name)
			if err != nil {
				return fmt.Errorf("%s: %w", context, err)
			}

			if seen[name] {
				return fmt.Errorf("%s: duplicate %s %q", context, kind, name)
			}

			seen[name] = true
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
