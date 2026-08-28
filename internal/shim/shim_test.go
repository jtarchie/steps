package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// peer drives a shim over a pair of pipes, standing in for the orchestrator.
// It is deliberately not the real client — that lands with the venue — so
// these tests pin the protocol as specified rather than as one implementation
// happens to speak it.
type peer struct {
	t       *testing.T
	encoder *wire.Encoder
	decoder *wire.Decoder
	served  chan error
	op      uint32

	// waitOnce memoizes the served result: a test that waits for the session
	// to end itself must not leave the cleanup blocking on a channel nobody
	// will write to again.
	waitOnce sync.Once
	waitErr  error
}

func newPeer(t *testing.T, opts Options) *peer {
	t.Helper()

	toShim, fromPeer := io.Pipe()
	toPeer, fromShim := io.Pipe()

	p := &peer{
		t:       t,
		encoder: wire.NewEncoder(fromPeer),
		decoder: wire.NewDecoder(toPeer),
		served:  make(chan error, 1),
	}

	go func() { p.served <- Serve(context.Background(), toShim, fromShim, opts) }()

	t.Cleanup(func() {
		_ = fromPeer.Close()
		_ = p.wait()
		_ = toPeer.Close()
	})

	return p
}

// wait blocks until the served session returns, and answers the same way
// however many times it is asked.
func (p *peer) wait() error {
	p.waitOnce.Do(func() {
		select {
		case p.waitErr = <-p.served:
		case <-time.After(10 * time.Second):
			p.waitErr = errors.New("Serve did not return")
		}
	})

	return p.waitErr
}

// next advances the operation counter, as a real client does per operation.
func (p *peer) next() uint32 {
	p.op++

	return p.op
}

func (p *peer) send(frameType wire.FrameType, op uint32, payload any) {
	p.t.Helper()

	err := p.encoder.WriteJSON(frameType, op, payload)
	if err != nil {
		p.t.Fatalf("sending a type %d frame: %v", frameType, err)
	}
}

// sendRaw writes a frame whose payload is bytes rather than JSON — the
// stdout/data/docker frames, which carry no encoding of their own.
func (p *peer) sendRaw(frameType wire.FrameType, op uint32, payload []byte) {
	p.t.Helper()

	err := p.encoder.Write(wire.Frame{Type: frameType, Op: op, Payload: payload})
	if err != nil {
		p.t.Fatalf("writing a %v frame: %v", frameType, err)
	}
}

func (p *peer) sendEmpty(frameType wire.FrameType, op uint32) {
	p.t.Helper()

	err := p.encoder.Write(wire.Frame{Type: frameType, Op: op})
	if err != nil {
		p.t.Fatalf("sending a type %d frame: %v", frameType, err)
	}
}

// readAny reads a frame without treating FrameError as fatal, for the tests
// whose subject IS the error frame.
func (p *peer) readAny() wire.Frame {
	p.t.Helper()

	frame, err := p.decoder.Read()
	if err != nil {
		p.t.Fatalf("reading a frame: %v", err)
	}

	return frame
}

func (p *peer) read() wire.Frame {
	p.t.Helper()

	frame, err := p.decoder.Read()
	if err != nil {
		p.t.Fatalf("reading a frame: %v", err)
	}

	if frame.Type == wire.FrameError {
		var wireErr wire.Error
		_ = wire.DecodeJSON(frame, &wireErr)
		p.t.Fatalf("the shim reported an error: %s", wireErr.Message)
	}

	return frame
}

// hello opens the session and returns the shim's answer.
func (p *peer) hello() wire.HelloOK {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameHello, op, wire.Hello{Protocol: wire.Protocol, Build: "test", Session: "session-under-test"})

	var ok wire.HelloOK

	err := wire.DecodeJSON(p.read(), &ok)
	if err != nil {
		p.t.Fatalf("decoding the hello answer: %v", err)
	}

	return ok
}

