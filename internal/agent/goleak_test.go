package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from agent tests. clibridge
// streams a CLI subprocess's stdout/stderr on background goroutines; tests
// driving it must let the process exit and those goroutines drain before
// the test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
