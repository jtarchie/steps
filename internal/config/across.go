package config

// The across: modifier — run one step once per combination of values.

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

// AcrossVar is one axis of a matrix: a variable name and the values it takes.
type AcrossVar struct {
	Var    string   `yaml:"var"`
	Values []string `yaml:"values"`
}

// ExpandAcross renders an across: step into its cells: one step per
// combination of values, with `{{ .vars.<name> }}` substituted into the
// templatable fields, in row-major order over the axes as declared.
//
// The cells are SIBLINGS, not a sequence. Each is hashed and cached
// independently under the across block, which is what gives the headline
// property: changing one value in one axis re-runs only the cells that value
// appears in. Chaining them instead would mean a changed cell invalidated
// every cell before it, since a chain is identified by its leaf — which is
// exactly the whole-matrix re-run this exists to avoid.
//
// Cells run in declaration order rather than concurrently: they commonly share
// a workspace, and a matrix's value is mostly in not hand-maintaining the
// copies. Put an in_parallel: inside a cell if a cell's own work should
// overlap.
func ExpandAcross(label string, step Step) ([]Step, error) {
	err := validateAcrossAxes(label, step.Across)
	if err != nil {
		return nil, err
	}

	combos := combinations(step.Across)

	cells := make([]Step, 0, len(combos))

	for _, vars := range combos {
		cell := step
		cell.Across = nil

		err = renderCell(label, &cell, vars)
		if err != nil {
			return nil, err
		}

		cells = append(cells, cell)
	}

	return cells, nil
}

// validateAcross checks every across: step in the pipeline at load, so a
// malformed matrix costs a load rather than a run.
func (c *Config) validateAcross() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if len(step.Across) == 0 {
				return nil
			}

			_, expandErr := ExpandAcross(label, *step)

			return expandErr
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAcrossAxes rejects a matrix that cannot mean anything: no axes, an
// axis with no name or no values, or two axes sharing a name (where one would
// silently shadow the other).
func validateAcrossAxes(label string, axes []AcrossVar) error {
	seen := map[string]bool{}

	for i, axis := range axes {
		switch {
		case axis.Var == "":
			return fmt.Errorf("%s: across[%d] has no var: name", label, i)
		case len(axis.Values) == 0:
			return fmt.Errorf("%s: across[%d] (%s) has no values:; an axis with nothing in it would expand to no steps at all", label, i, axis.Var)
		case seen[axis.Var]:
			return fmt.Errorf("%s: across declares var %q twice; the second would silently shadow the first", label, axis.Var)
		}

		seen[axis.Var] = true
	}

	return nil
}

// combinations returns every combination of the axes' values, row-major: the
// LAST axis varies fastest, so `[go_version, package]` reads
// 1.25/agent, 1.25/pipeline, 1.26/agent, 1.26/pipeline.
func combinations(axes []AcrossVar) []map[string]string {
	combos := []map[string]string{{}}

	for _, axis := range axes {
		next := make([]map[string]string, 0, len(combos)*len(axis.Values))

		for _, combo := range combos {
			for _, value := range axis.Values {
				expanded := make(map[string]string, len(combo)+1)
				for k, v := range combo {
					expanded[k] = v
				}

				expanded[axis.Var] = value
				next = append(next, expanded)
			}
		}

		combos = next
	}

	return combos
}

// renderCell substitutes `{{ .vars.<name> }}` into the fields where a matrix
// value can meaningfully differ per cell.
//
// The set is deliberately small: the command, the container it runs in, the
// prompt, the working directory, and the names the step is known by. Rendering
// everything would mean a template failure in an unrelated field could break a
// pipeline that never asked for one.
func renderCell(label string, cell *Step, vars map[string]string) error {
	// A try: cell keeps every renderable field on the step it wraps rather
	// than on itself, so render through the wrapper. Without this a matrix
	// whose body is a try: rendered NOTHING: each cell ran the literal
	// `{{ .vars.x }}` text and every cell answered to the same unroutable
	// name, since the auto-naming below reads a Task that is always empty on
	// the wrapper.
	//
	// The copy is the load-bearing part. ExpandAcross builds a cell by
	// assigning the step, which copies the struct but SHARES the Try pointer
	// with every other cell — rendering through it in place means cell 1
	// consumes the template and cell 2 finds nothing left to substitute, so
	// all of them end up with cell 1's command under one name.
	if cell.Try != nil {
		inner := *cell.Try

		err := renderCell(label, &inner, vars)
		if err != nil {
			return err
		}

		cell.Try = &inner

		return nil
	}

	fields := []struct {
		name  string
		value *string
	}{
		{"task", &cell.Task},
		{"run", &cell.Run},
		{"image", &cell.Image},
		{"prompt", &cell.Prompt},
		{"dir", &cell.Dir},
		{"put", &cell.Put},
		{"get", &cell.Get},
	}

	for _, field := range fields {
		rendered, err := renderVars(*field.value, vars)
		if err != nil {
			return fmt.Errorf("%s: across %s: %w", label, field.name, err)
		}

		*field.value = rendered
	}

	// A cell that renders to the same name as its siblings is unroutable and
	// unreadable in a log, so name it by its coordinates when the author did
	// not do it themselves.
	if cell.Task != "" && !strings.Contains(cell.Task, "{{") && len(vars) > 0 && cellNameIsShared(cell, vars) {
		cell.Task += " [" + coordinates(vars) + "]"
	}

	return nil
}

// cellNameIsShared reports whether the step's name would be identical across
// cells — true whenever the author interpolated no variable into it.
func cellNameIsShared(cell *Step, vars map[string]string) bool {
	for _, value := range vars {
		if strings.Contains(cell.Task, value) {
			return false
		}
	}

	return true
}

// coordinates renders a cell's variables in declaration-independent, stable
// order for a name suffix.
func coordinates(vars map[string]string) string {
	keys := make([]string, 0, len(vars))
	for key := range vars {
		keys = append(keys, key)
	}

	// Sorted so a cell's name is the same on every run, which routing and
	// assert.execution both depend on.
	slices.Sort(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+vars[key])
	}

	return strings.Join(parts, " ")
}

// renderVars applies `{{ .vars.<name> }}` substitution to one field.
func renderVars(value string, vars map[string]string) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}

	parsed, err := template.New("across").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("could not parse the template: %w", err)
	}

	var out bytes.Buffer

	err = parsed.Execute(&out, map[string]any{"vars": vars})
	if err != nil {
		return "", fmt.Errorf("could not render the template: %w", err)
	}

	return out.String(), nil
}
