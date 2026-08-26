package ssmdial

import (
	"os"
	"testing"

	"go.uber.org/goleak"
)

// TestMain holds the package to the no-leaked-goroutines rule: a channel runs
// a read loop and two timers, and one that returned while any was still alive
// would strand it for the life of the process.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, sdkPoolIgnores()...)
}

// sdkPoolIgnores relaxes the leak check for the AWS SDK's own connection
// pool, and ONLY when the real-AWS tests are the ones running.
//
// Those goroutines are net/http's persistConn read and write loops, parked on
// an idle keep-alive connection the SDK holds for reuse; they are the
// stdlib's to reap, not this package's, and nothing here can close them
// without reaching into the SDK's transport. Conditional on the fixture
// rather than unconditional, so the fake-agent tests — which is to say every
// run that does not touch AWS — keep the strict check that has caught real
// leaks in this package's own read loop and timers.
func sdkPoolIgnores() []goleak.Option {
	if os.Getenv("STEPS_TEST_AWS_INSTANCE") == "" {
		return nil
	}

	return []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	}
}
