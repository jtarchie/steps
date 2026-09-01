package config

// What a matrix must satisfy at load, including the template checks a
// from_file: matrix would otherwise only fail mid-run.

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"text/template"
	"text/template/parse"
)

// validateAcross checks every across: step in the pipeline at load, so a
// malformed matrix costs a load rather than a run.
//
// A matrix with a from_file: axis is checked but not expanded: its width is
// not known until the step that writes the file has run. The axis rules still
// apply, so the shape mistakes a load can catch are still caught at load.
func (c *Config) validateAcross() error {
	for _, job := range c.Jobs {
		// Hooks first: runHookStep dispatches task/put/agent/try and never
		// expands a matrix, so an across: there would load clean, run ONCE
		// with its {{ .vars.* }} text unrendered, and stay green — the silent
		// no-op shape rejectVolatileOnHook and friends exist to prevent.
		err := job.visitHookSteps(rejectAcrossOnHook)
		if err != nil {
			return err
		}

		err = job.visitSteps(c.validateAcrossStep)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateAcrossStep is the per-step half of validateAcross.
func (c *Config) validateAcrossStep(label string, step *Step) error {
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
}

// rejectAcrossOnHook refuses a matrix on a hook step, worded for the spelling
// the author wrote: a desugared parallelism: still carries its marker, and
// speaking "across:" back at it would name a key that is not in the file.
func rejectAcrossOnHook(label string, step *Step) error {
	if len(step.Across) == 0 {
		return nil
	}

	if step.Sharded() {
		return fmt.Errorf("%s: parallelism: is not valid on hook steps; a hook runs a single step", label)
	}

	return fmt.Errorf("%s: across: is not valid on hook steps; a hook runs a single step", label)
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
