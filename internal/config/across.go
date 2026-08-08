package config

// The across: modifier — run one step once per combination of values.

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// AcrossVar is one axis of a matrix: a variable name and the values it takes.
//
// The values come from exactly one of two places. values: is the static list,
// known when the pipeline is written. from: names a run-context key holding a
// JSON array, so an earlier step decides at run time how wide the matrix is —
// "investigate each of these findings", where nobody knew the findings when
// the pipeline was authored.
type AcrossVar struct {
	Var    string   `yaml:"var"`
	Values []string `yaml:"values,omitempty"`
	// From is a run-context key (see ContextDir / set_context) whose value must
	// be a JSON array. Mutually exclusive with Values.
	From string `yaml:"from,omitempty"`
	// Label names the field a cell is IDENTIFIED by, when a from: axis holds
	// objects rather than strings. Coordinates need a scalar — a cell's name is
	// a routing target and an assert.execution entry — and an object has no one
	// obvious rendering. Absent, a cell is named by its 1-based position (#3),
	// which is deterministic but says nothing; naming the field that carries the
	// item's identity is what makes a matrix readable in a log.
	//
	// Only meaningful on a from: axis, since values: holds strings that already
	// name themselves.
	Label string `yaml:"label,omitempty"`
}

// Runtime reports whether this axis takes its values from the run context
// rather than from the pipeline text.
func (a AcrossVar) Runtime() bool {
	return a.From != ""
}

// HasRuntimeAxis reports whether any axis of step's matrix is runtime-valued,
// which is what makes the whole matrix un-expandable at load time.
func HasRuntimeAxis(step Step) bool {
	for _, axis := range step.Across {
		if axis.Runtime() {
			return true
		}
	}

	return false
}

// MaxAcrossItems bounds how many items one runtime axis may expand to.
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

// ExpandAcrossValues is ExpandAcross with the runtime axes already resolved:
// runtime maps each from: axis's var name to the values it took this run.
//
// Two entry points rather than one because the two happen at different times.
// A static matrix expands at load, so a malformed one costs a load rather than
// a run; a runtime matrix cannot exist until the step that fills its source
// has run, so it expands in internal/pipeline instead. Both share every rule
// below, which is what keeps a runtime cell hashing identically to the static
// cell it is indistinguishable from.
func ExpandAcrossValues(label string, step Step, runtime map[string][]any) ([]Step, error) {
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
	// a field reference an all-or-nothing check over an array nobody reviewed:
	// an item missing the field a template names fails the whole block loudly,
	// rather than failing cell 7 of 40 after the first six have already spent
	// their model calls.
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

// acrossItem is one value an axis takes: what the template sees, and the short
// scalar a cell taking it is NAMED by.
//
// The two differ only for an object item, where the template sees the fields
// and the name comes from whichever one label: points at. For a string the two
// are the same text, which is why an axis of strings behaves exactly as it did
// before objects existed.
type acrossItem struct {
	value   any // string, or map[string]string for an object item
	display string
}

// acrossAxis is one resolved axis: its var name and the items it takes.
type acrossAxis struct {
	name  string
	items []acrossItem
}

// acrossCombo is one cell's coordinates: the values its template renders
// against, and the scalar each of those is named by.
type acrossCombo struct {
	vars    map[string]any
	display map[string]string
	// objects names the vars holding an object, so a template that interpolates
	// one WITHOUT naming a field can be refused rather than rendering Go's map
	// syntax into a command.
	objects map[string]bool
}

// resolveAxes substitutes the runtime values into any from: axis, leaving a
// static axis untouched. A from: axis with nothing supplied is a programming
// error in the caller, not a config error — every runtime path resolves every
// axis before expanding — so it says so plainly rather than expanding to zero
// cells and looking like a matrix that ran.
func resolveAxes(label string, declared []AcrossVar, runtime map[string][]any) ([]acrossAxis, error) {
	axes := make([]acrossAxis, len(declared))

	for i, axis := range declared {
		if !axis.Runtime() {
			axes[i] = acrossAxis{name: axis.Var, items: staticItems(axis.Values)}

			continue
		}

		values, ok := runtime[axis.Var]
		if !ok {
			return nil, fmt.Errorf("%s: across var %q takes its values from context key %q, which was not resolved before expanding", label, axis.Var, axis.From)
		}

		items, err := runtimeItems(label, axis, values)
		if err != nil {
			return nil, err
		}

		axes[i] = acrossAxis{name: axis.Var, items: items}
	}

	return axes, nil
}

// staticItems turns a values: list into items. A static value is a string that
// names itself, so display is the value.
func staticItems(values []string) []acrossItem {
	items := make([]acrossItem, 0, len(values))
	for _, value := range values {
		items = append(items, acrossItem{value: value, display: value})
	}

	return items
}

// runtimeItems turns one from: axis's decoded array into items, naming each
// cell along the way.
//
// The array holds strings or flat objects — internal/pipeline decodes and
// enforces that, since it holds the raw JSON and can say what shape it found.
// What is decided HERE is naming, because label: is the field that says how.
func runtimeItems(label string, axis AcrossVar, values []any) ([]acrossItem, error) {
	items := make([]acrossItem, 0, len(values))

	for i, value := range values {
		switch item := value.(type) {
		case string:
			items = append(items, acrossItem{value: item, display: item})
		case map[string]string:
			display, err := objectDisplay(label, axis, item, i)
			if err != nil {
				return nil, err
			}

			items = append(items, acrossItem{value: item, display: display})
		default:
			return nil, fmt.Errorf("%s: across var %q: item %d has an unsupported type %T", label, axis.Var, i, value)
		}
	}

	return items, nil
}

// objectDisplay is the scalar an object item's cell is named by: the label:
// field when the axis names one, otherwise the item's 1-based position.
//
// A named field that the item does not carry is an error rather than a
// fallback to the index. The author asked for cells named by that field, and
// silently naming one of them "#4" would produce a matrix whose names cannot be
// predicted from the pipeline — which is what routing and assert.execution
// both depend on.
func objectDisplay(label string, axis AcrossVar, item map[string]string, index int) (string, error) {
	if axis.Label == "" {
		return fmt.Sprintf("#%d", index+1), nil
	}

	value, ok := item[axis.Label]
	if !ok {
		return "", fmt.Errorf("%s: across var %q: item %d has no %q field, which label: names as what its cell is called; every item must carry it",
			label, axis.Var, index, axis.Label)
	}

	if value == "" {
		return "", fmt.Errorf("%s: across var %q: item %d has an empty %q field, so its cell would have no name",
			label, axis.Var, index, axis.Label)
	}

	return value, nil
}

// validateAcross checks every across: step in the pipeline at load, so a
// malformed matrix costs a load rather than a run.
//
// A matrix with a runtime axis is checked but not expanded: its width is not
// known until the step that fills its source has run. The axis rules above
// still apply, so the shape mistakes a load can catch are still caught at load.
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
				return validateAcrossAxes(label, step.Across)
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
		case len(axis.Values) > 0 && axis.Runtime():
			return fmt.Errorf("%s: across[%d] (%s) sets both values: and from:; an axis takes its values from one place or the other", label, i, axis.Var)
		case len(axis.Values) == 0 && !axis.Runtime():
			return fmt.Errorf("%s: across[%d] (%s) has no values: and no from:; an axis with nothing in it would expand to no steps at all", label, i, axis.Var)
		case seen[axis.Var]:
			return fmt.Errorf("%s: across declares var %q twice; the second would silently shadow the first", label, axis.Var)
		case axis.Label != "" && !axis.Runtime():
			// A values: axis holds strings, which already name their own cells.
			// label: on one names a field of something that has no fields.
			return fmt.Errorf("%s: across[%d] (%s) sets label:, which names the field an OBJECT item's cell is called by; a values: axis holds strings that already name themselves", label, i, axis.Var)
		}

		seen[axis.Var] = true
	}

	return nil
}