// exec runs one command and collects everything it produced.
func (p *peer) exec(command string, env map[string]string) (stdout, stderr string, exit wire.Exit) {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameExec, op, wire.Exec{Command: command, Env: env})

	var out, errOut strings.Builder

	for {
		frame := p.read()

		switch frame.Type { //nolint:exhaustive // a stand-in shim answers only the frames its test sends
		case wire.FrameStdout:
			out.Write(frame.Payload)
		case wire.FrameStderr:
			errOut.Write(frame.Payload)
		case wire.FrameExit:
			decodeErr := wire.DecodeJSON(frame, &exit)
			if decodeErr != nil {
				p.t.Fatalf("decoding the exit: %v", decodeErr)
			}

			return out.String(), errOut.String(), exit
		case wire.FrameHello, wire.FrameHelloOK, wire.FrameUpload, wire.FrameExec,
			wire.FrameFetch, wire.FrameData, wire.FrameEnd, wire.FrameCancel,
			wire.FrameError, wire.FrameBye, wire.FrameDraining:
			p.t.Fatalf("unexpected type %d frame while running a command", frame.Type)
		}
	}
}

// TestShimRunsACommand is the happy path the whole venue rests on: a tree
// goes out, a command runs against it, and its outputs come back.
func TestShimRunsACommand(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	peer := newPeer(t, Options{Build: "test", Root: root})

	ok := peer.hello()
	if ok.Protocol != wire.Protocol {
		t.Fatalf("protocol = %d, want %d", ok.Protocol, wire.Protocol)
	}

	if ok.Workdir == "" {
		t.Fatal("the shim reported no work directory")
	}

	stdout, _, exit := peer.exec("echo hello", nil)

	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want a clean start and code 0", exit)
	}

	if stdout != "hello\n" {
		t.Errorf("stdout = %q, want %q", stdout, "hello\n")
	}
}

// TestShimReportsANonzeroExitAsData is the distinction the orchestrator turns
// back into a step failure rather than an infrastructure error.
func TestShimReportsANonzeroExitAsData(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	stdout, stderr, exit := peer.exec("echo out; echo err >&2; exit 3", nil)

	if !exit.Started {
		t.Error("Started = false, want true — the command ran, it just failed")
	}

	if exit.Code != 3 {
		t.Errorf("Code = %d, want 3", exit.Code)
	}

	if stdout != "out\n" || stderr != "err\n" {
		t.Errorf("streams = %q / %q, want %q / %q", stdout, stderr, "out\n", "err\n")
	}
}

// TestShimReportsACommandThatNeverStarted covers the half that must NOT look
// like a verdict. A missing interpreter is infrastructure; reporting it as an
// exit code would let it read as the command's own answer.
func TestShimReportsACommandThatNeverStarted(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	ok := peer.hello()

	// Remove the work directory out from under the shim: os/exec then fails to
	// chdir, before the process exists at all, which is the shape of every
	// never-started failure.
	err := os.RemoveAll(ok.Workdir)
	if err != nil {
		t.Fatalf("removing the work directory: %v", err)
	}

	_, _, exit := peer.exec("echo unreachable", nil)

	if exit.Started {
		t.Error("Started = true for a command that could not launch, want false")
	}

	if exit.Reason == "" {
		t.Error("Reason is empty; a never-started command has to say why")
	}
}

// TestShimRoundTripsATree covers both bulk directions at once: a tree is
// uploaded, a command changes it, and only the named outputs come back.
func TestShimRoundTripsATree(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	ok := peer.hello()

	// A step's inputs, packed the way the venue will pack them.
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(src, "out"))

	peer.upload(src)

	_, _, exit := peer.exec("cat data/seed.txt > out/report.txt", nil)
	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want success", exit)
	}

	dst := t.TempDir()
	peer.fetch([]string{"out"}, dst)

	got := mustRead(t, filepath.Join(dst, "out", "report.txt"))
	if got != "seed\n" {
		t.Errorf("fetched report = %q, want %q", got, "seed\n")
	}

	// The input was not asked for, so it must not have travelled back: that is
	// the difference between a feature that works and one anybody uses.
	_, err := os.Stat(filepath.Join(dst, "data"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Error("an input came back that was never requested")
	}

	// And the command really ran where the shim said it did.
	_, err = os.Stat(filepath.Join(ok.Workdir, "out", "report.txt"))
	if err != nil {
		t.Errorf("the output is not in the reported work directory: %v", err)
	}
}

