package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzRepairJSONArgs(f *testing.F) {
	for _, seed := range []string{
		`{"a":1}`, `{"a":1,}`, `{"a":"x`, `{"a":`, `{"a":[1,2`, `junk {"a":1} tail`, `{"a":1}}`, `[1,2,]`,
		`{"s":"a\"b",`, `{"a":{"b":[1,{"c":`, ``, `   `, `null`, `"{"`, `{]`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		out, changed := repairJSONArgs(raw)
		if !changed {
			if out != raw {
				t.Fatalf("repairJSONArgs(%q) = %q, false: an unchanged answer must be the input", raw, out)
			}

			return
		}

		if json.Valid([]byte(strings.TrimSpace(raw))) {
			t.Fatalf("repairJSONArgs(%q) rewrote input that already parses into %q", raw, out)
		}

		if !json.Valid([]byte(out)) || out == raw {
			t.Fatalf("repairJSONArgs(%q) = %q, true: a repair must parse and differ", raw, out)
		}

		again, changedAgain := repairJSONArgs(out)
		if changedAgain || again != out {
			t.Fatalf("repairJSONArgs is not idempotent: %q -> %q -> %q", raw, out, again)
		}
	})
}

func FuzzScanJSONObject(f *testing.F) {
	for _, seed := range []string{`{}`, `{"a":"}"}`, `{"a":[1,{"b":2}]} tail`, `{"a":"\"}`, `{`, `{]`} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if !strings.HasPrefix(s, "{") {
			return
		}

		got, complete := scanJSONObject(s)
		if problem := scanShapeProblem(s, got, complete); problem != "" {
			t.Fatalf("scanJSONObject(%q) = %q, %v: %s", s, got, complete, problem)
		}

		if json.Valid([]byte(s)) && (got != strings.TrimRight(s, " \t\r\n") || !complete) {
			t.Fatalf("scanJSONObject(%q) = %q, %v on a whole valid object", s, got, complete)
		}
	})
}

func scanShapeProblem(s, got string, complete bool) string {
	switch {
	case !strings.HasPrefix(s, got) || got == "":
		return "not a non-empty prefix"
	case complete && !strings.HasSuffix(got, "}") && !strings.HasSuffix(got, "]"):
		return "complete but not ended by a closer"
	case !complete && got != s:
		return "incomplete but not the whole input"
	default:
		return ""
	}
}

func FuzzStripTrailingCommas(f *testing.F) {
	for _, seed := range []string{`[1,]`, `{"a":1 , }`, `{"a":",]"}`, `{"a":"\",}"}`, `[[1,],]`, `[1,2]`} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got := stripTrailingCommas(s)
		if len(got) > len(s) {
			t.Fatalf("stripTrailingCommas(%q) = %q grew", s, got)
		}

		if json.Valid([]byte(s)) && got != s {
			t.Fatalf("stripTrailingCommas(%q) = %q changed valid JSON", s, got)
		}

		if strings.ReplaceAll(got, ",", "") != strings.ReplaceAll(s, ",", "") {
			t.Fatalf("stripTrailingCommas(%q) = %q removed something other than a comma", s, got)
		}
	})
}

func FuzzRepairChatCompletionBody(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{\"a\":1,"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":3}}`,
		`{"choices":null}`, `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":7}}]}}]}`, `[]`, `nope`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		got := repairChatCompletionBody(body)
		if !json.Valid(body) {
			if string(got) != string(body) {
				t.Fatalf("repairChatCompletionBody rewrote a body that does not parse: %q -> %q", body, got)
			}

			return
		}

		if !json.Valid(got) {
			t.Fatalf("repairChatCompletionBody(%q) = %q, no longer JSON", body, got)
		}

		if again := repairChatCompletionBody(got); string(again) != string(got) {
			t.Fatalf("repairChatCompletionBody is not idempotent: %q -> %q -> %q", body, got, again)
		}
	})
}

