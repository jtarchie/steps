package shim

// The docker socket carried on the session's connection, proven without a
// docker daemon: what this file forwards is opaque bytes, so a socket that
// echoes them is a complete test of the forwarding.

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// echoSocket serves a unix socket that returns whatever it is sent, standing
// in for the docker daemon.
func echoSocket(t *testing.T) string {
	t.Helper()

	// Short path: a unix socket address has a ~104 byte limit, and t.TempDir
	// under macOS's /var/folders is most of it already.
	path := filepath.Join(t.TempDir(), "s")

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				buffer := make([]byte, 4096)

				for {
					read, readErr := conn.Read(buffer)
					if read > 0 {
						_, _ = conn.Write(buffer[:read])
					}

					if readErr != nil {
						return
					}
				}
			}()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})

	return path
}

// TestDockerStreamCarriesBytesBothWays is the forwarding contract: the
// orchestrator opens a stream, writes, and reads back what the worker's
// socket answered — all on the session's existing connection, which is what
// lets an aws:// worker keep --once and no inbound port.
func TestDockerStreamCarriesBytesBothWays(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir(), DockerSocket: echoSocket(t)})
	peer.hello()

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)
	// The stream is raw bytes, never JSON.
	peer.sendRaw(wire.FrameDockerData, op, []byte("GET /_ping HTTP/1.0\r\n\r\n"))

	frame := peer.read()
	if frame.Type != wire.FrameDockerData {
		t.Fatalf("frame type = %v, want FrameDockerData", frame.Type)
	}

	if string(frame.Payload) == "" {
		t.Error("the stream carried nothing back")
	}

	peer.sendEmpty(wire.FrameDockerClose, op)
}

// TestDockerStreamReportsAWorkerWithNoDaemon pins that a worker without
// docker fails the STEP rather than the session: a machine with no daemon is
// a perfectly good worker for every step that names no image.
func TestDockerStreamReportsAWorkerWithNoDaemon(t *testing.T) {
	peer := newPeer(t, Options{
		Build: "test", Root: t.TempDir(),
		DockerSocket: filepath.Join(t.TempDir(), "absent.sock"),
	})
	peer.hello()

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)

	frame := peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v, want FrameError for a worker with no docker socket", frame.Type)
	}

	var reported wire.Error

	err := wire.DecodeJSON(frame, &reported)
	if err != nil {
		t.Fatalf("decoding the error: %v", err)
	}

	if reported.Message == "" {
		t.Error("the error named nothing an author could act on")
	}
}

// TestDockerNeedsAHello pins that the one frame handing out a raw proxy
// to a root docker daemon is behind the handshake, like every other operation.
//
// upload, fetch and exec all refuse an operation that arrives before a hello.
// The docker family did not, so the protocol-version check, the build check
// and checkSessionName could all be skipped by sending this first — on a
// listener that is unauthenticated by design and, under the aws:// bootstrap,
// running as root.
func TestDockerNeedsAHello(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir(), DockerSocket: echoSocket(t)})

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)

	frame := peer.readAny()
	if frame.Type != wire.FrameError {
		t.Fatalf("frame type = %v, want a refusal for a docker stream opened before the hello", frame.Type)
	}
}

// TestDockerRefusesASecondOpen is what a fire-and-forget frame owes its peer
// when it cannot do as it was asked.
//
// add() has always refused a duplicate; what was missing was saying so.
// FrameDockerOpen gets no acknowledgement, so silence is indistinguishable from
// success — and the peer's next FrameDockerData for that op then resolves to the
// FIRST stream, writing one client's bytes into another client's socket.
func TestDockerRefusesASecondOpen(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir(), DockerSocket: echoSocket(t)})
	peer.hello()

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)
	peer.sendEmpty(wire.FrameDockerOpen, op)

	// Read under a bound, because SILENCE is the answer under test: a plain
	// read would hang on the defect rather than report it.
	answered := make(chan wire.FrameType, 1)

	go func() {
		frame, err := peer.decoder.Read()
		if err == nil {
			answered <- frame.Type
		}
	}()

	select {
	case frameType := <-answered:
		if frameType != wire.FrameError {
			t.Fatalf("frame type = %v for a second open on operation %d, want a refusal", frameType, op)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a second open on an operation already open was answered with silence, which reads as success")
	}
}