// TestShimPassesEnvValues pins that a variable resolved on the orchestrator
// reaches the command, and that the worker's own baseline is still there —
// PATH has to come from the machine the command runs on, not from whichever
// machine scheduled it.
func TestShimPassesEnvValues(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	stdout, _, exit := peer.exec(`echo "$STEPS_TEST_VALUE"; test -n "$PATH" && echo path-present`,
		map[string]string{"STEPS_TEST_VALUE": "from-the-orchestrator"})

	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want success", exit)
	}

	if !strings.Contains(stdout, "from-the-orchestrator") {
		t.Errorf("stdout = %q, want the supplied value", stdout)
	}

	if !strings.Contains(stdout, "path-present") {
		t.Error("PATH did not reach the command; the baseline must come from the worker")
	}
}

// TestShimCleansUpItsScratch pins the promise that makes a venue safe to point
// at someone else's machine.
func TestShimCleansUpItsScratch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	peer := newPeer(t, Options{Build: "test", Root: root})
	ok := peer.hello()

	peer.sendEmpty(wire.FrameBye, peer.next())

	err := peer.wait()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}

	_, err = os.Stat(ok.Workdir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the work directory outlived the session: %v", err)
	}
}

// TestShimCancelsARunningCommand pins that the read loop keeps reading while a
// command runs.
//
// It is a regression test with a specific bug behind it: the shim first ran
// commands inline, which meant a cancel frame could not be read until the
// command it was meant to stop had already finished. Nothing failed loudly —
// the command simply ran to completion, so a cancelled step took as long as
// whatever it started, and every feature built on cancellation (timeouts,
// fail_fast, race:, Ctrl-C) silently did nothing to a placed step.
//
// The cancel is sent only once the command has PROVED it is running, because
// that is the case that regressed. A cancel racing the launch is a different
// path, handled upstream by the context rather than by the exit frame.
func TestShimCancelsARunningCommand(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	op := peer.next()
	peer.send(wire.FrameExec, op, wire.Exec{Command: "echo running; sleep 60"})

	// Wait for the command to say it is alive. Until this arrives, a cancel
	// would be racing the launch instead of interrupting the work.
	frame := peer.read()
	if frame.Type != wire.FrameStdout || !strings.Contains(string(frame.Payload), "running") {
		t.Fatalf("expected the command to announce itself, got a type %d frame", frame.Type)
	}

	peer.sendEmpty(wire.FrameCancel, op)

	done := make(chan wire.Exit, 1)

	go func() {
		for {
			next, err := peer.decoder.Read()
			if err != nil {
				close(done)

				return
			}

			if next.Type == wire.FrameExit {
				var exit wire.Exit
				_ = wire.DecodeJSON(next, &exit)
				done <- exit

				return
			}
		}
	}()

	select {
	case exit, ok := <-done:
		if !ok {
			t.Fatal("the session ended without reporting the cancelled command")
		}

		if !exit.Started {
			t.Error("Started = false, want true — the command was running, it was just cut short")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled command kept running: the shim could not hear a cancel while a command was in flight")
	}
}

// TestShimIgnoresACancelForAnotherOperation pins the op check. A cancel races
// the exit it was trying to prevent, so one aimed at a command that already
// finished must not kill the command that started after it.
func TestShimIgnoresACancelForAnotherOperation(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})
	peer.hello()

	// A cancel for an operation that never existed, standing in for one that
	// arrived late.
	peer.sendEmpty(wire.FrameCancel, 9999)

	stdout, _, exit := peer.exec("echo survived", nil)

	if !exit.Started || exit.Code != 0 {
		t.Fatalf("exit = %+v, want the command to have run untouched", exit)
	}

	if stdout != "survived\n" {
		t.Errorf("stdout = %q, want the command to have completed", stdout)
	}
}

// TestShimRefusesAMismatchedProtocol: the shim is the binary the orchestrator
// pushed, so a version difference means somebody is pointing at a stale or
// foreign one. Saying so beats degrading.
func TestShimRefusesAMismatchedProtocol(t *testing.T) {
	t.Parallel()

	peer := newPeer(t, Options{Build: "test", Root: t.TempDir()})

	op := peer.next()
	peer.send(wire.FrameHello, op, wire.Hello{Protocol: wire.Protocol + 1, Build: "other", Session: "mismatch"})

	frame, err := peer.decoder.Read()
	if err != nil {
		t.Fatalf("reading the answer: %v", err)
	}

	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %d, want an error frame", frame.Type)
	}
}

