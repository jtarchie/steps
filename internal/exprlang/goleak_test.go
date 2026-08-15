package exprlang

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from exprlang tests. http()
// fans a batch out over one goroutine per request and releases the client's
// idle connections when an expression finishes, so a leak here means either
// a worker outliving its batch or a transport nobody closed.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
