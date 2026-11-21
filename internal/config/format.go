package config

// The standard layout `steps fmt` writes (docs/fmt.md): a yaml.v4 node round trip, keys reordered by what the config structs say a mapping is, blank lines put back by structure, and a guard that refuses any rewrite that changed what the file means.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// ErrNotPipeline is Format's refusal of YAML that is not a pipeline (a vars file, a task file), which a directory walk skips and names.
var ErrNotPipeline = errors.New("not a pipeline")

// Format rewrites a pipeline's raw source (before ((var)) substitution) into the standard layout, reading includes through fsys for the guard. It returns an error, and nothing to write, when the result would mean something else or has lost a comment.
func Format(source []byte, fsys Files) ([]byte, error) {
	var doc, second yaml.Node

	dec := yaml.NewDecoder(bytes.NewReader(source))

	err := dec.Decode(&doc)
	if errors.Is(err, io.EOF) {
		return nil, ErrNotPipeline
	}

	if err != nil {
		return nil, fmt.Errorf("could not parse YAML: %w", err)
	}

	// A pipeline is one document; formatting the first of several would drop the rest.
	if !errors.Is(dec.Decode(&second), io.EOF) || !isPipeline(&doc) {
		return nil, ErrNotPipeline
	}

	lines := strings.Split(string(source), "\n")

	// Hoisted first, so an anchored item's comment counts as its first line when looking for a blank above it.
	hoistAnchoredComments(&doc)
	hoistDanglingComments(&doc, lines)
	blanks := map[*yaml.Node]bool{}
	markAuthorBlanks(doc.Content[0], lines, blanks)

	reorder(doc.Content[0], reflect.TypeFor[Config]())
	unmergeTags(&doc)

	encoded, err := dump(&doc, source)
	if err != nil {
		return nil, err
	}

	// The encoded text, re-read, has the reordered tree's shape: walking the two side by side is how each node is found in the output.
	var out yaml.Node

	err = yaml.Unmarshal(encoded, &out)
	if err != nil {
		return nil, fmt.Errorf("could not re-read formatted YAML: %w", err)
	}

	outLines := strings.Split(string(encoded), "\n")

	hoistAnchoredComments(&out)
	hoistDanglingComments(&out, outLines)
	restoreScalars(doc.Content[0], out.Content[0], lines, outLines)
	restoreCommentColumns(doc.Content[0], out.Content[0], lines, outLines)

	formatted := insertBlanks(doc.Content[0], out.Content[0], outLines, blanks)

	err = sameMeaning(source, formatted, fsys)
	if err != nil {
		return nil, err
	}

	return formatted, nil
}

func dump(doc *yaml.Node, source []byte) ([]byte, error) {
	unmask, err := maskSupplementary(doc, source)
	if err != nil {
		return nil, err
	}

	var encoded bytes.Buffer

	enc, err := yaml.NewDumper(&encoded, yaml.WithV3Defaults(), yaml.WithIndent(2), yaml.WithLineWidth(-1))
	if err != nil {
		return nil, fmt.Errorf("could not configure YAML encoder: %w", err)
	}

	err = enc.Dump(doc)
	if err == nil {
		err = enc.Close()
	}

	if err != nil {
		return nil, fmt.Errorf("could not encode YAML: %w", err)
	}

	mapText(doc, unmask.Replace)

	return []byte(unmask.Replace(encoded.String())), nil
}

