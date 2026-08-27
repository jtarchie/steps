package shim

// The docker socket, carried on the session's own connection.
//
// A placed step that names an image runs its command in a container ON the
// worker, and the orchestrator drives that with its own docker client rather
// than teaching the shim what a container is: this file forwards bytes to the
// worker's docker socket and parses none of them. The shim stays what its
// depguard entry says it is — a binary that runs a command in a directory —
// and there is exactly one implementation of container execution in the repo.
//
// On the session's existing connection, deliberately: a second port would
// need a second SSM session and would outlive `--once`, giving up both the
// "nothing is left running" and the "no inbound ports" properties that are
// the whole reason aws:// is reached the way it is.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/wire"
)

// defaultDockerSocket is where a daemon listens on a machine that has one.
const defaultDockerSocket = "/var/run/docker.sock"

// dockerSocketPath answers which daemon THIS machine talks to.
//
// DOCKER_HOST before the default, because that variable is already the
// answer to exactly this question everywhere else — and on a machine whose
// daemon runs in a VM (colima, Rancher, Docker Desktop) the socket is under
// the user's home directory, not in /var/run. Only the unix form: a worker
// pointed at a tcp daemon is one this forwarding has no reason to reach,
// since the orchestrator could dial that itself.
func dockerSocketPath(configured string) string {
	if configured != "" {
		return configured
	}

	host := os.Getenv("DOCKER_HOST")
	if path, ok := strings.CutPrefix(host, "unix://"); ok {
		return path
	}

	return defaultDockerSocket
}

// dockerStreams holds the sockets one session has open, by operation.
type dockerStreams struct {
	mu     sync.Mutex
	conns  map[uint32]net.Conn
	closed bool
}

func (d *dockerStreams) add(op uint32, conn net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return false
	}

	if d.conns == nil {
		d.conns = map[uint32]net.Conn{}
	}

	d.conns[op] = conn

	return true
}

func (d *dockerStreams) get(op uint32) (net.Conn, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, ok := d.conns[op]

	return conn, ok
}

// remove takes a stream out and returns it, so exactly one caller closes it.
func (d *dockerStreams) remove(op uint32) (net.Conn, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, ok := d.conns[op]
	if ok {
		delete(d.conns, op)
	}

	return conn, ok
}

// closeAll ends every stream and refuses later ones, for session teardown: a
// docker socket outliving the session that opened it is the same leak in
// miniature as a shim outliving its dial.
func (d *dockerStreams) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.closed = true

	for op, conn := range d.conns {
		_ = conn.Close()
		delete(d.conns, op)
	}
}

// handleDocker routes the three stream frames, kept as one case in the
// session's switch so the docker family costs it one branch rather than three.
func (s *session) handleDocker(ctx context.Context, frame wire.Frame) error {
	//kindswitch:ignore this switch is over three frame types, not over a kind
	switch frame.Type { //nolint:exhaustive // the caller has already selected the docker family
	case wire.FrameDockerOpen:
		return s.dockerOpen(ctx, frame)
	case wire.FrameDockerData:
		return s.dockerData(frame)
	default:
		return s.dockerClose(frame)
	}
}

// dockerOpen dials the worker's docker socket for one operation.
func (s *session) dockerOpen(ctx context.Context, frame wire.Frame) error {
	socket := dockerSocketPath(s.opts.DockerSocket)

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		// Returned, not sent: the run loop turns a handler error into a
		// FrameError for this operation and keeps the session alive, which is
		// the behaviour wanted here — a worker with no docker daemon is a
		// perfectly good worker for every step that names no image.
		return fmt.Errorf("dialling the docker socket at %s: %w", socket, err)
	}

	if !s.docker.add(frame.Op, conn) {
		_ = conn.Close()

		return nil
	}

	go s.pumpDocker(frame.Op, conn)

	return nil
}

// pumpDocker relays the socket's bytes back until it ends.
func (s *session) pumpDocker(op uint32, conn net.Conn) {
	buffer := make([]byte, dockerChunkBytes)

	for {
		read, err := conn.Read(buffer)
		if read > 0 {
			sendErr := s.sendData(wire.FrameDockerData, op, buffer[:read])
			if sendErr != nil {
				break
			}
		}

		if err != nil {
			break
		}
	}

	// Whoever removes it closes it, so a close racing the peer's own cannot
	// double-close or leak.
	if closing, ok := s.docker.remove(op); ok {
		_ = closing.Close()
		_ = s.sendData(wire.FrameDockerClose, op, nil)
	}
}

// dockerChunkBytes is one relay read. The same order as a data frame's chunk:
// docker's API is mostly small messages with occasional large bodies, and a
// bigger buffer buys nothing a stream does not already pipeline.
const dockerChunkBytes = 32 * 1024

// dockerData writes the peer's bytes to the socket.
func (s *session) dockerData(frame wire.Frame) error {
	conn, ok := s.docker.get(frame.Op)
	if !ok {
		// A stream the shim has already closed: the peer's write crossed the
		// close in flight, which is ordinary, not an error.
		return nil
	}

	_, err := conn.Write(frame.Payload)
	if err != nil {
		if closing, removed := s.docker.remove(frame.Op); removed {
			_ = closing.Close()
		}

		if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
			return nil
		}

		return fmt.Errorf("writing to the docker socket: %w", err)
	}

	return nil
}

// dockerClose ends a stream the peer is done with, and always answers.
//
// Answered even for an operation this end has never heard of, which is what
// gives the orchestrator a way to know the wire is quiet again: it owns the
// connection with a reader while docker streams are open, and it cannot take
// that reader back until something it sent comes back. A close for an unknown
// stream is the cheapest such round trip, and is harmless by construction —
// there was nothing to close.
func (s *session) dockerClose(frame wire.Frame) error {
	if conn, ok := s.docker.remove(frame.Op); ok {
		_ = conn.Close()
	}

	return s.sendData(wire.FrameDockerClose, frame.Op, nil)
}
