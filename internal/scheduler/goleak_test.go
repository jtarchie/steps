package scheduler

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from scheduler tests: a drain's
// workers and a retired provider's background close must both have exited by
// the time a test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
