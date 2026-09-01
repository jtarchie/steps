package venue

import (
	ctx "context"
	"fmt"
	"net"
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
		serveShim()
	}

	goleak.VerifyTestMain(m, sdkPoolIgnores()...)
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

// serveShim is the far half of a local: venue, told by environment which
// worker to impersonate: environment rather than argv because the venue execs
// a fixed "<binary> _shim". It never returns.
func serveShim() {
	variants := []struct {
		env   string
		serve func()
	}{
		// A shim that refuses the tree, so a test can drive the path where a
		// worker rejects an upload — and its break-the-fetch sibling.
		{rejectUploadEnv, serveRejectingShim},
		{breakFetchEnv, serveRejectingShim},
		// A shim that stops answering once a command starts, so a test can
		// drive the path where a worker wedges mid-step.
		{deafExecEnv, serveDeafShim},
		// A shim that is not the binary that was pushed.
		{wrongBuildEnv, serveWrongBuildShim},
		// A shim on a filesystem that cannot store an executable bit.
		{windowsWorkerEnv, serveWindowsShim},
		// A shim on an ordinary OS whose workdir cannot store one.
		{lossyFSWorkerEnv, serveLossyFSShim},
		// A shim built before compression existed.
		{legacyShimEnv, serveLegacyShim},
		// A shim that greets and then dies as the tree arrives, once.
		{dieOnUploadEnv, serveUploadDyingShim},
		// A shim that crashes on start, counting how often it was asked to.
		{crashCountEnv, serveCrashingShim},
		// A shim on a machine being reclaimed.
		{drainingShimEnv, serveDrainingShim},
	}

	for _, variant := range variants {
		if os.Getenv(variant.env) != "" {
			variant.serve()
			os.Exit(0)
		}
	}

	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	// A listening shim, when the venue under test asked for one — an aws://
	// bootstrap starts `_shim --listen`, and a helper that only ever spoke
	// stdio would leave that whole path untested.
	if address, once, root, listening := listenArgs(); listening {
		listener, listenErr := (&net.ListenConfig{}).Listen(ctx.Background(), "tcp", address)
		if listenErr != nil {
			os.Exit(1)
		}

		// The same line the real command prints, which is what the bootstrap
		// script greps for.
		fmt.Printf("listening on %s\n", listener.Addr())

		serveErr := shim.ServeListener(ctx.Background(), listener,
			shim.ListenOptions{Options: shim.Options{Build: build, Root: root}, Once: once})
		if serveErr != nil {
			os.Exit(1)
		}

		os.Exit(0)
	}

	err = shim.Serve(ctx.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

// listenArgs reads the listen flags off argv, the way the real command's kong
// parser would.
func listenArgs() (address string, once bool, root string, listening bool) {
	args := os.Args[2:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			if i+1 < len(args) {
				address, listening = args[i+1], true
				i++
			}
		case "--once":
			once = true
		case "--root":
			if i+1 < len(args) {
				root = args[i+1]
				i++
			}
		}
	}

	return address, once, root, listening
}
