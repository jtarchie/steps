package web

import (
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v2/lexers"
)

// TestEntryPointsAreEmptyForEmptyInput matches highlightCode's own "" for ""
// convention — an empty <pre> must not collapse, and an inline .say must not
// gain a stray space from a placeholder character.
func TestEntryPointsAreEmptyForEmptyInput(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"", "   ", "\n\n"} {
		if got := highlightMessage(input); got != "" {
			t.Errorf("highlightMessage(%q) = %q, want empty", input, got)
		}

		if got := renderModelText(input); got != "" {
			t.Errorf("renderModelText(%q) = %q, want empty", input, got)
		}
	}

	// highlightPayload does not trim: an empty string is the only case
	// treated as "nothing", since a payload of pure whitespace is still
	// content a tool produced.
	if got := highlightPayload(""); got != "" {
		t.Errorf("highlightPayload(\"\") = %q, want empty", got)
	}
}

// TestEntryPointsSkipHighlightingAboveTheLimit pins highlightLimit: past it,
// detection and highlighting are skipped in favor of escaped plain text, so
// a future caller cannot make page render time unbounded by accident.
func TestEntryPointsSkipHighlightingAboveTheLimit(t *testing.T) {
	t.Parallel()

	oversized := "<script>" + strings.Repeat("package main\n", highlightLimit/8) + "</script>"
	if len(oversized) <= highlightLimit {
		t.Fatalf("test input is only %d bytes, want more than highlightLimit (%d)", len(oversized), highlightLimit)
	}

	for name, got := range map[string]string{
		"highlightMessage": string(highlightMessage(oversized)),
		"highlightPayload": string(highlightPayload(oversized)),
	} {
		if strings.Contains(got, "<script") {
			t.Errorf("%s did not escape an oversized input: leaked <script>", name)
		}

		if strings.Contains(got, "<span style=") {
			t.Errorf("%s highlighted an oversized input instead of skipping it", name)
		}
	}
}

// TestModelTextRendersWhatAModelWroteForAReader is the regression guard on
// the one detection renderModelText must NOT act on. An answer with a fenced
// block, or a heading beside a list or a table, detects as markdown — and
// that is the shape of nearly every answer an agent ends on, so colouring it
// would show a reader the source of a review where prose.go exists to show
// the review.
func TestModelTextRendersWhatAModelWroteForAReader(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{"fenced block", "Here is the fix:\n\n```go\nfunc main() {}\n```\n", "<pre class=\"json code\">"},
		{"heading and list", "## Findings\n\n- one thing\n- another\n", "<ul>"},
		{"heading and table", "# Report\n\n| a | b |\n|---|---|\n| 1 | 2 |\n", "<table>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := detectLanguage(test.text); got != "markdown" {
				t.Fatalf("detectLanguage = %q, want markdown — this test no longer covers the case it names", got)
			}

			got := string(renderModelText(test.text))

			if !strings.Contains(got, test.want) {
				t.Errorf("answer was not rendered as markdown (no %s):\n%s", test.want, got)
			}

			if strings.Contains(got, "## Findings") || strings.Contains(got, "```go") {
				t.Errorf("answer was shown as its own markdown source:\n%s", got)
			}
		})
	}
}

// TestHighlightedModelTextKeepsItsLineBreaks covers the other half: text a
// model wrote that IS a diff is coloured rather than rendered, and it lands
// in "prosebody"'s <div class="md">, which has no white-space rule of its
// own. Without a <pre> around it every newline collapses and a diff arrives
// as one run-on line.
func TestHighlightedModelTextKeepsItsLineBreaks(t *testing.T) {
	t.Parallel()

	got := string(renderModelText("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n"))

	if !strings.Contains(got, "<pre") {
		t.Errorf("highlighted model text carries no <pre>, so its line breaks collapse in prose:\n%s", got)
	}

	if !strings.Contains(got, "#e0645a") {
		t.Errorf("model text that is a diff was not highlighted:\n%s", got)
	}
}

