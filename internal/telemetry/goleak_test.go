package telemetry

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from telemetry tests. A tracer provider's batch processor runs on a background goroutine until Shutdown.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
