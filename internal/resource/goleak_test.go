package resource

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from resource tests. The expr
// backend fans an http() batch out over one goroutine per request, and both
// the expr and mcp backends hold HTTP clients — a leak here means a worker
// outliving its batch or a transport nobody released.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
