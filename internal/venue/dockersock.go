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
	"sync/atomic"
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
	relay := &dockerRelay{
		session: s, decoder: s.decoder, conns: map[uint32]net.Conn{}, doneOp: s.nextOp(), listener: listener,
	}

	relay.wg.Add(1)

	go func() { defer relay.wg.Done(); relay.accept(ctx, listener) }()

	if route {
		relay.startRouting()
	}

	s.relay = relay

	stop := sync.OnceFunc(func() {
		_ = listener.Close()
		relay.closeAll()

		if route {
			relay.stopRouting(s)
		}

		relay.settle()
		relay.reportLost(s)
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

	// Cleared for THIS bracket: the relay outlives every command, and a
	// write-once verdict noted during an earlier one would be returned in
	// place of every later command's real result — including the teardown's.
	relay.clearFailure()

	done, started := relay.startRouting()
	if !started {
		// An earlier bracket's router never came back, so there is no wire to
		// get — and the one thing that must not be done about it is starting a
		// second reader on the decoder the first one still owns. fn does not
		// run either: everything it could ask the worker travels this
		// connection, so it would only wait out its own timeout for a reply
		// nobody is there to carry.
		s.broken.Store(true)

		return fmt.Errorf("%w %q: %w", ErrWorker, s.worker.URL, errRelayBusy)
	}

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

	// The router deliberately does not mark a dead transport itself — it can
	// outlive its conversation, and the session it would mark may by then be a
	// fresh one. This call IS the conversation, so it can.
	relay.reportLost(s)

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

// errRelayBusy is a bracket asking for a wire an earlier one never returned.
var errRelayBusy = errors.New("an earlier command's docker router still owns the connection")

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

// startRouting puts a router on the wire, reporting whether it got there.
//
// False is a bracket whose handoff timed out: its router is still blocked in a
// read nothing local can interrupt, and starting a second one on the same
// decoder is the failure this guard exists for — two io.ReadFulls tearing the
// header they share, and only one of the two ever able to consume the single
// echo that ends either, so the loser returns claiming the wire was handed
// back while a router still owns it.
//
// On the relay's own WaitGroup, like every other goroutine it starts: a router
// still reading when stop() removes the socket directory is a frame arriving
// on a wire the session believes it has taken back.
func (d *dockerRelay) startRouting() (<-chan struct{}, bool) {
	if !d.routing.CompareAndSwap(false, true) {
		return nil, false
	}

	done := make(chan struct{})

	d.wg.Add(1)

	go func() {
		defer d.wg.Done()
		defer close(done)
		defer d.routing.Store(false)

		d.route()
	}()

	return done, true
}

// reportLost hands the router's verdict about the TRANSPORT to the session,
// from a caller that is synchronous with the conversation.
//
// The router cannot do it itself. Its read outlives a stalled bracket, and by
// the time it fails the session may have redialled — at which point marking
// broken abandons a healthy worker's scratch and re-ships the whole tree, one
// command after another. session.read() already guards this hazard for
// readHello's detached goroutine; the router needs the same treatment for the
// same reason.
func (d *dockerRelay) reportLost(s *session) {
	if d.lost.Load() {
		s.broken.Store(true)
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
	// decoder is the reader of the conversation this relay was built for,
	// captured rather than reached for through the session in the goroutine.
	// connect() replaces the session's field on a redial, and a router can
	// outlive the bracket that started it, so reading through the session
	// would put a stale goroutine on the NEXT conversation's frames — under no
	// lock in common with the write that replaced them. Same reasoning as
	// exec.go's captured transport.
	decoder *wire.Decoder
	// listener is closed when accept gives up, so a later dial fails fast
	// instead of landing in a backlog nobody is accepting from — which the
	// client reads as a daemon that is merely slow.
	listener net.Listener
	// routing is held by whoever owns the wire, so a bracket cannot start a
	// second router beside one an earlier bracket left behind.
	routing atomic.Bool
	// lost records a router that ended because the TRANSPORT did, for a caller
	// synchronous with the conversation to act on — see reportLost.
	lost atomic.Bool
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

// route owns the wire while a bracket runs, handing each frame to its stream.
func (d *dockerRelay) route() {
	for {
		frame, err := d.readFrame()
		if err != nil {
			// The transport, not an operation. Noted for the bracket rather
			// than marked here: this goroutine can outlive its conversation.
			d.lost.Store(true)

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
		case wire.FrameError:
			// ONE stream's failure, and only that one. The shim answers a
			// dockerOpen it could not dial — a daemon restart, an fd limit —
			// or a dockerData whose write got EPIPE, with an error frame
			// carrying that stream's op. Folding it into the relay's death
			// latched `closed` and shut the listener with nothing anywhere to
			// clear it, while openDockerSocket runs once per session: from
			// that instant the teardown `docker rm -f` could not reach the
			// daemon holding the step's container, and nothing sweeps a
			// worker.
			d.failStream(frame)
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
			// the shim answering an operation nobody is listening for. The
			// conversation has lost its place, which is exactly where reuse is
			// wrong — see session.desync, which says so for the other readers.
			d.note(fmt.Errorf("%w: a type %d frame arrived on the docker stream", wire.ErrProtocol, frame.Type))
			d.lost.Store(true)
			d.closeAll()

			return
		}
	}
}

// readFrame reads through the decoder this relay captured, leaving an error
// frame INTACT.
//
// Both halves are the point. The decoder is the one this conversation opened,
// so a router that outlives its bracket cannot end up reading a later one's
// frames. And the session's own reader folds an error frame into a plain Go
// error, discarding the op — which is the only thing that says WHICH stream
// failed, and so the difference between ending that stream and ending the
// relay.
func (d *dockerRelay) readFrame() (wire.Frame, error) {
	frame, err := d.decoder.Read()
	if err != nil {
		return wire.Frame{}, fmt.Errorf("%w: %w", errWorkerLost, err)
	}

	return frame, nil
}

// failStream ends the one stream the shim could not serve, keeping the relay
// and noting the worker's own account of why.
func (d *dockerRelay) failStream(frame wire.Frame) {
	var reported wire.Error

	cause := errStreamRefused
	if decode(frame, &reported) == nil && reported.Message != "" {
		cause = errors.New(reported.Message)
	}

	d.note(cause)

	if conn, ok := d.remove(frame.Op); ok {
		_ = conn.Close()
	}
}

// errStreamRefused stands in for an error frame that said nothing readable.
var errStreamRefused = errors.New("the shim refused a docker stream and did not say why")

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

// clearFailure forgets the worker's account of an earlier command.
func (d *dockerRelay) clearFailure() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.failed = nil
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
			// Paired with the remove, exactly as pump pairs its own. The
			// stream's own pump cannot say it later — its remove finds
			// nothing and stays silent — so without this the shim keeps the
			// op, keeps reading the worker's daemon socket, and keeps putting
			// FrameDockerData for a forgotten stream onto the one wire an
			// aws:// session has, for the rest of that session.
			_ = d.session.writeFrame(wire.Frame{Type: wire.FrameDockerClose, Op: frame.Op})
		}
	}
}

func (d *dockerRelay) add(op uint32, conn net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return false
	}

	// Refused, not overwritten — the same rule the shim's own table keeps, and
	// for the same reason: overwriting strands the first socket with its pump
	// goroutine alive, after which that goroutine's own remove(op) pulls out
	// and closes the SECOND stream's connection. Op ids wrap at wire.MaxOp, so
	// a long-lived session genuinely repeats one.
	if _, taken := d.conns[op]; taken {
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