// maskSupplementary swaps every rune above U+FFFF in the tree's scalars and comments for a private-use rune the source does not contain, and returns the replacer that swaps them back, in the dump and the tree. yaml.v4's emitter calls anything above U+FFFF unprintable (yaml/go-yaml#354) and rewrites a `run: |` holding an emoji as one double-quoted string, breaking restoreScalars' premise that formatting never rewrites a scalar; its plugins hook only loading, so there is no emitter option to set instead (yaml/go-yaml#521).
func maskSupplementary(doc *yaml.Node, source []byte) (*strings.Replacer, error) {
	used := map[rune]bool{}
	for _, r := range string(source) {
		used[r] = true
	}

	standIn := map[rune]rune{}
	next := rune(0xE000)

	var failed bool

	mask := func(text string) string {
		return strings.Map(func(r rune) rune {
			if r <= 0xFFFF {
				return r
			}

			if s, ok := standIn[r]; ok {
				return s
			}

			for next <= 0xF8FF && used[next] {
				next++
			}

			if next > 0xF8FF {
				failed = true
				return r
			}

			standIn[r] = next
			next++

			return standIn[r]
		}, text)
	}

	mapText(doc, mask)

	if failed {
		return nil, errors.New("refusing to format: too many distinct characters above U+FFFF")
	}

	pairs := make([]string, 0, 2*len(standIn))
	for original, s := range standIn {
		pairs = append(pairs, string(s), string(original))
	}

	return strings.NewReplacer(pairs...), nil
}

func mapText(node *yaml.Node, fn func(string) string) {
	node.Value = fn(node.Value)
	node.HeadComment = fn(node.HeadComment)
	node.LineComment = fn(node.LineComment)
	node.FootComment = fn(node.FootComment)

	for _, child := range node.Content {
		mapText(child, fn)
	}
}

// isPipeline is a top-level mapping whose keys are all Config's and include jobs: — a vars file or a task file fails one of the two.
func isPipeline(doc *yaml.Node) bool {
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false
	}

	known := yamlKeys(&Config{})
	root := doc.Content[0]
	jobs := false

	for i := 0; i+1 < len(root.Content); i += 2 {
		key := root.Content[i].Value
		if !slices.Contains(known, key) {
			return false
		}

		jobs = jobs || key == "jobs"
	}

	return jobs
}

// The places a key can sit in a config mapping, in the order they are written; keys of one rank keep the author's order.
const (
	rankVerb = iota
	rankName
	rankType
	rankSetting
	rankBody
	rankAssert
	rankHook
)

// keyRank is where key sits in a mapping of structType: what it is, its settings, its body, what it asserts about the outcome, and its hooks last.
func keyRank(key string, structType reflect.Type) int {
	switch {
	case structType == reflect.TypeFor[Step]() && slices.ContainsFunc(stepKinds[:], func(spec stepKindSpec) bool { return string(spec.kind) == key }):
		return rankVerb
	case key == "name":
		return rankName
	case key == "type":
		return rankType
	case slices.Contains(bodyKeys, key):
		return rankBody
	case key == "assert":
		return rankAssert
	case slices.Contains(yamlKeys(&Hooks{}), key):
		return rankHook
	default:
		return rankSetting
	}
}

// bodyKeys are what a block DOES, so they read after everything that configures it: a job's plan, an agent's system prompt, a task's script, an agent step's messages.
var bodyKeys = []string{"plan", "system", "run", "messages"}

// reorder sorts each mapping the config structs describe, stably by keyRank, and recurses with the type each key decodes into. A map type (source:, params:, env:) is the author's order, and a node whose shape the type does not describe is left alone.
func reorder(node *yaml.Node, typ reflect.Type) {
	typ = deref(typ)

	if node.Kind == yaml.MappingNode && typ.Kind() == reflect.Struct {
		reorderStruct(node, typ)

		return
	}

	list := node.Kind == yaml.SequenceNode && (typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array)
	if list || (node.Kind == yaml.MappingNode && typ.Kind() == reflect.Map) {
		reorderElems(node, typ.Elem(), list)
	}
}

// reorderElems recurses into a list's items, or a map's values.
func reorderElems(node *yaml.Node, elem reflect.Type, list bool) {
	for i, child := range node.Content {
		if list || i%2 == 1 {
			reorder(child, elem)
		}
	}
}

func deref(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}

	return typ
}

type pair struct{ key, value *yaml.Node }

// trailing is a key whose line ends in a comment.
func trailing(p pair) bool {
	return p.key.LineComment != "" || p.value.LineComment != ""
}