// combinations returns every combination of the axes' values, row-major: the
// LAST axis varies fastest, so `[go_version, package]` reads
// 1.25/agent, 1.25/pipeline, 1.26/agent, 1.26/pipeline.
func combinations(axes []acrossAxis) []acrossCombo {
	combos := []acrossCombo{{vars: map[string]any{}, display: map[string]string{}, objects: map[string]bool{}}}

	for _, axis := range axes {
		next := make([]acrossCombo, 0, len(combos)*len(axis.items))

		for _, combo := range combos {
			for _, item := range axis.items {
				expanded := acrossCombo{
					vars:    make(map[string]any, len(combo.vars)+1),
					display: make(map[string]string, len(combo.display)+1),
					objects: make(map[string]bool, len(combo.objects)+1),
				}

				for k, v := range combo.vars {
					expanded.vars[k] = v
				}

				for k, v := range combo.display {
					expanded.display[k] = v
				}

				for k, v := range combo.objects {
					expanded.objects[k] = v
				}

				expanded.vars[axis.name] = item.value
				expanded.display[axis.name] = item.display

				if _, isObject := item.value.(map[string]string); isObject {
					expanded.objects[axis.name] = true
				}

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
		rendered, err := renderVars(*field.value, combo)
		if err != nil {
			return fmt.Errorf("%s: across %s: %w", label, field.name, err)
		}

		*field.value = rendered
	}

	nameCell(cell, templateName, combo.display)

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
func renderVars(value string, combo acrossCombo) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}

	parsed, err := template.New("across").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("could not parse the template: %w", err)
	}

	err = rejectBareObjectRefs(parsed.Tree, combo.objects)
	if err != nil {
		return "", err
	}

	var out bytes.Buffer

	err = parsed.Execute(&out, map[string]any{"vars": combo.vars})
	if err != nil {
		return "", fmt.Errorf("could not render the template: %w", err)
	}

	return out.String(), nil
}

// rejectBareObjectRefs refuses `{{ .vars.x }}` where x is an object item,
// leaving `{{ .vars.x.field }}` alone.
//
// Rendered rather than refused, a bare object interpolates Go's own map syntax
// — `map[claim:… file:…]` — into a command or a prompt. That is the "rendering
// of an object that would be a rule invented on the spot" which objects were
// originally kept out of from: for. Naming a field is the whole interface, so
// the one spelling that has no answer is an error that says so.
//
// The parse tree is asked rather than the text, because the text has too many
// spellings to match: `{{.vars.x}}`, `{{ .vars.x }}`, `{{ printf "%s" .vars.x }}`
// are one question, and a regex over them is a guess.
func rejectBareObjectRefs(tree *parse.Tree, objects map[string]bool) error {
	if len(objects) == 0 || tree == nil || tree.Root == nil {
		return nil
	}

	var found string

	walkTemplateFields(tree.Root, func(field *parse.FieldNode) {
		// `.vars.x` is exactly two identifiers; `.vars.x.field` is three or
		// more and is the form this feature exists to serve.
		if found == "" && len(field.Ident) == 2 && field.Ident[0] == "vars" && objects[field.Ident[1]] {
			found = field.Ident[1]
		}
	})

	if found != "" {
		return fmt.Errorf("across var %q holds an object, so {{ .vars.%s }} has no single rendering; name a field, as in {{ .vars.%s.id }}", found, found, found)
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
// Skipping them would leave the one hole this check exists to close: a bare
// object interpolated inside an {{ if }} body still renders Go's map syntax
// into a command.
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
