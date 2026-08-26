package main

import (
	"os"
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
	// A local: worker execs `<this binary> _shim`, and under `go test` this
	// binary is the test binary — which answers to nothing but the suite, so
	// without this a placed step would re-run the whole suite as a subprocess
	// instead of serving one session. Dispatching before goleak and before
	// m.Run is what makes the documented example an example that runs.
	//
	// The os/exec TestHelperProcess pattern: same binary, told which half to
	// be.
	if len(os.Args) > 1 && os.Args[1] == "_shim" {
		// One impersonated worker, for the eviction e2e: environment rather
		// than argv, because the venue execs a fixed "<binary> _shim" — the
		// same seam the venue package's own variants use.
		if count := os.Getenv(drainingWorkerEnv); count != "" {
			serveEvictedWorker(count)
		}

		err := run(os.Args[1:])
		if err != nil {
			os.Exit(1)
		}

		os.Exit(0)
	}

	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("github.com/modelcontextprotocol/go-sdk/mcp.(*streamableServerConn).Read"))
}