func reorderStruct(node *yaml.Node, typ reflect.Type) {
	pairs := make([]pair, 0, len(node.Content)/2)

	for i := 0; i+1 < len(node.Content); i += 2 {
		pairs = append(pairs, pair{node.Content[i], node.Content[i+1]})
	}

	sorted := slices.Clone(pairs)
	slices.SortStableFunc(sorted, func(a, b pair) int {
		return keyRank(a.key.Value, typ) - keyRank(b.key.Value, typ)
	})

	if separatesComments(pairs, sorted) {
		sorted = pairs
	}

	node.Content = node.Content[:0]

	for _, p := range sorted {
		node.Content = append(node.Content, p.key, p.value)

		if field, ok := fieldByTag(typ, p.key.Value); ok {
			reorder(p.value, field)
		}
	}
}

// separatesComments is whether sorting would part a comment from its keys, in which case the mapping keeps the author's order.
func separatesComments(pairs, sorted []pair) bool {
	if footLeaves(pairs, sorted) {
		return true
	}

	// Trailing comments on consecutive lines are often one sentence across several keys, which a sort must not split or reverse.
	for i := 0; i+1 < len(pairs); i++ {
		if !trailing(pairs[i]) || !trailing(pairs[i+1]) || pairs[i].key.Line+1 != pairs[i+1].key.Line {
			continue
		}

		at := slices.Index(sorted, pairs[i])
		if at+1 >= len(sorted) || sorted[at+1] != pairs[i+1] {
			return true
		}
	}

	return false
}

// footLeaves is whether a comment written under the last key, which yaml.v4 holds as that key's foot comment, would leave the end of the mapping with it.
func footLeaves(pairs, sorted []pair) bool {
	if len(pairs) == 0 {
		return false
	}

	last := pairs[len(pairs)-1]

	return (last.key.FootComment != "" || last.value.FootComment != "") && sorted[len(sorted)-1] != last
}

// fieldByTag is the type the yaml key decodes into, looking through ",inline" fields.
func fieldByTag(typ reflect.Type, key string) (reflect.Type, bool) {
	for i := range typ.NumField() {
		field := typ.Field(i)
		name, opts, _ := strings.Cut(field.Tag.Get("yaml"), ",")

		if name == "" && strings.Contains(opts, "inline") {
			if found, ok := fieldByTag(deref(field.Type), key); ok {
				return found, true
			}

			continue
		}

		if name == key {
			return field.Type, true
		}
	}

	return nil, false
}

// unmergeTags drops the !!merge tag yaml.v4 resolves `<<` to, which its encoder otherwise prints as `!!merge <<:`.
func unmergeTags(node *yaml.Node) {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!merge" {
		node.Tag = ""
	}

	for _, child := range node.Content {
		unmergeTags(child)
	}
}

// hoistAnchoredComments moves a comment written above `- &name` back onto the mapping: yaml.v4 parses it onto the first key, where the encoder prints it inside the item. One written below the anchor stays where it was.
func hoistAnchoredComments(node *yaml.Node) {
	if node.Kind == yaml.MappingNode && node.Anchor != "" && len(node.Content) > 0 && node.HeadComment == "" && aboveAnchor(node) {
		node.HeadComment, node.Content[0].HeadComment = node.Content[0].HeadComment, ""
	}

	for _, child := range node.Content {
		hoistAnchoredComments(child)
	}
}

// hoistDanglingComments moves a comment written between two items, blank line below it, from the second item's first key back to the head of that item. yaml.v4 parses such a comment as the first key's foot comment, so the encoder printed it under `- name: ...` inside the item, and fmt was not idempotent: work/prs.yml's lone `# /bot's resources.` moved one item down per pass. A head comment the item already has stays below it, a blank line apart, as written.
func hoistDanglingComments(node *yaml.Node, lines []string) {
	if node.Kind == yaml.MappingNode && len(node.Content) > 0 && writtenAbove(node, node.Content[0].FootComment, lines) {
		key := node.Content[0]

		if node.HeadComment == "" {
			node.HeadComment = key.FootComment
		} else {
			node.HeadComment = key.FootComment + "\n\n" + node.HeadComment
		}

		key.FootComment = ""
	}

	for _, child := range node.Content {
		hoistDanglingComments(child, lines)
	}
}

