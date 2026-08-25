package venue

// Moving the step's tree, in both directions.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/wire"
)

// upload sends the step's whole materialized tree once, when the session opens.
//
// Whole rather than declared-inputs-only: the tree is already exactly what the
// step is supposed to see — internal/workspace built it from the declared
// inputs and nothing else — so filtering again here would be a second, drifting
// opinion about the same question.
func (s *session) upload(ctx context.Context) error {
	stop := s.watchTransfer(ctx)
	defer stop()

	op := s.nextOp()

	err := s.writeEmpty(wire.Frame{Type: wire.FrameUpload, Op: op})
	if err != nil {
		return err
	}

	// The upload runs AFTER a successful hello, so a transport that dies under
	// it marks the conversation broken and the next command redials — the
	// same footing as a death mid-command. broke is what carries that marking
	// out of the chunked writes.
	writer := &chunkWriter{encoder: s.encoder, op: op, broke: func() { s.broken.Store(true) }}

	err = s.packTree(writer)
	if err != nil {
		return err
	}

	err = writer.flush()
	if err != nil {
		return err
	}

	err = s.writeEmpty(wire.Frame{Type: wire.FrameEnd, Op: op})
	if err != nil {
		return err
	}

	// Wait for the worker to say it took the tree. A refusal — a full disk, a
	// read-only scratch, an entry it will not write — arrives as an error
	// frame that read turns into a Go error, and the session fails here
	// rather than at the first command. It matters that this is BEFORE the
	// exec: a step's command has real side effects, and running it against a
	// tree the far end rejected is worse than any transfer failure.
	frame, err := s.read()
	if err != nil {
		return err
	}

	if frame.Type != wire.FrameEnd || frame.Op != op {
		return fmt.Errorf("%w: the worker answered a type %d frame for operation %d instead of acknowledging the tree",
			wire.ErrProtocol, frame.Type, frame.Op)
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
func (s *session) fetch(ctx context.Context) error {
	stop := s.watchTransfer(ctx)
	defer stop()

	if len(s.outputs) == 0 {
		return nil
	}

	op := s.nextOp()

	err := s.write(wire.Frame{Type: wire.FrameFetch, Op: op}, wire.Fetch{Paths: s.outputs})
	if err != nil {
		return err
	}

	// Staged, then swapped. Removing the destinations first and unpacking over
	// them would destroy the step's outputs the moment a transfer died — a
	// dropped connection, a truncated stream — and the retry that follows
	// would then re-upload the emptied tree and fail for an unrelated reason.
	// Local execution has no such window, and placement must not add one.
	//
	// Inside cwd deliberately: the swap below is a rename, which needs to stay
	// on one filesystem.
	staging, err := os.MkdirTemp(s.cwd, ".steps-fetch-")
	if err != nil {
		return fmt.Errorf("staging the fetched outputs: %w", err)
	}

	defer func() { _ = os.RemoveAll(staging) }()

	err = s.receive(op, staging)
	if err != nil {
		return err
	}

	return s.swapFetched(staging)
}

// swapFetched moves each fetched output into place, replacing what was there.
//
// Per output rather than all at once: an output the worker sent nothing for is
// left alone rather than emptied, which is what a step that declared an output
// and produced nothing already means everywhere else — a fact reported where
// outputs are checked, not a reason to delete the previous one here.
func (s *session) swapFetched(staging string) error {
	for _, name := range s.outputs {
		src := filepath.Join(staging, name)

		_, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil {
			return fmt.Errorf("fetching %q back: %w", name, err)
		}

		dst := filepath.Join(s.cwd, name)

		err = os.RemoveAll(dst)
		if err != nil {
			return fmt.Errorf("clearing %q before replacing it: %w", name, err)
		}

		err = os.Rename(src, dst)
		if err != nil {
			return fmt.Errorf("replacing %q with what the worker sent: %w", name, err)
		}
	}

	return nil
}

// receive unpacks one operation's data frames into the local tree.
func (s *session) receive(op uint32, into string) error {
	reader, writer := io.Pipe()

	var (
		wg        sync.WaitGroup
		unpackErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		unpackErr = s.unpackTree(reader, into)
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

// packTree writes the step's tree into w, through the negotiated compression.
func (s *session) packTree(w io.Writer) error {
	err := compress.Pack(w, s.compression == wire.CompressionZstd, func(w io.Writer) error {
		return wire.PackTree(w, s.cwd)
	})
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// unpackTree reads one tar stream into a directory, through the negotiated
// compression.
func (s *session) unpackTree(r io.Reader, into string) error {
	//nolint:wrapcheck // receive wraps with the transfer's own context
	return compress.Unpack(r, s.compression == wire.CompressionZstd, func(r io.Reader) error {
		return wire.UnpackTree(r, into) //nolint:wrapcheck // receive wraps with the transfer's own context
	})
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
	// broke reports an encoder failure to the session, because a write that
	// died mid-upload is the transport gone, not the tree unreadable. nil
	// when nobody is listening.
	broke func()
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
		if w.broke != nil {
			w.broke()
		}

		return fmt.Errorf("%w", err)
	}

	w.buf = w.buf[:0]

	return nil
}
