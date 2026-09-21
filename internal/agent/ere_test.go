package agent

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// TestPatternToERERenders pins the spellings a container's grep is actually handed. The shorthands are the point: RE2's parser expands them before this sees them, which is why `\d` comes out as a class rather than as the two different things busybox and GNU grep each make of it.
func TestPatternToERERenders(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		want    string
	}{
		{`hello`, `hello`},
		{`\d+`, `[0-9]+`},
		{`\w+`, `[0-9A-Z_a-z]+`},
		{`\s`, "[\t\f-\r ]"},
		{`\bfoo\b`, `\bfoo\b`},
		{`foo|bar`, `foo|bar`},
		{`(?:ab)+`, `(ab)+`},
		{`(ab)+`, `(ab)+`},
		{`[a-z]{2,4}`, `[a-z]{2,4}`},
		{`x{3}`, `x{3}`},
		{`x{2,}`, `x{2,}`},
		{`a\.b`, `a\.b`},
		{`^x$`, `^x$`},
		{`.`, `.`},
		{`[^abc]`, `[^a-c]`},
		{`(?i)hi`, `[Hh][Ii]`},
		// A folded literal holding something with no other case: the fold set is one rune, and spelling it as a bracket alternation would be wrong as well as pointless. Models write this constantly — a version, an error code — and every other (?i) case here is pure letters.
		{`(?i)v2`, `[Vv]2`},
		{`(?i)err-42`, `[Ee][Rr][Rr]-42`},
		{`a(b|c)d`, `a([b-c])d`},
		{`func\s+\w+\(`, "func[\t\f-\r ]+[0-9A-Z_a-z]+\\("},
		// The four characters a bracket expression can only POSITION. The collating-symbol spelling GNU grep accepts is what musl's regcomp refuses outright, so each of these is a search that silently found nothing on alpine and everything on debian.
		{`[a-zA-Z0-9_-]+`, `[0-9A-Z_a-z-]+`},
		{`[-+]`, `[+-]`},
		{`[^-a]`, `[^a-]`},
		{`[\]\^\-]`, `[]^-]`},
		{`[[-\]]`, `[][\]`},
		{`[\\/]`, `[/\]`},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()

			got, err := patternToERE(tc.pattern)
			if err != nil {
				t.Fatalf("patternToERE(%q) = error %v", tc.pattern, err)
			}

			if got != tc.want {
				t.Errorf("patternToERE(%q) = %q, want %q", tc.pattern, got, tc.want)
			}
		})
	}
}

// TestPatternToERERefusesWhatPOSIXCannotSay covers the constructs that must come back to the model as data. Non-greedy is the one that matters: measured on alpine and debian alike, `a.*?X` under -E matches greedily and reports success, so letting it through answers a different question without saying so.
func TestPatternToERERefusesWhatPOSIXCannotSay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		wantSay string
	}{
		{`a.*?X`, "non-greedy"},
		{`a+?`, "non-greedy"},
		{`\p{Greek}+`, "Unicode character class"},
		{`a\nb`, "line-oriented"},
	}

	for _, tc := range cases {
		t.Run(tc.pattern, func(t *testing.T) {
			t.Parallel()

			got, err := patternToERE(tc.pattern)
			if err == nil {
				t.Fatalf("patternToERE(%q) = %q, want a refusal", tc.pattern, got)
			}

			if !strings.Contains(err.Error(), tc.wantSay) {
				t.Errorf("patternToERE(%q) error = %q, want it to name %q", tc.pattern, err, tc.wantSay)
			}

			if !errors.Is(err, errUnportablePattern) {
				t.Errorf("patternToERE(%q) error = %v, want it to carry errUnportablePattern so the caller reports it as tool data", tc.pattern, err)
			}
		})
	}
}

// TestPatternToEREAgreesWithGoOnRealLines checks the rendered ERE and the Go regexp reach the same verdict on the same line, which is cheap and catches most of what the rendering can get wrong.
//
// Go is NOT a faithful stand-in for grep, and one case proves it: RE2 treats a backslash inside a bracket expression as an escape, POSIX treats it as a literal, so the correct rendering of `[a\\b]` is `[a-b\]` — which both real greps compile and match, and which Go rejects as an unterminated class. A class holding a backslash is therefore tested against real greps in TestContainerTreeMatchesHostTree and deliberately absent here.
func TestPatternToEREAgreesWithGoOnRealLines(t *testing.T) {
	t.Parallel()

	// Chosen to reach the branches the table above cannot: a negated class is stored by RE2 as ranges that RUN TO the top of the rune space, so complementASCII decides what a bracket expression actually names; a class whose ends are ], ^, - or backslash has to be positioned rather than escaped; \\s spans the newline, which must be split out or grep reads the pattern as two; and every ERE metacharacter has to survive as a literal.
	patterns := []string{
		`\d+`, `\w+`, `\s`, `\S`, `\D`, `\W`,
		`func\s+\w+\(`, `^package `, `[a-z]{2,4}`, `x{3}`, `x{2,}`,
		`foo|bar`, `(ab|cd)+e`, `a(b|c)d`, `x*y`, `.`,
		`[^abc]`, `[^a-z]`, `[^0-9]`, `[^ ]`, `[^-a-z]`, `[^a-]`,
		`[a-zA-Z0-9_-]+`, `[0-9-]+`, `[+-]`, `[-]`, `[]^-]`, `[!-/]`,
		`[\]]`, `[\^]`, `[.]`, `[*+?]`, `[|()]`, `[{}]`,
		`a\.b`, `a\*b`, `a\+b`, `a\?b`, `a\|b`, `a\(b`, `a\[b`, `a\{b`, `a\\b`, `a\$b`, `a\^b`,
		`(?i)error`, `(?i)v2`, `(?i)err-42`, `\bmain\b`, `\Bmain`,
		`^$`, `^.$`, `[[:alpha:]]`, `[[:digit:]]+`,
	}

	lines := []string{
		"package main", "func Handle(w http.ResponseWriter) {", "error: 42 things",
		"ERROR: shouted", "a.b", "axb", "abcd", "xxxy", "cdcde", "foo", "bar",
		"", "   ", "main()", "mainly", "9", "_under_score", "----", "v2", "V2",
		"err-42", "ERR-42", "kebab-case-name", "a-b", "a^b", "x]y", `a\b`, "+", "-",
		"]", "^", "[bracket]", "a!b/c", "a*b", "a+b", "a?b", "a|b", "a(b", "a[b",
		"a{b", "a$b", "tab\there", "ünïcödé", "naïve", "100%", "{}", "()",
	}

	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			t.Parallel()

			ere, err := patternToERE(pattern)
			if err != nil {
				t.Fatalf("patternToERE(%q) = error %v", pattern, err)
			}

			original := regexp.MustCompile(pattern)

			rendered, err := regexp.Compile(ere)
			if err != nil {
				t.Fatalf("rendered ERE %q does not compile: %v", ere, err)
			}

			for _, line := range lines {
				if original.MatchString(line) != rendered.MatchString(line) {
					t.Errorf("%q vs rendered %q disagree on %q: %v != %v",
						pattern, ere, line, original.MatchString(line), rendered.MatchString(line))
				}
			}
		})
	}
}
