package config

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// SourceBlock is where one named entry of a top-level list — or one step of a job — sits in a pipeline's source text, by 1-based inclusive line.
type SourceBlock struct {
	// Section is the top-level key the entry is listed under: "jobs", "resources".
	Section string
	Name    string
	// Path is empty for the entry itself and a step's position inside its job otherwise, by the keys that reach it: "plan.2", "plan.2.do.0", "plan.1.on_failure", "ensure".
	Path string
	// StepKind and StepName are a step's kind key and its scalar value (task: compile), which is how a run's rows name it.
	StepKind string
	StepName string
	Start    int
	End      int
}

// RefKind says what a SourceRef's text points at.
type RefKind int

const (
	// RefEntry is a name used where the file declares it elsewhere: get: weekly, passed: [build].
	RefEntry RefKind = iota
	// RefSelf is the name: of an entry that has a page of its own.
	RefSelf
	// RefInclude is the path of a file whose contents are folded into the revision: run_file:, system_file:.
	RefInclude
)

// SourceRef is one name in the source that refers to something: the bytes [Start, End) of 1-based Line.
type SourceRef struct {
	Line  int
	Start int
	End   int
	Kind  RefKind
	// Section and Name are the entry referred to; for a RefInclude, Name is the path and Section is empty.
	Section string
	Name    string
}

// SourceMap is every landing place and every reference in one pipeline source.
type SourceMap struct {
	Blocks []SourceBlock
	Refs   []SourceRef
}

// stepChildLists are the step keys holding a list of steps, with the key the list sits under when the value is a mapping instead.
var stepChildLists = map[string]string{"plan": "", "do": "", "in_parallel": "steps", "race": "steps", "ensemble": "agents"}

// stepChildren are the step and job keys holding one step.
var stepChildren = map[string]bool{"try": true, "on_success": true, "on_failure": true, "on_error": true, "on_abort": true, "ensure": true, "on_pending": true}

// refTargets is the section a key's value names an entry of. type: is a resource's alone, and get:/put:/resource: a job's, which mapRefs checks.
var refTargets = map[string]string{"get": "resources", "put": "resources", "resource": "resources", "agent": "agents", "task": "tasks", "passed": "jobs", "mcp": "mcp_servers", "type": "resource_types"}

var includeKeys = map[string]bool{"run_file": true, "system_file": true, "message_files": true}

var selfSections = map[string]bool{"jobs": true, "resources": true, "mcp_exports": true}

// MapSource locates every named entry of every top-level list in source, every step of every job, and every name that refers to one of them, in file order. A source that will not parse has none. Neither does an entry sharing its first line with another (a flow list): a block is whole lines.
func MapSource(source string) SourceMap {
	var doc yaml.Node

	err := yaml.Unmarshal([]byte(source), &doc)
	if err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return SourceMap{}
	}

	root := doc.Content[0].Content
	mapper := sourceMapper{lines: strings.Split(source, "\n"), declared: map[string]map[string]bool{}}

	for i := 0; i+1 < len(root); i += 2 {
		names := map[string]bool{}
		for _, item := range root[i+1].Content {
			names[entryName(item)] = true
		}

		mapper.declared[root[i].Value] = names
	}

	for i := 0; i+1 < len(root); i += 2 {
		key, list := root[i], root[i+1]
		if list.Kind != yaml.SequenceNode {
			continue
		}

		end := len(mapper.lines)
		if i+2 < len(root) {
			end = root[i+2].Line - 1
		}

		mapper.section(key, list, end)
	}

	return SourceMap{Blocks: mapper.blocks, Refs: mapper.refs}
}

type sourceMapper struct {
	lines    []string
	declared map[string]map[string]bool
	blocks   []SourceBlock
	refs     []SourceRef
}

func (m *sourceMapper) section(key, list *yaml.Node, sectionEnd int) {
	last := key.Line

	for j, item := range list.Content {
		name := entryName(item)
		if name == "" {
			continue
		}

		m.entryRefs(item, key.Value)

		if item.Line <= last {
			continue
		}

		last = item.Line

		end := sectionEnd
		if j+1 < len(list.Content) {
			end = list.Content[j+1].Line - 1
		}

		end = trimTrailing(m.lines, item.Line, end, list.Column-1)
		m.blocks = append(m.blocks, SourceBlock{Section: key.Value, Name: name, Start: item.Line, End: end})

		if key.Value == "jobs" {
			m.stepsUnder(item, name, "", end)
		}
	}
}

// stepsUnder records the steps a job or a step holds, each bounded by the key after it or by its holder's own end.
func (m *sourceMapper) stepsUnder(holder *yaml.Node, job, prefix string, holderEnd int) {
	pairs := holder.Content

	for i := 0; i+1 < len(pairs); i += 2 {
		key, value := pairs[i], pairs[i+1]

		end := holderEnd
		if i+2 < len(pairs) {
			end = min(end, pairs[i+2].Line-1)
		}

		path := strings.TrimPrefix(prefix+"."+key.Value, ".")

		if under, ok := stepChildLists[key.Value]; ok {
			m.stepList(key, value, under, job, path, end)
		}

		if stepChildren[key.Value] && value.Kind == yaml.MappingNode {
			m.step(value, job, path, key.Line, trimTrailing(m.lines, key.Line, end, key.Column-1))
		}
	}
}

