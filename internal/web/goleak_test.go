package web

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from web tests. The run watcher
// and the SSE broadcaster both run on background goroutines; tests must
// close the server (or cancel its context) so those goroutines exit before
// the test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
