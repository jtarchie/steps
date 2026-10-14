package llmclient

import (
	"strings"
	"testing"
)

// TestUnrunSessionsAreDistinct: two concurrent probes sharing one session would be pinned to one upstream instance, which a health check must not be.
func TestUnrunSessionsAreDistinct(t *testing.T) {
	t.Parallel()

	first, second := unrunSessionID(), unrunSessionID()
	if first == second || !strings.HasPrefix(first, "probe-") {
		t.Errorf("unrunSessionID() = %q then %q; want two distinct probe- ids", first, second)
	}
}
