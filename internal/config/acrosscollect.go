package config

// A collecting matrix: one artifact, each cell captured under its own
// coordinates instead of over its siblings.

import (
	"fmt"
)

// collectsOutputs reports whether this matrix captures each cell's outputs
// under a per-cell subdirectory — which it does exactly when the cell template
// declares outputs: (through a try: wrapper, where the template's fields live).
func collectsOutputs(step Step) bool {
	return len(CollectedOutputs(step)) > 0
}

// CollectedOutputs returns the output names an across: step collects — the
// outputs: of the step its try: chain wraps, which is where a cell template
// keeps its fields. Empty when the matrix does not collect. Exported for
// internal/pipeline, which resets the collected artifacts before the block's
// cells capture (see CollectedArtifacts).
func CollectedOutputs(step Step) []string {
	return step.Unwrap().Outputs
}

// validateCollectedNotAlsoInput refuses read-modify-write on a COLLECTED
// output. An ordinary step may name one artifact as both input and output —
// the directory materializes from what the artifact holds and is captured
// back over it. A collecting matrix cannot: the block resets each collected
// artifact to empty before any cell runs (see internal/pipeline's
// resetCollectedArtifacts, which is what makes per-coordinate capture
// replace rather than merge), so every cell would materialize an empty
// directory where the author expected the previous content. Refused rather
// than silently emptied.
func validateCollectedNotAlsoInput(label string, step Step) error {
	inner := step.Unwrap()

	collected := make(map[string]bool, len(inner.Outputs))
	for _, out := range inner.Outputs {
		collected[out] = true
	}

	for _, in := range inner.InputNames() {
		if collected[in] {
			return fmt.Errorf("%s: %q is both an input and a collected output of an across: step; the block empties a collected artifact before its cells run, so every cell would see an empty %q — read from a differently-named artifact",
				label, in, in)
		}
	}

	return nil
}

// CollectedArtifacts returns the store-level artifact names a collecting
// matrix's cells capture into: the collected outputs, renamed through any
// author-written output_mapping — the same rename CollectedOutputMapping
// composes the cell coordinates onto.
func CollectedArtifacts(step Step) []string {
	inner := step.Unwrap()

	names := make([]string, 0, len(inner.Outputs))

	for _, out := range inner.Outputs {
		if mapped, ok := inner.OutputMapping[out]; ok {
			out = mapped
		}

		names = append(names, out)
	}

	return names
}

// CollectedOutputMapping is the output mapping a collecting matrix's cell
// captures through: each declared output, redirected under the cell's own
// coordinates — findings -> findings/alpha/fast. Composed AFTER any
// author-written output_mapping (mapping renames the artifact; the
// coordinates say where inside it this cell lands), and nil when the cell is
// not part of a collecting matrix, so every other step passes through
// untouched.
//
// One function, called by both internal/pipeline (task cells) and
// internal/agent (agent cells), so the two kinds of cell can never disagree
// about where a capture lands.
func CollectedOutputMapping(outputs []string, mapping map[string]string, subdir string) map[string]string {
	if subdir == "" || len(outputs) == 0 {
		return mapping
	}

	collected := make(map[string]string, len(outputs))

	for _, out := range outputs {
		name := out
		if mapped, ok := mapping[out]; ok {
			name = mapped
		}

		collected[out] = name + "/" + subdir
	}

	return collected
}

// validateCollectedValues enforces what a collecting matrix needs of its axis
// values, which ordinary interpolation does not: each value becomes a path
// segment of the collected artifact, so it must be directory-name-shaped, and
// it must be unique within its axis — two cells with one value would capture
// into one directory, the exact clobber collection exists to remove.
func validateCollectedValues(label string, axes []acrossAxis) error {
	for _, axis := range axes {
		seen := make(map[string]bool, len(axis.values))

		for _, value := range axis.values {
			if !artifactNamePattern.MatchString(value) {
				return fmt.Errorf("%s: across var %q value %q cannot name a directory of the collected output (must match %s); use short ids as values and put the detail in files",
					label, axis.name, value, artifactNamePattern.String())
			}

			if seen[value] {
				return fmt.Errorf("%s: across var %q lists %q twice; a collecting matrix captures each cell under its value, so two cells with one value would write one directory",
					label, axis.name, value)
			}

			seen[value] = true
		}
	}

	return nil
}

// validateAcrossOutputs enforces what a matrix that produces artifacts needs.
//
// strategy: copy, because collection IS the capture step: each cell's outputs
// are materialized under its own coordinates (findings/alpha/...), which the
// btrfs backend's snapshot capture cannot express.
//
// And the outputs must be declared ON THE STEP, not inherited from a tasks:
// entry: the step is where a reader looks to see whether a matrix collects,
// and an inherited declaration would make N cells capture one artifact name
// with nothing in this file saying so.
func (c *Config) validateAcrossOutputs(label string, step *Step) error {
	err := c.validateCollectShape(label, step)
	if err != nil {
		return err
	}

	if collectsOutputs(*step) {
		// ponytail: btrfs captures artifacts as subvolume snapshots, which can
		// neither land under an uncreated parent nor survive being nested
		// inside a later snapshot (nested subvolumes come out empty) — so a
		// collected artifact would silently lose every cell's files on the
		// consuming side. Supporting it means teaching that backend a
		// flattening capture; until then, refuse rather than corrupt.
		if c.Workspace.EffectiveStrategy() != "copy" {
			return fmt.Errorf("%s: an across: step with outputs: is not supported under workspace strategy %q; use strategy: copy", label, c.Workspace.EffectiveStrategy())
		}

		return validateCollectedNotAlsoInput(label, *step)
	}

	inner := step.Unwrap()
	if inner.Task == "" {
		return nil
	}

	rt, err := c.ResolveTask(inner)
	if err != nil {
		return nil //nolint:nilerr // validateStepReferences reports an unresolvable task
	}

	if len(rt.Outputs) > 0 {
		return fmt.Errorf("%s: task %q declares outputs %v, so every cell of this matrix would capture the same artifact; declare outputs: on the across: step itself, which collects each cell's under its own coordinates", label, rt.Name, rt.Outputs)
	}

	return nil
}

