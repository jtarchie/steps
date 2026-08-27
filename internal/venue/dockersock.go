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
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

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
	relay := &dockerRelay{session: s, conns: map[uint32]net.Conn{}, doneOp: s.nextOp(), listener: listener}

	relay.wg.Add(1)

	go func() { defer relay.wg.Done(); relay.accept(ctx, listener) }()

	if route {
		relay.wg.Add(1)

		go func() { defer relay.wg.Done(); relay.route() }()
	}

	s.relay = relay

	stop := sync.OnceFunc(func() {
		_ = listener.Close()
		relay.closeAll()

		if route {
			relay.stopRouting(s)
		}

		relay.settle()
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
func (s *session) withDockerRouting(ctx context.Context, fn func() error) error {
	relay := s.relay
	if relay == nil {
		return fn()
	}

	done := make(chan struct{})

	go func() { defer close(done); relay.route() }()

	err := fn()

	relay.stopRouting(s)

	handoffErr := awaitHandoff(ctx, done)
	if handoffErr != nil {
		// The router may still be blocked in a read, and will end when the
		// transport does. What must not happen is this call waiting with it:
		// the wire is desynced either way, so the session is marked broken
		// and the next command redials rather than talking into it.
		s.broken.Store(true)

		if err != nil {
			return err
		}

		return handoffErr
	}

	// The worker's own account wins. A shim that cannot reach its daemon says
	// so in a frame the router is the only reader of, and without this the
	// step reports the local client's confusion about a socket path on THIS
	// machine — naming neither the worker nor the cause.
	reported := relay.failure()
	if reported != nil {
		return reported
	}

	return err
}

// dockerHandoffTimeout bounds waiting for the shim to answer the close that
// hands the wire back, and dockerHandoffGrace is that bound for a caller who
// has already given up.
//
// Variables rather than constants so a test can shrink them: what is worth
// proving is that the wait ends without the worker's cooperation, not how
// patient it is.
//
//nolint:gochecknoglobals // test seams for a wait on another machine
var (
	dockerHandoffTimeout = 30 * time.Second
	dockerHandoffGrace   = 2 * time.Second
)

// errHandoffStalled is a worker that never gave the wire back.
var errHandoffStalled = errors.New("the worker did not finish with the docker socket")

// awaitHandoff waits for the router to let go, on a bound rather than on the
// worker.
//
// The unbounded version of this hung a step on a machine that had stopped
// answering — which is exactly the machine a cancel is usually racing, so the
// wait outlived the thing it was waiting to cancel. A caller that has already
// been cancelled gets the shorter bound, because it is not waiting for an
// answer any more, only for the goroutine to notice.
func awaitHandoff(ctx context.Context, done <-chan struct{}) error {
	bound := dockerHandoffTimeout
	if ctx.Err() != nil {
		bound = dockerHandoffGrace
	}

	timer := time.NewTimer(bound)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		return fmt.Errorf("%w after %s", errHandoffStalled, bound)
	}
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
	// listener is closed when accept gives up, so a later dial fails fast
	// instead of landing in a backlog nobody is accepting from — which the
	// client reads as a daemon that is merely slow.
	listener net.Listener
	// wg covers every goroutine this relay starts, pumps included: one still
	// writing when stop() removes the socket directory is a frame arriving on
	// a wire the session believes it has taken back.
	wg sync.WaitGroup

	mu      sync.Mutex
	conns   map[uint32]net.Conn
	closed  bool
	failed  error
	halfEOF map[uint32]bool
}

// accept turns each local docker connection into a stream on the wire.
func (d *dockerRelay) accept(ctx context.Context, listener net.Listener) {
	defer func() { _ = listener.Close() }()

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

		d.wg.Add(1)

		go func() { defer d.wg.Done(); d.pump(ctx, op, conn) }()
	}
}

