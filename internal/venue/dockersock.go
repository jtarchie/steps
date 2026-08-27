package venue

// The worker's docker daemon, exposed here as a unix socket.
//
// A placed step that names an image runs its command in a container ON the
// worker, and the orchestrator drives that with the same container code every
// local step uses — it shells out to `docker`, so pointing DOCKER_HOST at
// this socket retargets preflight, pull, run, exec, rm and the sweep at once,
// without a second implementation and without the shim learning what a
// container is.
//
// The bytes ride the session's own connection: no second port, no second SSM
// session, so `--once` and the no-inbound-ports property both survive.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/jtarchie/steps/internal/wire"
)

// dockerRelayChunk is one read off a local docker connection.
const dockerRelayChunk = 32 * 1024

// serveDockerSocket exposes the worker's docker socket at a path on this
// machine, returning it and a function that tears the whole thing down.
//
// The returned stop is idempotent and must be called: it closes the listener,
// every open stream, and the reader that owns the wire while this is running.
//
// One reader, deliberately. While a step's command runs in a container, the
// venue sends no exec frames — the command is not going over this wire at all
// — so the docker streams are the only conversation on it, and a single
// goroutine can own the decoder without racing the operation loops. Starting
// this while a command IS in flight would be a bug, and there is no path that
// does.
func (s *session) serveDockerSocket(ctx context.Context) (string, func(), error) {
	return s.openDockerSocket(ctx, true)
}

// openDockerSocket is serveDockerSocket, with a say in whether the router
// starts with it.
//
// A step's commands and its OUTPUT FETCH share one connection, so the router
// cannot own the wire for the session's lifetime: it would read the fetch's
// frames and the step would come home empty. It runs only while a command is
// executing — see withDockerRouting — and the listener outlives it, so the
// socket path stays the one the container runner was built with.
func (s *session) openDockerSocket(ctx context.Context, route bool) (string, func(), error) {
	dir, err := os.MkdirTemp("", "steps-docker-")
	if err != nil {
		return "", nil, fmt.Errorf("making the docker socket directory: %w", err)
	}

	// Short name: a unix socket path is capped near 104 bytes, and the temp
	// directory has already spent most of it.
	path := filepath.Join(dir, "d.sock")

	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		_ = os.RemoveAll(dir)

		return "", nil, fmt.Errorf("listening for docker on %s: %w", path, err)
	}

	// The op whose close the shim echoes back, telling the router the wire
	// is quiet and it may hand the connection back to the session.
	relay := &dockerRelay{session: s, conns: map[uint32]net.Conn{}, doneOp: s.nextOp()}

	var wg sync.WaitGroup

	wg.Add(1)

	go func() { defer wg.Done(); relay.accept(ctx, listener) }()

	if route {
		wg.Add(1)

		go func() { defer wg.Done(); relay.route() }()
	}

	s.relay = relay

	stop := sync.OnceFunc(func() {
		_ = listener.Close()
		relay.closeAll()

		if route {
			relay.stopRouting(s)
		}

		wg.Wait()
		_ = os.RemoveAll(dir)
	})

	return path, stop, nil
}

// withDockerRouting runs fn with the router owning the wire, and gives the
// wire back before returning.
//
// The bracket is the whole point: a command's docker traffic and the step's
// output fetch travel the same connection, and only one reader may own it at
// a time. Whatever fn does, routing stops before this returns — a step whose
// command failed still has to be able to fetch what it produced.
func (s *session) withDockerRouting(fn func() error) error {
	relay := s.relay
	if relay == nil {
		return fn()
	}

	done := make(chan struct{})

	go func() { defer close(done); relay.route() }()

	err := fn()

	relay.stopRouting(s)
	<-done

	return err
}

