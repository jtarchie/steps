package venue

// A worker whose filesystem cannot hold an executable bit.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shim"
	"github.com/jtarchie/steps/internal/wire"
)

// windowsWorkerEnv makes the helper-process shim answer the handshake as a
// Windows worker. Impersonating one is the only way to test this from any
// machine the suite actually runs on, and it is enough: the refusal reads one
// field of the handshake and nothing else.
const windowsWorkerEnv = "STEPS_TEST_WINDOWS_WORKER"

// serveWindowsShim is a correct shim, of the right build, on a filesystem that
// cannot store 0111.
func serveWindowsShim() {
	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		if frame.Type == wire.FrameHello {
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Build:    build,
				GOOS:     "windows",
				GOARCH:   "amd64",
				Workdir:  os.TempDir(),
			})
		}
	}
}

// TestVenueRefusesAWorkerThatCannotHoldAnExecutableBit is #83's answer turned
// into a refusal.
//
// Such a worker does not fail, disagree, or report anything: it takes the
// tree, silently drops every 0111 its filesystem cannot represent (os.Chmod
// there sets only the read-only attribute and returns no error), and the
// repack on the way home reads that back. The tree returns with its scripts
// no longer executable, the step cache sees content that changed for no
// reason a reader can point at, and the next step that tries to run the file
// cannot.
//
// Refusing rather than warning is the posture the codec already takes one
// package over: PackTree refuses to ship a fifo, because dropping an entry
// would change what digestTree computes over the extracted copy and "a cache
// that quietly disagrees with itself is worse than a step that refuses to
// ship one". This is that hazard exactly, one layer up — and the shim has
// been reporting the fact needed to catch it since the handshake existed,
// into a struct field nobody read.
func TestVenueRefusesAWorkerThatCannotHoldAnExecutableBit(t *testing.T) {
	t.Setenv(windowsWorkerEnv, "1")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	// Deadlined, because the failure mode without the refusal is a WAIT, not
	// a wrong answer: this shim answers the handshake and nothing else, so a
	// session that got past the greeting blocks on the upload it will never
	// be acknowledged for. Without this the whole package hangs to its own
	// timeout instead of reporting which assertion stopped holding.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runner.Run(ctx, "true")
	if err == nil {
		t.Fatal("a worker that cannot store an executable bit ran the step, and would have returned a tree missing them")
	}

	if !errors.Is(err, errLossyWorker) {
		t.Fatalf("error = %v, want it to name the worker's filesystem", err)
	}

	// The operating system, because that is the thing to act on: the answer
	// is to pick a different machine, and an error that named only "the
	// worker" would leave an operator re-reading their flags.
	if !strings.Contains(err.Error(), "windows") {
		t.Errorf("error = %v, want it to say which OS was refused", err)
	}
}

// TestCheckHelloAcceptsAWorkerThatNamesNoOS is the compatibility edge the
// refusal must not swallow. GOOS is a field of the handshake, so a shim that
// leaves it empty — one an operator started by hand over a bare ssh command,
// or any future shim answering a shorter hello — has said nothing about its
// filesystem, and refusing on silence would reject workers that are fine. The
// build check beside it takes the same view of an empty Build.
//
// Asserted against checkHello directly rather than through a running worker,
// because the real shim always reports runtime.GOOS: a test driving it would
// never produce the empty case it is named for, and could not fail however
// the refusal was written.
func TestCheckHelloAcceptsAWorkerThatNamesNoOS(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		goos    string
		refused bool
	}{
		{"a shim that names no OS", "", false},
		{"an ordinary worker", "linux", false},
		{"a worker with nowhere to put an executable bit", "windows", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			session := &session{worker: Worker{URL: "ssh://box"}} //nolint:exhaustruct // checkHello reads the URL and nothing else

			err := session.checkHello(wire.HelloOK{ //nolint:exhaustruct // the handshake fields under test
				Protocol: wire.Protocol,
				GOOS:     tc.goos,
				Workdir:  os.TempDir(),
			}, "")

			if refused := errors.Is(err, errLossyWorker); refused != tc.refused {
				t.Errorf("refused = %v (err %v), want %v", refused, err, tc.refused)
			}
		})
	}
}
