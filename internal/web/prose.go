package web

// An agent's final response is model-authored prose that in practice carries
// fenced code — a restock order, a patch, a config it is proposing. Rendered
// as one plain block, the fences show up as literal ``` lines and the payload
// they delimit reads as prose, which is the one thing it is not.
//
// So the fences are honored and NOTHING else is: no headings, no lists, no
// links, no raw HTML. This is a deliberate stopping point rather than an
// unfinished markdown renderer. The text comes from a model, and the moment
// this becomes a general HTML rendering surface, every response is one prompt
// injection away from being page structure. A fence is a lexical boundary a
// scanner can honor without interpreting anything inside it.

import (
	"html/template"
	"strings"

	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
)

// proseSegment is one run of an agent response: prose, or one fenced block.
type proseSegment struct {
	// Code is the highlighted code block, empty for a prose run.
	Code template.HTML
	// Text is the prose, escaped, empty for a code block.
	Text template.HTML
	// Lang labels the block, so a reader can tell a proposed yaml from a
	// shell transcript without reading it first.
	Lang string
}

// Code reports a segment that is a fenced block rather than prose.
func (s proseSegment) IsCode() bool { return s.Code != "" }

// renderProse splits a response into prose runs and fenced code blocks.
func renderProse(text string) []proseSegment {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}

	var scanner proseScanner

	for _, line := range strings.Split(trimmed, "\n") {
		scanner.read(line)
	}

	return scanner.done()
}

// proseScanner accumulates segments while walking a response line by line.
type proseScanner struct {
	segments []proseSegment
	prose    []string
	code     []string
	lang     string
	inFence  bool
}

// read consumes one line, opening or closing a fence or adding to whichever
// run is currently open.
func (s *proseScanner) read(line string) {
	fence, isFence := fenceLang(line)

	switch {
	case isFence && !s.inFence:
		s.flushProse()

		s.inFence, s.lang = true, fence
	case isFence:
		s.flushCode()

		s.inFence, s.lang = false, ""
	case s.inFence:
		s.code = append(s.code, line)
	default:
		s.prose = append(s.prose, line)
	}
}

// done closes whatever run is still open. An unterminated fence — a truncated
// response, which is exactly when a reader most needs the page not to swallow
// anything — closes as the code block it was opening.
func (s *proseScanner) done() []proseSegment {
	if s.inFence {
		s.flushCode()
	} else {
		s.flushProse()
	}

	return s.segments
}

func (s *proseScanner) flushProse() {
	body := strings.Trim(strings.Join(s.prose, "\n"), "\n")
	s.prose = s.prose[:0]

	if body != "" {
		//nolint:gosec // G203: escaped here, and nothing else is added
		s.segments = append(s.segments, proseSegment{Text: template.HTML(template.HTMLEscapeString(body))})
	}
}

func (s *proseScanner) flushCode() {
	s.segments = append(s.segments,
		proseSegment{Code: highlightCode(strings.Join(s.code, "\n"), s.lang), Lang: s.lang})
	s.code = s.code[:0]
}

// fenceLang reports a fence line and the language it names. Only a fence at
// the start of a line counts, which is the same rule every markdown reader
// applies and the reason a ``` inside a sentence stays a sentence.
func fenceLang(line string) (string, bool) {
	trimmed := strings.TrimRight(line, " \t")
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}

	lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))

	// A fence's info string can carry more than a language; only the first
	// word selects a lexer.
	lang, _, _ = strings.Cut(lang, " ")

	return strings.ToLower(lang), true
}

// highlightCode colors one fenced block.
//
// JSON goes through this package's own renderer rather than chroma's lexer, so
// that the JSON in a response looks identical to the JSON in a tool result one
// row above it — one data shape, one rendering, wherever it appears. Every
// other language is source text in a language this package has no business
// knowing, which is precisely what chroma is for.
func highlightCode(body, lang string) template.HTML {
	if body == "" {
		return " "
	}

	// A complete document goes through this package's renderer; a fence cut
	// off mid-object falls through to chroma, which lexes a fragment where the
	// parser can only refuse it.
	if lang == "json" {
		if block, ok := jsonBlock(body); ok {
			return block
		}
	}

	lexer := lexers.Get(lang)
	if lexer == nil {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(body))
	}

	iterator, err := lexer.Tokenise(nil, body)
	if err != nil {
		//nolint:gosec // G203: as above
		return template.HTML(template.HTMLEscapeString(body))
	}

	var out strings.Builder

	// Standalone: no wrapping <pre> (the template owns that, so the block
	// matches every other code block on the page) and no class prefix, since
	// the style is carried inline rather than by a stylesheet this page does
	// not serve.
	formatter := html.New(html.WithClasses(false), html.PreventSurroundingPre(true))

	err = formatter.Format(&out, docsCodeStyle, iterator)
	if err != nil {
		//nolint:gosec // G203: as above
		return template.HTML(template.HTMLEscapeString(body))
	}

	//nolint:gosec // G203: chroma escapes token text; the markup is its own <span style=…>
	return template.HTML(out.String())
}