// posixUnquote reads back what shQuote writes: single-quoted runs joined by \' escapes.
func posixUnquote(t *testing.T, q string) string {
	t.Helper()

	var out strings.Builder

	for q != "" {
		switch {
		case strings.HasPrefix(q, `\'`):
			out.WriteByte('\'')
			q = q[2:]
		case strings.HasPrefix(q, "'"):
			end := strings.IndexByte(q[1:], '\'')
			if end < 0 {
				t.Fatalf("unterminated quote in %q", q)
			}

			out.WriteString(q[1 : 1+end])
			q = q[2+end:]
		default:
			t.Fatalf("unquoted byte %q in shell word", q[0])
		}
	}

	return out.String()
}

func FuzzShQuote(f *testing.F) {
	for _, seed := range []string{"", "plain", "it's", "'''", `\'`, "$(rm -rf /)", "a\nb", "é'ü"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if got := posixUnquote(t, shQuote(s)); got != s {
			t.Fatalf("shQuote(%q) reads back as %q", s, got)
		}
	})
}

// FuzzChunkQuoted repeats the input past the chunk bound, because the split is only exercised on content larger than any fuzzer would grow on its own.
func FuzzChunkQuoted(f *testing.F) {
	for _, seed := range []string{"", "a", "'", "é", "😀'", "ab'cd", "\xff"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		content := s
		if s != "" {
			content = strings.Repeat(s, 2*maxWriteChunkBytes/len(s)+1)
		}

		chunks := chunkQuoted(content)

		var joined strings.Builder

		for _, chunk := range chunks {
			raw := posixUnquote(t, chunk)
			if len(chunk) > maxWriteChunkBytes+2 && utf8.RuneCountInString(raw) > 1 {
				t.Fatalf("chunk of %d quoted bytes holds %d runes, over the %d bound", len(chunk), utf8.RuneCountInString(raw), maxWriteChunkBytes)
			}

			joined.WriteString(raw)
		}

		if joined.String() != content {
			t.Fatalf("chunks of %d bytes of %q do not read back as the content", len(content), s)
		}
	})
}

// eREBackslashInBracket is where Go's POSIX parser and a real grep part ways: Go reads `\` in a bracket as an escape, POSIX as itself.
func eREBackslashInBracket(ere string) bool {
	for i := 0; i < len(ere); i++ {
		switch ere[i] {
		case '\\':
			i++
		case '[':
			end, backslash := bracketEnd(ere, i+1)
			if backslash {
				return true
			}

			i = end
		}
	}

	return false
}

// bracketEnd finds the ] closing a bracket whose body starts at j, where a leading ^ and then a leading ] are members rather than syntax.
func bracketEnd(ere string, j int) (int, bool) {
	if j < len(ere) && ere[j] == '^' {
		j++
	}

	if j < len(ere) && ere[j] == ']' {
		j++
	}

	for ; j < len(ere) && ere[j] != ']'; j++ {
		if ere[j] == '\\' {
			return j, true
		}
	}

	return j, false
}