// writtenAbove is whether comment's first line is in lines above node's own line, with only comments and blank lines between: a foot comment is otherwise below what it belongs to.
func writtenAbove(node *yaml.Node, comment string, lines []string) bool {
	if comment == "" {
		return false
	}

	first, _, _ := strings.Cut(comment, "\n")
	first = strings.TrimSpace(first)

	for i := min(node.Line-2, len(lines)-1); i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == first {
			return true
		}

		if line != "" && !strings.HasPrefix(line, "#") {
			return false
		}
	}

	return false
}

// aboveAnchor is whether the first key's head comment does not fit between the anchor's line and the key, so was written above the anchor.
func aboveAnchor(node *yaml.Node) bool {
	key := node.Content[0]

	return key.HeadComment != "" && key.Line-strings.Count(key.HeadComment, "\n")-1 <= node.Line
}

// markAuthorBlanks records each key and list item the author set off with a blank line above it (above its comment, when it has one).
func markAuthorBlanks(node *yaml.Node, lines []string, blanks map[*yaml.Node]bool) {
	entries := node.Content

	if node.Kind == yaml.MappingNode {
		entries = nil

		for i := 0; i < len(node.Content); i += 2 {
			entries = append(entries, node.Content[i])
		}
	}

	for _, entry := range entries[min(1, len(entries)):] {
		above := startLine(entry) - 2 //nolint:mnd // the 1-based line above, as a 0-based index
		if above >= 0 && above < len(lines) && strings.TrimSpace(lines[above]) == "" {
			blanks[entry] = true
		}
	}

	for _, child := range node.Content {
		markAuthorBlanks(child, lines, blanks)
	}
}

// startLine is the first line an entry occupies: its head comment's, or its own.
func startLine(node *yaml.Node) int {
	if node.HeadComment == "" {
		return node.Line
	}

	return node.Line - strings.Count(node.HeadComment, "\n") - 1
}

// alongside calls fn on each node of the tree and its counterpart in the re-read output, which has the same shape: it is how a node is found in the formatted text.
func alongside(node, out *yaml.Node, fn func(node, out *yaml.Node)) {
	if node.Kind != out.Kind || len(node.Content) != len(out.Content) {
		return
	}

	fn(node, out)

	for i := range node.Content {
		alongside(node.Content[i], out.Content[i], fn)
	}
}

// insertBlanks puts back the blank lines the encoder never writes: before every top-level key but the first, around each multi-line entry of a top-level list, and wherever the author left one.
func insertBlanks(root, outRoot *yaml.Node, lines []string, blanks map[*yaml.Node]bool) []byte {
	before := map[int]bool{}

	for i := 2; i < len(root.Content); i += 2 {
		before[startLine(outRoot.Content[i])] = true
	}

	for i := 1; i < len(root.Content); i += 2 {
		list := root.Content[i]

		for j := 1; j < len(list.Content) && list.Kind == yaml.SequenceNode; j++ {
			if multiline(list.Content[j]) || multiline(list.Content[j-1]) {
				blanks[list.Content[j]] = true
			}
		}
	}

	alongside(root, outRoot, func(node, out *yaml.Node) {
		if blanks[node] {
			before[startLine(out)] = true
		}
	})

	return joinWithBlanks(lines, before)
}

// joinWithBlanks is lines with a blank line before each 1-based line number in before.
func joinWithBlanks(lines []string, before map[int]bool) []byte {
	var b strings.Builder

	for i, line := range lines[:len(lines)-1] {
		// The encoder writes one blank line of its own, after a document's head comment; a second would be a run.
		if before[i+1] && i > 0 && strings.TrimSpace(lines[i-1]) != "" {
			b.WriteString("\n")
		}

		b.WriteString(line + "\n")
	}

	return []byte(b.String())
}

