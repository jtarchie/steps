package shim

// The docker socket carried on the session's connection, proven without a
// docker daemon: what this file forwards is opaque bytes, so a socket that
// echoes them is a complete test of the forwarding.

import (
	"context"
	"net"
	"path/filepath"
	"testing"

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
