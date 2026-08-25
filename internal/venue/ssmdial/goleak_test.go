package ssmdial

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain holds the package to the no-leaked-goroutines rule: a channel runs
// a read loop and two timers, and one that returned while any was still alive
// would strand it for the life of the process.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
