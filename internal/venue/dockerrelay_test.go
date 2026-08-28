package venue

// The forwarded docker socket, across more than one command — and with more
// than one goroutine on the wire.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/wire"
)

// pipedSession is a session whose peer is a channel of frames, so a test can
// drive the relay without a shim or a docker daemon.
func pipedSession(t *testing.T) (*session, *wire.Decoder, *wire.Encoder) {
	t.Helper()

	// os.Pipe, not io.Pipe: an unbuffered pipe deadlocks the moment a frame
	// is written while no reader owns the wire, which is ordinary here — a
	// stream's own close can land after the handoff frame, and production
	// readers skip it rather than block on it.
	toVenue, fromShim, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}

	fromVenue, toShim, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}

	t.Cleanup(func() {
		_ = toVenue.Close()
		_ = fromShim.Close()
		_ = fromVenue.Close()
		_ = toShim.Close()
	})

	return &session{
		decoder: wire.NewDecoder(toVenue),
		encoder: wire.NewEncoder(toShim),
	}, wire.NewDecoder(fromVenue), wire.NewEncoder(fromShim)
}

// TestRelaySurvivesEveryCommandsBracket is the latch that made the forwarded
// socket single-use.
//
// route() ran closeAll on its ORDINARY handoff — the doneOp echo that gives
// the wire back at the end of a command — and closeAll marks the relay closed
// for good. So the second bracket refused every connection: a step's second
// command, an attempts: retry, and above all the teardown `docker rm -f` that
// is the only thing which removes the step's container from the worker.
func TestRelaySurvivesEveryCommandsBracket(t *testing.T) {
	t.Parallel()

	session, fromVenue, fromShim := pipedSession(t)

	// A shim that answers every close, which is the whole handoff protocol.
	go func() {
		for {
			frame, err := fromVenue.Read()
			if err != nil {
				return
			}

			if frame.Type == wire.FrameDockerClose {
				_ = fromShim.Write(wire.Frame{Type: wire.FrameDockerClose, Op: frame.Op})
			}
		}
	}()

	socket, stop, err := session.openDockerSocket(context.Background(), false)
	if err != nil {
		t.Fatalf("openDockerSocket: %v", err)
	}

	defer stop()

	for round := 1; round <= 3; round++ {
		var dialErr error

		err = session.withDockerRouting(context.Background(), func() error {
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", socket)
			if err != nil {
				dialErr = err

				return nil
			}

			defer func() { _ = conn.Close() }()

			_, dialErr = conn.Write([]byte("GET /_ping HTTP/1.0\r\n\r\n"))

			return nil
		})
		if err != nil {
			t.Fatalf("round %d: withDockerRouting: %v", round, err)
		}

		if dialErr != nil {
			t.Fatalf("round %d: the forwarded socket stopped serving after an earlier command: %v", round, dialErr)
		}
	}
}

// TestSessionSerializesItsEncoder pins wire.Encoder's own stated contract —
// "frames must not interleave, so every write goes through one goroutine or
// one lock" — at the layer that stopped honouring it.
//
// The venue had exactly one writer until the docker relay arrived. Now accept,
// one pump per open socket, stopRouting, the cancel watchdog and close()'s
// goodbye all write, and Encoder stamps a shared header and a shared payload
// buffer — so two overlapping writes put one frame's header in front of
// another's bytes and the shim reads a frame nobody sent.
func TestSessionSerializesItsEncoder(t *testing.T) {
	t.Parallel()

	session, fromVenue, _ := pipedSession(t)

	const (
		writers = 8
		each    = 40
	)

	read := make(chan error, 1)

	go func() { read <- readWholeFrames(fromVenue, writers*each) }()

	var wg sync.WaitGroup

	for w := range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			mark := byte(w + 1) //nolint:gosec // w is a loop index below 8
			payload := make([]byte, 512)

			for i := range payload {
				payload[i] = mark
			}

			for range each {
				_ = session.writeFrame(wire.Frame{Type: wire.FrameDockerData, Op: uint32(mark), Payload: payload})
			}
		}()
	}

	wg.Wait()

	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("the stream came apart: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the reader never saw every frame — the stream desynced")
	}
}

// errInterleaved is one frame's header in front of another's payload.
var errInterleaved = errors.New("a frame carried another frame's payload")

// readWholeFrames reads count frames and reports the first whose payload does
// not match its own header.
//
// The payload is the op repeated, so a header spliced onto another frame's
// bytes shows up as a mismatch rather than as silence.
func readWholeFrames(decoder *wire.Decoder, count int) error {
	for range count {
		frame, err := decoder.Read()
		if err != nil {
			return fmt.Errorf("%w", err)
		}

		for _, b := range frame.Payload {
			if uint32(b) != frame.Op {
				return errInterleaved
			}
		}
	}

	return nil
}

// deadLocalConn is a docker client that went away — a CLI killed by a step's
// cancel or timeout. Writes to it fail; nothing else about it is asked.
type deadLocalConn struct{ net.Conn }

func (deadLocalConn) Write(_ []byte) (int, error) { return 0, io.ErrClosedPipe }

func (deadLocalConn) Close() error { return nil }

