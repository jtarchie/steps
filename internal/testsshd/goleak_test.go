package testsshd_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from testsshd tests. Every
// connection, channel and forward runs on its own goroutine, and one that
// outlived its client would hang the Cleanup of every test that uses it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