// FuzzPatternToERE is differential: for an ASCII line, the ERE a container's grep is given matches exactly when the RE2 pattern the host would use does.
func FuzzPatternToERE(f *testing.F) {
	for _, seed := range [][2]string{
		{`\d+`, "abc 123"}, {`(?i)hello`, "HeLLo"}, {`[a-zA-Z0-9_-]+`, "x-y"}, {`[^abc]`, "abc"}, {`a|b*c`, "bbc"},
		{`(ab)+`, "abab"}, {`x{2,3}`, "xx"}, {`\s`, "a b"}, {`[\]\-^]`, "^"}, {`\.\*`, ".*"}, {`^foo$`, "foo"},
		{`a.*?b`, "ab"}, {`\p{Greek}`, "α"}, {`a\nb`, "a"}, {`\x00|plain`, "plain"}, {`[\x00-a]`, "a"}, {`[^\n]`, "a"},
		{`\bword\b`, "a word"}, {`[[:alpha:]]+`, "abc"}, {`(?i)k`, "K"}, {`[\\]`, `\`},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, pattern, subject string) {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return
		}

		ere, err := patternToERE(pattern)
		if err != nil {
			if !errors.Is(err, errUnportablePattern) {
				t.Fatalf("patternToERE(%q) refused without errUnportablePattern: %v", pattern, err)
			}

			return
		}

		if strings.ContainsAny(ere, "\x00\n") {
			t.Fatalf("patternToERE(%q) = %q, which no shell argument can carry to grep", pattern, ere)
		}

		if eREBackslashInBracket(ere) {
			return
		}

		posix, err := regexp.CompilePOSIX(ere)
		if err != nil {
			return
		}

		if !plainASCIILine(subject) {
			return
		}

		if want, got := re.MatchString(subject), posix.MatchString(subject); want != got {
			t.Fatalf("pattern %q matches %q: %v, its ERE %q: %v", pattern, subject, want, ere, got)
		}
	})
}

// plainASCIILine is a subject both engines read the same way: grep in the C locale compares bytes and busybox splits lines at NUL.
func plainASCIILine(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf || s[i] == '\n' || s[i] == 0 {
			return false
		}
	}

	return true
}

type listRow struct {
	dir  bool
	size int64
	name string
}

// FuzzParseListRows round-trips rows as the list script prints them, names being whatever find hands back for one directory level.
func FuzzParseListRows(f *testing.F) {
	f.Add("/w", "a\x1fb c\x1fd\te\x1f.hidden", uint8(0b101))
	f.Add("/w x", "a\tb", uint8(2))
	f.Add("/w", "a:b\x1f\x1f", uint8(0))

	f.Fuzz(func(t *testing.T, base, names string, dirs uint8) {
		if base == "" || strings.ContainsAny(base, "\n\t") || strings.HasSuffix(base, "/") {
			return
		}

		out, want, ok := listRowsFixture(base, names, dirs)
		if !ok {
			return
		}

		got := parseListRows(out, base)
		if len(got) != len(want) {
			t.Fatalf("parseListRows read %d entries from %d rows", len(got), len(want))
		}

		for i, entry := range got {
			if entry.name != want[i].name || entry.isDir != want[i].dir || entry.size != want[i].size {
				t.Fatalf("row %d = %+v, want %+v", i, entry, want[i])
			}
		}
	})
}

// listRowsFixture prints names the way the list script does, one \x1f-separated name per row and the bits of dirs saying which are directories.
func listRowsFixture(base, names string, dirs uint8) (string, []listRow, bool) {
	var (
		want []listRow
		out  strings.Builder
	)

	for i, name := range strings.Split(names, "\x1f") {
		if name == "" || strings.ContainsAny(name, "/\n") {
			return "", nil, false
		}

		row := listRow{dir: dirs&(1<<(i%8)) != 0, size: int64(len(name)), name: name}

		kind := "f"
		if row.dir {
			kind, row.size = "d", 0
		}

		fmt.Fprintf(&out, "%s\t%d\t%s/%s\n", kind, row.size, base, name)

		want = append(want, row)
	}

	return out.String(), want, true
}

// FuzzParseGrepHits: whenever a row has exactly one reading against the files asked about, that is the reading parseGrepHits returns.
func FuzzParseGrepHits(f *testing.F) {
	f.Add("/w", "a.txt\x1fa.txt:1.txt", "/w/a.txt:1.txt:3:hello\n/w/a.txt:7:x:y\nnoise\n")
	f.Add("/w", "b", "/w/b:x:y\n/w/b:12:\n")

	f.Fuzz(func(t *testing.T, base, rels, out string) {
		batch := strings.Split(rels, "\x1f")

		byPath := map[string]string{}
		for _, rel := range batch {
			byPath[path.Join(base, rel)] = rel
		}

		hits := parseGrepHits(out, base, batch)

		want := unambiguousGrepRows(out, byPath)

		for _, w := range want {
			if !slices.Contains(hits, w) {
				t.Fatalf("unambiguous row %+v missing from %+v", w, hits)
			}
		}

		for _, hit := range hits {
			if !slices.Contains(batch, hit.rel) {
				t.Fatalf("hit %+v names a file nobody asked about", hit)
			}
		}
	})
}

// unambiguousGrepRows reads every row every possible way against the files asked about, keeping the rows with exactly one reading.
func unambiguousGrepRows(out string, byPath map[string]string) []grepHit {
	var want []grepHit

	for _, row := range strings.Split(out, "\n") {
		var readings []grepHit

		for full, rel := range byPath {
			if hit, ok := readGrepRow(row, full, rel); ok {
				readings = append(readings, hit)
			}
		}

		if len(readings) == 1 {
			want = append(want, readings[0])
		}
	}

	return want
}

func readGrepRow(row, full, rel string) (grepHit, bool) {
	rest, ok := strings.CutPrefix(row, full+":")
	if !ok {
		return grepHit{}, false
	}

	number, text, ok := strings.Cut(rest, ":")
	if !ok {
		return grepHit{}, false
	}

	line, err := strconv.Atoi(number)
	if err != nil {
		return grepHit{}, false
	}

	return grepHit{rel: rel, line: line, text: text}, true
}

func FuzzParseCLIStream(f *testing.F) {
	for _, seed := range []string{
		`{"type":"system","subtype":"init","tools":["Read"]}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"Read","input":{"p":"x"}}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"no"}]}]}}` + "\n" +
			`{"type":"result","result":"done","num_turns":2,"usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"ghost"}]}}`,
		`{"type":"result","errors":["boom"]}`, "not json\n\n{}", `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"x","input":"str"}]}}`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, stream string) {
		_, err := parseCLIStream(strings.NewReader(stream), &transcriptRecorder{}, nil)
		if err != nil {
			t.Fatalf("an unattested parse failed: %v", err)
		}

		_, err = parseCLIStream(strings.NewReader(stream), &transcriptRecorder{}, []string{"Read"})
		if err != nil && !errors.Is(err, errCLIToolSurface) {
			t.Fatalf("an attested parse failed for a reason other than the tool surface: %v", err)
		}
	})
}

