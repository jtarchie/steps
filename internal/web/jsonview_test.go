package web

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestJSONValueFoldsOnSize covers the one decision every transcript row makes:
// stay on the row, or fold. A tool's arguments are small and belong on the
// row; its result usually is not.
func TestJSONValueFoldsOnSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantInline bool
	}{
		{"short args map", `{"path":"notes/inventory.json"}`, true},
		{"empty object", `{}`, true},
		{"bare number is not a document", `3`, true},
		{"short non-json text", "widgets ship on tuesday", true},
		{"trailing newline is not structure", `{"stdout":"3\n"}`, true},
		{"long single-line document", `{"base":"` + strings.Repeat("a/", 60) + `"}`, false},
		{"multi-line text", "line one\nline two", false},
		{"document nested in a string", `{"content":"{\"a\":1}"}`, false},
		{"nothing at all", "  ", true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			view := jsonValue(test.raw)
			if view.Inline != test.wantInline {
				t.Errorf("jsonValue(%q).Inline = %v, want %v (summary %q)",
					test.raw, view.Inline, test.wantInline, view.Summary)
			}
		})
	}
}

// TestJSONValueUnescapesEmbeddedDocument is the read_file shape, and the whole
// reason this is a parser rather than a re-indent: the interesting document
// arrives escaped inside a string, and it has to come out as a document.
func TestJSONValueUnescapesEmbeddedDocument(t *testing.T) {
	t.Parallel()

	view := jsonValue(`{"content":"{\"warehouse\":\"sea-1\",\"on_hand\":12}"}`)
	html := string(view.HTML)

	if strings.Contains(html, `\&#34;`) {
		t.Errorf("embedded document is still escaped:\n%s", html)
	}

	// The inner keys are highlighted as keys, which only happens if the string
	// was parsed rather than printed.
	for _, want := range []string{
		`<span class="j-key">&#34;warehouse&#34;</span>`,
		`<span class="j-key">&#34;on_hand&#34;</span>`,
		`<span class="j-num">12</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s in:\n%s", want, html)
		}
	}

	// The quotes survive on their own lines: this value IS a string, and a
	// reader has to be able to tell that from a document.
	if strings.Count(html, `<span class="j-str">&#34;</span>`) != 2 {
		t.Errorf("embedded document lost the quotes that say it was a string:\n%s", html)
	}
}

// TestJSONValueKeepsEmbeddedOrderAndNumbers pins what a round trip through
// map[string]any would destroy and this renderer must not.
//
// The OUTER envelope's order is already lost upstream (internal/agent records
// args and results as maps), so it is not what this asserts. The document
// inside a "content" string never passed through a Go map — it is a file, or a
// model-authored body — and re-marshaling to re-indent would sort it.
func TestJSONValueKeepsEmbeddedOrderAndNumbers(t *testing.T) {
	t.Parallel()

	html := string(jsonValue(`{"content":"{\"zebra\":1,\"alpha\":2,\"id\":12345678901234567890,\"ratio\":0.50}"}`).HTML)

	zebra, alpha := strings.Index(html, "zebra"), strings.Index(html, "alpha")
	if zebra < 0 || alpha < 0 || zebra > alpha {
		t.Errorf("the embedded document's keys were sorted:\n%s", html)
	}

	// A 20-digit id must not come back as 1.2345678901234567e+19, and 0.50
	// must not become 0.5 — this is a receipt for a content hash.
	for _, want := range []string{"12345678901234567890", "0.50"} {
		if !strings.Contains(html, want) {
			t.Errorf("number %s was rewritten:\n%s", want, html)
		}
	}
}

// TestJSONDepthLimitStopsUnwrapping keeps a chain of documents-inside-strings
// from recursing without bound over input a model authored.
func TestJSONDepthLimitStopsUnwrapping(t *testing.T) {
	t.Parallel()

	// Nest deeper than jsonDepthLimit, each level a document escaped inside the
	// previous one's string.
	payload := `{"n":0}`
	for range jsonDepthLimit + 3 {
		wrapped, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		payload = `{"content":` + string(wrapped) + `}`
	}

	html := string(jsonValue(payload).HTML)

	// It rendered, and stopped: the innermost levels stay escaped strings
	// rather than becoming more blocks.
	if !strings.Contains(html, `class="j-key"`) {
		t.Fatalf("deeply nested payload did not render:\n%s", html)
	}

	if got := strings.Count(html, `<span class="j-key">&#34;content&#34;</span>`); got > jsonDepthLimit+1 {
		t.Errorf("unwrapped %d levels, want at most %d", got, jsonDepthLimit+1)
	}
}

// TestJSONBlockRejectsFragments is the distinction prose.go depends on: a
// ```json fence cut off mid-object must fall through to a lexer that tolerates
// a fragment, not be silently accepted as plain text.
func TestJSONBlockRejectsFragments(t *testing.T) {
	t.Parallel()

	if _, ok := jsonBlock(`{"qty": 60`); ok {
		t.Error("a truncated document was accepted as a complete one")
	}

	if _, ok := jsonBlock(`"qty": 60`); ok {
		t.Error("a bare member was accepted as a document")
	}

	if _, ok := jsonBlock(`{"qty": 60}`); !ok {
		t.Error("a complete document was rejected")
	}
}

