package cli

import "testing"

// TestProgressAutoIsPlainOffATerminal: go test's stdout is not a terminal, which is exactly the case auto exists for — a pipe, a CI log, an agent calling steps must get lines, never cursor movement. The explicit modes win either way.
func TestProgressAutoIsPlainOffATerminal(t *testing.T) {
	t.Parallel()

	for mode, want := range map[string]bool{"auto": false, "plain": false, "tty": true} {
		if got := (ProgressFlags{Progress: mode}).wantsLive(); got != want {
			t.Errorf("--progress=%s live = %v, want %v", mode, got, want)
		}
	}
}
