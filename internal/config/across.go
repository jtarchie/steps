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

	// A collecting matrix turns axis values into directory names, so they get
	// the checks a name that becomes a path needs — for values: at load, for
	// from_file: here at dispatch, since the items are often model-authored.
	if collectsOutputs(step) {
		err = validateCollectedValues(label, axes)
		if err != nil {
			return nil, err
		}
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
// against, and the same values in axis DECLARATION order — the path a
// collecting matrix captures this cell's outputs under (see Step.OutputSubdir).
// vars alone cannot carry that, since a map has no order.
type acrossCombo struct {
	vars map[string]string
	segs []string
}

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

// staticAxes returns just the values: axes as resolved axes — what a matrix
// with a file axis can still have checked at load.
func staticAxes(vars []AcrossVar) []acrossAxis {
	axes := make([]acrossAxis, 0, len(vars))

	for _, v := range vars {
		if !v.Runtime() {
			axes = append(axes, acrossAxis{name: v.Var, values: v.Values})
		}
	}

	return axes
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

			err = c.validateAcrossOutputs(label, step)
			if err != nil {
				return err
			}

			if HasRuntimeAxis(*step) {
				err = validateAcrossAxes(label, step.Across)
				if err != nil {
					return err
				}

				// A collecting matrix's static axes are fully known here, so
				// they get the directory-name checks at load even when a file
				// axis defers its own to dispatch — a hostile values: entry
				// must not wait for the producer step to run before failing.
				if collectsOutputs(*step) {
					err = validateCollectedValues(label, staticAxes(step.Across))
					if err != nil {
						return err
					}
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

// validateAcrossConcurrency checks max_in_flight: — that it describes a matrix
// at all, and that its value counts something.
//
// Concurrent cells are safe by construction now that every step materializes
// its own directory: a matrix's cells are one step's clones, declaring the
// same outputs: and writing the same paths, and each cell writes its own
// copy.
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
				expanded := acrossCombo{
					vars: make(map[string]string, len(combo.vars)+1),
					segs: make([]string, 0, len(combo.segs)+1),
				}

				for k, v := range combo.vars {
					expanded.vars[k] = v
				}

				expanded.vars[axis.name] = value
				expanded.segs = append(append(expanded.segs, combo.segs...), value)

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

	// The cell's coordinates as a capture path, stamped whether or not the
	// matrix collects: it is pure bookkeeping derived from declared axes, and
	// the consumers (executeTask, prepareAgentStep) apply it only to a cell
	// that declares outputs.
	cell.OutputSubdir = strings.Join(combo.segs, "/")

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
