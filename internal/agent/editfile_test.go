package agent

import (
	"errors"
	"strings"
	"testing"
)

// TestReplaceEditSpanExact pins strategy 1's unique-match behavior.
func TestReplaceEditSpanExact(t *testing.T) {
	t.Parallel()

	t.Run("unique match", func(t *testing.T) {
		t.Parallel()

		outcome, err := replaceEditSpan("alpha\nbeta\ngamma\n", "beta", "BETA", false)
		if err != nil {
			t.Fatal(err)
		}

		if outcome.updated != "alpha\nBETA\ngamma\n" {
			t.Errorf("updated = %q", outcome.updated)
		}

		if outcome.mode != "exact" {
			t.Errorf("mode = %q, want exact", outcome.mode)
		}

		if outcome.replacements != 1 {
			t.Errorf("replacements = %d, want 1", outcome.replacements)
		}

		if got := outcome.matchIndex; got != len("alpha\n") {
			t.Errorf("matchIndex = %d, want %d", got, len("alpha\n"))
		}
	})
}

// TestReplaceEditSpanExactMulti covers strategy 1's many-occurrence cases —
// which are also the pre-forgiveness edit_file contract, so these must keep
// holding byte-for-byte.
func TestReplaceEditSpanExactMulti(t *testing.T) {
	t.Parallel()

	t.Run("ambiguous match carries the count", func(t *testing.T) {
		t.Parallel()

		_, err := replaceEditSpan("x\nx\n", "x", "y", false)

		var ambiguous editAmbiguousError
		if !errors.As(err, &ambiguous) {
			t.Fatalf("err = %v, want editAmbiguousError", err)
		}

		if ambiguous.count != 2 {
			t.Errorf("count = %d, want 2", ambiguous.count)
		}
	})

	t.Run("replace_all replaces every occurrence", func(t *testing.T) {
		t.Parallel()

		outcome, err := replaceEditSpan("x\nx\n", "x", "y", true)
		if err != nil {
			t.Fatal(err)
		}

		if outcome.updated != "y\ny\n" || outcome.replacements != 2 {
			t.Errorf("got %+v", outcome)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		_, err := replaceEditSpan("alpha\n", "missing", "y", false)
		if !errors.Is(err, errEditNotFound) {
			t.Errorf("err = %v, want errEditNotFound", err)
		}
	})
}

// TestReplaceEditSpanLineTrimmed covers strategy 2: the model's block is
// right except for per-line whitespace, so the original block (with the
// FILE's own indentation) is what gets replaced.
func TestReplaceEditSpanLineTrimmed(t *testing.T) {
	t.Parallel()

	src := "func f() {\n\t\tif x {\n\t\t\treturn 1\n\t\t}\n}\n"

	// The model re-derived the block with 4-space indentation; the file
	// uses tabs. An exact match fails; line-trimmed must not.
	find := "    if x {\n        return 1\n    }"

	outcome, err := replaceEditSpan(src, find, "\t\tif x {\n\t\t\treturn 2\n\t\t}", false)
	if err != nil {
		t.Fatal(err)
	}

	if outcome.mode != "line-trimmed" {
		t.Errorf("mode = %q, want line-trimmed", outcome.mode)
	}

	want := "func f() {\n\t\tif x {\n\t\t\treturn 2\n\t\t}\n}\n"
	if outcome.updated != want {
		t.Errorf("updated = %q, want %q", outcome.updated, want)
	}

	// Untouched lines keep the file's own bytes — forgiveness must never
	// rewrite the surrounding block to the model's spelling.
	if !strings.HasPrefix(outcome.updated, "func f() {\n") || !strings.HasSuffix(outcome.updated, "\n}\n") {
		t.Errorf("surrounding content changed: %q", outcome.updated)
	}
}

// TestReplaceEditSpanBlockAnchor covers strategy 3: anchors hold, the middle
// drifts a little, similarity decides.
func TestReplaceEditSpanBlockAnchor(t *testing.T) {
	t.Parallel()

	src := strings.Join([]string{
		"func validate() error {",
		"\tif name == \"\" {",
		"\t\treturn errors.New(\"name required\")",
		"\t}",
		"\treturn nil",
		"}",
		"",
	}, "\n")

	t.Run("one misquoted interior line still matches", func(t *testing.T) {
		t.Parallel()

		// The last anchor must be a DISTINCTIVE line: block-anchor (like
		// opencode's original) only considers the first closing-anchor
		// occurrence, so a lone "}" would be shadowed by the block's own
		// inner brace. "\treturn nil" appears exactly once.
		find := strings.Join([]string{
			"func validate() error {",
			"\tif name == \"\" {",
			"\t\treturn errors.New(\"name is required\")", // "is" added: one line off
			"\t}",
			"\treturn nil",
		}, "\n")

		outcome, err := replaceEditSpan(src, find, "REPLACED", false)
		if err != nil {
			t.Fatal(err)
		}

		if outcome.mode != "block-anchor" {
			t.Errorf("mode = %q, want block-anchor", outcome.mode)
		}

		want := "REPLACED\n}\n"
		if outcome.updated != want {
			t.Errorf("updated = %q, want %q", outcome.updated, want)
		}
	})

	t.Run("a wholly different middle does not match", func(t *testing.T) {
		t.Parallel()

		find := strings.Join([]string{
			"func validate() error {",
			"\tcompletely()",
			"\tdifferent()",
			"\tlines()",
			"\treturn nil",
		}, "\n")

		_, err := replaceEditSpan(src, find, "REPLACED", false)
		if !errors.Is(err, errEditNotFound) {
			t.Errorf("err = %v, want errEditNotFound (similarity below threshold)", err)
		}
	})

	t.Run("two-line finds get no block-anchor rescue", func(t *testing.T) {
		t.Parallel()

		// blockAnchorSpans declines finds under three lines (anchors alone
		// could match anywhere), so a two-line find that exact and
		// line-trimmed both miss stays not-found.
		_, err := replaceEditSpan(src, "xyzzy()\nplugh()", "x", false)
		if !errors.Is(err, errEditNotFound) {
			t.Errorf("err = %v, want errEditNotFound", err)
		}
	})

	t.Run("the most similar of several anchor candidates wins", func(t *testing.T) {
		t.Parallel()

		multi := strings.Join([]string{
			"func a() {",
			"\talpha()",
			"\tbeta()",
			"}",
			"func b() {",
			"\tgamma()",
			"\tdelta()",
			"}",
			"",
		}, "\n")

		// Anchors match BOTH blocks; the middle matches only b's, and one
		// middle line is corrupted so exact/line-trimmed both fail and
		// block-anchor is the strategy under test.
		find := strings.Join([]string{
			"func b() {",
			"\tgamma()",
			"\tdelta() // tweaked",
			"}",
		}, "\n")

		outcome, err := replaceEditSpan(multi, find, "REPLACED", false)
		if err != nil {
			t.Fatal(err)
		}

		if outcome.mode != "block-anchor" {
			t.Errorf("mode = %q, want block-anchor", outcome.mode)
		}

		want := "func a() {\n\talpha()\n\tbeta()\n}\nREPLACED\n"
		if outcome.updated != want {
			t.Errorf("updated = %q, want %q (the b block, not a)", outcome.updated, want)
		}
	})
}

// TestIsDisproportionateMatch units the guard directly: within the current
// chain a disproportionate span is rare (block-anchor caps span growth),
// but the guard is what keeps a future looser replacer honest.
func TestIsDisproportionateMatch(t *testing.T) {
	t.Parallel()

	if isDisproportionateMatch("same", "same") {
		t.Error("identical span and find flagged")
	}

	bigSpan := strings.Repeat("line\n", 10)
	if !isDisproportionateMatch(bigSpan, "one\ntwo\nthree") {
		t.Error("a 10-line span for a 3-line find was not flagged")
	}

	if isDisproportionateMatch("a\nb\nc\nd", "a\nb\nc") {
		t.Error("a 4-line span for a 3-line find flagged (within +3 grace)")
	}
}

// TestLineSimilarity sanity-checks the scoring block-anchor averages.
func TestLineSimilarity(t *testing.T) {
	t.Parallel()

	if got := lineSimilarity("return 1", "return 1"); got != 1 {
		t.Errorf("identical lines scored %v, want 1", got)
	}

	if got := lineSimilarity("return 1", "return 2"); got <= 0 || got >= 1 {
		t.Errorf("one-char drift scored %v, want strictly between 0 and 1", got)
	}

	if got := lineSimilarity("alpha", "zzzzzzzz"); got != 0 {
		t.Errorf("unrelated lines scored %v, want 0", got)
	}
}
