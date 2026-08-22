// Package shim is the remote half of a tagged step.
//
// Nobody types it. The orchestrator pushes this binary to a worker, execs it
// as `steps _shim`, and the process's stdin and stdout ARE the protocol — so
// nothing on this path may write to stdout except frames.
//
// It knows nothing about pipelines: no config, no store, no workspace, no
// merkle. That ignorance is the contract. A worker runs a binary and a
// command in a directory; every decision about what the command means, what
// its output is worth, and whether the result may be cached stays on the
// orchestrator, where the pipeline is.
package shim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/jtarchie/steps/internal/wire"
)

// Options configure one served session.
type Options struct {
	// Build is this binary's content hash, echoed back so the orchestrator can
	// prove the shim it reached is the one it pushed.
	Build string
	// Root is where session scratch directories are made. Empty takes the
	// system temp dir.
	Root string
}

// Serve runs one session over a byte pipe until the peer says goodbye or goes
// away.
//
// It takes a reader and a writer rather than a connection because the transport
// is not its business: today that is this process's stdin and stdout at the far
// end of an SSH exec channel, and a later venue that hands it an accepted
// socket does not change a line of what happens here.
func Serve(ctx context.Context, in io.Reader, out io.Writer, opts Options) (err error) {
	session := &session{
		decoder: wire.NewDecoder(in),
		encoder: wire.NewEncoder(out),
		opts:    opts,
	}

	defer func() {
		cleanupErr := session.cleanup()
		if err == nil {
			err = cleanupErr
		}
	}()

	return session.run(ctx)
}

type session struct {
	decoder *wire.Decoder
	// mu guards encoder: a command's output is pumped from its own goroutines
	// while the main loop may answer a cancel, and two writers interleaving
	// mid-frame would corrupt the stream rather than merely reorder it.
	mu      sync.Mutex
	encoder *wire.Encoder

	opts    Options
	workdir string
	keep    bool

	// cancel stops the command belonging to op, under mu. Both are nil when no
	// command is running.
	cancel   context.CancelFunc
	cancelOp uint32
	// running tracks commands still in flight, so a session cannot tear its
	// scratch out from under one on the way out.
	running sync.WaitGroup
}

func (s *session) run(ctx context.Context) error {
	// A command outlives the frame that asked for it (see handle), so the
	// session must not return — and cleanup must not run — while one is still
	// writing to the tree it is about to remove.
	defer s.running.Wait()

	for {
		frame, err := s.decoder.Read()
		if errors.Is(err, io.EOF) {
			// The orchestrator is gone. This is the ordinary end of a session
			// — and, when it arrives mid-step, the only cancellation that
			// reliably works: sshd does not forward signals to an exec
			// channel, so a killed or disconnected orchestrator is heard here
			// as a closed stdin and nowhere else. The deferred cleanup takes
			// the scratch directory with it.
			return nil
		}

		if err != nil {
			return fmt.Errorf("shim: %w", err)
		}

		done, err := s.handle(ctx, frame)
		if err != nil {
			// An operation that failed is reported and the session continues:
			// the orchestrator decides whether one bad step ends the run. A
			// transport error, by contrast, has already returned above.
			sendErr := s.send(wire.FrameError, frame.Op, wire.Error{Message: err.Error()})
			if sendErr != nil {
				return sendErr
			}

			continue
		}

		if done {
			return nil
		}
	}
}

func (s *session) handle(ctx context.Context, frame wire.Frame) (bool, error) {
	switch frame.Type {
	case wire.FrameHello:
		return false, s.hello(frame)
	case wire.FrameUpload:
		return false, s.upload(frame.Op)
	case wire.FrameExec:
		return false, s.startExec(ctx, frame)
	case wire.FrameFetch:
		return false, s.fetch(frame)
	case wire.FrameCancel:
		s.cancelRunning(frame.Op)

		return false, nil
	case wire.FrameBye:
		return true, nil
	case wire.FrameHelloOK, wire.FrameStdout, wire.FrameStderr, wire.FrameExit,
		wire.FrameData, wire.FrameEnd, wire.FrameError:
		// Frames the shim sends, never receives.
		return false, fmt.Errorf("%w: a shim cannot answer a type %d frame", wire.ErrProtocol, frame.Type)
	default:
		return false, fmt.Errorf("%w: unknown frame type %d", wire.ErrProtocol, frame.Type)
	}
}

