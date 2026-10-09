package cli

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from this package's tests, which
// drive commands that start servers, probe MCP endpoints and open stores.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
