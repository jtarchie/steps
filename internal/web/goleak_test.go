package web

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from web tests. The run watcher
// and the SSE broadcaster both run on background goroutines; tests must
// close the server (or cancel its context) so those goroutines exit before
// the test returns.
//
// One third-party exception, and it is bounded rather than leaked: chroma's
// lexers (prose.go, for the fenced code inside an agent's response) match with
// regexp2, whose match-timeout support runs a shared background clock. Read
// fastclock.go in regexp2 v2.6.0: runClock loops only while `current <=
// clockEnd` and then clears `running` and returns, and every match extends
// clockEnd by about a second — so the goroutine exits on its own shortly after
// the last match, and there is no handle to close it sooner. goleak simply
// samples inside that window. Re-check when regexp2 moves past v2.6.0.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/dlclark/regexp2/v2.runClock"),
	)
}
