package dockerapi

// Nothing in this package starts a goroutine of its own, but the engine
// client's HTTP transport does, and a client that is never closed leaves an
// idle connection's reader behind. That is the leak worth catching here: it is
// invisible in a short-lived CLI and cumulative in a `steps web` that opens one
// per job.

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
