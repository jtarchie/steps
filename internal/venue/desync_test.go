package venue

// A conversation that lost its place must not be handed to the next command.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// scriptedSession answers with frames a test wrote, so a protocol violation
// can be provoked without a worker.
func scriptedSession(t *testing.T, frames ...wire.Frame) *session {
	t.Helper()

	var script bytes.Buffer

	encoder := wire.NewEncoder(&script)

	for _, frame := range frames {
		err := encoder.Write(frame)
		if err != nil {
			t.Fatalf("scripting a %v frame: %v", frame.Type, err)
		}
	}

	return &session{
		decoder: wire.NewDecoder(&script),
		encoder: wire.NewEncoder(io.Discard),
		workdir: "/tmp/scripted",
	}
}

// TestProtocolErrorBreaksTheSession pins that a conversation which lost its
// place is redialled rather than reused.
//
// ensure() reuses a session unless it is marked broken, and a protocol
// violation is precisely the state where reuse is wrong: the reader stopped
// mid-stream, so whatever is still queued belongs to an operation nobody is
// listening for. The next command then reads the previous one's leftovers as
// its own answer — the failure mode that already has a message written for
// it, "a type 9 frame for operation N arrived during operation N+1".
func TestProtocolErrorBreaksTheSession(t *testing.T) {
	t.Parallel()

	// A command's operation answered by a frame that belongs to no command.
	session := scriptedSession(t, wire.Frame{Type: wire.FrameUpload, Op: 1})

	_, err := session.run(context.Background(), "true", outputSinks{stdout: io.Discard, stderr: io.Discard})
	if !errors.Is(err, wire.ErrProtocol) {
		t.Fatalf("error = %v, want a protocol violation", err)
	}

	if !session.broken.Load() {
		t.Error("the session was left usable after losing its place in the conversation")
	}
}

// TestNextOpStaysInsideTheFrameField pins that the side minting operation ids
// respects the ceiling the encoder enforces.
//
// The counter is 32 bits and the field is 24, so a long-lived process — a
// steps web that keeps placing steps, now with one id per docker connection —
// eventually mints an id Encoder.Write refuses, killing a working session for
// arithmetic. Ids need only be unique among operations in flight.
func TestNextOpStaysInsideTheFrameField(t *testing.T) {
	t.Parallel()

	session := &session{}
	session.op.Store(wire.MaxOp - 2)

	for range 8 {
		op := session.nextOp()

		if op > wire.MaxOp {
			t.Fatalf("op = %d, above the %d the frame field holds", op, wire.MaxOp)
		}

		if op == wire.DrainOp {
			t.Fatalf("op = %d, which means 'about the session' rather than an operation", op)
		}
	}
}