// TestDetectLanguagePositives covers content each rule exists for.
func TestDetectLanguagePositives(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		text string
		want string
	}{
		{"git diff", "diff --git a/main.go b/main.go\nindex abc..def 100644\n--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,4 @@\n func main() {}\n", "diff"},
		{"hunk-only diff", "@@ -12,7 +12,8 @@ func run() {\n-old\n+new\n", "diff"},
		{"unified diff headers", "--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-old\n+new\n", "diff"},
		{"yaml list of maps", "resources:\n  - name: repo\n    type: git\n", "yaml"},
		{"yaml mapping", "name: build\nsteps:\n  - task: compile\n", "yaml"},
		{"go file", "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n", "go"},
		{"python file", "import os\n\ndef main():\n    print(os.getcwd())\n", "python"},
		{"bash shebang", "#!/usr/bin/env bash\nset -eu\necho hi\n", "bash"},
		{"python shebang", "#!/usr/bin/env python3\nprint('hi')\n", "python"},
		{"ruby shebang", "#!/usr/bin/env ruby\nputs 'hi'\n", "ruby"},
		{"xml", "<?xml version=\"1.0\"?>\n<root><child/></root>\n", "xml"},
		{"html doctype", "<!doctype html>\n<html><body>hi</body></html>\n", "html"},
		{"html tag", "<html>\n<body>hi</body>\n</html>\n", "html"},
		{"markdown fence", "Some notes:\n```\ncode here\n```\n", "markdown"},
		{"markdown headings and list", "# Title\n\n- one\n- two\n", "markdown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := detectLanguage(test.text); got != test.want {
				t.Errorf("detectLanguage(%q) = %q, want %q", test.text, got, test.want)
			}
		})
	}
}

// TestDetectLanguageNegatives is where the value is: prose that merely
// resembles a rule's marker must not be misdetected, and a wrong lexer is
// worse than none — it miscolours and implies a claim the content does not
// support.
func TestDetectLanguageNegatives(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		text string
	}{
		{"prose with a colon", "Note: the build failed. Check the logs for details."},
		{"prose with a hashtag", "Ship it! #hashtag #done, everyone is thrilled."},
		{"go test output", "go test ./...\nok  \tgithub.com/jtarchie/steps/internal/web\t3.458s\n"},
		{"http-request-shaped log line", "GET /health 200 12ms\nGET /health 200 9ms\n"},
		{"plain english paragraph", "The reviewer found three issues in the diff, all minor, and recommended merging once the tests pass."},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := detectLanguage(test.text); got != "" {
				t.Errorf("detectLanguage(%q) = %q, want no detection", test.text, got)
			}
		})
	}
}

// TestDetectLanguageStableUnderTruncation pins the reason detection is
// bounded to a prefix: a message cut mid-token by transcriptRecorder's 16KB
// cap must detect the same as it would whole.
func TestDetectLanguageStableUnderTruncation(t *testing.T) {
	t.Parallel()

	var body strings.Builder

	body.WriteString("package main\n\nimport \"fmt\"\n\nfunc main() {\n")

	for body.Len() < 20000 {
		body.WriteString("\tfmt.Println(\"padding to force truncation of this function body\")\n")
	}

	body.WriteString("}\n")

	whole := body.String()
	cut := whole[:16000] + "\n... [truncated 4096 bytes]"

	wholeLang := detectLanguage(whole)
	if wholeLang != "go" {
		t.Fatalf("detectLanguage(whole) = %q, want go", wholeLang)
	}

	if got := detectLanguage(cut); got != wholeLang {
		t.Errorf("detectLanguage(cut) = %q, want %q (stable under truncation)", got, wholeLang)
	}
}

// TestDetectLanguageTruncatedJSON covers a JSON object cut mid-string —
// exactly the shape jsonBlock declines and highlightCode's lexer must still
// colour.
func TestDetectLanguageTruncatedJSON(t *testing.T) {
	t.Parallel()

	cut := `{"path": "notes/inventory.json", "content": "line one\nline tw`

	if got := detectLanguage(cut); got != "json" {
		t.Errorf("detectLanguage(cut json) = %q, want json", got)
	}
}

// TestEveryDetectedLanguageResolvesToALexer catches a typo in the rule
// table before it ships: a lang string that lexers.Get cannot resolve
// silently degrades to plain text, which is the exact failure mode
// detection exists to avoid.
func TestEveryDetectedLanguageResolvesToALexer(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}

	for _, rule := range detectRules {
		if seen[rule.lang] {
			continue
		}

		seen[rule.lang] = true

		if lexers.Get(rule.lang) == nil {
			t.Errorf("rule lang %q resolves to no chroma lexer", rule.lang)
		}
	}
}