// validateCollectShape refuses the outputs: placements a matrix cannot honor.
//
// On a try: wrapper level, an output is dead text: collection reads the step
// the chain wraps (collectsOutputs), capture never touches wrapper fields,
// and flow validation doesn't credit them either — the author who wrote it
// watches their matrix silently not collect.
//
// Buried anywhere else a cell executes — a hook, or a step nested inside a
// container body — an output is captured at its PLAIN name by every cell:
// none of the per-cell coordinate mapping the collect position gets, so
// serially the last cell wins and under max_in_flight: it is a
// remove-during-copy race. The exact clobber collection exists to remove.
//
// And a collecting matrix whose try: chain wraps ANOTHER across: step
// re-expands that inner matrix at run time, where the inner cells' renderCell
// stamps THEIR coordinates over the outer cell's — every outer cell captures
// the same inner paths, silently erasing its siblings' contribution.
func (c *Config) validateCollectShape(label string, step *Step) error {
	for wrapper := step; wrapper.Try != nil; wrapper = wrapper.Try {
		if len(wrapper.Outputs) > 0 {
			return fmt.Errorf("%s: outputs: on the try: wrapper of an across: step is never collected; declare it on the step the try: wraps, which is where the block reads it", label)
		}
	}

	if name, site, found := c.buriedCellOutput(step); found {
		return fmt.Errorf("%s: output %q is declared on the matrix cell's %s, which every cell would capture at its plain name — the clobber outputs: on the across: step exists to remove; a matrix cell's hooks and nested steps must not declare outputs", label, name, site)
	}

	if collectsOutputs(*step) {
		for wrapper := step.Try; wrapper != nil; wrapper = wrapper.Try {
			if len(wrapper.Across) > 0 {
				return fmt.Errorf("%s: a collecting across: step cannot wrap another across: step; the inner matrix's capture coordinates would replace the outer cell's, so outer cells would silently overwrite each other — collect in the inner matrix and copy its artifact in an ordinary step instead", label)
			}
		}
	}

	return nil
}

// buriedCellOutput finds an outputs: declaration in a part of the matrix's
// cell template that per-cell collection never coordinates: a hook step at
// any level of the try: chain, or a step nested inside a container body
// (do:/in_parallel:/race:/ensemble:). The collect position itself — the step
// the try: chain wraps — is the one place an output is legal, and a container
// can never be it (outputs: on a container step is rejected at decode).
func (c *Config) buriedCellOutput(step *Step) (name, site string, found bool) {
	for level := step; level != nil; level = level.Try {
		if n, s, ok := c.hookOutput(level, ""); ok {
			return n, s, true
		}
	}

	return c.containerChildOutput(unwrapStep(step), "")
}

// subtreeOutput returns the first output this step's tree produces: its own
// (declared, or inherited from the tasks: entry it references — capture uses
// the RESOLVED set), a try: level's, a hook's, or a container child's.
func (c *Config) subtreeOutput(step *Step, site string) (name string, foundSite string, found bool) {
	if outs := c.stepProducedOutputs(*step); len(outs) > 0 {
		return outs[0], site, true
	}

	if n, s, ok := c.hookOutput(step, site+" "); ok {
		return n, s, true
	}

	if step.Try != nil {
		return c.subtreeOutput(step.Try, site)
	}

	return c.containerChildOutput(step, site+" ")
}

// hookOutput scans a step's hooks for an output declaration anywhere in their
// trees. prefix carries the enclosing site ("" at the top, "<site> " within
// one), so the reported location reads outermost-first.
func (c *Config) hookOutput(step *Step, prefix string) (name, site string, found bool) {
	var foundName, foundSite string

	_ = step.Hooks.Each(func(hook string, hookStep *Step) error {
		if foundName == "" {
			if n, s, ok := c.subtreeOutput(hookStep, prefix+hook+" hook"); ok {
				foundName, foundSite = n, s
			}
		}

		return nil
	})

	return foundName, foundSite, foundName != ""
}

// containerChildOutput scans a container step's children — do: steps and
// in_parallel:/race:/ensemble: branches — for an output declaration anywhere
// in their trees. Nothing for a leaf step.
func (c *Config) containerChildOutput(step *Step, prefix string) (name, site string, found bool) {
	for i := range step.Do {
		if n, s, ok := c.subtreeOutput(&step.Do[i], prefix+"do: step"); ok {
			return n, s, true
		}
	}

	for kind, branches := range branchesOf(step) {
		for i := range branches {
			if n, s, ok := c.subtreeOutput(&branches[i], prefix+kind+" branch"); ok {
				return n, s, true
			}
		}
	}

	return "", "", false
}

// stepProducedOutputs is what a step's capture would persist: the resolved
// tasks: entry's outputs for a task step (a step-level declaration overrides,
// see ResolveTask), the step's own declaration otherwise. Unresolvable task
// references fall back to the declaration — validateStepReferences reports
// those separately.
func (c *Config) stepProducedOutputs(step Step) []string {
	if step.Task != "" {
		rt, err := c.ResolveTask(step)
		if err == nil {
			return rt.Outputs
		}
	}

	return step.Outputs
}
