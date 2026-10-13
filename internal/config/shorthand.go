package config

// in_parallel:'s list shorthand, expanded before the strict decode.

import (
	"bytes"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// expandInParallelShorthand rewrites a step's `in_parallel: [steps...]` — the
// form Concourse accepts beside {steps, limit, fail_fast} — into the mapping
// with only steps: set, returning the source to decode and a function that
// maps a decode error's line numbers back onto the source as written.
//
// It is a rewrite of the TEXT rather than an UnmarshalYAML on InParallel
// because KnownFields does not reach through a custom unmarshaler (see
// stampLines): every step nested inside one would lose its typo detection.
// And it inserts a `steps:` line rather than re-encoding the document, so a
// decode error still names the line the author wrote.
//
// Only step positions are walked — a plan, a hook, a block's branches — so an
// in_parallel: key inside free-form params: or source: is the author's data.
func expandInParallelShorthand(source []byte) ([]byte, func(error) error) {
	unchanged := func(err error) error { return err }

	var doc yaml.Node

	err := yaml.Unmarshal(source, &doc)
	if err != nil || len(doc.Content) == 0 {
		return source, unchanged
	}

	var found shorthands

	if jobs := mappingValue(doc.Content[0], "jobs"); jobs != nil && jobs.Kind == yaml.SequenceNode {
		for _, job := range jobs.Content {
			found.walkStep(job)
		}
	}

	if len(found.sites) == 0 {
		return source, unchanged
	}

	if found.reencode {
		return found.reencoded(&doc, source)
	}

	return found.inserted(source)
}

// shorthandSite is one in_parallel: whose value is a list.
type shorthandSite struct {
	key, list *yaml.Node
}

type shorthands struct {
	sites []shorthandSite
	// reencode is set by a list the text rewrite cannot reach: a flow list,
	// or one at its key's own indentation, where a `steps:` inserted above it
	// would be read as the key's sibling.
	reencode bool
}

func (s *shorthands) walkStep(step *yaml.Node) {
	if step == nil || step.Kind != yaml.MappingNode {
		return
	}

	for i := 0; i+1 < len(step.Content); i += 2 {
		s.walkKey(step.Content[i], step.Content[i+1])
	}
}

// walkKey follows one key of a step (or a job) to the steps under it. A kind's
// key is followed by its nesting in stepKinds.
func (s *shorthands) walkKey(key, value *yaml.Node) {
	switch key.Value {
	case "plan":
		s.walkSteps(value)

		return
	case "on_success", "on_failure", "on_error", "on_abort", "ensure":
		s.walkStep(value)

		return
	case string(StepKindInParallel):
		s.walkInParallel(key, value)

		return
	}

	spec, ok := stepKindByKey(key.Value)
	if !ok {
		return
	}

	switch spec.nest {
	case wraps:
		s.walkStep(value)
	case sequence:
		s.walkSteps(value)
	case branches:
		s.walkSteps(mappingValue(value, spec.branchesKey))
	case leaf:
	}
}

func (s *shorthands) walkInParallel(key, value *yaml.Node) {
	if value.Kind != yaml.SequenceNode {
		s.walkSteps(mappingValue(value, "steps"))

		return
	}

	s.sites = append(s.sites, shorthandSite{key: key, list: value})

	flow := value.Style&yaml.FlowStyle != 0
	s.reencode = s.reencode || flow || len(value.Content) == 0 || value.Column <= key.Column

	s.walkSteps(value)
}

func (s *shorthands) walkSteps(steps *yaml.Node) {
	if steps == nil || steps.Kind != yaml.SequenceNode {
		return
	}

	for _, step := range steps.Content {
		s.walkStep(step)
	}
}

// inserted puts a `steps:` line, at the list's own indentation, above each
// list's first item: a block list may sit at its key's indentation, so the
// list becomes steps:' value without moving.
func (s *shorthands) inserted(source []byte) ([]byte, func(error) error) {
	lines := bytes.SplitAfter(source, []byte("\n"))

	// Ascending, so each insertion's position in the new text is its source
	// line plus the insertions above it.
	slices.SortFunc(s.sites, func(a, b shorthandSite) int { return a.list.Line - b.list.Line })

	insertedAt := make([]int, 0, len(s.sites))

	var out bytes.Buffer

	next := 0

	for i, site := range s.sites {
		before := site.list.Line - 1
		for ; next < before && next < len(lines); next++ {
			out.Write(lines[next])
		}

		out.WriteString(strings.Repeat(" ", site.list.Column-1) + "steps:\n")
		insertedAt = append(insertedAt, site.list.Line+i)
	}

	for ; next < len(lines); next++ {
		out.Write(lines[next])
	}

	remap := func(line int) int {
		shift := 0

		for _, at := range insertedAt {
			if at < line {
				shift++
			}
		}

		return line - shift
	}

	return out.Bytes(), func(err error) error { return remapLines(err, remap) }
}

// reencoded rewrites each list into {steps: list} on the node tree and encodes
// it again, for the shapes the text rewrite cannot reach.
//
// ponytail: a decode error in such a pipeline names a line of the re-encoded
// document, not the file. Reaching those shapes in the text as well is the
// upgrade if it is ever seen to matter.
func (s *shorthands) reencoded(doc *yaml.Node, source []byte) ([]byte, func(error) error) {
	for _, site := range s.sites {
		list := *site.list
		*site.list = yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "steps"},
			&list,
		}}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return source, func(err error) error { return err }
	}

	return out, func(err error) error { return err }
}

var yamlLine = regexp.MustCompile(`line (\d+)`)

// remappedError is a decode error whose line numbers name the source as written.
type remappedError struct {
	text string
	err  error
}

func (e *remappedError) Error() string { return e.text }
func (e *remappedError) Unwrap() error { return e.err }

func remapLines(err error, remap func(int) int) error {
	if err == nil {
		return nil
	}

	text := yamlLine.ReplaceAllStringFunc(err.Error(), func(match string) string {
		line, convErr := strconv.Atoi(strings.TrimPrefix(match, "line "))
		if convErr != nil {
			return match
		}

		return "line " + strconv.Itoa(remap(line))
	})

	if text == err.Error() {
		return err
	}

	return &remappedError{text: text, err: err}
}