// stopRouting ends the router by asking the shim to answer.
//
// The router is blocked in a read on the transport, which nothing local can
// interrupt; only a frame can. This close is for a stream that never existed,
// and the shim's answer to it is what ends the router — after which the
// session owns its wire again.
func (d *dockerRelay) stopRouting(s *session) {
	_ = s.writeFrame(wire.Frame{Type: wire.FrameDockerClose, Op: d.doneOp})
}

// dockerRelay carries one session's docker streams in both directions.
type dockerRelay struct {
	session *session
	// doneOp is the stream that never was: the shim echoes its close, and
	// that echo is the router's signal to stop reading.
	doneOp uint32

	mu     sync.Mutex
	conns  map[uint32]net.Conn
	closed bool
}

// accept turns each local docker connection into a stream on the wire.
func (d *dockerRelay) accept(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}

		op := d.session.nextOp()

		err = d.session.writeFrame(wire.Frame{Type: wire.FrameDockerOpen, Op: op})
		if err != nil {
			_ = conn.Close()

			return
		}

		if !d.add(op, conn) {
			_ = conn.Close()

			return
		}

		go d.pump(ctx, op, conn)
	}
}

// pump relays one local connection's bytes to the worker.
func (d *dockerRelay) pump(_ context.Context, op uint32, conn net.Conn) {
	buffer := make([]byte, dockerRelayChunk)

	for {
		read, err := conn.Read(buffer)
		if read > 0 {
			writeErr := d.session.writeFrame(wire.Frame{
				Type: wire.FrameDockerData, Op: op, Payload: buffer[:read],
			})
			if writeErr != nil {
				break
			}
		}

		if err != nil {
			break
		}
	}

	if closing, ok := d.remove(op); ok {
		_ = closing.Close()
		_ = d.session.writeFrame(wire.Frame{Type: wire.FrameDockerClose, Op: op})
	}
}

// route owns the wire while the socket is up, handing each frame to its
// stream.
func (d *dockerRelay) route() {
	// Whatever ends this — a shim that cannot reach its daemon, a dead
	// transport, a stray frame — every local docker client must be told, or
	// it waits for an answer that is never coming. A hang here reads as the
	// daemon being slow and is far harder to diagnose than an error.
	defer d.closeAll()

	for {
		frame, err := d.session.readFrame()
		if err != nil {
			return
		}

		//kindswitch:ignore this switch is over frame types, not over a kind
		switch frame.Type { //nolint:exhaustive // the docker streams are the only conversation on the wire here
		case wire.FrameDockerData:
			d.deliver(frame)
		case wire.FrameDockerClose:
			if frame.Op == d.doneOp {
				return
			}

			if conn, ok := d.remove(frame.Op); ok {
				_ = conn.Close()
			}
		case wire.FrameDraining:
			d.session.noteDrain(frame)
		default:
			// Nothing else belongs here, and a frame that arrives anyway is
			// the shim answering an operation nobody is listening for.
			return
		}
	}
}

// deliver writes one stream's bytes to its local connection, ending the
// stream if that fails — a client that has gone away is ordinary, not an
// error worth stopping the others for.
func (d *dockerRelay) deliver(frame wire.Frame) {
	conn, ok := d.get(frame.Op)
	if !ok {
		return
	}

	_, err := conn.Write(frame.Payload)
	if err != nil {
		if closing, removed := d.remove(frame.Op); removed {
			_ = closing.Close()
		}
	}
}

func (d *dockerRelay) add(op uint32, conn net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return false
	}

	d.conns[op] = conn

	return true
}

func (d *dockerRelay) get(op uint32) (net.Conn, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, ok := d.conns[op]

	return conn, ok
}

// remove takes a stream out and returns it, so exactly one caller closes it.
func (d *dockerRelay) remove(op uint32) (net.Conn, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	conn, ok := d.conns[op]
	if ok {
		delete(d.conns, op)
	}

	return conn, ok
}

func (d *dockerRelay) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.closed = true

	for op, conn := range d.conns {
		_ = conn.Close()
		delete(d.conns, op)
	}

	_ = errors.ErrUnsupported
}
