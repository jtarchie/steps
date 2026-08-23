package venue

import (
	ctx "context"
	"os"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/jtarchie/steps/internal/shim"
)

// shortWait is how long a cancellation test lets a command run before cutting
// it off. Long enough that the command has certainly started, short enough
// that a test suite does not wait on it.
const shortWait = 250 * time.Millisecond

// TestMain does two jobs.
//
// A local: venue execs `<this binary> _shim`, and under `go test` this binary
// is the test binary — which answers to nothing but the suite. Dispatching
// here, before goleak and before m.Run, is what lets the venue exercise a real
// shim in a real child process rather than a stub. It is the os/exec
// TestHelperProcess pattern: the same binary, told which half to be.
//
// Then goleak, because a session pumps a command's output and watches for
// cancellation on goroutines, and a session that returned while either was
// still running would strand it.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "_shim" {
		// A shim that refuses the tree, so a test can drive the path where a
		// worker rejects an upload. Selected by environment rather than argv
		// because the venue execs a fixed "<binary> _shim".
		if os.Getenv(rejectUploadEnv) != "" {
			serveRejectingShim()
			os.Exit(0)
		}

		build, err := shim.SelfBuild()
		if err != nil {
			os.Exit(1)
		}

		err = shim.Serve(ctx.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
		if err != nil {
			os.Exit(1)
		}

		os.Exit(0)
	}

	goleak.VerifyTestMain(m)
}
