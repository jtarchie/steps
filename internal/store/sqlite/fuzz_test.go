package sqlite

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jtarchie/steps/internal/store"
)

func FuzzTruncateUTF8(f *testing.F) {
	f.Add("hello", 3)
	f.Add("日本語", 4)
	f.Add("\xff"+strings.Repeat("a", 64), 32)
	f.Add("ab\xe6\x97", 3)

	f.Fuzz(func(t *testing.T, s string, limit int) {
		if limit < 0 || limit > 1<<16 {
			t.Skip()
		}

		got := truncateUTF8(s, limit)

		if !strings.HasPrefix(s, got) {
			t.Fatalf("%q is not a prefix of %q", got, s)
		}

		if len(s) <= limit {
			if got != s {
				t.Fatalf("under the limit, %q became %q", s, got)
			}

			return
		}

		if len(got) > limit {
			t.Fatalf("kept %d bytes over a %d limit", len(got), limit)
		}

		// Only the rune straddling the cut may be dropped; invalid bytes before it are the caller's, not a reason to discard the rest.
		if len(got) < limit-(utf8.UTFMax-1) {
			t.Fatalf("kept %d bytes of a %d limit from %q", len(got), limit, s)
		}

		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Fatalf("split a rune: %q", got)
		}
	})
}

func FuzzTruncateTranscript(f *testing.F) {
	f.Add(`[{"type":"text","text":"hi"}]`, 1)
	f.Add(`[{"a":1},{"b":2}]`, store.MaxTranscriptBytes/8)
	f.Add(`not json`, store.MaxTranscriptBytes/4)
	f.Add(`[]`, 1)

	f.Fuzz(func(t *testing.T, seed string, repeat int) {
		if repeat < 1 || repeat > store.MaxTranscriptBytes || len(seed)*repeat > 2*store.MaxTranscriptBytes {
			t.Skip()
		}

		transcript, isArray := repeatTranscript(seed, repeat)

		got := truncateTranscript(transcript)

		if len(got) > store.MaxTranscriptBytes {
			t.Fatalf("kept %d bytes over the %d cap", len(got), store.MaxTranscriptBytes)
		}

		if len(transcript) <= store.MaxTranscriptBytes {
			if got != transcript {
				t.Fatal("a transcript under the cap was rewritten")
			}

			return
		}

		if isArray && !json.Valid([]byte(got)) {
			t.Fatalf("a JSON transcript came back invalid: %.200q", got)
		}
	})
}

// repeatTranscript scales a seed past the cap: a JSON array by repeating its events, so the result is still one valid array, and anything else verbatim.
func repeatTranscript(seed string, repeat int) (string, bool) {
	var events []json.RawMessage
	if json.Unmarshal([]byte(seed), &events) != nil || len(events) == 0 {
		return strings.Repeat(seed, repeat), false
	}

	parts := make([]string, 0, repeat*len(events))
	for range repeat {
		for _, event := range events {
			parts = append(parts, string(event))
		}
	}

	return "[" + strings.Join(parts, ",") + "]", true
}
