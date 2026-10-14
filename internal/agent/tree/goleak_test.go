package tree

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from tree tests: the container half runs through a shell.Runner, whose docker session streams on background goroutines that must be gone when a test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
