package config

// The across: modifier — run one step once per combination of values.

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// AcrossVar is one axis of a matrix: a variable name and the values it takes.
//
// The values come from exactly one of two places. values: is the static list,
// known when the pipeline is written. from_file: names a JSON file an earlier
// step produced, so the matrix's width is decided during the run — "review
// each of these findings", where nobody knew the findings when the pipeline
// was authored.
type AcrossVar struct {
	Var    string   `yaml:"var"`
	Values []string `yaml:"values,omitempty"`
	// FromFile is a workspace-relative path to a JSON array of strings, whose
	// first path component names the artifact holding it (`findings/items.json`
	// requires an artifact `findings` fetched or produced earlier in the plan —
	// the same rule dir: follows, checked by workspace.ValidateArtifactFlow).
	//
	// A file rather than a key in a store: the producing step already declares
	// the artifact in its outputs:, so the value travels the way every other
	// piece of inter-step data does, and there is nothing extra to opt into on
	// the writing side. Mutually exclusive with Values.
	FromFile string `yaml:"from_file,omitempty"`
}

// Runtime reports whether this axis takes its values from a file an earlier
// step wrote, rather than from the pipeline text.
func (a AcrossVar) Runtime() bool {
	return a.FromFile != ""
}

// SourceArtifact is the artifact a from_file: axis reads from: the path's
// first component, the way dir: names one. "" for a static axis.
//
// Defined here rather than in each consumer so the plan-time availability
// check (internal/workspace) and the run-time read (internal/pipeline) can
// never disagree about which artifact a path names.
func (a AcrossVar) SourceArtifact() string {
	if !a.Runtime() {
		return ""
	}

	cleaned := path.Clean(a.FromFile)
	if i := strings.Index(cleaned, "/"); i >= 0 {
		return cleaned[:i]
	}

	return cleaned
}

// HasRuntimeAxis reports whether any axis of step's matrix is file-valued,
// which is what makes the whole matrix un-expandable at load time.
func HasRuntimeAxis(step Step) bool {
	for _, axis := range step.Across {
		if axis.Runtime() {
			return true
		}
	}

	return false
}

// MaxAcrossItems bounds how many items one from_file: axis may expand to.
//
// The array is produced during the run, often by a model, so its length is not
// something the pipeline author reviewed. Each item becomes a cell with its own
// hash, workspace and possibly its own model call, so an unbounded array turns
// a typo upstream into an unbounded bill. Refusing with a message that says
// what to do beats discovering it halfway through.
const MaxAcrossItems = 1000

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
	return ExpandAcrossValues(label, step, nil)
}

