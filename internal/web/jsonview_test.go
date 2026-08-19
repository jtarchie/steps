package web

import (
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

// TestJSONValueKeepsSourceOrderAndNumbers pins the two things a round trip
// through map[string]any silently destroys.
func TestJSONValueKeepsSourceOrderAndNumbers(t *testing.T) {
	t.Parallel()

	html := string(jsonValue(`{"zebra":1,"alpha":2,"id":12345678901234567890,"ratio":0.50}`).HTML)

	zebra, alpha := strings.Index(html, "zebra"), strings.Index(html, "alpha")
	if zebra < 0 || alpha < 0 || zebra > alpha {
		t.Errorf("keys were sorted rather than kept in source order:\n%s", html)
	}

	// A 20-digit id must not come back as 1.2345678901234567e+19, and 0.50
	// must not become 0.5 — this is a receipt for a content hash.
	for _, want := range []string{"12345678901234567890", "0.50"} {
		if !strings.Contains(html, want) {
			t.Errorf("number %s was rewritten:\n%s", want, html)
		}
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

// TestRenderProseHonorsFencesOnly is the deliberate stopping point: a fence is
// a lexical boundary, and nothing else in the model's text becomes structure.
func TestRenderProseHonorsFencesOnly(t *testing.T) {
	t.Parallel()

	segments := renderProse("Here is the order:\n\n```json\n{\"qty\": 60}\n```\n")
	if len(segments) != 2 {
		t.Fatalf("got %d segments, want prose + code: %+v", len(segments), segments)
	}

	if segments[0].IsCode() || !strings.Contains(string(segments[0].Text), "Here is the order") {
		t.Errorf("first segment is not the prose: %+v", segments[0])
	}

	if !segments[1].IsCode() || segments[1].Lang != "json" {
		t.Errorf("second segment is not a json block: %+v", segments[1])
	}

	// JSON in a response is rendered by this package, so it matches the JSON
	// in a tool result one row above it.
	if !strings.Contains(string(segments[1].Code), `class="j-key"`) {
		t.Errorf("fenced json did not use the shared renderer: %s", segments[1].Code)
	}

	// A heading is not a heading, and raw HTML is not HTML.
	plain := renderProse("# not a heading\n<script>alert(1)</script>")
	if len(plain) != 1 || plain[0].IsCode() {
		t.Fatalf("prose became something else: %+v", plain)
	}

	if strings.Contains(string(plain[0].Text), "<script>") {
		t.Errorf("raw HTML survived: %s", plain[0].Text)
	}
}

// TestRenderProseKeepsAnUnterminatedFence covers a truncated response — which
// is exactly when a reader most needs the page not to swallow anything.
func TestRenderProseKeepsAnUnterminatedFence(t *testing.T) {
	t.Parallel()

	segments := renderProse("Proposed:\n\n```yaml\njobs:\n- name: buil")
	if len(segments) != 2 || !segments[1].IsCode() {
		t.Fatalf("unterminated fence was dropped: %+v", segments)
	}

	if !strings.Contains(string(segments[1].Code), "buil") {
		t.Errorf("truncated block lost its content: %s", segments[1].Code)
	}
}

// TestRenderProseHighlightsOtherLanguages checks the chroma path — every
// language this package has no business knowing.
func TestRenderProseHighlightsOtherLanguages(t *testing.T) {
	t.Parallel()

	segments := renderProse("```go\nfunc main() {}\n```")
	if len(segments) != 1 || !segments[0].IsCode() {
		t.Fatalf("go block not recognized: %+v", segments)
	}

	if !strings.Contains(string(segments[0].Code), "<span") {
		t.Errorf("go block was not highlighted: %s", segments[0].Code)
	}

	// An unknown language degrades to escaped text rather than failing.
	unknown := renderProse("```nosuchlang\nplain <b>\n```")
	if strings.Contains(string(unknown[0].Code), "<b>") {
		t.Errorf("unknown language passed markup through: %s", unknown[0].Code)
	}
}

func TestThousandsMatchesTheCLI(t *testing.T) {
	t.Parallel()

	for input, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 34500: "34,500", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := thousands(input); got != want {
			t.Errorf("thousands(%d) = %q, want %q", input, got, want)
		}
	}
}
