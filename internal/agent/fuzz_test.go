package agent

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/agent/llmclient"
)

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

// FuzzCheckWebFetchHost holds the fence and llmclient.HostMatches to one reading of "this host or a subdomain of it".
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

		if lowered == entry && llmclient.HostMatches(raw, entry) != allowed {
			t.Fatalf("llmclient.HostMatches(%q, %q) = %v, the fence says %v", raw, entry, !allowed, allowed)
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
