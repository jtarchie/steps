package wire

// The framing both ends of a venue speak.
//
// Length-prefixed binary, because the bulk path is the point: a step's tree
// moves through here, and any encoding that has to escape or re-copy those
// bytes taxes the one thing this protocol exists to do. Control frames carry
// JSON, which is small, self-describing, and readable in a packet dump when
// something goes wrong on a machine you cannot attach a debugger to.
//
// The shape is deliberately the one internal/workspace's digest already uses:
// a length prefix before every variable-length field, so no arrangement of
// payloads can be read as a different sequence of frames.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// FrameType names what a frame carries. The zero value is deliberately not a
// valid type, so a zeroed or truncated header is a protocol error rather than
// a plausible-looking hello.
type FrameType uint8

const (
	// FrameHello opens a session: the orchestrator states the protocol it
	// speaks and the binary it pushed, and the shim answers with its own.
	FrameHello FrameType = iota + 1
	// FrameHelloOK is the shim's answer, naming its platform and scratch dir.
	FrameHelloOK
	// FrameUpload begins a tree transfer into the shim's work directory.
	FrameUpload
	// FrameExec asks the shim to run one command.
	FrameExec
	// FrameStdout carries a running command's stdout. Raw bytes, never JSON:
	// this is the interactive path and it must not be re-encoded.
	FrameStdout
	// FrameStderr is FrameStdout's twin for the other stream.
	FrameStderr
	// FrameExit ends a command, carrying the distinction os/exec expresses by
	// type and no wire can: whether the process started at all.
	FrameExit
	// FrameFetch asks for named subtrees back.
	FrameFetch
	// FrameData carries tar bytes, in whichever direction the current
	// operation runs. Raw.
	FrameData
	// FrameEnd closes a bulk transfer.
	FrameEnd
	// FrameCancel asks the shim to kill the command running under one op.
	FrameCancel
	// FrameError reports a failed operation. The session survives it; a
	// transport-level problem closes the connection instead.
	FrameError
	// FrameBye tears the session down: the shim removes its scratch and exits.
	FrameBye
	// FrameDraining is the worker saying it is about to go away — a spot
	// eviction notice, a rebalance recommendation. Unsolicited, and the only
	// frame that is: it belongs to no operation, so it carries DrainOp rather
	// than the op of whatever is in flight.
	//
	FrameDraining
	// FrameDockerOpen asks the shim to dial the docker socket ON THE WORKER
	// and treat this op as a byte stream to it. The path is the shim's own,
	// never the peer's: a socket path from the wire would be a peer choosing
	// which of the worker's services to talk to.
	FrameDockerOpen
	// FrameDockerData carries socket bytes, in whichever direction. Raw, and
	// opaque — this end parses none of the docker API.
	FrameDockerData
	// FrameDockerClose ends one stream, from whichever end noticed first.
	FrameDockerClose
	// FrameNeed is the shim asking for an artifact's bytes: the orchestrator
	// named one by digest, and this end does not have it. Its absence is the
	// answer too — a FrameEnd instead means the worker already holds it and
	// nothing needs to cross.
	//
	// Last deliberately: the decoder's range check ends here, so a new frame
	// type goes after this one or the check moves with it.
	FrameNeed
)

// DrainOp is the operation id an unsolicited frame carries. Zero is never
// minted for a real operation — session op counters start at one — so a
// reader can tell "about the session" from "about the thing I asked for".
const DrainOp uint32 = 0

// headerBytes is [type:1][op:3][length:4], big-endian.
const headerBytes = 8

// MaxFrameBytes bounds one frame's payload. A frame claiming more is a
// protocol violation, and failing on the claim rather than on the allocation
// is what keeps a corrupt length from being a memory exhaustion.
const MaxFrameBytes = 1 << 20

// DataChunkBytes is how much tree a single FrameData carries. Bounded so a
// cancel can be interleaved between chunks rather than queueing behind a
// multi-gigabyte transfer.
const DataChunkBytes = 256 << 10

// MaxOp is the largest operation id the 3-byte field holds, exported because
// the side that MINTS ids has to respect the same ceiling the encoder
// enforces. Ids wrap, which is harmless: only one operation is ever in flight,
// so an id has to collide with itself 16 million operations later to be
// ambiguous.
const MaxOp = 1<<24 - 1

// dockerAbort is the one payload byte that turns a FrameDockerClose from a
// half-close into a drop.
const dockerAbort = 1

// DockerAbortPayload is the FrameDockerClose payload meaning "drop this
// stream", against the empty payload meaning "I have finished writing".
//
// The two are not interchangeable and cannot share a spelling. An empty close
// is ordinary: the docker CLI shuts down the write side of a hijacked stream
// the moment it has no stdin left, while the daemon is still writing the
// container's output back, so ending the stream there cuts a step off from its
// own output. An abort says the stream is GONE at the sender's end — without a
// way to say it, a receiver went on reading a socket whose bytes had nowhere
// to land and putting them on the one wire an aws:// session has, for the rest
// of that session.
func DockerAbortPayload() []byte { return []byte{dockerAbort} }