// FuzzReplaceEditSpan checks the splice arithmetic every strategy shares: the result is the original with exactly one span swapped for new_string.
func FuzzReplaceEditSpan(f *testing.F) {
	f.Add("a\nb\nc\n", "b", "B", false)
	f.Add("x x x", "x", "y", true)
	f.Add("  if a {\n    b()\n  }\n", "if a {\nb()\n}", "if z {}", false)
	f.Add("one\ntwo\nthree\nfour\n", "one\ntwx\nthree", "1", false)
	f.Add("abc", "", "z", false)

	f.Fuzz(func(t *testing.T, content, oldString, newString string, replaceAll bool) {
		if len(content) > 2048 || len(oldString) > 512 {
			return
		}

		outcome, err := replaceEditSpan(content, oldString, newString, replaceAll)
		if err != nil {
			if uniqueIn(content, oldString) {
				t.Fatalf("a unique exact old_string %q was refused: %v", oldString, err)
			}

			return
		}

		checkEditOutcome(t, content, oldString, newString, replaceAll, outcome)
	})
}

func checkEditOutcome(t *testing.T, content, oldString, newString string, replaceAll bool, outcome editOutcome) {
	t.Helper()

	if outcome.replacements < 1 || outcome.matchIndex < 0 || outcome.matchIndex > len(content) {
		t.Fatalf("outcome %+v is out of range", outcome)
	}

	if !strings.HasPrefix(outcome.updated, content[:outcome.matchIndex]) {
		t.Fatalf("the text before the match changed: %q -> %q", content, outcome.updated)
	}

	if !replaceAll && outcome.replacements == 1 {
		checkOneSplice(t, content, oldString, newString, outcome)
	}
}

// uniqueIn counts overlapping occurrences too, which strings.Count does not: "\n\n\n" is in "\n\n\n\n" twice.
func uniqueIn(content, find string) bool {
	at := strings.Index(content, find)

	return find != "" && at >= 0 && at == strings.LastIndex(content, find)
}

// checkOneSplice recovers the replaced span's length from the sizes alone, so every strategy is held to the same arithmetic without the test knowing which span it chose.
func checkOneSplice(t *testing.T, content, oldString, newString string, outcome editOutcome) {
	t.Helper()

	spanLen := len(content) - len(outcome.updated) + len(newString)
	if spanLen < 0 || outcome.matchIndex+spanLen > len(content) {
		t.Fatalf("outcome %+v implies a span of %d bytes", outcome, spanLen)
	}

	want := content[:outcome.matchIndex] + newString + content[outcome.matchIndex+spanLen:]
	if outcome.updated != want {
		t.Fatalf("updated = %q, want %q", outcome.updated, want)
	}

	if outcome.mode == "exact" && content[outcome.matchIndex:outcome.matchIndex+spanLen] != oldString {
		t.Fatalf("an exact match replaced %q, not %q", content[outcome.matchIndex:outcome.matchIndex+spanLen], oldString)
	}
}

