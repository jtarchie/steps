package web

// JSON is the shape most of this UI's payloads actually arrive in: a tool
// call's arguments, a tool result, a node's content map, an agent's recorded
// result, a task's stdout. Every one of them used to reach the page as the
// stored string, and on the shape that matters most — a read_file result,
// whose whole point is a document escaped inside a "content" string — that
// rendered as a wall of \" nobody reads.
//
// So JSON is PARSED here rather than printed, which buys four things a
// re-indent could not:
//
//   - key order survives. encoding/json's MarshalIndent sorts, because a
//     round trip through map[string]any has no order left to keep — but the
//     order a provider or a tool chose is information, and reading a
//     trajectory whose fields shuffle costs real attention.
//   - a string value that is ITSELF a JSON document renders as that
//     document, unescaped, still inside its own quotes so the nesting stays
//     honest rather than being flattened away.
//   - a multi-line string keeps its real newlines, for exactly the same
//     reason: \n\n between two paragraphs of an agent's answer is the answer
//     being hidden.
//   - every token carries a class, so the stylesheet owns the palette and it
//     agrees with app.css's :root instead of with a stock chroma theme.
//
// Chroma still owns FENCED CODE (docs.go's style, reused by prose blocks
// below) — a different job: that is source text in an arbitrary language,
// this is one known data shape whose nesting the renderer must understand in
// order to unescape it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"strconv"
	"strings"
)

const (
	// jsonInlineWidth is the widest single-line rendering that still reads as
	// part of a transcript row instead of as a block interrupting it. A
	// read_file's {"path": "..."} fits; its result does not, which is the
	// distinction the whole fold exists to make.
	jsonInlineWidth = 96
	// jsonDepthLimit bounds the embedded-document unwrapping. A tool result
	// carrying a tool result is real — that is a sub-agent's trajectory — but
	// past a few levels the escaped string is the more honest answer than an
	// unbounded recursion over model-authored input.
	jsonDepthLimit = 6
	// jsonIndent is one level of block indentation, in spaces.
	jsonIndent = 2
)

// jsonKind is what a parsed node is.
type jsonKind int

const (
	kindObject jsonKind = iota
	kindArray
	kindString
	kindNumber
	// kindLiteral is true, false, or null: the three values whose rendering is
	// their source text and whose color is one shared meaning.
	kindLiteral
)

// jsonNode is one parsed JSON value, in source order.
type jsonNode struct {
	kind jsonKind
	// key is set on an object member, and read only by the parent that knows
	// it is an object — an array element and a member named "" are otherwise
	// indistinguishable here, and neither cares.
	key  string
	text string // a string value, already unescaped by the decoder
	lit  string // a number or literal, verbatim from the source
	kids []jsonNode
	// doc is the document this string value turned out to hold. Non-nil is
	// what makes the read_file case readable.
	doc *jsonNode
}

// parseJSONDocument parses a complete JSON object or array, reporting false
// for anything else.
//
// Only a document is worth reformatting: a bare string, number, or bool IS
// its own best rendering, and treating a tool's one-line "3" answer as JSON
// would dress a number up as a highlighted literal for no gain.
func parseJSONDocument(raw string, depth int) (jsonNode, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		return jsonNode{}, false
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	// UseNumber keeps a number's source text. Without it 499 comes back as a
	// float64 and renders as 499 by luck, while a 19-digit id renders in
	// scientific notation — a receipt that quietly rewrites its own values.
	decoder.UseNumber()

	node, err := parseJSONValue(decoder, depth)
	if err != nil {
		return jsonNode{}, false
	}

	// Trailing content means this was never one document — two concatenated
	// objects, or JSON followed by a log line — and re-rendering it would
	// silently drop everything after the first.
	_, err = decoder.Token()
	if !errors.Is(err, io.EOF) {
		return jsonNode{}, false
	}

	return node, true
}

