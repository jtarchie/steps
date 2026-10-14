package tree

import (
	"strings"
	"testing"
)

// TestSearchContentByteBudgetStopsKeeping drives addMatches past the byte
// budget and checks it stops keeping matches while total still counts them.
func TestSearchContentByteBudgetStopsKeeping(t *testing.T) {
	t.Parallel()

	var r SearchResult

	const path = "big.txt"

	line := strings.Repeat("x", MaxSearchLineBytes)
	perMatch := MaxSearchLineBytes + len(path) + SearchMatchOverheadBytes
	fits := MaxSearchContentBudgetBytes / perMatch

	matches := make([]Match, fits+10)
	for i := range matches {
		matches[i] = Match{Path: path, Line: i + 1, Text: line}
	}

	r.addMatches(matches, len(matches), generousHeadLimit)

	if len(r.Matches) != fits {
		t.Errorf("kept %d matches, want %d — the byte budget must stop collection", len(r.Matches), fits)
	}

	if r.Total != len(matches) {
		t.Errorf("total = %d, want %d — total reports the true scale even past the budget", r.Total, len(matches))
	}

	if r.contentBytes > MaxSearchContentBudgetBytes {
		t.Errorf("contentBytes = %d, over the %d budget", r.contentBytes, MaxSearchContentBudgetBytes)
	}
}

func TestMatchGlob(t *testing.T) {
	t.Parallel()

	cases := []struct {
		rel, glob string
		want      bool
	}{
		{"main.go", "*.go", true},
		{"internal/agent/tools.go", "*.go", true},    // bare glob matches the base name
		{"internal/agent/tools.go", "**/*.go", true}, // leading ** is any depth
		{"internal/agent/tools_test.go", "*_test.go", true},
		{"internal/agent/tools.go", "*_test.go", false},
		{"docs/guide.md", "*.go", false},
		{"internal/agent/tools.go", "internal/*", false}, // * does not cross a separator
	}

	for _, tc := range cases {
		if got := matchGlob(tc.rel, tc.glob); got != tc.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", tc.rel, tc.glob, got, tc.want)
		}
	}
}

func TestSearchContentBudgetExactlyFilled(t *testing.T) {
	t.Parallel()

	const path = "a.txt"

	perMatch := MaxSearchContentBudgetBytes / 56
	line := strings.Repeat("x", perMatch-len(path)-SearchMatchOverheadBytes)

	matches := make([]Match, 60)
	for i := range matches {
		matches[i] = Match{Path: path, Line: i + 1, Text: line}
	}

	var r SearchResult

	r.addMatches(matches, len(matches), generousHeadLimit)

	if len(r.Matches) != 56 || r.contentBytes != MaxSearchContentBudgetBytes {
		t.Errorf("kept %d matches (%d bytes), want 56 filling the %d budget exactly", len(r.Matches), r.contentBytes, MaxSearchContentBudgetBytes)
	}
}

func TestTruncateSearchLineEdges(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("x", MaxSearchLineBytes)
	if got := truncateSearchLine(exact); got != exact {
		t.Errorf("a line of exactly %d bytes came back %d bytes", MaxSearchLineBytes, len(got))
	}

	// No rune start anywhere: the cut must stop at the line's beginning rather than run past it.
	if got := truncateSearchLine(strings.Repeat("\x80", 600)); got != " …[line truncated]" {
		t.Errorf("truncateSearchLine(continuation bytes) = %q", got)
	}
}

// generousHeadLimit is above any count the content byte budget allows, so only the budget can stop collection.
const generousHeadLimit = 200