func FuzzTruncateToolOutputLimit(f *testing.F) {
	f.Add("héllo wörld", 2)
	f.Add("😀😀", 5)
	f.Add("abc", 0)

	f.Fuzz(func(t *testing.T, s string, limit int) {
		if limit < 0 {
			return
		}

		got := truncateToolOutputLimit(s, limit)
		if len(s) <= limit {
			if got != s {
				t.Fatalf("truncateToolOutputLimit(%q, %d) changed output under the limit", s, limit)
			}

			return
		}

		kept, _, found := strings.Cut(got, "\n... [truncated ")
		if !found || len(kept) > limit || !strings.HasPrefix(s, kept) {
			t.Fatalf("truncateToolOutputLimit(%q, %d) = %q", s, limit, got)
		}

		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncateToolOutputLimit(%q, %d) split a rune: %q", s, limit, got)
		}
	})
}

func FuzzTruncateSearchLine(f *testing.F) {
	f.Add("a" + strings.Repeat("é", maxSearchLineBytes))
	f.Add("short")

	f.Fuzz(func(t *testing.T, s string) {
		got := truncateSearchLine(s)
		if len(s) <= maxSearchLineBytes {
			if got != s {
				t.Fatalf("truncateSearchLine changed a short line %q", s)
			}

			return
		}

		kept, found := strings.CutSuffix(got, " …[line truncated]")
		if !found || len(kept) > maxSearchLineBytes || !strings.HasPrefix(s, kept) {
			t.Fatalf("truncateSearchLine(%q) = %q", s, got)
		}

		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncateSearchLine split a rune: %q", got)
		}
	})
}

// FuzzScanLineRange compares the paged read against splitting the whole input, which small inputs make exact.
func FuzzScanLineRange(f *testing.F) {
	f.Add("a\nb\nc\n", 2, 3, true)
	f.Add("\nx\n", 1, 2, true)
	f.Add("a\r\n\r\nb", 1, 0, false)
	f.Add("", 1, 1, true)

	f.Fuzz(func(t *testing.T, input string, start, end int, hasEnd bool) {
		if start < 1 || start > 64 || (hasEnd && (end < start || end > 64)) {
			return
		}

		want, wantLast := wholeLineRange(input, start, end, hasEnd)

		content, lastLine, truncated, err := scanLineRange(strings.NewReader(input), start, end, hasEnd)
		if err != nil || truncated {
			t.Fatalf("scanLineRange(%q, %d, %d, %v) = %v, truncated %v", input, start, end, hasEnd, err, truncated)
		}

		if content != want || lastLine != wantLast {
			t.Fatalf("scanLineRange(%q, %d, %d, %v) = %q through %d, want %q through %d", input, start, end, hasEnd, content, lastLine, want, wantLast)
		}
	})
}

// wholeLineRange is the paged read done the obvious way, splitting everything: lines as bufio.ScanLines sees them, one trailing \r dropped.
func wholeLineRange(input string, start, end int, hasEnd bool) (string, int) {
	lines := strings.Split(input, "\n")
	if strings.HasSuffix(input, "\n") || input == "" {
		lines = lines[:len(lines)-1]
	}

	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}

	last := len(lines)
	if hasEnd {
		last = min(end, len(lines))
	}

	if start > last {
		return "", 0
	}

	return strings.Join(lines[start-1:last], "\n"), last
}

func FuzzMatchGlob(f *testing.F) {
	for _, seed := range [][2]string{{"a/b/c_test.go", "*_test.go"}, {"a/b.go", "a/*.go"}, {"x", "**/x"}, {"a/b", "[a-"}} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, rel, glob string) {
		got := matchGlob(rel, glob)

		if !strings.Contains(glob, "/") && got && !matchGlob(rel, "**/"+glob) {
			t.Fatalf("%q matches %q at the top level but not at any depth", rel, glob)
		}
	})
}

