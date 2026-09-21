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

// TestPatternToEREAgreesWithGoOnRealLines is the guarantee the shell-out puts at risk, checked the only way that means anything: the rendered ERE and the Go regexp must reach the same verdict on the same line. Go standing in for grep is sound here because the rendering is what is under test — ERE is a subset of RE2's syntax, so a pattern that survives it means the same thing to both engines, and the container-side halves are pinned by the docker tests.
func TestPatternToEREAgreesWithGoOnRealLines(t *testing.T) {
	t.Parallel()

	patterns := []string{
		`\d+`, `\w+`, `func\s+\w+\(`, `[^abc]`, `foo|bar`, `^package `,
		`[a-z]{2,4}`, `a\.b`, `(?i)error`, `\bmain\b`, `x*y`, `(ab|cd)+e`,
	}

	lines := []string{
		"package main", "func Handle(w http.ResponseWriter) {", "error: 42 things",
		"ERROR: shouted", "a.b", "axb", "abcd", "xxxy", "cdcde", "foo", "bar",
		"", "   ", "main()", "mainly", "9", "_under_score", "----",
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
