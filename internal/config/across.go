package config

// The across: modifier — run one step once per combination of values.

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/template"
)

// AcrossVar is one axis of a matrix: a variable name and the values it takes,
// known when the pipeline is written.
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

	axes := resolveAxes(step.Across)
	combos := combinations(axes)

	cells := make([]Step, 0, len(combos))

	// Every cell is rendered HERE, before any of them runs. That is what makes
	// a field reference an all-or-nothing check: an author's typo fails the
	// whole block loudly, rather than failing cell 7 of 40 after the first six
	// have already run.
	for _, combo := range combos {
		cell := step
		cell.Across = nil

		err = renderCell(label, &cell, combo)
		if err != nil {
			return nil, err
		}

		cells = append(cells, cell)
	}

	return cells, nil
}

// acrossAxis is one resolved axis: its var name and the values it takes.
type acrossAxis struct {
	name   string
	values []string
}

// acrossCombo is one cell's coordinates: the values its template renders
// against.
type acrossCombo struct {
	vars map[string]string
}

// resolveAxes converts the declared axes into their resolved form.
func resolveAxes(declared []AcrossVar) []acrossAxis {
	axes := make([]acrossAxis, len(declared))

	for i, axis := range declared {
		axes[i] = acrossAxis{name: axis.Var, values: axis.Values}
	}

	return axes
}

// validateAcross checks every across: step in the pipeline at load, so a
// malformed matrix costs a load rather than a run.
func (c *Config) validateAcross() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			// Checked before the early return below, so max_in_flight: on a
			// step with no across: is caught rather than silently ignored.
			err := c.validateAcrossConcurrency(label, step)
			if err != nil {
				return err
			}

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