func parseJSONValue(decoder *json.Decoder, depth int) (jsonNode, error) {
	token, err := decoder.Token()
	if err != nil {
		return jsonNode{}, fmt.Errorf("web: json: %w", err)
	}

	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return parseJSONMembers(decoder, depth, kindObject)
		case '[':
			return parseJSONMembers(decoder, depth, kindArray)
		}

		return jsonNode{}, fmt.Errorf("web: json: unexpected %q", value)
	case string:
		return jsonStringNode(value, depth), nil
	case json.Number:
		return jsonNode{kind: kindNumber, lit: value.String()}, nil
	case bool:
		return jsonNode{kind: kindLiteral, lit: strconv.FormatBool(value)}, nil
	case nil:
		return jsonNode{kind: kindLiteral, lit: "null"}, nil
	}

	return jsonNode{}, errors.New("web: json: unknown token")
}

// parseJSONMembers reads an object's members or an array's elements up to the
// closing delimiter the decoder is already positioned inside.
func parseJSONMembers(decoder *json.Decoder, depth int, kind jsonKind) (jsonNode, error) {
	node := jsonNode{kind: kind}

	for decoder.More() {
		if kind == kindObject {
			key, err := decoder.Token()
			if err != nil {
				return jsonNode{}, fmt.Errorf("web: json: %w", err)
			}

			name, ok := key.(string)
			if !ok {
				return jsonNode{}, errors.New("web: json: object key is not a string")
			}

			child, err := parseJSONValue(decoder, depth)
			if err != nil {
				return jsonNode{}, err
			}

			child.key = name
			node.kids = append(node.kids, child)

			continue
		}

		child, err := parseJSONValue(decoder, depth)
		if err != nil {
			return jsonNode{}, err
		}

		node.kids = append(node.kids, child)
	}

	_, err := decoder.Token()
	if err != nil {
		return jsonNode{}, fmt.Errorf("web: json: %w", err)
	}

	return node, nil
}

// jsonStringNode builds a string node, parsing the string's own contents as a
// nested document when they are one.
func jsonStringNode(text string, depth int) jsonNode {
	node := jsonNode{kind: kindString, text: text}

	if depth < jsonDepthLimit {
		if doc, ok := parseJSONDocument(text, depth+1); ok {
			node.doc = &doc
		}
	}

	return node
}

// block reports a string value that has to be rendered as its own block: a
// nested document, or text with real line breaks inside it.
//
// Inside: a TRAILING newline is not structure. Nearly every tool result ends
// with one, and counting it would fold "3\n" — an entire answer — behind a
// disclosure summarising it as three lines of text.
func (n jsonNode) block() bool {
	return n.kind == kindString &&
		(n.doc != nil || strings.Contains(strings.TrimRight(n.text, "\n"), "\n"))
}

// inlineable reports a value that reads better on a transcript row than in a
// block beneath it.
func (n jsonNode) inlineable() bool {
	if n.hasBlock() {
		return false
	}

	return n.inlineWidth() <= jsonInlineWidth
}

// hasBlock reports a block-rendered string anywhere in the tree — one is
// enough to force the whole value into a block, since a document cannot be
// nested inside a single line.
func (n jsonNode) hasBlock() bool {
	if n.block() {
		return true
	}

	for _, kid := range n.kids {
		if kid.hasBlock() {
			return true
		}
	}

	return false
}

// inlineWidth is how wide this value would render on one line. Approximate by
// construction — an escaped quote costs more characters than it counts — and
// that is fine: it decides a layout, not a correctness property.
func (n jsonNode) inlineWidth() int {
	switch n.kind {
	case kindObject, kindArray:
		width := len("{}")

		for i, kid := range n.kids {
			if i > 0 {
				width += len(",")
			}

			width += len(" ") + kid.inlineWidth()

			if n.kind == kindObject {
				width += len(`"": `) + len(kid.key)
			}
		}

		if len(n.kids) > 0 {
			width += len(" ")
		}

		return width
	case kindString:
		return len(n.text) + len(`""`)
	case kindNumber, kindLiteral:
		return len(n.lit)
	}

	return 0
}