// IsDockerAbort reports a FrameDockerClose that ends the stream outright.
func IsDockerAbort(payload []byte) bool {
	return len(payload) == 1 && payload[0] == dockerAbort
}

// ErrProtocol is a frame that cannot be part of a conversation this code
// wrote: a bad type, an impossible length, a truncated header.
var ErrProtocol = errors.New("protocol error")

// Frame is one message. Op ties a frame to the operation it belongs to, which
// matters for exactly one reason: cancellation races completion. Without it a
// cancel aimed at a command that has already exited can arrive after the next
// command started, and kill the wrong one.
type Frame struct {
	Type    FrameType
	Op      uint32
	Payload []byte
}

// Encoder writes frames to a stream. One Encoder owns its writer: frames must
// not interleave, so every write goes through one goroutine or one lock.
type Encoder struct {
	w      io.Writer
	header [headerBytes]byte
	// frame is the header and its payload contiguously, so one frame costs
	// one Write. See Write.
	frame []byte
}

// NewEncoder returns an Encoder writing frames to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

// Write emits one frame.
func (e *Encoder) Write(frame Frame) error {
	if len(frame.Payload) > MaxFrameBytes {
		return fmt.Errorf("%w: frame of %d bytes exceeds the %d limit", ErrProtocol, len(frame.Payload), MaxFrameBytes)
	}

	if frame.Op > MaxOp {
		return fmt.Errorf("%w: operation id %d does not fit in the header", ErrProtocol, frame.Op)
	}

	e.header[0] = byte(frame.Type)
	e.header[1] = byte((frame.Op >> 16) & 0xff)
	e.header[2] = byte((frame.Op >> 8) & 0xff)
	e.header[3] = byte(frame.Op & 0xff)
	binary.BigEndian.PutUint32(e.header[4:], uint32(len(frame.Payload))) //nolint:gosec // bounded by MaxFrameBytes above

	if len(frame.Payload) == 0 {
		_, err := e.w.Write(e.header[:])
		if err != nil {
			return fmt.Errorf("writing frame header: %w", err)
		}

		return nil
	}

	// One Write for the pair, not two. A transport underneath may treat every
	// call as a MESSAGE rather than as bytes in a stream — the SSM data
	// channel does, minting a sequence number, a digest and a retransmission
	// entry per call — so a header written apart from its payload doubles
	// what every frame costs on the one venue with the least bandwidth.
	e.frame = append(append(e.frame[:0], e.header[:]...), frame.Payload...)

	_, err := e.w.Write(e.frame)
	if err != nil {
		return fmt.Errorf("writing frame payload: %w", err)
	}

	return nil
}

// WriteJSON emits a control frame carrying v.
func (e *Encoder) WriteJSON(frameType FrameType, op uint32, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding %T: %w", v, err)
	}

	return e.Write(Frame{Type: frameType, Op: op, Payload: payload})
}

// Decoder reads frames from a stream.
type Decoder struct {
	r      io.Reader
	header [headerBytes]byte
}

// NewDecoder returns a Decoder reading frames from r.
func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r} }

// Read returns the next frame. The payload is only valid until the next Read:
// callers that keep it must copy, which is what the bulk path does by writing
// it straight through rather than retaining it.
func (d *Decoder) Read() (Frame, error) {
	_, err := io.ReadFull(d.r, d.header[:])
	if err != nil {
		// io.EOF at a frame boundary is an orderly close, and callers
		// distinguish it from a truncated frame, so it passes through
		// unwrapped.
		if errors.Is(err, io.EOF) {
			return Frame{}, io.EOF
		}

		return Frame{}, fmt.Errorf("reading frame header: %w", err)
	}

	frameType := FrameType(d.header[0])
	if frameType < FrameHello || frameType > FrameNeed {
		return Frame{}, fmt.Errorf("%w: unknown frame type %d", ErrProtocol, frameType)
	}

	op := uint32(d.header[1])<<16 | uint32(d.header[2])<<8 | uint32(d.header[3])

	length := binary.BigEndian.Uint32(d.header[4:])
	if length > MaxFrameBytes {
		return Frame{}, fmt.Errorf("%w: frame claims %d bytes, over the %d limit", ErrProtocol, length, MaxFrameBytes)
	}

	frame := Frame{Type: frameType, Op: op}
	if length == 0 {
		return frame, nil
	}

	frame.Payload = make([]byte, length)

	_, err = io.ReadFull(d.r, frame.Payload)
	if err != nil {
		// A header arrived and its payload did not, so the stream ended
		// MID-FRAME. io.ReadFull says io.EOF for that when it read nothing at
		// all, and passing that sentinel up makes a truncated frame
		// indistinguishable from the orderly close callers check for — the
		// shim reads it as the orchestrator saying goodbye and exits 0,
		// discarding the only account of a connection that died.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}

		return Frame{}, fmt.Errorf("reading %d-byte frame payload: %w", length, err)
	}

	return frame, nil
}

// DecodeJSON unmarshals a control frame's payload.
func DecodeJSON(frame Frame, v any) error {
	err := json.Unmarshal(frame.Payload, v)
	if err != nil {
		return fmt.Errorf("%w: decoding a type %d frame: %w", ErrProtocol, frame.Type, err)
	}

	return nil
}