func (m *sourceMapper) stepList(key, list *yaml.Node, under, job, path string, end int) {
	if list.Kind == yaml.MappingNode && under != "" {
		key, list, end = listUnder(list, under, end)
	}

	if key == nil || list == nil || list.Kind != yaml.SequenceNode {
		return
	}

	last := key.Line

	for j, item := range list.Content {
		if item.Kind != yaml.MappingNode || item.Line <= last {
			continue
		}

		last = item.Line

		itemEnd := end
		if j+1 < len(list.Content) {
			itemEnd = list.Content[j+1].Line - 1
		}

		m.step(item, job, path+"."+strconv.Itoa(j), item.Line, trimTrailing(m.lines, item.Line, itemEnd, list.Column-1))
	}
}

// listUnder is the key and value of holder's under: pair — the long form of a step list, in_parallel: {steps: [...]} — and where that pair ends.
func listUnder(holder *yaml.Node, under string, end int) (key, list *yaml.Node, listEnd int) {
	pairs := holder.Content

	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i].Value != under {
			continue
		}

		if i+2 < len(pairs) {
			end = min(end, pairs[i+2].Line-1)
		}

		return pairs[i], pairs[i+1], end
	}

	return nil, nil, end
}

func (m *sourceMapper) step(node *yaml.Node, job, path string, start, end int) {
	if end < start {
		return
	}

	block := SourceBlock{Section: "jobs", Name: job, Path: path, Start: start, End: end}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if _, isKind := stepKindByKey(node.Content[i].Value); isKind {
			block.StepKind = node.Content[i].Value

			if node.Content[i+1].Kind == yaml.ScalarNode {
				block.StepName = node.Content[i+1].Value
			}

			break
		}
	}

	m.blocks = append(m.blocks, block)
	m.stepsUnder(node, job, path, end)
}

// entryRefs records the references inside one entry of section: its own name where it has a page, and every name under it that the file declares.
func (m *sourceMapper) entryRefs(entry *yaml.Node, section string) {
	for i := 0; i+1 < len(entry.Content); i += 2 {
		key, value := entry.Content[i], entry.Content[i+1]

		switch {
		case key.Value == "name" && selfSections[section]:
			m.ref(value, RefSelf, section)
		case key.Value == "type" && section == "resources":
			m.ref(value, RefEntry, "resource_types")
		}
	}

	m.refsUnder(entry, section)
}

func (m *sourceMapper) refsUnder(node *yaml.Node, section string) {
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			m.refsUnder(item, section)
		}
	}

	if node.Kind != yaml.MappingNode {
		return
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		m.pairRefs(node.Content[i].Value, node.Content[i+1], section)
		m.refsUnder(node.Content[i+1], section)
	}
}

func (m *sourceMapper) pairRefs(key string, value *yaml.Node, section string) {
	target, refers := refTargets[key]
	if target == "resource_types" || (target == "resources" && section != "jobs") {
		refers = false
	}

	for _, scalar := range scalarsOf(value) {
		if refers {
			m.ref(scalar, RefEntry, target)
		}

		if includeKeys[key] {
			m.ref(scalar, RefInclude, "")
		}
	}
}

func scalarsOf(node *yaml.Node) []*yaml.Node {
	if node.Kind == yaml.ScalarNode {
		return []*yaml.Node{node}
	}

	var scalars []*yaml.Node

	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			if item.Kind == yaml.ScalarNode {
				scalars = append(scalars, item)
			}
		}
	}

	return scalars
}

// ref records scalar as a reference when its value is written out on its own line as it reads — plain, or inside one pair of quotes — and, for a RefEntry, when the file declares what it names.
func (m *sourceMapper) ref(scalar *yaml.Node, kind RefKind, section string) {
	name := scalar.Value
	if name == "" || scalar.Line < 1 || scalar.Line > len(m.lines) || (kind == RefEntry && !m.declared[section][name]) {
		return
	}

	line := m.lines[scalar.Line-1]
	start := 0

	for range scalar.Column - 1 {
		_, size := utf8.DecodeRuneInString(line[start:])
		start += size
	}

	if !strings.HasPrefix(line[start:], name) {
		start++
	}

	if start > len(line) || !strings.HasPrefix(line[start:], name) {
		return
	}

	m.refs = append(m.refs, SourceRef{Line: scalar.Line, Start: start, End: start + len(name), Kind: kind, Section: section, Name: name})
}

func entryName(item *yaml.Node) string {
	if item.Kind != yaml.MappingNode {
		return ""
	}

	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "name" && item.Content[i+1].Kind == yaml.ScalarNode {
			return item.Content[i+1].Value
		}
	}

	return ""
}

// trimTrailing pulls end back over blank lines and over comments indented no deeper than the list's dash: those introduce whatever follows, and nothing inside the entry (a block scalar's text included) can sit that far left.
func trimTrailing(lines []string, start, end, dash int) int {
	for end > start && end <= len(lines) {
		text := strings.TrimLeft(lines[end-1], " ")
		indent := len(lines[end-1]) - len(text)

		if text != "" && (!strings.HasPrefix(text, "#") || indent > dash) {
			break
		}

		end--
	}

	return end
}