// TestJSONValueRejectsNonDocuments keeps the renderer from claiming input it
// cannot faithfully reproduce.
func TestJSONValueRejectsNonDocuments(t *testing.T) {
	t.Parallel()

	// Two concatenated documents, and JSON followed by a log line: rendering
	// either would silently drop everything after the first.
	for _, raw := range []string{`{"a":1}{"b":2}`, `{"a":1}` + "\nwrote 3 files", `{"a":`} {
		if strings.Contains(string(jsonValue(raw).HTML), `class="j-key"`) {
			t.Errorf("%q was rendered as one JSON document", raw)
		}
	}
}

func TestJSONSummaryNamesWhatIsInside(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		// Few enough keys to name: "content" answers whether to open the row,
		// where "1 key" does not.
		{`{"content":"` + strings.Repeat("x", 200) + `"}`, "content · 214 B"},
		{`["a","b","c"]`, "array · 3 items"},
		{`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":` + strings.Repeat("6", 120) + `}`, "object · 6 keys"},
		{strings.Repeat("line\n", 4), "text · 4 lines"},
	}

	for _, test := range tests {
		got := jsonValue(test.raw).Summary
		if !strings.Contains(got, test.want) {
			t.Errorf("summary for %.30q = %q, want it to contain %q", test.raw, got, test.want)
		}
	}
}

// TestJSONPreAlwaysBlocks covers the node page, which gives a payload a <pre>
// of its own and has nothing to fold it behind.
func TestJSONPreAlwaysBlocks(t *testing.T) {
	t.Parallel()

	if got := jsonPre(""); got != "" {
		t.Errorf("jsonPre(\"\") = %q, want empty", got)
	}

	// Not JSON: shown as-is, escaped, rather than dropped.
	if got := string(jsonPre("not json <b>")); got != "not json &lt;b&gt;" {
		t.Errorf("jsonPre passed markup through: %q", got)
	}

	// A container of short scalars stays on one line even inside a block —
	// exploding {"qty":60} over three lines buries the value.
	if got := string(jsonPre(`{"qty":60}`)); strings.Contains(got, "\n") {
		t.Errorf("a short object was exploded over lines: %q", got)
	}
}

// TestJSONLineNeverWraps covers a table cell, whose height belongs to the row.
func TestJSONLineNeverWraps(t *testing.T) {
	t.Parallel()

	got := string(jsonLine(`{"ref":"abc123","branch":"main"}`))
	if strings.Contains(got, "\n") {
		t.Errorf("jsonLine broke a line: %q", got)
	}

	if !strings.Contains(got, `class="j-key"`) {
		t.Errorf("jsonLine did not highlight: %q", got)
	}
}

// TestRenderProseRendersJSONFencesLikeEveryOtherPayload keeps the property
// the fence-only renderer was built around, now that markdown replaced it: a
// ```json block in an answer must look like the JSON in the tool result one
// row above it, not like chroma's idea of JSON.
func TestRenderProseRendersJSONFencesLikeEveryOtherPayload(t *testing.T) {
	t.Parallel()

	got := string(renderProse("Here is the order:\n\n```json\n{\"qty\": 60}\n```\n"))

	if !strings.Contains(got, "Here is the order") {
		t.Errorf("prose around the fence was lost: %s", got)
	}

	if !strings.Contains(got, `class="j-key"`) {
		t.Errorf("fenced json did not use the shared renderer: %s", got)
	}
}

// TestRenderProseHighlightsATruncatedJSONFence covers the case the fragment
// distinction exists for: a response cut off mid-object still gets colored, by
// the lexer, because this package's parser correctly refuses it.
func TestRenderProseHighlightsATruncatedJSONFence(t *testing.T) {
	t.Parallel()

	got := string(renderProse("```json\n{\"restock\": [\n  {\"sku\": \"WID-001\""))
	if !strings.Contains(got, "<span") {
		t.Errorf("truncated json fence lost its highlighting: %s", got)
	}
}

// TestRenderProseKeepsAnUnterminatedFence covers a truncated response — which
// is exactly when a reader most needs the page not to swallow anything.
func TestRenderProseKeepsAnUnterminatedFence(t *testing.T) {
	t.Parallel()

	got := string(renderProse("Proposed:\n\n```yaml\njobs:\n- name: buil"))
	if !strings.Contains(got, "buil") {
		t.Errorf("truncated block lost its content: %s", got)
	}

	if !strings.Contains(got, "codeblock") {
		t.Errorf("unterminated fence did not render as code: %s", got)
	}
}

// TestRenderProseHighlightsOtherLanguages checks the chroma path — every
// language this package has no business knowing.
func TestRenderProseHighlightsOtherLanguages(t *testing.T) {
	t.Parallel()

	if got := string(renderProse("```go\nfunc main() {}\n```")); !strings.Contains(got, "<span") {
		t.Errorf("go block was not highlighted: %s", got)
	}

	// An unknown language degrades to escaped text rather than failing.
	if got := string(renderProse("```nosuchlang\nplain <b>\n```")); strings.Contains(got, "<b>") {
		t.Errorf("unknown language passed markup through: %s", got)
	}
}

func TestThousandsMatchesTheCLI(t *testing.T) {
	t.Parallel()

	// math.MinInt is the one that mattered: -n is itself, so a recursive
	// negative branch never terminated and took the process with it.
	for input, want := range map[int]string{
		0: "0", 999: "999", 1000: "1,000", 34500: "34,500", 1234567: "1,234,567",
		-4200: "-4,200", math.MinInt64: "-9,223,372,036,854,775,808",
	} {
		if got := thousands(input); got != want {
			t.Errorf("thousands(%d) = %q, want %q", input, got, want)
		}
	}
}
