package wire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestReadTruncatedPayloadIsNotAnOrderlyClose is the distinction the whole
// session teardown hangs off.
//
// io.EOF at a frame BOUNDARY is the peer saying goodbye, and callers check for
// it by sentinel: the shim's frame loop returns nil on it and the process exits
// 0. io.ReadFull answers the same bare io.EOF for a payload it never got a byte
// of — so a header that arrived followed by a connection that died read as an
// orderly close, and the only account of the death was discarded.
func TestReadTruncatedPayloadIsNotAnOrderlyClose(t *testing.T) {
	t.Parallel()

	whole := new(bytes.Buffer)

	err := NewEncoder(whole).Write(Frame{Type: FrameStdout, Op: 1, Payload: []byte("payload bytes")})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	// Exactly the header, and nothing of what it promised.
	_, err = NewDecoder(bytes.NewReader(whole.Bytes()[:headerBytes])).Read()
	if err == nil {
		t.Fatal("a header with no payload decoded without error")
	}

	if errors.Is(err, io.EOF) {
		t.Errorf("a truncated frame reported io.EOF (%v), which every caller reads as the peer saying goodbye", err)
	}

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want io.ErrUnexpectedEOF", err)
	}

	// An empty stream at a frame boundary still IS the orderly close, and the
	// two must stay distinguishable in both directions.
	_, err = NewDecoder(bytes.NewReader(nil)).Read()
	if !errors.Is(err, io.EOF) {
		t.Errorf("an empty stream reported %v, want io.EOF", err)
	}
}

// TestWriteEmitsOneFrameInOneWrite pins the cost of a frame on a transport that
// counts calls rather than bytes.
//
// The SSM data channel mints a sequence number, a sha256 and a retransmission
// entry per Write, so a header written apart from its payload doubles what
// every frame costs on the venue with the least bandwidth to spare.
func TestWriteEmitsOneFrameInOneWrite(t *testing.T) {
	t.Parallel()

	counter := &countingWriter{}
	encoder := NewEncoder(counter)

	err := encoder.Write(Frame{Type: FrameStdout, Op: 7, Payload: []byte("some output")})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	if counter.writes != 1 {
		t.Errorf("a frame with a payload took %d writes, want 1", counter.writes)
	}

	// The bytes have to be the same ones the decoder reads back, since the
	// buffer is reused across frames.
	decoded, err := NewDecoder(bytes.NewReader(counter.buf.Bytes())).Read()
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if decoded.Type != FrameStdout || decoded.Op != 7 || string(decoded.Payload) != "some output" {
		t.Errorf("decoded = %+v, want the frame that went in", decoded)
	}

	// A second frame reuses the buffer, so this is where an aliasing mistake
	// would show up.
	counter.writes = 0

	err = encoder.Write(Frame{Type: FrameStderr, Op: 8, Payload: []byte("x")})
	if err != nil {
		t.Fatalf("encoding the second frame: %v", err)
	}

	if counter.writes != 1 {
		t.Errorf("the second frame took %d writes, want 1", counter.writes)
	}
}

type countingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.writes++

	return c.buf.Write(p) //nolint:wrapcheck // a test double over bytes.Buffer
}
