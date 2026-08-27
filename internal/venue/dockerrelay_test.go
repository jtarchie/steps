package venue

// The forwarded docker socket, across more than one command — and with more
// than one goroutine on the wire.

import (
	"context"
	"errors"
	"fmt"
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