// writeInline renders the value on one line, with spaces inside the brackets:
// { "path": "notes/inventory.json" } reads as data on a row, where the
// compact source spelling reads as a blob.
func (n jsonNode) writeInline(out *strings.Builder) {
	switch n.kind {
	case kindObject, kindArray:
		openTok, closeTok := "{", "}"
		if n.kind == kindArray {
			openTok, closeTok = "[", "]"
		}

		punc(out, openTok)

		for i, kid := range n.kids {
			if i > 0 {
				punc(out, ",")
			}

			out.WriteString(" ")

			if n.kind == kindObject {
				writeJSONKey(out, kid.key)
			}

			kid.writeInline(out)
		}

		if len(n.kids) > 0 {
			out.WriteString(" ")
		}

		punc(out, closeTok)
	case kindString:
		span(out, "j-str", strconv.Quote(n.text))
	case kindNumber:
		span(out, "j-num", n.lit)
	case kindLiteral:
		span(out, "j-lit", n.lit)
	}
}

// writeBlock renders the value indented over several lines. depth is the
// indentation this value's own closing bracket sits at.
func (n jsonNode) writeBlock(out *strings.Builder, depth int) {
	switch n.kind {
	case kindObject, kindArray:
		n.writeContainerBlock(out, depth)
	case kindString:
		n.writeStringBlock(out, depth)
	case kindNumber:
		span(out, "j-num", n.lit)
	case kindLiteral:
		span(out, "j-lit", n.lit)
	}
}

func (n jsonNode) writeContainerBlock(out *strings.Builder, depth int) {
	openTok, closeTok := "{", "}"
	if n.kind == kindArray {
		openTok, closeTok = "[", "]"
	}

	if len(n.kids) == 0 {
		punc(out, openTok+closeTok)

		return
	}

	// A container of nothing but short scalars is one line even inside a
	// block: exploding {"qty": 60} over three lines buries the value it
	// exists to show under punctuation.
	if n.inlineable() {
		n.writeInline(out)

		return
	}

	punc(out, openTok)
	out.WriteString("\n")

	for i, kid := range n.kids {
		indent(out, depth+1)

		if n.kind == kindObject {
			writeJSONKey(out, kid.key)
		}

		kid.writeBlock(out, depth+1)

		if i < len(n.kids)-1 {
			punc(out, ",")
		}

		out.WriteString("\n")
	}

	indent(out, depth)
	punc(out, closeTok)
}

// writeStringBlock renders a string whose contents deserve their own lines.
// The quotes stay, on lines of their own: this value IS a string in the
// source, and a reader deciding whether a tool returned a document or a
// document-shaped string needs to be able to tell.
func (n jsonNode) writeStringBlock(out *strings.Builder, depth int) {
	if !n.block() {
		span(out, "j-str", strconv.Quote(n.text))

		return
	}

	span(out, "j-str", `"`)
	out.WriteString("\n")

	if n.doc != nil {
		indent(out, depth+1)
		n.doc.writeBlock(out, depth+1)
		out.WriteString("\n")
	} else {
		for _, line := range strings.Split(strings.TrimRight(n.text, "\n"), "\n") {
			indent(out, depth+1)
			span(out, "j-doc", line)
			out.WriteString("\n")
		}
	}

	indent(out, depth)
	span(out, "j-str", `"`)
}

func writeJSONKey(out *strings.Builder, key string) {
	span(out, "j-key", strconv.Quote(key))
	punc(out, ":")
	out.WriteString(" ")
}

func span(out *strings.Builder, class, text string) {
	out.WriteString(`<span class="`)
	out.WriteString(class)
	out.WriteString(`">`)
	out.WriteString(template.HTMLEscapeString(text))
	out.WriteString(`</span>`)
}

func punc(out *strings.Builder, text string) {
	span(out, "j-punc", text)
}

func indent(out *strings.Builder, depth int) {
	out.WriteString(strings.Repeat(" ", depth*jsonIndent))
}

// jsonView is one payload as a template reads it.
type jsonView struct {
	// HTML is the highlighted rendering: one line when Inline, an indented
	// block otherwise.
	HTML template.HTML
	// Summary describes a folded payload well enough to decide against
	// opening it.
	Summary string
	// Inline reports a payload short enough to sit on a transcript row.
	Inline bool
	// Empty reports nothing to render at all, so a caller can omit the
	// element rather than render an empty one.
	Empty bool
}

