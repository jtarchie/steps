package sqlite

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from the driver's tests. Every
// handle owns a database/sql pool, and a pool runs a goroutine until Close —
// so a test that forgets a Close, in this package or in the conformance suite
// it runs, fails here instead of leaking a file handle quietly.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
