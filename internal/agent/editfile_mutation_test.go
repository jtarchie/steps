package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editfileMustReplace(t *testing.T, content, find, mode, want string) {
	t.Helper()

	outcome, err := replaceEditSpan(content, find, "REPLACED", false)
	if err != nil {
		t.Fatalf("replaceEditSpan(%q, %q) = %v", content, find, err)
	}

	if outcome.mode != mode || outcome.updated != want {
		t.Errorf("replaceEditSpan(%q, %q) = %q via %s, want %q via %s", content, find, outcome.updated, outcome.mode, want, mode)
	}
}

func editfileMustNotFind(t *testing.T, content, find string) {
	t.Helper()

	_, err := replaceEditSpan(content, find, "REPLACED", false)
	if !errors.Is(err, errEditNotFound) {
		t.Errorf("replaceEditSpan(%q, %q) err = %v, want errEditNotFound", content, find, err)
	}
}

func TestReplaceEditSpanLineTrimmedAtEndOfFile(t *testing.T) {
	t.Parallel()

	editfileMustReplace(t, "x\n\tfoo", "foo  ", "line-trimmed", "x\nREPLACED")
}

// A find's trailing newline is dropped, never its last real line.
func TestReplaceEditSpanFindsEveryLineOfTheFind(t *testing.T) {
	t.Parallel()

	editfileMustReplace(t, "\tfoo\n\tbar\n", "foo\nbar", "line-trimmed", "REPLACED\n")
	editfileMustReplace(t, "\tfoo\n\tbar\n", "foo\nbar\n", "line-trimmed", "REPLACED\n")
}

func TestBlockAnchorBoundaries(t *testing.T) {
	t.Parallel()

	t.Run("a three-line find at exactly the similarity threshold matches", func(t *testing.T) {
		t.Parallel()

		// 7 edits over 20 characters is a similarity of exactly 0.65.
		content := "start {\n" + strings.Repeat("a", 20) + "\nend }\n"
		find := "start {\n" + strings.Repeat("a", 13) + strings.Repeat("b", 7) + "\nend }"

		editfileMustReplace(t, content, find, "block-anchor", "REPLACED\n")
	})

	t.Run("a block one line longer than the find is within tolerance", func(t *testing.T) {
		t.Parallel()

		// Only both middle lines together average past the threshold; the drifted first alone would not.
		content := "begin\n\ta()\n\tb()\n\tc()\nfinish\n"
		find := "begin\n\ta() // x\n\tb()\nfinish"

		editfileMustReplace(t, content, find, "block-anchor", "REPLACED\n")
	})

	t.Run("a block one line shorter than the find is within tolerance", func(t *testing.T) {
		t.Parallel()

		// The find's third middle line has no counterpart here, so it must not be scored against the closing anchor.
		content := "pre\nbegin\n\ta()\n\tb()\nfinish\n"
		find := "begin\n\ta()\n\tb() // x\n\tzzzzzzzz\nfinish"

		editfileMustReplace(t, content, find, "block-anchor", "pre\nREPLACED\n")
	})

	t.Run("a block two lines longer than a four-line find is not a candidate", func(t *testing.T) {
		t.Parallel()

		editfileMustNotFind(t, "begin\n\ta()\n\tb()\n\tc()\n\td()\nfinish\n", "begin\n\ta()\n\tb() // x\nfinish")
	})

	t.Run("an opening anchor with no closing one is not found", func(t *testing.T) {
		t.Parallel()

		editfileMustNotFind(t, "begin\n\ta()\n", "begin\n\ta()x\nnever")
	})

	t.Run("similarity is the average of the middle lines, not their sum", func(t *testing.T) {
		t.Parallel()

		editfileMustNotFind(t, "begin\n\ta()\n\tb()\nfinish\n", "begin\n\ta() // x\n\tb() // x\nfinish")
	})

	t.Run("of two candidates the more similar wins even when it comes second", func(t *testing.T) {
		t.Parallel()

		content := "if err != nil {\n\talpha()\n\tbeta()\n}\nif err != nil {\n\tgamma()\n\tdelta()\n}\n"
		find := "if err != nil {\n\tgamma()\n\tdelta() // tweaked\n}"

		editfileMustReplace(t, content, find, "block-anchor", "if err != nil {\n\talpha()\n\tbeta()\n}\nREPLACED\n")
	})

	t.Run("of two candidates the better one at exactly the threshold matches", func(t *testing.T) {
		t.Parallel()

		content := "start {\n" + strings.Repeat("c", 20) + "\nend }\nstart {\n" + strings.Repeat("a", 20) + "\nend }\n"
		find := "start {\n" + strings.Repeat("a", 13) + strings.Repeat("b", 7) + "\nend }"

		editfileMustReplace(t, content, find, "block-anchor", "start {\n"+strings.Repeat("c", 20)+"\nend }\nREPLACED\n")
	})

	t.Run("of two equally similar candidates the first wins", func(t *testing.T) {
		t.Parallel()

		editfileMustReplace(t, "begin\n\tab()\nend\nbegin\n\tba()\nend\n", "begin\n\taa()\nend", "block-anchor", "REPLACED\nbegin\n\tba()\nend\n")
	})
}

func TestLineSimilarityLevenshteinCap(t *testing.T) {
	t.Parallel()

	at := strings.Repeat("a", 1024)
	if got := lineSimilarity(at, at[:1023]+"b"); got <= 0.99 {
		t.Errorf("two 1024-byte lines one edit apart scored %v; a product exactly at the cap is still compared", got)
	}

	over := strings.Repeat("a", 2048)
	if got := lineSimilarity(over, over[:2047]+"b"); got != 0 {
		t.Errorf("two 2048-byte lines scored %v, want 0 past the cap", got)
	}

	if got := levenshtein("ab", "a"); got != 1 {
		t.Errorf(`levenshtein("ab", "a") = %d, want 1`, got)
	}
}

func TestIsDisproportionateMatchThresholds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		span, find string
		want       bool
	}{
		{"a span of twice a three-line find's lines", "1\n2\n3\n4\n5\n6", "a\nb\nc", true},
		{"a two-line span for a one-line find", "1\n2", "a", false},
		{"an eight-line span for a five-line find", "1\n2\n3\n4\n5\n6\n7\n8", "a\nb\nc\nd\ne", false},
		{"a long one-line span for a one-line find", strings.Repeat("s", 600), "x", false},
		{"a long span for a short two-line find", "a\n" + strings.Repeat("s", 600), "x\ny", true},
		{"a span exactly 500 characters past the find", "a\n" + strings.Repeat("s", 598), "x\n" + strings.Repeat("y", 98), false},
		{"a span under four times a long find", "a\n" + strings.Repeat("s", 998), "x\n" + strings.Repeat("y", 398), false},
	} {
		if got := isDisproportionateMatch(tc.span, tc.find); got != tc.want {
			t.Errorf("%s: isDisproportionateMatch = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestExecEditFileAtTheSizeLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")

	err := os.WriteFile(path, []byte("needle"+strings.Repeat("x", maxEditFileBytes-len("needle"))), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result := execEditFile(context.Background(), map[string]any{
		"path": "big.txt", "old_string": "needle", "new_string": "pin",
	}, testEnv(dir))
	if result["error"] != nil {
		t.Fatalf("editing a file of exactly the limit = %v, want it edited", result["error"])
	}

	data, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(string(data), "pinxxx") {
		t.Errorf("file starts %q, want the edit applied", data[:10])
	}
}
