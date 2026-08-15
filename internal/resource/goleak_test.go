package resource

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from resource tests. The expr
// backend fans an http() batch out over one goroutine per request, and both
// the expr and mcp backends hold HTTP clients — a leak here means a worker
// outliving its batch or a transport nobody released.
//
// streamableServerConn.Read is ignored: it's the go-sdk's server-side
// streamable-HTTP read loop, which does not exit when a client sends the
// session-termination DELETE (go-sdk@v1.7.0). This package's mcp tests run
// those servers, so it inherits the same exception the root package carries.
// Not steps' leak to fix.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}
