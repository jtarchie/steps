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

// lossyFSWorkerEnv makes the helper-process shim answer as the worker the
// GOOS check cannot see: an ordinary Linux machine whose WORKDIR is on a
// filesystem that cannot hold 0111.
const lossyFSWorkerEnv = "STEPS_TEST_LOSSY_FS_WORKER"

// serveLossyFSShim is a correct shim, of the right build, running on linux,
// that measured its own workdir and found it cannot store an executable bit —
// the /mnt/c-on-WSL2 worker, and the vfat/CIFS/9p ones with it.
func serveLossyFSShim() {
	build, err := shim.SelfBuild()
	if err != nil {
		os.Exit(1)
	}

	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)
	lossy := false

	for {
		frame, err := decoder.Read()
		if err != nil {
			return
		}

		if frame.Type == wire.FrameHello {
			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol,
				Build:    build,
				GOOS:     "linux",
				GOARCH:   "amd64",
				Workdir:  "/mnt/c/scratch",
				ExecBit:  &lossy,
			})
		}
	}
}

// TestVenueRefusesAWorkerWhoseFilesystemCannotHoldAnExecutableBit is the case
// the GOOS check was named for and never covered.
//
// `ssh://user@host/mnt/c/scratch` on WSL2 reports GOOS=linux and sails
// through, while /mnt/c is DrvFs over NTFS and cannot round-trip 0111 — the
// exact silent loss errLossyWorker exists to prevent, on a worker docs/infra.md
// called unaffected. ?root= aimed at vfat, exfat, CIFS without unix
// extensions, or 9p/virtiofs under Lima is the same worker with a different
// spelling.
//
// Driven end to end rather than against checkHello, because the bug this
// replaces was a comment saying FILESYSTEM over code asking about the
// OPERATING SYSTEM: the assertion that matters is that a session refuses a
// machine whose OS is fine, which only a running handshake can show.
func TestVenueRefusesAWorkerWhoseFilesystemCannotHoldAnExecutableBit(t *testing.T) {
	t.Setenv(lossyFSWorkerEnv, "1")

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	// Deadlined for the reason the windows case is: a shim that answers only
	// the handshake leaves a session that got past it blocking on an upload
	// nobody will acknowledge.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := runner.Run(ctx, "true")
	if err == nil {
		t.Fatal("a worker that measured its filesystem as unable to store an executable bit ran the step anyway")
	}

	if !errors.Is(err, errLossyWorker) {
		t.Fatalf("error = %v, want it to name the worker's filesystem", err)
	}

	// The workdir, because that is the thing to act on here and the OS is
	// not: the machine is fine and the answer is to aim --worker's path at a
	// different directory on it.
	if !strings.Contains(err.Error(), "/mnt/c/scratch") {
		t.Errorf("error = %v, want it to say which directory was refused", err)
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

	holds, lossy := true, false

	for _, tc := range []struct {
		name    string
		goos    string
		execBit *bool
		refused bool
	}{
		{"a shim that names no OS", "", nil, false},
		{"an ordinary worker", "linux", nil, false},
		{"a worker with nowhere to put an executable bit", "windows", nil, true},
		// The measurement, which is the answer when there is one.
		{"a worker that measured its filesystem and can", "linux", &holds, false},
		{"a worker that measured its filesystem and cannot", "linux", &lossy, true},
		// Both halves are consulted, and either one refuses: a windows shim
		// reporting that it CAN hold the bit is answering about a chmod that
		// silently sets the read-only attribute and a Stat that synthesizes
		// 0111 back for directories, so the measurement is the one thing on
		// that platform that cannot be trusted to disagree usefully. Failing
		// closed costs nothing real — a shim built from this tree measures
		// false there — and the alternative is a single wrong answer
		// unlocking the platform the check was written for.
		{"a windows worker that claims it can", "windows", &holds, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			session := &session{worker: Worker{URL: "ssh://box"}} //nolint:exhaustruct // checkHello reads the URL and nothing else

			err := session.checkHello(wire.HelloOK{ //nolint:exhaustruct // the handshake fields under test
				Protocol: wire.Protocol,
				GOOS:     tc.goos,
				ExecBit:  tc.execBit,
				Workdir:  os.TempDir(),
			}, "")

			if refused := errors.Is(err, errLossyWorker); refused != tc.refused {
				t.Errorf("refused = %v (err %v), want %v", refused, err, tc.refused)
			}
		})
	}
}