// multiline is a block collection the encoder spreads over more than one line.
func multiline(node *yaml.Node) bool {
	if node.Style&yaml.FlowStyle != 0 || (node.Kind != yaml.MappingNode && node.Kind != yaml.SequenceNode) {
		return false
	}

	if len(node.Content) > 2 { //nolint:mnd // one key and its value
		return true
	}

	return slices.ContainsFunc(node.Content, func(child *yaml.Node) bool {
		return multiline(child) || child.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0
	})
}

type scalarEdit struct {
	column   int
	was, now string
}

// restoreScalars writes each one-line scalar back exactly as the author spelled it: the encoder quotes `factory:planning` in a flow list and re-renders escapes, and the text of a value is the author's.
func restoreScalars(node, out *yaml.Node, source, lines []string) {
	edits := map[int][]scalarEdit{}

	alongside(node, out, func(node, out *yaml.Node) {
		if node.Kind != yaml.ScalarNode || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
			return
		}

		was, wasOK := scalarToken(source, node)
		now, nowOK := scalarToken(lines, out)

		if wasOK && nowOK && was != now {
			edits[out.Line] = append(edits[out.Line], scalarEdit{out.Column, now, was})
		}
	})

	for line, list := range edits {
		// Right to left, so an edit does not move the columns of the ones still to apply.
		slices.SortFunc(list, func(a, b scalarEdit) int { return b.column - a.column })

		text := []rune(lines[line-1])
		for _, e := range list {
			start := e.column - 1
			text = slices.Concat(text[:start], []rune(e.now), text[start+len([]rune(e.was)):])
		}

		lines[line-1] = string(text)
	}
}

// scalarToken is a one-line scalar's text as it appears at its position, quotes included; false for a block or multi-line scalar.
func scalarToken(lines []string, node *yaml.Node) (string, bool) {
	if node.Line < 1 || node.Line > len(lines) || node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return "", false
	}

	rest := []rune(lines[node.Line-1])
	if node.Column < 1 || node.Column > len(rest) {
		return "", false
	}

	rest = rest[node.Column-1:]

	if rest[0] == '\'' || rest[0] == '"' {
		return quotedToken(rest)
	}

	if strings.Contains(node.Value, "\n") || !strings.HasPrefix(string(rest), node.Value) {
		return "", false
	}

	return node.Value, true
}

// quotedToken is the quoted scalar rest starts with, through its closing quote.
func quotedToken(rest []rune) (string, bool) {
	quote := rest[0]

	for i := 1; i < len(rest); i++ {
		escaped := (quote == '"' && rest[i] == '\\') || (quote == '\'' && rest[i] == '\'' && i+1 < len(rest) && rest[i+1] == '\'')

		switch {
		case escaped:
			i++
		case rest[i] == quote:
			return string(rest[:i+1]), true
		}
	}

	return "", false
}

// restoreCommentColumns puts each trailing comment back in the author's column (or, where the deeper indentation leaves no room, after the author's gap), so a column of aligned comments stays aligned, and puts a comment-only line that continued a trailing comment back under it.
func restoreCommentColumns(node, out *yaml.Node, source, lines []string) {
	srcScalar, outScalar := scalarBodies(node), scalarBodies(out)

	alongside(node, out, func(node, out *yaml.Node) {
		if node.LineComment == "" || out.LineComment != node.LineComment {
			return
		}

		_, authored, gap, wasOK := trailingAt(source[node.Line-1], node.LineComment)
		head, _, _, nowOK := trailingAt(lines[out.Line-1], node.LineComment)

		if !wasOK || !nowOK {
			return
		}

		end := utf8.RuneCountInString(head)

		column := authored
		if column <= end {
			column = end + gap
		}

		lines[out.Line-1] = head + strings.Repeat(" ", column-end) + node.LineComment

		for next := 1; !srcScalar[node.Line+next] && !outScalar[out.Line+next]; next++ {
			text, ok := continuation(source, node.Line+next, authored, lines, out.Line+next)
			if !ok {
				break
			}

			lines[out.Line+next-1] = strings.Repeat(" ", column) + text
		}
	})
}