// FuzzParseWebFetchURL: the host the allow: fence judged is the host the request built from it dials.
func FuzzParseWebFetchURL(f *testing.F) {
	for _, seed := range []string{
		"https://example.com/x", "http://user@evil.com@example.com/", "https://example.com\\@evil.com", "https://[::1]:80/",
		"https://EXAMPLE.com./", "https://example.com%2F@evil.com", "ftp://example.com", "https://a.example.com:443", "//example.com",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		target, msg := parseWebFetchURL(map[string]any{"url": raw})
		if target == nil {
			if msg == "" {
				t.Fatalf("parseWebFetchURL(%q) refused without saying why", raw)
			}

			return
		}

		if target.Scheme != "http" && target.Scheme != "https" {
			t.Fatalf("parseWebFetchURL(%q) accepted scheme %q", raw, target.Scheme)
		}

		allow := []string{strings.ToLower(target.Hostname())}
		if checkWebFetchHost(target, allow) != nil {
			return
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.String(), nil)
		if err != nil {
			return
		}

		if !strings.EqualFold(req.URL.Hostname(), target.Hostname()) {
			t.Fatalf("parseWebFetchURL(%q) was fenced as %q but dials %q", raw, target.Hostname(), req.URL.Hostname())
		}
	})
}

// FuzzCheckWebFetchHost holds the fence and hostMatches to one reading of "this host or a subdomain of it".
func FuzzCheckWebFetchHost(f *testing.F) {
	for _, seed := range [][2]string{
		{"https://api.example.com", "example.com"}, {"https://badexample.com", "example.com"}, {"https://EXAMPLE.com", "Example.COM"},
		{"https://example.com.evil.net", "example.com"}, {"https://x.", ""}, {"http://[::1]/", "::1"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, raw, entry string) {
		u, err := url.Parse(raw)
		if err != nil {
			return
		}

		allowed := checkWebFetchHost(u, []string{entry}) == nil

		host, lowered := strings.ToLower(u.Hostname()), strings.ToLower(entry)
		if allowed != (host == lowered || strings.HasSuffix(host, "."+lowered)) {
			t.Fatalf("checkWebFetchHost(%q, [%q]) = %v", raw, entry, allowed)
		}

		if lowered == entry && hostMatches(raw, entry) != allowed {
			t.Fatalf("hostMatches(%q, %q) = %v, the fence says %v", raw, entry, !allowed, allowed)
		}
	})
}

func FuzzParseAnswerSeed(f *testing.F) {
	for _, seed := range []string{"deploy?=yes", "a=b=c", " x = y ", "=y", "x=", "noequals"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		seed, err := ParseAnswerSeed(raw)
		if err != nil {
			return
		}

		if seed.Match == "" || seed.Answer == "" || strings.Contains(seed.Match, "=") {
			t.Fatalf("ParseAnswerSeed(%q) = %+v", raw, seed)
		}

		if seed.Match != strings.TrimSpace(seed.Match) || seed.Answer != strings.TrimSpace(seed.Answer) {
			t.Fatalf("ParseAnswerSeed(%q) = %+v, untrimmed", raw, seed)
		}

		again, err := ParseAnswerSeed(seed.Match + "=" + seed.Answer)
		if err != nil || again != seed {
			t.Fatalf("ParseAnswerSeed does not round-trip %+v: %+v, %v", seed, again, err)
		}
	})
}

const labelAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func FuzzSanitizeLabel(f *testing.F) {
	for _, seed := range []string{"build job", "déploy", strings.Repeat("x", 100), "a\r\nX-Injected: 1"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := sanitizeLabel(name)
		if len(got) > maxLabelLen {
			t.Fatalf("sanitizeLabel(%q) = %d bytes", name, len(got))
		}

		for _, r := range got {
			if !strings.ContainsRune(labelAlphabet, r) {
				t.Fatalf("sanitizeLabel(%q) = %q, holding %q", name, got, r)
			}
		}

		if sanitizeLabel(got) != got {
			t.Fatalf("sanitizeLabel is not idempotent on %q", got)
		}
	})
}
