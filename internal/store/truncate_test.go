package store

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzTruncateUTF8(f *testing.F) {
	f.Add("hello", 3)
	f.Add("日本語", 4)
	f.Add("\xff"+strings.Repeat("a", 64), 32)
	f.Add("ab\xe6\x97", 3)
	f.Add("abc", 3)
	f.Add("\x80\x80\x80\x80", 2)
	f.Add("a\x80\x80\x80\x80\x80\x80", 6)

	f.Fuzz(func(t *testing.T, s string, limit int) {
		if limit < 0 || limit > 1<<16 {
			t.Skip()
		}

		got := TruncateUTF8(s, limit)

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
	f.Add(`[{"a":1},{"b":2}]`, MaxTranscriptBytes/8)
	f.Add(`not json`, MaxTranscriptBytes/4)
	f.Add(`[]`, 1)

	f.Fuzz(func(t *testing.T, seed string, repeat int) {
		if repeat < 1 || repeat > MaxTranscriptBytes || len(seed)*repeat > 2*MaxTranscriptBytes {
			t.Skip()
		}

		transcript, isArray := repeatTranscript(seed, repeat)

		got := TruncateTranscript(transcript)

		if len(got) > MaxTranscriptBytes {
			t.Fatalf("kept %d bytes over the %d cap", len(got), MaxTranscriptBytes)
		}

		if len(transcript) <= MaxTranscriptBytes {
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

// TestTranscriptUnderTheCapIsStoredVerbatim: the common case must be untouched.
// A truncation path that rewrites every transcript would be a silent change to
// what a node page shows for conversations that were never too big.
func TestTranscriptUnderTheCapIsStoredVerbatim(t *testing.T) {
	t.Parallel()

	exactly := transcriptJSON(MaxTranscriptBytes - len(transcriptJSON(0)))

	for _, original := range []string{transcriptJSON(1_024), exactly} {
		if got := TruncateTranscript(original); got != original {
			t.Errorf("a transcript of %d bytes, at most the cap, was rewritten to %d", len(original), len(got))
		}
	}
}

// TestTranscriptTruncationKeepsAsMuchAsFits: the head kept is the longest run of whole events the cap has room for beside the notice. A budget off by a few bytes passes every check that only asks for "under the cap".
func TestTranscriptTruncationKeepsAsMuchAsFits(t *testing.T) {
	t.Parallel()

	// One-byte events after the first, so each one more costs exactly two; two widths of the first, so one of them fills the cap to the byte.
	for _, first := range []string{"1", "12"} {
		events := []string{first}
		for len(events) < MaxTranscriptBytes {
			events = append(events, "1")
		}

		got := TruncateTranscript("[" + strings.Join(events, ",") + "]")

		if !json.Valid([]byte(got)) || !strings.HasPrefix(got, "["+first+",1,") || !strings.HasSuffix(got, ","+TruncationEvent+"]") {
			t.Fatalf("first event %s: truncated to %.60q…, want the head, then the notice, as one array", first, got)
		}

		if len(got) > MaxTranscriptBytes || len(got) < MaxTranscriptBytes-1 {
			t.Errorf("first event %s: kept %d bytes, want %d or one less — one more event costs two", first, len(got), MaxTranscriptBytes)
		}
	}
}

// TestTranscriptTruncationSurvivesNonJSON: the cap is a storage bound, not a
// validator. Something that is not a JSON array still has to come back under the
// limit rather than panic or grow.
func TestTranscriptTruncationSurvivesNonJSON(t *testing.T) {
	t.Parallel()

	// Multi-byte runes on purpose: a byte-offset cut can split one in half, which
	// is how the first version could produce invalid UTF-8 as well as invalid JSON.
	got := TruncateTranscript(strings.Repeat("日本語", MaxTranscriptBytes))

	if len(got) > MaxTranscriptBytes {
		t.Errorf("non-JSON transcript stored %d bytes, want at most %d", len(got), MaxTranscriptBytes)
	}

	if !utf8.ValidString(got) {
		t.Error("truncation split a multi-byte rune, leaving invalid UTF-8")
	}
}

// transcriptJSON is storetest.TranscriptJSON, which this package cannot
// import: storetest imports it.
func transcriptJSON(size int) string {
	encoded, err := json.Marshal([]map[string]string{{"type": "text", "text": strings.Repeat("t", size)}})
	if err != nil {
		panic(err)
	}

	return string(encoded)
}