// readFrameWithin reads one frame on a bound, so a test that proves a frame
// was sent fails rather than hangs when it was not.
func readFrameWithin(t *testing.T, decoder *wire.Decoder, bound time.Duration) wire.Frame {
	t.Helper()

	type result struct {
		frame wire.Frame
		err   error
	}

	answered := make(chan result, 1)

	go func() {
		frame, err := decoder.Read()
		answered <- result{frame: frame, err: err}
	}()

	timer := time.NewTimer(bound)
	defer timer.Stop()

	select {
	case answer := <-answered:
		if answer.err != nil {
			t.Fatalf("reading what the venue sent: %v", answer.err)
		}

		return answer.frame
	case <-timer.C:
		t.Fatal("the venue sent nothing within the bound")

		return wire.Frame{}
	}
}

// TestRelayTellsTheShimWhenALocalWriteFails pins the half of the stream's end
// that only one side was doing.
//
// pump pairs its remove() with a close frame; deliver dropped the stream
// silently. Once deliver has removed the op, the relay's own pump for it
// skips the frame too — its remove returns !ok — so the shim never hears it,
// keeps the op in its table, and keeps reading the worker's daemon socket
// into FrameDockerData for a stream nobody holds, onto the ONE wire an aws://
// session has, for the rest of the session.
func TestRelayTellsTheShimWhenALocalWriteFails(t *testing.T) {
	t.Parallel()

	session, fromVenue, _ := pipedSession(t)

	relay := newTestRelay(session)

	const op = 7

	if !relay.add(op, deadLocalConn{}) {
		t.Fatal("the relay refused a stream it had never seen")
	}

	relay.deliver(wire.Frame{Type: wire.FrameDockerData, Op: op, Payload: []byte("bytes for a client that left")})

	if _, still := relay.get(op); still {
		t.Error("the relay kept a stream whose local end could not be written to")
	}

	frame := readFrameWithin(t, fromVenue, 5*time.Second)
	if frame.Type != wire.FrameDockerClose || frame.Op != op {
		t.Errorf("the venue sent a type %d frame for operation %d, want a close for %d — the shim was never told the stream ended",
			frame.Type, frame.Op, op)
	}
}

// dialAndPoke opens the forwarded socket and sends something down it, which
// is all a stream has to do here — the shim in these tests answers frames,
// not docker's API.
func dialAndPoke(socket string) error {
	conn, err := (&net.Dialer{}).Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("GET /_ping HTTP/1.0\r\n\r\n"))
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// TestOneStreamsFailureLeavesTheRelayServing is the second latch that made the
// forwarded socket single-use — this one on an ordinary, per-stream event.
//
// A FrameError names the OPERATION it belongs to, and every docker-stream one
// the shim sends is about a single stream: a dockerOpen that could not dial
// the daemon (a restart, an fd limit), a dockerData whose write got EPIPE.
// route() read it through the session's decoder, which discards the op, so it
// arrived as a plain Go error and took the relay's death branch — closeAll,
// which latches `closed` and shuts the listener, with nothing anywhere to
// clear it and openDockerSocket running once per session. From that instant
// the teardown `docker rm -f` could not reach the daemon holding the step's
// container, and nothing sweeps a worker.
func TestOneStreamsFailureLeavesTheRelayServing(t *testing.T) {
	t.Parallel()

	session, fromVenue, fromShim := pipedSession(t)

	opened := make(chan uint32, 8)

	refuseNext := true

	go func() {
		for {
			frame, err := fromVenue.Read()
			if err != nil {
				return
			}

			//nolint:exhaustive // a stand-in shim answers only the frames its test sends
			switch frame.Type {
			case wire.FrameDockerOpen:
				if refuseNext {
					refuseNext = false

					_ = fromShim.WriteJSON(wire.FrameError, frame.Op,
						wire.Error{Message: "dialling the docker socket at /var/run/docker.sock: connection refused"})
				}

				// Announced AFTER the answer, so a bracket that waits on this
				// knows the answer is already on the wire — and the wire is
				// ordered, so the router meets it before the echo that ends
				// the bracket.
				opened <- frame.Op
			case wire.FrameDockerClose:
				_ = fromShim.Write(wire.Frame{Type: wire.FrameDockerClose, Op: frame.Op})
			}
		}
	}()

	socket, stop, err := session.openDockerSocket(context.Background(), false)
	if err != nil {
		t.Fatalf("openDockerSocket: %v", err)
	}

	defer stop()

	// The command whose one stream the shim refuses. fn does not return until
	// the refusal is on the wire, so the router meets it inside this bracket
	// rather than the next one.
	err = session.withDockerRouting(context.Background(), func() error {
		dialErr := dialAndPoke(socket)
		awaitOp(t, opened, "the first stream")

		return dialErr
	})
	if err == nil {
		t.Error("the bracket reported success though the shim said it could not reach its daemon")
	}

	// The teardown's `docker rm -f`: the only thing that removes the step's
	// container from the worker.
	err = session.withDockerRouting(context.Background(), func() error {
		dialErr := dialAndPoke(socket)
		awaitOp(t, opened, "the stream after the refused one")

		return dialErr
	})
	if err != nil {
		t.Errorf("the command after a refused stream could not use the forwarded socket: %v", err)
	}
}

// awaitOp waits for the stand-in shim to have been asked to open a stream.
func awaitOp(t *testing.T, opened <-chan uint32, what string) {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	select {
	case <-opened:
	case <-timer.C:
		t.Fatalf("the shim was never asked to open %s — the relay had stopped serving", what)
	}
}