// pump relays one local connection's bytes to the worker.
//
// A read EOF is a HALF close, never a teardown. The docker CLI shuts down the
// write side of its hijacked exec/attach connection as soon as it has no
// stdin left to send, and the container's output is still coming back the
// other way — so tearing the stream down here cut every placed containerized
// step off from its own output, and the fetch that followed found a tree the
// container had not finished writing. The step then succeeded, empty.
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

		if errors.Is(err, io.EOF) {
			// Say so, and stop reading — but leave the stream open so the
			// shim's answers keep arriving. The stream ends when the shim
			// closes it, or when the session tears everything down.
			if d.noteHalfClosed(op) {
				_ = d.session.writeFrame(wire.Frame{Type: wire.FrameDockerClose, Op: op})
			}

			return
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
	for {
		// read, not readFrame: a transport that dies while a containerized
		// command is running has to mark the conversation broken, or the next
		// command finds a cached nil startErr and pushes frames into a dead
		// pipe instead of redialling.
		frame, err := d.session.read()
		if err != nil {
			// A shim that could not reach its daemon says so in a FrameError,
			// which readFrame has already turned into this error — and this
			// is its only reader. Kept, or the step reports the local client's
			// confusion about a socket path on THIS machine, naming neither
			// the worker nor the cause.
			if !errors.Is(err, errWorkerLost) {
				d.note(err)
			}

			// Every local docker client must be told either way, or it waits
			// for an answer that is never coming — and a hang there reads as
			// the daemon being slow.
			d.closeAll()

			return
		}

		//kindswitch:ignore this switch is over frame types, not over a kind
		switch frame.Type { //nolint:exhaustive // the docker streams are the only conversation on the wire here
		case wire.FrameDockerData:
			d.deliver(frame)
		case wire.FrameDockerClose:
			if frame.Op == d.doneOp {
				// The handoff, NOT the end: the wire goes back to the session
				// and the streams stay open for the next command's bracket.
				// Closing here latched the relay shut after one command, and
				// the teardown `docker rm -f` could then never reach the
				// daemon that holds the step's container.
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
			d.note(fmt.Errorf("%w: a type %d frame arrived on the docker stream", wire.ErrProtocol, frame.Type))
			d.closeAll()

			return
		}
	}
}

// note records the worker's account of a docker failure, so withDockerRouting
// can report it instead of the local client's confusion.
func (d *dockerRelay) note(cause error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failed == nil {
		d.failed = fmt.Errorf("%w %q: the worker's docker daemon: %w", ErrWorker, d.session.worker.URL, cause)
	}
}

// settleTimeout bounds how long teardown waits for the relay's goroutines.
//
// Bounded rather than absolute: a pump caught mid-write on a wedged transport
// would otherwise park teardown forever, holding the session mutex and never
// reaching the transport close that closeTimeout exists to bound — which is
// the same trade session.close() makes about its goodbye, and a worse bug
// than the straggler the wait is there to catch.
const settleTimeout = 5 * time.Second

// settle waits for this relay's goroutines to finish, up to settleTimeout.
func (d *dockerRelay) settle() {
	done := make(chan struct{})

	go func() { defer close(done); d.wg.Wait() }()

	timer := time.NewTimer(settleTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
	}
}

// failure is what the worker said, or nil.
func (d *dockerRelay) failure() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.failed
}

// noteHalfClosed records that this end is done writing to op, reporting
// whether it is the first to say so.
func (d *dockerRelay) noteHalfClosed(op uint32) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.halfEOF == nil {
		d.halfEOF = map[uint32]bool{}
	}

	if d.halfEOF[op] {
		return false
	}

	d.halfEOF[op] = true

	return true
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
		delete(d.halfEOF, op)
	}

	return conn, ok
}

// closeAll ends every stream and refuses later ones. It is the relay's death,
// never a command's end — see route, which used to run this on its ordinary
// handoff and left the socket unusable for the rest of the session.
func (d *dockerRelay) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.closed = true

	if d.listener != nil {
		_ = d.listener.Close()
	}

	for op, conn := range d.conns {
		_ = conn.Close()
		delete(d.conns, op)
		delete(d.halfEOF, op)
	}
}
