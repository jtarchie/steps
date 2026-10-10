package postgres

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzTruncateUTF8 is the sqlite driver's, against this driver's copy: clean() makes every stored transcript valid UTF-8, so the conformance suite never reaches the invalid-byte bound and only a direct call does.
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
