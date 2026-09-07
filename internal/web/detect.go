package web

// Best-effort language detection over a fragment of text that was never
// fenced or labeled: a system prompt, a user message, an unfenced tool
// payload. chroma's own lexers.Analyse cannot do this job — most of the
// lexers this UI actually sees (yaml, json, python, bash, diff, markdown)
// define no analyser at all, and the few that do fire on a leading token
// (HTTP's on a verb), which is a false-positive generator on ordinary tool
// output. So the sniffer here trades recall for precision: every rule is a
// marker that essentially cannot occur in prose, and anything undecided
// falls back to plain text rather than guessing.
//
// ponytail: filename-hinted detection. lexers.Match(filename) would be the
// highest-confidence signal, but a read_file result arrives as
// {"content": …} with no path — the path lives in the preceding call turn's
// args, and pairing the two means threading state through three renderers
// for a case content sniffing mostly already handles. Upgrade path: carry
// the call's path onto its result turn.

import (
	"html/template"
	"regexp"
	"strings"
)

// detectBudget bounds detection to a fragment's first bytes. It runs on
// every turn of every step on every page render and every stream flush, and
// bounding it makes detection STABLE under truncation: a message cut at
// 16KB never touches the prefix a rule matched against, so the same content
// detects the same way whole or truncated.
const detectBudget = 4096

// langRule is one detection rule: every pattern must match for lang to win.
type langRule struct {
	lang string
	all  []*regexp.Regexp
}

// detectRules is ordered, and the order is load-bearing: a diff's
// `--- `/`+++ ` lines read as markdown, and a YAML `# comment` reads as a
// markdown heading — both are checked before markdown gets a turn.
var detectRules = []langRule{
	{"diff", pat(`(?m)^diff --git `)},
	{"diff", pat(`(?m)^@@ -\d`)},
	{"diff", pat(`(?m)^--- `, `(?m)^\+\+\+ `)},
	{"bash", pat(`\A#!\S*.*\b(ba|z|k)?sh\b`)},
	{"python", pat(`\A#!\S*.*\bpython`)},
	{"ruby", pat(`\A#!\S*.*\bruby`)},
	{"json", pat(`\A\s*[\[{]`, `"\s*:`)},
	{"xml", pat(`\A\s*<\?xml`)},
	{"html", pat(`(?i)\A\s*(<!doctype html|<html[\s>])`)},
	{"go", pat(`(?m)^package \w+$`, `(?m)^(func |import |type |var )`)},
	{"python", pat(`(?m)^\s*(def|class) \w+.*:\s*$`, `(?m)^\s*(import|from) \w`)},
	// Tolerates a leading "- ": most real YAML is a list of maps
	// (`  - name: repo`), and an indented-key pattern without the optional
	// dash misses that common case entirely. There is no bare `\A---` rule:
	// it collides with markdown frontmatter and a diff's `--- a/file`, and
	// the indented-key + top-level-key pair is what actually distinguishes a
	// YAML document from prose containing "Note: something" — prose has no
	// indented key line.
	{"yaml", pat(`(?m)^[ \t]+(- )?[\w.-]+:( |$)`, `(?m)^(- )?[\w.-]+:( |$)`)},
	{"markdown", pat("(?m)^```")},
	{"markdown", pat(`(?m)^#{1,6} \S`, `(?m)^([-*+] |\d+\. |\|)`)},
}

// pat compiles a rule's required patterns.
func pat(exprs ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(exprs))
	for _, expr := range exprs {
		out = append(out, regexp.MustCompile(expr))
	}

	return out
}

// detectLanguage reports the first rule whose patterns all match, or "" when
// none does.
func detectLanguage(text string) string {
	if len(text) > detectBudget {
		text = text[:detectBudget]
	}

	for _, rule := range detectRules {
		if rule.matches(text) {
			return rule.lang
		}
	}

	return ""
}

func (r langRule) matches(text string) bool {
	for _, re := range r.all {
		if !re.MatchString(text) {
			return false
		}
	}

	return true
}

// highlightLimit is the size above which detection and highlighting are
// skipped in favor of plain escaped text. Step output is capped at 32KB and
// a transcript entry at 16KB (see internal/agent's transcriptRecorder), so
// nothing recorded today reaches this — it exists so a future caller cannot
// make page render time unbounded by accident.
const highlightLimit = 64 << 10

// highlightMessage renders a system or user message: coloured when
// detection is decisive, otherwise run through the markdown lexer so it
// stays literal — never re-rendered, since this is text that was SENT to a
// model, not written by one for a reader (see prose.go's file comment for
// why that distinction is the rendering boundary).
func highlightMessage(text string) template.HTML {
	if strings.TrimSpace(text) == "" {
		return ""
	}

	if len(text) > highlightLimit {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(text))
	}

	lang := detectLanguage(text)
	if lang == "" {
		lang = "markdown"
	}

	return highlightCode(text, lang)
}

// renderModelText is the hybrid this issue asks for: a model turn (or the
// final answer) that IS a diff or an unfenced JSON blob is highlighted
// instead of being mangled by goldmark's markdown rules; anything else
// falls back to renderProse, exactly as the node page renders an answer
// today.
//
// A markdown detection is the one answer that argues for RENDERING rather
// than colouring: it fires on a fenced block or a heading beside a list,
// which is what an agent's answer looks like — the exact text prose.go
// exists to render, and showing it coloured would show a reader the source
// of a review instead of the review.
func renderModelText(text string) template.HTML {
	if len(text) <= highlightLimit {
		if lang := detectLanguage(text); lang != "" && lang != "markdown" {
			return highlightBlock(text, lang)
		}
	}

	// renderProse already answers "" for empty and whitespace-only input.
	return renderProse(text)
}

// highlightBlock is highlighted markup a PROSE context can hold. Its callers
// drop it into "prosebody"'s <div class="md">, which carries no white-space
// rule of its own — bare, a highlighted diff arrives as one run-on line —
// so it takes the same block writeCodeBlock gives a fenced ```diff, minus
// the language label: detection is a guess, and every other label on these
// rows is a fact.
func highlightBlock(text, lang string) template.HTML {
	//nolint:gosec // G203: constant markup around highlightCode's escaped output
	return template.HTML(`<div class="codeblock"><pre class="json code">` +
		string(highlightCode(text, lang)) + `</pre></div>`)
}

// highlightPayload colors a tool payload or step output: coloured when
// detection is decisive, otherwise plain escaped text — never markdown,
// since an undetected payload is far more likely to be a log or a plain
// value than prose.
func highlightPayload(text string) template.HTML {
	if text == "" {
		return ""
	}

	if len(text) > highlightLimit {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(text))
	}

	lang := detectLanguage(text)
	if lang == "" {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(text))
	}

	return highlightCode(text, lang)
}