// upload sends a tree the way the venue will.
// offerArtifact names one artifact and returns its operation once the shim
// has asked for the bytes — the tunnel's upload handshake, for tests that
// then write the payload themselves.
func (p *peer) offerArtifact(name string) uint32 {
	p.t.Helper()

	op := p.next()
	digest := digestOf(name)

	p.send(wire.FrameUpload, op, wire.Upload{
		Artifacts: []wire.UploadArtifact{{Name: name, Digest: digest}},
	})

	answer := p.read()
	if answer.Type != wire.FrameNeed {
		p.t.Fatalf("expected the shim to ask for %q, got a type %d frame", name, answer.Type)
	}

	return op
}

// upload offers one artifact and sends it if the shim asks, which is the
// tunnel's grain: the orchestrator names an artifact by digest and the worker
// answers whether it needs the bytes.
func (p *peer) upload(src string) {
	p.t.Helper()

	entries, err := os.ReadDir(src)
	if err != nil {
		p.t.Fatalf("reading %q: %v", src, err)
	}

	for _, entry := range entries {
		p.uploadArtifact(src, entry.Name())
	}
}

// uploadArtifact offers one named entry, keyed by a digest of its own name so
// two different artifacts never collide and the same one always repeats.
func (p *peer) uploadArtifact(src, name string) {
	p.t.Helper()

	op := p.next()
	digest := digestOf(name)

	p.send(wire.FrameUpload, op, wire.Upload{
		Artifacts: []wire.UploadArtifact{{Name: name, Digest: digest}},
	})

	answer := p.read()
	if answer.Type == wire.FrameEnd {
		// Already held by this worker: nothing to send.
		return
	}

	if answer.Type != wire.FrameNeed {
		p.t.Fatalf("expected the shim to ask for the artifact, got a type %d frame", answer.Type)
	}

	writer := p.dataWriter(op)

	err := wire.PackPaths(writer, src, []string{name})
	if err != nil {
		p.t.Fatalf("packing %q: %v", name, err)
	}

	err = writer.flush()
	if err != nil {
		p.t.Fatalf("flushing the upload: %v", err)
	}

	p.sendEmpty(wire.FrameEnd, op)

	// The shim acknowledges a tree it accepted, so the far end can tell a
	// refusal from a reply that has not arrived yet.
	if frame := p.read(); frame.Type != wire.FrameEnd || frame.Op != op {
		p.t.Fatalf("expected the upload to be acknowledged, got a type %d frame for operation %d", frame.Type, frame.Op)
	}
}

// fetch asks for named subtrees and unpacks them into dst.
func (p *peer) fetch(paths []string, dst string) {
	p.t.Helper()

	op := p.next()
	p.send(wire.FrameFetch, op, wire.Fetch{Paths: paths})

	reader, writer := io.Pipe()

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		err := wire.UnpackTree(reader, dst)
		_ = reader.CloseWithError(err)
	}()

	for {
		frame := p.read()
		if frame.Type == wire.FrameEnd {
			break
		}

		if frame.Type != wire.FrameData {
			p.t.Fatalf("unexpected type %d frame during a fetch", frame.Type)
		}

		_, err := writer.Write(frame.Payload)
		if err != nil {
			p.t.Fatalf("unpacking a fetch: %v", err)
		}
	}

	_ = writer.Close()

	wg.Wait()
}

type peerDataWriter struct {
	peer *peer
	op   uint32
	buf  []byte
}

func (p *peer) dataWriter(op uint32) *peerDataWriter {
	return &peerDataWriter{peer: p, op: op}
}

func (w *peerDataWriter) Write(b []byte) (int, error) {
	w.buf = append(w.buf, b...)

	for len(w.buf) >= wire.DataChunkBytes {
		err := w.peer.encoder.Write(wire.Frame{Type: wire.FrameData, Op: w.op, Payload: w.buf[:wire.DataChunkBytes]})
		if err != nil {
			return 0, fmt.Errorf("%w", err)
		}

		w.buf = w.buf[wire.DataChunkBytes:]
	}

	return len(b), nil
}

func (w *peerDataWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}

	err := w.peer.encoder.Write(wire.Frame{Type: wire.FrameData, Op: w.op, Payload: w.buf})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatalf("mkdir for %q: %v", path, err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()

	err := os.MkdirAll(path, 0o750)
	if err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path) //nolint:gosec // a path this test just built
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}

	return string(content)
}
