package tree

import "testing"

// TestPatternToEREClassEdges pins the class renderings TestPatternToERERenders leaves at their boundaries: a class reaching exactly the top of ASCII, a negated class with gaps, a `^` opening a range, and ranges that start at, end at, or span the newline.
func TestPatternToEREClassEdges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		pattern string
		want    string
	}{
		// DEL is the last ASCII rune: a positive class ending on it names no high rune, and a negated class whose high range starts just past it still covers everything above ASCII.
		{`[a\x7f]`, "[a\x7f]"},
		{`[^\x7f]`, "[^\x7f]"},
		// A gap between two excluded runes has to survive the complement, or the negation silently grows.
		{`[^ac]`, `[^ac]`},
		// A range whose low end is `^` cannot start the bracket, where it would negate it.
		{`[\^-a]`, `[_-a^]`},
		{`[\n-\r]`, "[\v-\r]"},
		{`[\t\n]`, "[\t]"},
		{`[\t-\r]`, "[\t\v-\r]"},
	}

	for _, tc := range cases {
		got, err := patternToERE(tc.pattern)
		if err != nil {
			t.Errorf("patternToERE(%q) = error %v", tc.pattern, err)

			continue
		}

		if got != tc.want {
			t.Errorf("patternToERE(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}
