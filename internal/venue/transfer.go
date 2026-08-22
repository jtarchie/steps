package venue

// Moving the step's tree, in both directions.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jtarchie/steps/internal/wire"
)

// upload sends the step's whole materialized tree once, when the session opens.
//
// Whole rather than declared-inputs-only: the tree is already exactly what the
// step is supposed to see — internal/workspace built it from the declared
// inputs and nothing else — so filtering again here would be a second, drifting
// opinion about the same question.
func (s *session) upload() error {
	op := s.nextOp()

	err := s.encoder.Write(wire.Frame{Type: wire.FrameUpload, Op: op})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	writer := &chunkWriter{encoder: s.encoder, op: op}

	err = wire.PackTree(writer, s.cwd)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	err = writer.flush()
	if err != nil {
		return err
	}

	err = s.encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: op})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// fetch brings the named directories back into the local tree.
//
// It runs after EVERY command rather than once at the end of the step, and
// that timing is the whole point. A task's assert: files: is checked against
// the local step directory inside the retry loop, long before the workspace
// captures anything — so a design that synced only at capture time would make
// every assertion on a placed step fail with "does not exist". After a Run*
// method returns, the local tree reflects the worker, exactly as it would have
// if the command had run here.
func (s *session) fetch() error {
	if len(s.outputs) == 0 {
		return nil
	}

	op := s.nextOp()

	err := s.write(wire.Frame{Type: wire.FrameFetch, Op: op}, wire.Fetch{Paths: s.outputs})
	if err != nil {
		return err
	}

	// Clear each destination first, the same way workspace.Capture removes
	// before it materializes: an output is replaced by what the command
	// produced, not merged with what was there. It also keeps the unpack's
	// refusal to overwrite meaningful — nothing it writes can already exist.
	for _, name := range s.outputs {
		err = os.RemoveAll(filepath.Join(s.cwd, name))
		if err != nil {
			return fmt.Errorf("clearing %q before fetching it back: %w", name, err)
		}
	}

	return s.receive(op)
}

// receive unpacks one operation's data frames into the local tree.
func (s *session) receive(op uint32) error {
	reader, writer := io.Pipe()

	var (
		wg        sync.WaitGroup
		unpackErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		unpackErr = wire.UnpackTree(reader, s.cwd)
		// Closing with the error stops the frame loop below from blocking on a
		// pipe nobody is reading any more.
		_ = reader.CloseWithError(unpackErr)
	}()

	readErr := s.pump(op, writer)

	_ = writer.Close()

	wg.Wait()

	if readErr != nil {
		return readErr
	}

	if unpackErr != nil {
		return fmt.Errorf("unpacking what the worker sent back: %w", unpackErr)
	}

	return nil
}

// pump forwards this operation's data frames into w until the transfer ends.
func (s *session) pump(op uint32, w io.Writer) error {
	for {
		frame, err := s.read()
		if err != nil {
			return err
		}

		if frame.Op != op {
			return fmt.Errorf("%w: a type %d frame for operation %d arrived during operation %d",
				wire.ErrProtocol, frame.Type, frame.Op, op)
		}

		switch frame.Type {
		case wire.FrameEnd:
			return nil
		case wire.FrameData:
			_, err = w.Write(frame.Payload)
			if err != nil {
				return fmt.Errorf("%w", err)
			}
		case wire.FrameHello, wire.FrameHelloOK, wire.FrameUpload, wire.FrameExec,
			wire.FrameStdout, wire.FrameStderr, wire.FrameExit, wire.FrameFetch,
			wire.FrameCancel, wire.FrameError, wire.FrameBye:
			return fmt.Errorf("%w: a type %d frame interrupted a transfer", wire.ErrProtocol, frame.Type)
		}
	}
}

// chunkWriter turns writes into bounded data frames, so a cancel can be heard
// between chunks rather than queueing behind a whole tree.
type chunkWriter struct {
	encoder *wire.Encoder
	op      uint32
	buf     []byte
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	written := len(p)

	for len(p) > 0 {
		room := wire.DataChunkBytes - len(w.buf)
		if room > len(p) {
			room = len(p)
		}

		w.buf = append(w.buf, p[:room]...)
		p = p[room:]

		if len(w.buf) == wire.DataChunkBytes {
			err := w.flush()
			if err != nil {
				return 0, err
			}
		}
	}

	return written, nil
}

func (w *chunkWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}

	err := w.encoder.Write(wire.Frame{Type: wire.FrameData, Op: w.op, Payload: w.buf})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	w.buf = w.buf[:0]

	return nil
}