// jsonValue renders a payload for a transcript row: inline when it is small,
// and otherwise as a block behind a summary the caller folds.
func jsonValue(raw string) jsonView {
	if strings.TrimSpace(raw) == "" {
		return jsonView{Empty: true, Inline: true}
	}

	node, ok := parseJSONDocument(raw, 0)
	if !ok {
		return plainView(raw)
	}

	var out strings.Builder

	if node.inlineable() {
		node.writeInline(&out)

		//nolint:gosec // G203: every span this emits is built here; values go through HTMLEscapeString
		return jsonView{HTML: template.HTML(out.String()), Inline: true, Summary: jsonSummary(node, raw)}
	}

	node.writeBlock(&out, 0)

	//nolint:gosec // G203: as above — the only unescaped text is this file's own markup
	return jsonView{HTML: template.HTML(out.String()), Summary: jsonSummary(node, raw)}
}

// plainView renders a payload that is not a JSON document — a file's text, a
// command's stdout, a bare number. It still folds when it is bulky, so one
// tool returning 400 lines cannot bury the conversation around it.
func plainView(raw string) jsonView {
	text := strings.TrimRight(raw, "\n")
	single := !strings.Contains(text, "\n")

	view := jsonView{
		//nolint:gosec // G203: escaped here, and the only markup is the escape's own output
		HTML:    template.HTML(template.HTMLEscapeString(text)),
		Inline:  single && len(text) <= jsonInlineWidth,
		Summary: plainSummary(text, raw),
	}

	return view
}

// jsonPre renders a payload as a block, always — for the pages that give it a
// <pre> of its own and have nothing to fold it behind.
func jsonPre(raw string) template.HTML {
	if strings.TrimSpace(raw) == "" {
		return ""
	}

	node, ok := parseJSONDocument(raw, 0)
	if !ok {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(strings.TrimRight(raw, "\n")))
	}

	var out strings.Builder

	node.writeBlock(&out, 0)

	//nolint:gosec // G203: spans built above, values escaped
	return template.HTML(out.String())
}

// jsonLine renders a payload on one line, for a table cell that cannot host a
// fold — a row's height belongs to the table. Resource versions are the case:
// canonical JSON by construction, and short, because a version is a ref, a
// digest, or a semver.
func jsonLine(raw string) template.HTML {
	if strings.TrimSpace(raw) == "" {
		return ""
	}

	node, ok := parseJSONDocument(raw, 0)
	if !ok {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(raw))
	}

	var out strings.Builder

	node.writeInline(&out)

	//nolint:gosec // G203: spans built here, values escaped
	return template.HTML(out.String())
}

// jsonSummary describes a folded document: its shape, what is at the top of
// it, and what it costs to open. Naming the keys rather than counting them is
// the difference between "1 key" and "content" — one of those answers whether
// this is the row worth expanding.
func jsonSummary(node jsonNode, raw string) string {
	size := formatBytes(len(raw))

	if node.kind == kindArray {
		return fmt.Sprintf("array · %s · %s", plural(len(node.kids), "item"), size)
	}

	const maxNamed = 3

	if len(node.kids) > 0 && len(node.kids) <= maxNamed {
		names := make([]string, 0, len(node.kids))
		for _, kid := range node.kids {
			names = append(names, kid.key)
		}

		return fmt.Sprintf("%s · %s", strings.Join(names, ", "), size)
	}

	return fmt.Sprintf("object · %s · %s", plural(len(node.kids), "key"), size)
}

// plainSummary describes a folded non-JSON payload by the only two things it
// has: how tall it is and how big it is.
func plainSummary(text, raw string) string {
	lines := strings.Count(text, "\n") + 1

	return fmt.Sprintf("text · %s · %s", plural(lines, "line"), formatBytes(len(raw)))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}

// formatBytes renders a payload size in decimal units, matching the byte
// limits the agent's own tool output is bounded by (32,000 bytes, not 32 KiB)
// so the two numbers are comparable.
func formatBytes(size int) string {
	const unit = 1000

	if size < unit {
		return fmt.Sprintf("%d B", size)
	}

	value := float64(size) / unit
	if value < unit {
		return fmt.Sprintf("%.1f kB", value)
	}

	return fmt.Sprintf("%.1f MB", value/unit)
}
