package shim

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from shim tests. A served session
// pumps a command's stdout and stderr from goroutines os/exec starts, and a
// session that returned while one was still writing would strand it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