// TestDockerHalfClose is the bug that made every placed containerized step
// come home empty.
//
// A docker connection is not a pipe with one end: the CLI shuts down its WRITE
// side as soon as it has no stdin left to send — the hijacked exec stream does
// it immediately — while the daemon is still writing the container's output
// back. Treating that signal as a teardown closed the socket, so the command
// reported success with nothing on stdout and the fetch that followed found a
// tree the container had not finished writing.
//
// The double answers only AFTER it sees end-of-request, which is the shape
// that tells the two behaviours apart: a full close loses the answer entirely.
func TestDockerHalfClose(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir(), DockerSocket: lateAnswerSocket(t)})
	peer.hello()

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)
	peer.sendRaw(wire.FrameDockerData, op, []byte("GET /_ping HTTP/1.0\r\n\r\n"))

	// The client is done writing. The daemon is not done answering.
	peer.sendEmpty(wire.FrameDockerClose, op)

	frame := peer.read()
	if frame.Type != wire.FrameDockerData {
		t.Fatalf("frame type = %v after the half close, want the daemon's answer", frame.Type)
	}

	if string(frame.Payload) != lateAnswer {
		t.Errorf("payload = %q, want %q — the stream was torn down on a HALF close", frame.Payload, lateAnswer)
	}
}

// lateAnswer is what the double writes once it has seen end-of-request.
const lateAnswer = "the-daemons-answer"

// lateAnswerSocket serves a unix socket that reads a whole request, waits for
// the client to stop writing, and only then answers — the way a daemon
// handling a request with no more input to read does.
func lateAnswerSocket(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "s")

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("listening on %s: %v", path, err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				_, _ = io.Copy(io.Discard, conn)
				_, _ = conn.Write([]byte(lateAnswer))
			}()
		}
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})

	return path
}

// TestDockerAbortEndsTheStream is the other half of TestDockerHalfClose, and
// the reason the two cannot share one frame.
//
// A half close means the client has stopped writing while the daemon is still
// answering — tearing the stream down there cut every placed containerized
// step off from its own output. An ABORT means the client is GONE: the
// orchestrator's relay could not write to it, dropped the connection, and now
// has no reader for anything more on that op. Half-closing that one left the
// daemon socket open and the pump reading it, putting frames for a stream
// nobody holds onto the single wire an aws:// session has, for the rest of
// the session.
func TestDockerAbortEndsTheStream(t *testing.T) {
	peer := newPeer(t, Options{Build: "test", Root: t.TempDir(), DockerSocket: echoSocket(t)})
	peer.hello()

	op := peer.next()
	peer.sendEmpty(wire.FrameDockerOpen, op)
	peer.sendRaw(wire.FrameDockerData, op, []byte("one"))

	if frame := peer.read(); string(frame.Payload) != "one" {
		t.Fatalf("payload = %q, want the daemon's echo", frame.Payload)
	}

	peer.sendRaw(wire.FrameDockerClose, op, wire.DockerAbortPayload())

	// Reopening the SAME op is the observable proof the first was released:
	// dockerOpen refuses an id the table still holds, and it holds one until
	// something actually closes it. A half close leaves it held forever,
	// along with the daemon socket and the goroutine pumping it.
	peer.sendEmpty(wire.FrameDockerOpen, op)
	peer.sendRaw(wire.FrameDockerData, op, []byte("two"))

	frame := peer.readAny()
	if frame.Type == wire.FrameError {
		t.Fatalf("reopening the aborted stream was refused: %q — the abort only half-closed it", frame.Payload)
	}

	if string(frame.Payload) != "two" {
		t.Errorf("payload = %q, want the reopened stream's echo", frame.Payload)
	}
}
