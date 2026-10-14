package tree

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

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
			if entry.Name != want[i].name || entry.IsDir != want[i].dir || entry.Size != want[i].size {
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

func FuzzTruncateSearchLine(f *testing.F) {
	f.Add("a" + strings.Repeat("é", MaxSearchLineBytes))
	f.Add("short")

	f.Fuzz(func(t *testing.T, s string) {
		got := truncateSearchLine(s)
		if len(s) <= MaxSearchLineBytes {
			if got != s {
				t.Fatalf("truncateSearchLine changed a short line %q", s)
			}

			return
		}

		kept, found := strings.CutSuffix(got, " …[line truncated]")
		if !found || len(kept) > MaxSearchLineBytes || !strings.HasPrefix(s, kept) {
			t.Fatalf("truncateSearchLine(%q) = %q", s, got)
		}

		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("truncateSearchLine split a rune: %q", got)
		}
	})
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

type listRow struct {
	dir  bool
	size int64
	name string
}
