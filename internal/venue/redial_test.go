package venue

// What happens when the worker dies mid-conversation.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/shim"
	"github.com/jtarchie/steps/internal/wire"
)

// dieOnUploadEnv makes the helper-process shim greet correctly and then die
// the moment the tree starts arriving — once. The value is a marker file: its
// absence is the first dial, and later dials find it and serve normally, the
// shape of a worker that blipped and recovered.
const dieOnUploadEnv = "STEPS_TEST_DIE_ON_UPLOAD"

// crashCountEnv makes the helper-process shim record its start and exit
// before speaking, so a test can count how many times a session dialled a
// worker that can never open.
const crashCountEnv = "STEPS_TEST_CRASH_COUNT"

// serveUploadDyingShim answers the hello like the real shim — same build,
// real protocol — and exits when the upload begins. The handshake SUCCEEDED,
// which is what makes the death a conversation loss rather than an open
// failure.
func serveUploadDyingShim() {
	marker := os.Getenv(dieOnUploadEnv)

	_, err := os.Stat(marker) //nolint:gosec // a path this test's own env names
	if err == nil {
		// A later dial: the worker has recovered.
		build, buildErr := shim.SelfBuild()
		if buildErr != nil {
			os.Exit(1)
		}

		serveErr := shim.Serve(context.Background(), os.Stdin, os.Stdout, shim.Options{Build: build})
		if serveErr != nil {
			os.Exit(1)
		}

		os.Exit(0)
	}

	_ = os.WriteFile(marker, nil, 0o600) //nolint:gosec // a path this test's own env names

	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, readErr := decoder.Read()
		if readErr != nil {
			os.Exit(1)
		}

		switch frame.Type { //nolint:exhaustive // a stub that dies on the first upload has no opinion about the rest
		case wire.FrameHello:
			build, buildErr := shim.SelfBuild()
			if buildErr != nil {
				os.Exit(1)
			}

			_ = encoder.WriteJSON(wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol, Build: build, Workdir: os.TempDir(),
			})
		case wire.FrameUpload:
			os.Exit(1)
		default:
		}
	}
}

// serveCrashingShim records that it was started and dies before speaking.
func serveCrashingShim() {
	file, err := os.OpenFile(os.Getenv(crashCountEnv), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // a path this test's own env names
	if err == nil {
		_, _ = file.WriteString("x")
		_ = file.Close()
	}

	os.Exit(1)
}

// TestVenueRedialsAfterTheWorkerDies pins the difference between a host that
// could never be reached and a conversation that died after it opened. The
// first sticks — retrying an unreachable host once per command buys nothing
// but timeouts. The second must not: the step's next command is entitled to a
// fresh dial, or attempts: on a placed step retries into a dead pipe and every
// retry fails for a reason the author cannot see.
func TestVenueRedialsAfterTheWorkerDies(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "echo one > out/first.txt")
	if err != nil {
		t.Fatalf("the first command failed: %v", err)
	}

	err = runner.Run(context.Background(), "kill -9 $PPID")
	if err == nil {
		t.Fatal("killing the shim mid-command reported success")
	}

	// Infrastructure, not a verdict: a worker that vanished must never read
	// as the command having run and failed.
	if shell.IsExitError(err) {
		t.Fatalf("a dead worker was reported as a command's exit: %v", err)
	}

	// The next command reaches a fresh shim, and the tree it sees is the
	// LOCAL one — which already holds what the dead worker sent back after
	// the first command, so earlier progress survives the redial.
	err = runner.Run(context.Background(), "cat out/first.txt > out/second.txt")
	if err != nil {
		t.Fatalf("the command after the worker died did not reach a fresh shim: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "second.txt"))
	if got != "one\n" {
		t.Errorf("out/second.txt = %q, want %q — the redialled shim did not see the tree the first command produced", got, "one\n")
	}
}

// TestVenueKeepsTheWorkerAfterAShimError pins the boundary the redial guards:
// an error frame is the shim answering over a healthy transport — an operation
// that failed, not a worker that died. Redialling on it would abandon the
// worker's scratch and re-ship the whole tree for an error a fresh dial cannot
// fix.
func TestVenueKeepsTheWorkerAfterAShimError(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	// A fifo in a declared output makes the shim's fetch refuse: an error
	// frame, with the conversation still open.
	err := runner.Run(context.Background(), "echo kept > scratch.txt; mkfifo out/fifo")
	if err == nil {
		t.Fatal("a fifo in an output was fetched")
	}

	// The next command reaches the SAME shim: its scratch — which the local
	// tree does not hold — is still there, exactly as it is between any two
	// commands of a step.
	err = runner.Run(context.Background(), "rm out/fifo; cat scratch.txt > out/second.txt")
	if err != nil {
		t.Fatalf("the command after a shim error failed: %v", err)
	}

	if got := mustRead(t, filepath.Join(cwd, "out", "second.txt")); got != "kept\n" {
		t.Errorf("out/second.txt = %q, want %q — the worker was redialled and its scratch lost", got, "kept\n")
	}
}

// TestVenueRedialsWhenTheUploadDies pins where the stickiness boundary sits:
// the handshake. A worker that answered its hello and then died while the
// tree was arriving was reachable — the step's next try is entitled to a
// fresh dial, exactly as it is when the death comes mid-command. Sticking
// here converted a mid-upload blip into a permanent failure that attempts:
// could only re-read, never retry.
func TestVenueRedialsWhenTheUploadDies(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "died-once")
	t.Setenv(dieOnUploadEnv, marker)

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err == nil {
		t.Fatal("the first command succeeded against a shim that dies on upload")
	}

	err = runner.Run(context.Background(), "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("the retry did not redial a worker that died mid-upload: %v", err)
	}

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("out/report.txt = %q, want %q", got, "seed\n")
	}
}

// TestVenueAnUnopenableWorkerIsDialledOnce is the other side of that
// boundary, kept from regressing while the redial path exists: a worker that
// cannot OPEN answers the same way every time, and each re-ask costs another
// timeout — so the second command must return the recorded failure without
// spawning a second shim.
func TestVenueAnUnopenableWorkerIsDialledOnce(t *testing.T) {
	count := filepath.Join(t.TempDir(), "dials")
	t.Setenv(crashCountEnv, count)

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	first := runner.Run(context.Background(), "echo unreachable")
	if first == nil {
		t.Fatal("a crashing shim reported success")
	}

	second := runner.Run(context.Background(), "echo unreachable")
	if second == nil {
		t.Fatal("the second command succeeded against a shim that always crashes")
	}

	dials := mustRead(t, count)
	if len(dials) != 1 {
		t.Fatalf("the worker was dialled %d times, want 1 — an open failure must stick for the step", len(dials))
	}
}
