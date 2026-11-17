package mcpread

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from mcpread tests: every test serves the tools over the go-sdk's streamable HTTP handler.
//
// streamableServerConn.Read is ignored: the go-sdk's server-side streamable-HTTP read loop, which does not exit when a client sends the session-termination DELETE (seen at go-sdk v1.8.0; see internal/mcp/CLAUDE.md before removing it). Not steps' leak to fix.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}
