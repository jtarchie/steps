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
	"time"

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
	// wg covers the relay goroutines, so none is still writing frames onto a
	// stream the session has already said goodbye on.
	wg sync.WaitGroup

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

	// Refused, not overwritten. The key comes off the wire, and overwriting
	// stranded the first socket with its pump goroutine alive — after which
	// that goroutine's own remove(op) pulled out and closed the SECOND
	// stream's connection, cross-wiring one teardown onto another's socket.
	if _, taken := d.conns[op]; taken {
		return false
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
//
// The wait is the other half: a relay goroutine still in sendData when the
// session ends would put a data frame on the wire after the goodbye, which in
// stdio mode is the orchestrator's own protocol stream.
func (d *dockerStreams) closeAll() {
	d.mu.Lock()

	d.closed = true

	for op, conn := range d.conns {
		_ = conn.Close()
		delete(d.conns, op)
	}

	d.mu.Unlock()

	// Bounded: a relay caught mid-write against a peer that stopped READING
	// has nothing local to unstick it — serveConn's connection close runs
	// only after Serve returns — so an unbounded wait here would strand the
	// very process --linger exists to reap.
	settled := make(chan struct{})

	go func() { defer close(settled); d.wg.Wait() }()

	timer := time.NewTimer(dockerSettleTimeout)
	defer timer.Stop()

	select {
	case <-settled:
	case <-timer.C:
	}
}

// dockerSettleTimeout bounds closeAll's wait for the relay goroutines.
const dockerSettleTimeout = 5 * time.Second

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
	// Behind the handshake, like every other operation. Without this the one
	// frame that hands out a raw proxy to a root docker daemon was also the
	// one that needed no hello — so the protocol check, the build check and
	// checkSessionName could all be skipped by sending this first.
	if s.workdir == "" {
		return errUnopened
	}

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

		// Reported, not silent. FrameDockerOpen is fire-and-forget, so silence
		// is indistinguishable from success — and the peer's next
		// FrameDockerData for this op then resolves to the FIRST stream,
		// writing one client's bytes into another client's socket.
		return fmt.Errorf("%w: docker stream %d is already open", wire.ErrProtocol, frame.Op)
	}

	s.docker.wg.Add(1)

	go func() { defer s.docker.wg.Done(); s.pumpDocker(frame.Op, conn) }()

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

// dockerWriteTimeout bounds one write to the worker's daemon socket. Generous,
// because it is not a latency budget: a daemon still reading is normal, and
// the only thing being ruled out is one that has stopped forever while holding
// the session's only reader.
const dockerWriteTimeout = 30 * time.Second

// dockerData writes the peer's bytes to the socket.
func (s *session) dockerData(frame wire.Frame) error {
	conn, ok := s.docker.get(frame.Op)
	if !ok {
		// A stream the shim has already closed: the peer's write crossed the
		// close in flight, which is ordinary, not an error.
		return nil
	}

	// Bounded, because this runs ON the session's frame loop: a daemon that
	// stops draining its side parks the one goroutine that reads the wire, and
	// with it every cancel, every goodbye and the orchestrator's own EOF — the
	// same hazard the store client's own deadline exists for. A stream whose
	// daemon will not take bytes is ended; the session keeps listening.
	_ = conn.SetWriteDeadline(time.Now().Add(dockerWriteTimeout))

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

// dockerClose ends the peer's HALF of a stream, or answers for one this end
// does not have.
//
// Half, because a docker connection is not a pipe with one end. The client
// shuts down its write side as soon as it has nothing left to send — the
// hijacked exec stream does it the moment there is no stdin — while the
// daemon is still writing the container's output back. Closing the socket
// outright on that signal cut every placed containerized step off from its
// own output: the command reported success with nothing on stdout, and the
// fetch that followed found a tree the container had not finished writing.
// The stream ends when the DAEMON is done, which pumpDocker sees and reports.
//
// Answered only for an operation this end has never heard of, which is what
// gives the orchestrator a way to know the wire is quiet again: it owns the
// connection with a reader while docker streams are open, and cannot take
// that reader back until something it sent comes back. A close for an unknown
// stream is the cheapest such round trip, and is harmless by construction —
// there was nothing to close. Answering a stream that IS open would instead
// tell the orchestrator to drop the connection its answer is still coming on.
func (s *session) dockerClose(frame wire.Frame) error {
	if conn, ok := s.docker.get(frame.Op); ok {
		half, canHalfClose := conn.(*net.UnixConn)
		if canHalfClose && !wire.IsDockerAbort(frame.Payload) {
			_ = half.CloseWrite()

			return nil
		}

		// An abort: the peer's end of this stream is gone, so half-closing
		// would leave the daemon socket open and pumpDocker reading it — and
		// every byte it read after that went onto the one wire an aws://
		// session has, addressed to a stream nothing was left to receive it.
		if closing, removed := s.docker.remove(frame.Op); removed {
			_ = closing.Close()
		}

		// Not echoed. The answer below exists to give the orchestrator a round
		// trip while it owns the connection with a reader; an abort means it
		// has already dropped this stream, so there is nobody to answer.
		return nil
	}

	return s.sendData(wire.FrameDockerClose, frame.Op, nil)
}
