package cli

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from this package's tests, which
// drive commands that start servers, probe MCP endpoints and open stores.
//
// streamableServerConn.Read is ignored: it's the go-sdk's server-side
// streamable-HTTP read loop, which does not exit when a client sends the
// session-termination DELETE (verified in isolation, both with and without
// the client's standalone SSE stream — go-sdk@v1.7.0). Not steps' leak to
// fix; the same exception appears in every package that talks to it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"),
	)
}
