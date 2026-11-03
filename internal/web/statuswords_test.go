package web

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

// TestEveryFinishStatusHasAWebMapping: a status added to events without a word, glyph and disc here would render as the unknown default, so each one is pinned to a deliberate answer and the unknown value is shown to differ.
func TestEveryFinishStatusHasAWebMapping(t *testing.T) {
	t.Parallel()

	const unknown = "no-such-status"

	cases := []struct {
		status string
		word   string
		glyph  string
		disc   disc
	}{
		{events.StatusSucceeded, "passed", "✓", discPassed},
		{events.StatusFailed, "failed", "✗", discFailed},
		{events.StatusErrored, "errored", "!", discErrored},
		{events.StatusAborted, "aborted", "■", discAborted},
		{events.StatusSkipped, "skipped", "", discNone},
	}

	for _, c := range cases {
		if got := statusWord(c.status); got != c.word {
			t.Errorf("statusWord(%q) = %q, want %q", c.status, got, c.word)
		}

		if got := statusMark(c.status); got != c.glyph {
			t.Errorf("statusMark(%q) = %q, want %q", c.status, got, c.glyph)
		}

		if got := discOf(c.status); got != c.disc {
			t.Errorf("discOf(%q) = %v, want %v", c.status, got, c.disc)
		}
	}

	if statusMark(unknown) != "" || discOf(unknown) != discNone {
		t.Fatal("unknown status should map to the empty glyph and no disc")
	}
}
