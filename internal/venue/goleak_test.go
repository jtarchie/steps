package venue

import (
	"os"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// shortWait is how long a cancellation test lets a command run before cutting
// it off. Long enough that the command has certainly started, short enough
// that a test suite does not wait on it.
const shortWait = 250 * time.Millisecond

// TestMain checks for leaked goroutines, since a session pumps a command's output and watches its keepalive on goroutines, and gives this process its own docker+ cache namespace.
func TestMain(m *testing.M) {
	// One cache namespace per process: test shards share a daemon, and content-addressed entries would otherwise be shared across them.
	_ = os.Setenv("STEPS_TEST_CACHE_NAMESPACE", strconv.Itoa(os.Getpid())+"-")

	goleak.VerifyTestMain(m, append(sdkPoolIgnores(), goleak.Cleanup(func(code int) {
		removeProcessCache()
		os.Exit(code)
	}))...)
}

// sdkPoolIgnores relaxes the leak check for the cloud SDKs' own connection
// pools, and ONLY when the real-AWS or real-GCP tests are the ones running —
// see the identical note in internal/venue/ssmdial. Every other run keeps
// the strict check, which is what catches this package's own session
// goroutines.
func sdkPoolIgnores() []goleak.Option {
	if os.Getenv("STEPS_TEST_AWS_INSTANCE") == "" && os.Getenv("STEPS_TEST_GCP_INSTANCE") == "" {
		return nil
	}

	return []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		goleak.IgnoreAnyFunction("internal/poll.runtime_pollWait"),
	}
}
