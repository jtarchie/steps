package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from agent tests. clibridge
// streams a CLI subprocess's stdout/stderr on background goroutines; tests
// driving it must let the process exit and those goroutines drain before
// the test returns.
//
// streamableServerConn.Read is ignored: it's the go-sdk's server-side
// streamable-HTTP read loop, which does not exit when a client sends the
// session-termination DELETE (verified in isolation, both with and without
// the client's standalone SSE stream — go-sdk@v1.7.0, no fix in a later
// release as of this writing). Not steps' leak to fix.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}
