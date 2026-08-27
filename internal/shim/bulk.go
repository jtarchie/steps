package shim

// Turning a sequence of frames into the io.Reader and io.Writer the tree codec
// expects, so the codec never learns there is a wire underneath it.

import (
	"fmt"
	"io"

	"github.com/jtarchie/steps/internal/wire"
)

// dataReader presents the FrameData frames of one operation as a stream,
// ending at FrameEnd.
//
// It reads from the decoder directly rather than through a queue because
// operations are strictly sequential: while a tree is arriving, nothing else
// can be, so there is no frame to miss and no buffer to size.
func (s *session) dataReader(op uint32) (io.Reader, func() error) {
	reader := &frameReader{session: s, op: op}

	// The caller may stop early — an unpack that refuses an unsafe entry
	// returns before the last frame — and the sender is still mid-transfer.
	// Draining leaves the stream at a frame boundary, so the session can carry
	// on rather than reading the rest of a tree as commands.
	return reader, reader.drain
}

type frameReader struct {
	session *session
	op      uint32
	pending []byte
	done    bool
	err     error
}

func (r *frameReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			if r.err != nil {
				return 0, r.err
			}

			return 0, io.EOF
		}

		frame, err := r.session.decoder.Read()
		if err != nil {
			r.done, r.err = true, fmt.Errorf("reading a tree: %w", err)

			return 0, r.err
		}

		switch {
		case frame.Op != r.op:
			r.done, r.err = true, fmt.Errorf("%w: a type %d frame for operation %d arrived during operation %d",
				wire.ErrProtocol, frame.Type, frame.Op, r.op)

			return 0, r.err
		case frame.Type == wire.FrameEnd:
			r.done = true
		case frame.Type == wire.FrameData:
			r.pending = frame.Payload
		default:
			r.done, r.err = true, fmt.Errorf("%w: a type %d frame interrupted a tree", wire.ErrProtocol, frame.Type)

			return 0, r.err
		}
	}

	n := copy(p, r.pending)
	r.pending = r.pending[n:]

	return n, nil
}

// drain consumes whatever is left of this operation's frames, reporting a
// stream that is no longer where this end thinks it is.
func (r *frameReader) drain() error {
	for !r.done {
		frame, err := r.session.decoder.Read()
		if err != nil {
			r.done, r.err = true, fmt.Errorf("draining a tree: %w", err)

			return r.err
		}

		if frame.Op != r.op {
			// Reported, not swallowed. A frame for another operation arriving
			// mid-drain means the stream is ALREADY desynced, and consuming
			// it silently threw away that operation's opening frame — leaving
			// the orchestrator waiting forever for a reply to a command
			// nobody kept, the exact hazard upload()'s own comment names.
			r.done, r.err = true, fmt.Errorf("%w: a type %d frame for operation %d arrived while draining operation %d",
				wire.ErrProtocol, frame.Type, frame.Op, r.op)

			return r.err
		}

		if frame.Type == wire.FrameEnd {
			r.done = true
		}
	}

	return nil
}

// dataWriter chunks whatever is written to it into FrameData frames.
type dataWriter struct {
	session *session
	op      uint32
	buf     []byte
}

func (s *session) dataWriter(op uint32) *dataWriter {
	return &dataWriter{session: s, op: op, buf: make([]byte, 0, wire.DataChunkBytes)}
}

func (w *dataWriter) Write(p []byte) (int, error) {
	written := len(p)

	for len(p) > 0 {
		room := wire.DataChunkBytes - len(w.buf)
		if room > len(p) {
			room = len(p)
		}

		w.buf = append(w.buf, p[:room]...)
		p = p[room:]

		if len(w.buf) == wire.DataChunkBytes {
			err := w.Flush()
			if err != nil {
				return 0, err
			}
		}
	}

	return written, nil
}

// Flush emits whatever is buffered. Chunking rather than writing straight
// through is what leaves room between frames for a cancel to be heard: a
// single frame the size of a tree would have to finish before anything else
// could be read.
func (w *dataWriter) Flush() error {
	if len(w.buf) == 0 {
		return nil
	}

	err := w.session.sendData(wire.FrameData, w.op, w.buf)
	if err != nil {
		return err
	}

	w.buf = w.buf[:0]

	return nil
}

// streamWriter forwards a running command's output as it arrives.
type streamWriter struct {
	session   *session
	op        uint32
	frameType wire.FrameType
}

func (w streamWriter) Write(p []byte) (int, error) {
	written := len(p)

	// One frame per write, capped: os/exec hands over whatever a read
	// returned, and a command that writes a megabyte in one call would
	// otherwise exceed the frame limit.
	for len(p) > 0 {
		chunk := p
		if len(chunk) > wire.DataChunkBytes {
			chunk = chunk[:wire.DataChunkBytes]
		}

		err := w.session.sendData(w.frameType, w.op, chunk)
		if err != nil {
			return 0, err
		}

		p = p[len(chunk):]
	}

	return written, nil
}
