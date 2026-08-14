package main

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from the root package's e2e
// tests, which drive the full CLI stack: workspaces, agent conversations,
// resource fetches, and (opt-in) Docker/btrfs backends. A leak here usually
// means a spawned process, watcher, or HTTP client wasn't shut down before
// the test returned.
//
// streamableServerConn.Read is ignored: it's the go-sdk's server-side
// streamable-HTTP read loop, which does not exit when a client sends the
// session-termination DELETE (verified in isolation, both with and without
// the client's standalone SSE stream — go-sdk@v1.7.0, no fix in a later
// release as of this writing). Not steps' leak to fix.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}