func (s *session) hello(frame wire.Frame) error {
	var hello wire.Hello

	err := wire.DecodeJSON(frame, &hello)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if hello.Protocol != wire.Protocol {
		return fmt.Errorf("%w: orchestrator speaks protocol %d, this shim speaks %d — the pushed binary is not the one that pushed it",
			wire.ErrProtocol, hello.Protocol, wire.Protocol)
	}

	s.keep = hello.Keep

	root := s.opts.Root
	if root == "" {
		root = os.TempDir()
	}

	// Named after the session rather than randomly, so a scratch directory
	// left behind by a crash says which run to blame.
	s.workdir = filepath.Join(root, "steps-shim", hello.Session, "work")

	err = os.MkdirAll(s.workdir, 0o700)
	if err != nil {
		return fmt.Errorf("making the work directory: %w", err)
	}

	return s.send(wire.FrameHelloOK, frame.Op, wire.HelloOK{
		Protocol: wire.Protocol,
		Build:    s.opts.Build,
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Workdir:  s.workdir,
	})
}

// upload reads FrameData until FrameEnd, unpacking into the work directory as
// the bytes arrive rather than staging them: a step's tree can be larger than
// the worker's memory, and there is nothing to gain by holding it twice.
func (s *session) upload(op uint32) error {
	if s.workdir == "" {
		return errUnopened
	}

	reader, done := s.dataReader(op)
	defer done()

	err := wire.UnpackTree(reader, s.workdir)
	if err != nil {
		return fmt.Errorf("unpacking the step tree: %w", err)
	}

	return nil
}

func (s *session) fetch(frame wire.Frame) error {
	if s.workdir == "" {
		return errUnopened
	}

	var fetch wire.Fetch

	err := wire.DecodeJSON(frame, &fetch)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	writer := s.dataWriter(frame.Op)

	err = wire.PackPaths(writer, s.workdir, fetch.Paths)
	if err != nil {
		return fmt.Errorf("packing the step outputs: %w", err)
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return s.sendEmpty(wire.FrameEnd, frame.Op)
}

// errUnopened is any operation arriving before a hello. It cannot happen with
// a client this repo wrote, which is exactly why it is worth answering
// explicitly rather than dereferencing an empty path.
var errUnopened = errors.New("no session: the orchestrator sent an operation before its hello")

func (s *session) send(frameType wire.FrameType, op uint32, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.WriteJSON(frameType, op, payload)
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

func (s *session) sendEmpty(frameType wire.FrameType, op uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.Write(wire.Frame{Type: frameType, Op: op})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

func (s *session) sendData(frameType wire.FrameType, op uint32, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.encoder.Write(wire.Frame{Type: frameType, Op: op, Payload: payload})
	if err != nil {
		return fmt.Errorf("shim: %w", err)
	}

	return nil
}

// cleanup removes the session's scratch. It runs on every exit path, including
// the orchestrator vanishing mid-step, because a directory left on someone
// else's machine is the one failure nobody local ever sees.
func (s *session) cleanup() error {
	if s.workdir == "" || s.keep {
		return nil
	}

	// The parent, not the work directory: the session owns
	// <root>/steps-shim/<session>, and leaving the empty shell behind would
	// accumulate one directory per run forever.
	err := os.RemoveAll(filepath.Dir(s.workdir))
	if err != nil {
		return fmt.Errorf("removing the session scratch: %w", err)
	}

	return nil
}