// validateAcrossConcurrency checks max_in_flight: — that it describes a matrix
// at all, and that a concurrent one has somewhere safe to run.
//
// The isolation requirement is race:'s, for the same reason. A matrix's cells
// are one step's clones: they declare the same outputs: and their commands
// write the same paths, so under the shared strategy — where every step's
// working directory IS the build root — two cells running at once are two
// writers on one file. Serial cells never collide, which is why this is a
// requirement of the concurrency and not of across:.
func (c *Config) validateAcrossConcurrency(label string, step *Step) error {
	if step.MaxInFlight == 0 {
		return nil
	}

	if step.MaxInFlight < 0 {
		return fmt.Errorf("%s: max_in_flight is %d; it counts cells, so it cannot be negative", label, step.MaxInFlight)
	}

	if len(step.Across) == 0 {
		return fmt.Errorf("%s: max_in_flight is only valid on an across: step; it bounds how many cells run at once", label)
	}

	// 1 is the serial default spelled out. It changes nothing, so it needs
	// nothing — refusing it would make "be explicit about the default" an
	// error.
	if step.MaxInFlight > 1 && c.Workspace == nil {
		return fmt.Errorf("%s: max_in_flight: %d requires workspace isolation (set a top-level workspace: strategy); concurrent cells are clones of one step writing the same paths, and would otherwise share one mutable directory", label, step.MaxInFlight)
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
func combinations(axes []acrossAxis) []acrossCombo {
	combos := []acrossCombo{{vars: map[string]string{}}}

	for _, axis := range axes {
		next := make([]acrossCombo, 0, len(combos)*len(axis.values))

		for _, combo := range combos {
			for _, value := range axis.values {
				expanded := acrossCombo{vars: make(map[string]string, len(combo.vars)+1)}

				for k, v := range combo.vars {
					expanded.vars[k] = v
				}

				expanded.vars[axis.name] = value

				next = append(next, expanded)
			}
		}

		combos = next
	}

	return combos
}

// renderableField is one field a cell's `{{ .vars.<name> }}` substitution
// reaches, paired with the name an error should call it by.
type renderableField struct {
	name  string
	value *string
}

// renderableFields returns the fields where a matrix value can meaningfully
// differ per cell.
//
// The set is deliberately small: the command, the container it runs in, the
// prompt, the working directory, the files the conversation opens holding, and
// the names the step is known by. Rendering everything would mean a template
// failure in an unrelated field could break a pipeline that never asked for
// one.
//
// context_paths: is a LIST, so it contributes one entry per element rather
// than one entry: a renderableField is a single string, and an entry named
// `context_paths[1]` puts an error on the path that has the mistake instead of
// on the whole list. Each entry points INTO the slice, which is why renderCell
// clones it first.
func renderableFields(cell *Step) []renderableField {
	fields := make([]renderableField, 0, 7+len(cell.ContextPaths))
	fields = append(fields,
		renderableField{"task", &cell.Task},
		renderableField{"run", &cell.Run},
		renderableField{"image", &cell.Image},
		renderableField{"prompt", &cell.Prompt},
		renderableField{"dir", &cell.Dir},
		renderableField{"put", &cell.Put},
		renderableField{"get", &cell.Get},
	)

	for i := range cell.ContextPaths {
		fields = append(fields, renderableField{fmt.Sprintf("context_paths[%d]", i), &cell.ContextPaths[i]})
	}

	return fields
}

// renderCell substitutes `{{ .vars.<name> }}` into the fields where a matrix
// value can meaningfully differ per cell.
func renderCell(label string, cell *Step, combo acrossCombo) error {
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

		err := renderCell(label, &inner, combo)
		if err != nil {
			return err
		}

		cell.Try = &inner

		return nil
	}

	// Captured BEFORE rendering: whether the author distinguished the cells
	// themselves is a question about the template they wrote, not about the
	// text it happened to produce (see nameCell).
	templateName := stepName(*cell)

	// The same aliasing the Try pointer above has, for the same reason:
	// ExpandAcross builds a cell by assigning the step, which copies the struct
	// but SHARES the array behind context_paths with every other cell.
	// renderableFields hands out pointers INTO that array, so rendering in
	// place would have cell 1 consume the template and leave cell 2 with cell
	// 1's paths.
	cell.ContextPaths = slices.Clone(cell.ContextPaths)

	for _, field := range renderableFields(cell) {
		rendered, err := renderVars(*field.value, combo)
		if err != nil {
			return fmt.Errorf("%s: across %s: %w", label, field.name, err)
		}

		*field.value = rendered
	}

	nameCell(cell, templateName, combo.vars)

	return nil
}

// nameCell gives a cell an identity distinct from its siblings.
//
// The coordinates land on Label, NOT on the task:/agent:/put: field they used
// to be appended to. Those fields are lookup keys — FindTask, FindAgent, the
// resource — so renaming them renamed the thing being looked up: a matrix over
// a shared tasks: entry failed with `no task named "shared [shard=b]"`, and an
// agent cell could not be renamed at all, leaving every agent cell of a matrix
// answering to one name.
//
// An author who interpolated a variable into the name themselves has already
// made the cells distinct, so nothing is added — their name is the identity,
// and a coordinate suffix on top would be noise.
//
// templateName is the name BEFORE substitution, and asking it rather than the
// rendered name is the whole point. The old check looked for a cell's value as
// a substring of its rendered name, which is a coincidence detector: a matrix
// over `values: [a, b]` on a task named "shared" found the "a" in "shared" and
// concluded the author had distinguished that cell, so one cell was named
// "shared" and its sibling "shared [shard=b]". Whether the author interpolated
// anything is a fact about the template, and the template can simply be asked.
func nameCell(cell *Step, templateName string, vars map[string]string) {
	if len(vars) == 0 || templateName == "" || strings.Contains(templateName, "{{") {
		return
	}

	cell.Label = stepName(*cell) + " [" + coordinates(vars) + "]"
}

// coordinates renders a cell's variables in declaration-independent, stable
// order for a name suffix.
func coordinates(vars map[string]string) string {
	keys := slices.Sorted(maps.Keys(vars))

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+vars[key])
	}

	return strings.Join(parts, " ")
}

// renderVars applies `{{ .vars.<name> }}` substitution to one field.
func renderVars(value string, combo acrossCombo) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}

	parsed, err := template.New("across").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("could not parse the template: %w", err)
	}

	var out bytes.Buffer

	err = parsed.Execute(&out, map[string]any{"vars": combo.vars})
	if err != nil {
		return "", fmt.Errorf("could not render the template: %w", err)
	}

	return out.String(), nil
}
