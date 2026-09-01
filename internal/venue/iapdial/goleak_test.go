package iapdial

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain holds the package to the no-leaked-goroutines rule: a channel runs
// a read loop and a ping timer, and one that returned while either was still
// alive would strand it for the life of the process.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, poolIgnores()...)
}

// poolIgnores relaxes the leak check for the HTTP connection pool the token
// fetch and the real relay leave behind, and ONLY when the real-GCP tests are
// the ones running. Those goroutines are net/http's persistConn loops parked
// on an idle keep-alive connection — the stdlib's to reap, not this
// package's. Every fake-relay run keeps the strict check.
func poolIgnores() []goleak.Option {
	if os.Getenv("STEPS_TEST_GCP_INSTANCE") == "" {
		return nil
	}

	return []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	}
}
