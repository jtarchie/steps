package pipeline

import (
	"strings"
	"testing"
)

// isHeaderSafeRune mirrors the unreserved set newSessionID is allowed to emit,
// so a session id is always a legal HTTP header value.
func isHeaderSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}

func TestNewSessionID(t *testing.T) {
	t.Parallel()

	t.Run("carries the job name and a unique suffix", func(t *testing.T) {
		t.Parallel()

		first := newSessionID("build")
		if !strings.HasPrefix(first, "build-") {
			t.Errorf("got %q, want a %q prefix", first, "build-")
		}

		second := newSessionID("build")
		if first == second {
			t.Errorf("two runs of the same job produced the same session id %q", first)
		}
	})

	t.Run("stays inside the cap agent.WithSessionID enforces", func(t *testing.T) {
		t.Parallel()

		// agent.WithSessionID drops an id longer than OpenRouter's 256-char
		// cap outright, silently disabling caching — so no job name, however
		// long, may be able to push the generated id past it.
		const openRouterCap = 256

		got := newSessionID(strings.Repeat("long-job-name", 100))
		if len(got) > openRouterCap {
			t.Errorf("session id is %d chars, over the %d cap: %q", len(got), openRouterCap, got)
		}
	})

	t.Run("drops characters that are invalid in an HTTP header", func(t *testing.T) {
		t.Parallel()

		// Job names are free-form YAML: spaces and non-ASCII are legal there
		// but not in a header value.
		got := newSessionID("deploy to staging ✨")

		for _, r := range got {
			if !isHeaderSafeRune(r) {
				t.Errorf("session id %q contains an unsafe rune %q", got, r)
			}
		}
	})

	t.Run("an empty job name still yields a usable id", func(t *testing.T) {
		t.Parallel()

		got := newSessionID("")
		if strings.TrimPrefix(got, "-") == "" {
			t.Errorf("got %q, want a non-empty random suffix", got)
		}
	})
}
