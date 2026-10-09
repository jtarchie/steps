package trigger

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from trigger tests. The poller
// and the MCP watch loop run on background goroutines;
// tests must cancel their context and wait for shutdown before returning.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