// ExpandAcrossValues is ExpandAcross with the file axes already read: runtime
// maps each from_file: axis's var name to the values it took this run.
//
// Two entry points rather than one because the two happen at different times.
// A static matrix expands at load, so a malformed one costs a load rather than
// a run; a file axis cannot exist until the step that writes it has run, so it
// expands in internal/pipeline instead. Both share every rule below, which is
// what keeps a file-driven cell hashing identically to the static cell it is
// indistinguishable from.
func ExpandAcrossValues(label string, step Step, runtime map[string][]string) ([]Step, error) {
	err := validateAcrossAxes(label, step.Across)
	if err != nil {
		return nil, err
	}

	axes, err := resolveAxes(label, step.Across, runtime)
	if err != nil {
		return nil, err
	}

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

// resolveAxes substitutes the read values into any from_file: axis, leaving a
// static axis untouched. A file axis with nothing supplied is a programming
// error in the caller, not a config error — every run-time path reads every
// file axis before expanding — so it says so plainly rather than expanding to
// zero cells and looking like a matrix that ran.
func resolveAxes(label string, declared []AcrossVar, runtime map[string][]string) ([]acrossAxis, error) {
	axes := make([]acrossAxis, len(declared))

	for i, axis := range declared {
		if !axis.Runtime() {
			axes[i] = acrossAxis{name: axis.Var, values: axis.Values}

			continue
		}

		values, ok := runtime[axis.Var]
		if !ok {
			return nil, fmt.Errorf("%s: across var %q takes its values from %q, which was not read before expanding", label, axis.Var, axis.FromFile)
		}

		axes[i] = acrossAxis{name: axis.Var, values: values}
	}

	return axes, nil
}

// validateAcross checks every across: step in the pipeline at load, so a
// malformed matrix costs a load rather than a run.
//
// A matrix with a from_file: axis is checked but not expanded: its width is
// not known until the step that writes the file has run. The axis rules still
// apply, so the shape mistakes a load can catch are still caught at load.
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

			if HasRuntimeAxis(*step) {
				err = validateAcrossAxes(label, step.Across)
				if err != nil {
					return err
				}

				// The templates are the one thing a file-driven matrix would
				// otherwise never have checked. A static matrix gets them
				// validated as a side effect of expanding here; a file one
				// cannot expand until the step that writes its source has run,
				// so without this an unclosed brace loads clean and fails
				// mid-run — after the step that produced the array has already
				// been paid for.
				return validateAcrossTemplates(label, *step)
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
// axis with no name, an axis taking its values from nowhere or from two places
// at once, or two axes sharing a name (where one would silently shadow the
// other).
func validateAcrossAxes(label string, axes []AcrossVar) error {
	seen := map[string]bool{}

	for i, axis := range axes {
		switch {
		case axis.Var == "":
			return fmt.Errorf("%s: across[%d] has no var: name", label, i)
		case len(axis.Values) > 0 && axis.Runtime():
			return fmt.Errorf("%s: across[%d] (%s) sets both values: and from_file:; an axis takes its values from one place or the other", label, i, axis.Var)
		case len(axis.Values) == 0 && !axis.Runtime():
			return fmt.Errorf("%s: across[%d] (%s) has no values: and no from_file:; an axis with nothing in it would expand to no steps at all", label, i, axis.Var)
		case seen[axis.Var]:
			return fmt.Errorf("%s: across declares var %q twice; the second would silently shadow the first", label, axis.Var)
		}

		err := checkFromFilePath(label, i, axis)
		if err != nil {
			return err
		}

		seen[axis.Var] = true
	}

	return nil
}

// checkFromFilePath rejects a from_file: that does not name a file inside an
// artifact: an absolute path, or one that climbs out with "..". The path is
// resolved against the step's materialized workspace at run time, so anything
// escaping it names a file the pipeline does not own — a load error rather
// than a run-time surprise.
func checkFromFilePath(label string, i int, axis AcrossVar) error {
	if !axis.Runtime() {
		return nil
	}

	cleaned := path.Clean(axis.FromFile)

	switch {
	case path.IsAbs(axis.FromFile):
		return fmt.Errorf("%s: across[%d] (%s) from_file %q is absolute; it must be a path inside an artifact, like findings/items.json", label, i, axis.Var, axis.FromFile)
	case cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../"):
		return fmt.Errorf("%s: across[%d] (%s) from_file %q escapes the workspace; it must be a path inside an artifact, like findings/items.json", label, i, axis.Var, axis.FromFile)
	case !strings.Contains(cleaned, "/"):
		return fmt.Errorf("%s: across[%d] (%s) from_file %q names no artifact; the first path component is the artifact holding the file, as in findings/items.json", label, i, axis.Var, axis.FromFile)
	}

	return nil
}

// validateAcrossTemplates checks a from_file: matrix's templates at load time.
//
// A static matrix expands during validation, so a malformed template is caught
// as a side effect of rendering it. A file-driven matrix cannot expand until
// the step that writes its source has run — so without this, an unclosed brace
// or a misspelled axis name loads clean and fails mid-run, after the step that
// produced the array has already been paid for.
//
// Two things are knowable without the values: whether the template PARSES, and
// whether every `{{ .vars.x }}` names an axis this matrix actually declares.
func validateAcrossTemplates(label string, step Step) error {
	declared := make(map[string]bool, len(step.Across))
	for _, axis := range step.Across {
		declared[axis.Var] = true
	}

	// A try: cell keeps every renderable field on the step it wraps, so that is
	// where the templates are — the same unwrap renderCell does.
	cell := step

	for _, field := range renderableFields(unwrapStep(&cell)) {
		if !strings.Contains(*field.value, "{{") {
			continue
		}

		parsed, err := template.New("across").Option("missingkey=error").Parse(*field.value)
		if err != nil {
			return fmt.Errorf("%s: across %s: could not parse the template: %w", label, field.name, err)
		}

		err = checkAxisReferences(label, field.name, parsed.Tree, declared)
		if err != nil {
			return err
		}
	}

	return nil
}

// checkAxisReferences rejects a `{{ .vars.x }}` naming an axis the matrix does
// not declare — the file-matrix half of the misspelling a static matrix catches
// by expanding, where missingkey=error reports it.
func checkAxisReferences(label, field string, tree *parse.Tree, declared map[string]bool) error {
	if tree == nil || tree.Root == nil {
		return nil
	}

	var unknown string

	walkTemplateFields(tree.Root, func(node *parse.FieldNode) {
		if unknown == "" && len(node.Ident) >= 2 && node.Ident[0] == "vars" && !declared[node.Ident[1]] {
			unknown = node.Ident[1]
		}
	})

	if unknown != "" {
		// Sorted so the suggestion is drawn from the same list on every run.
		return fmt.Errorf("%s: across %s: {{ .vars.%s }} names no axis of this matrix%s",
			label, field, unknown, suggestion(unknown, slices.Sorted(maps.Keys(declared))))
	}

	return nil
}

// walkTemplateFields calls fn for every field reference in a parsed template.
func walkTemplateFields(node parse.Node, fn func(*parse.FieldNode)) {
	if field, ok := node.(*parse.FieldNode); ok {
		fn(field)

		return
	}

	for _, child := range templateChildren(node) {
		walkTemplateFields(child, fn)
	}
}

// templateChildren returns the nodes below one template node, so the walk above
// is a plain recursion rather than a dispatch that has to remember to recurse
// in every arm.
func templateChildren(node parse.Node) []parse.Node {
	switch n := node.(type) {
	case *parse.ListNode:
		// A nil list is the ordinary shape of an absent {{ else }} body, not an
		// error — and it is a TYPED nil, so it must be caught here rather than
		// by a nil check on the interface.
		if n == nil {
			return nil
		}

		return n.Nodes
	case *parse.ActionNode:
		return []parse.Node{n.Pipe}
	case *parse.PipeNode:
		if n == nil {
			return nil
		}

		children := make([]parse.Node, 0, len(n.Cmds))
		for _, cmd := range n.Cmds {
			children = append(children, cmd)
		}

		return children
	case *parse.CommandNode:
		return n.Args
	default:
		return branchChildren(node)
	}
}

// branchChildren returns the children of the three node kinds that carry a
// condition and two bodies. They are split out because they are identical to
// each other and to nothing else — {{ if }}, {{ range }} and {{ with }} all
// embed parse.BranchNode, but it exposes no interface to dispatch on.
//
// Skipping them would leave a hole: a misspelled axis inside an {{ if }} body
// would reach run time unchecked.
func branchChildren(node parse.Node) []parse.Node {
	var branch *parse.BranchNode

	switch n := node.(type) {
	case *parse.IfNode:
		branch = &n.BranchNode
	case *parse.RangeNode:
		branch = &n.BranchNode
	case *parse.WithNode:
		branch = &n.BranchNode
	default:
		return nil
	}

	return []parse.Node{branch.Pipe, branch.List, branch.ElseList}
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
