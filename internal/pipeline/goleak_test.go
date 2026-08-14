package pipeline

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from pipeline tests. across,
// ensemble, and parallel all fan work out with `go func()`; tests driving
// them must let every worker observe context cancellation and return before
// the test itself returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
