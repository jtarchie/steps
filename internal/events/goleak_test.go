package events

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from events tests. The bus
// dispatches to subscribers on a background goroutine; tests must
// unsubscribe or let the bus close before returning.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