// continuation is the comment on source line was (1-based) when it sits alone in the column of the trailing comment above it and output line now holds the same comment.
func continuation(source []string, was, column int, lines []string, now int) (string, bool) {
	if was > len(source) || now > len(lines) {
		return "", false
	}

	src, out := source[was-1], lines[now-1]
	text := strings.TrimSpace(src)
	indent := utf8.RuneCountInString(src) - utf8.RuneCountInString(strings.TrimLeft(src, " "))

	return text, strings.HasPrefix(text, "#") && indent == column && strings.TrimSpace(out) == text
}

// trailingAt splits a line at its trailing comment: what is before it, the comment's column in runes, and the spaces between.
func trailingAt(line, comment string) (string, int, int, bool) {
	at := strings.LastIndex(line, comment)
	if at < 0 {
		return "", 0, 0, false
	}

	head := strings.TrimRight(line[:at], " \t")

	return head, utf8.RuneCountInString(line[:at]), at - len(head), true
}

// scalarBodies is the body lines of every block scalar under root, where a "#" is text.
func scalarBodies(root *yaml.Node) map[int]bool {
	lines := map[int]bool{}

	var walk func(node *yaml.Node)
	walk = func(node *yaml.Node) {
		if node.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
			for line := range strings.Count(strings.TrimRight(node.Value, "\n"), "\n") + 1 {
				lines[node.Line+1+line] = true
			}
		}

		for _, child := range node.Content {
			walk(child)
		}
	}
	walk(root)

	return lines
}

// sameMeaning refuses a rewrite whose data, loaded pipeline or comments differ from the original's. The data check holds even when the file does not load (an unsupplied ((var)), a pipeline under construction), and the load check catches what only the loader reads from layout.
func sameMeaning(before, after []byte, fsys Files) error {
	var was, now any

	err := yaml.Unmarshal(before, &was)
	if err != nil {
		return fmt.Errorf("refusing to format: %w", err)
	}

	err = yaml.Unmarshal(after, &now)
	if err != nil {
		return fmt.Errorf("refusing to format: the result would not decode: %w", err)
	}

	if !reflect.DeepEqual(was, now) {
		return errors.New("refusing to format: the result would decode to different values")
	}

	if missing := missingComments(before, after); len(missing) > 0 {
		return fmt.Errorf("refusing to format: it would lose the comment %q", missing[0])
	}

	wasCfg, wasErr := parseQuietly(before, "fmt", "fmt", fsys, false)
	nowCfg, nowErr := parseQuietly(after, "fmt", "fmt", fsys, false)

	if (wasErr == nil) != (nowErr == nil) {
		return fmt.Errorf("refusing to format: the result would load differently (before: %w, after: %w)", wasErr, nowErr)
	}

	if wasErr == nil && nowErr == nil && !reflect.DeepEqual(withoutPositions(wasCfg), withoutPositions(nowCfg)) {
		return errors.New("refusing to format: the result would load as a different pipeline")
	}

	return nil
}

// withoutPositions drops what a load records about WHERE things were in the file, which formatting moves by design.
func withoutPositions(cfg *Config) *Config {
	cfg.Revision = Revision{}

	for i := range cfg.Jobs {
		cfg.Jobs[i].Line = 0

		for j := range cfg.Jobs[i].Plan {
			cfg.Jobs[i].Plan[j].Line = 0
		}
	}

	return cfg
}

// missingComments is each comment line of before that after does not have as many times.
func missingComments(before, after []byte) []string {
	count := map[string]int{}

	for _, line := range commentLines(after) {
		count[line]++
	}

	var missing []string

	for _, line := range commentLines(before) {
		if count[line] == 0 {
			missing = append(missing, line)

			continue
		}

		count[line]--
	}

	return missing
}

// commentLines is every "#..." tail in source, read as text rather than through yaml.v4, which drops a comment in some positions on parse and would then miss the same one dropped on encode. Text inside scalars counts too, harmlessly: formatting never rewrites a scalar, so it is on both sides.
func commentLines(source []byte) []string {
	var tails []string

	for _, line := range strings.Split(string(source), "\n") {
		for i := range len(line) {
			if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				tails = append(tails, strings.TrimSpace(line[i:]))

				break
			}
		}
	}

	return tails
}
