package venue

// One command, from the frame that asks for it to the frame that ends it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// run asks the worker for one command and pumps its output until it exits.
//
// Cancellation is sent as a frame rather than a signal: sshd does not forward
// signals to an exec channel, so the only thing that reliably reaches a remote
// process is a message it is already listening for. The frame names the
// operation, because a cancel races the exit it was trying to prevent — without
// the id, a cancel for a command that already finished kills the next one.
func (s *session) run(ctx context.Context, command string, sinks outputSinks) (wire.Exit, error) {
	op := s.nextOp()

	err := s.write(wire.Frame{Type: wire.FrameExec, Op: op}, wire.Exec{Command: command, Env: s.env})
	if err != nil {
		return wire.Exit{}, err
	}

	// Watching from here rather than checking between frames: the read below
	// blocks, and a cancel that only landed between frames would wait for a
	// command that has stopped producing output.
	stop := s.watchCancel(ctx, op)
	defer stop()

	// failed remembers a sink that could not take the output. The READ goes
	// on regardless, for the reason pump() gives on the transfer path: the
	// shim is still sending this operation's frames, so returning early would
	// leave them for the next command to meet as a protocol violation — the
	// session poisoned by its own tidiness, for a local failure the worker had
	// nothing to do with.
	var failed error

	// One flag per sink, not one for both. A shared flag meant a stdout that
	// could not be written stopped recording STDERR as well — and on a step
	// that is failing, that is the only account of why it did.
	var dropped droppedSinks

	for {
		// Drain notices are absorbed here rather than ending the command: the
		// machine has minutes left and the work may well finish in them.
		frame, err := s.awaitOperationFrame()
		if err != nil {
			return wire.Exit{}, err
		}

		if frame.Op != op {
			return wire.Exit{}, s.desync("a type %d frame for operation %d arrived during operation %d",
				frame.Type, frame.Op, op)
		}

		exit, done, err := deliver(frame, sinks, &dropped)
		if err != nil {
			// A protocol failure means the stream is already not where this
			// end thinks it is, so reading on would compound it — and the
			// session must not be handed to the next command, which would
			// read this operation's leftovers as its own answer.
			if !errors.Is(err, errSink) {
				if errors.Is(err, wire.ErrProtocol) {
					s.broken.Store(true)
				}

				return wire.Exit{}, err
			}

			failed = err
		}

		if done {
			if failed != nil {
				return wire.Exit{}, failed
			}

			return exit, nil
		}
	}
}

// errSink is a failure writing a command's output on THIS end — a closed
// stdout, a full spill directory — as opposed to a failure on the wire. The
// distinction is what lets run keep reading to the exit frame.
var errSink = errors.New("writing a command's output")

// droppedSinks records which of a command's two sinks has already failed, so
// the payload for that one is read off the wire and dropped rather than
// retried against it — while the other keeps recording.
type droppedSinks struct{ stdout, stderr bool }

// deliver routes one of a running command's frames, reporting whether it ended
// the command.
func deliver(frame wire.Frame, sinks outputSinks, dropped *droppedSinks) (wire.Exit, bool, error) {
	switch frame.Type {
	case wire.FrameStdout:
		return wire.Exit{}, false, writeStream(sinks.stdout, frame.Payload, &dropped.stdout)
	case wire.FrameStderr:
		return wire.Exit{}, false, writeStream(sinks.stderr, frame.Payload, &dropped.stderr)
	case wire.FrameExit:
		var exit wire.Exit

		err := decode(frame, &exit)
		if err != nil {
			return wire.Exit{}, false, err
		}

		return exit, true, nil
	case wire.FrameHello, wire.FrameHelloOK, wire.FrameUpload, wire.FrameExec,
		wire.FrameFetch, wire.FrameData, wire.FrameEnd, wire.FrameCancel,
		wire.FrameError, wire.FrameBye, wire.FrameDraining,
		wire.FrameDockerOpen, wire.FrameDockerData, wire.FrameDockerClose, wire.FrameNeed:
		return wire.Exit{}, false, fmt.Errorf("%w: a type %d frame interrupted a command", wire.ErrProtocol, frame.Type)
	default:
		return wire.Exit{}, false, fmt.Errorf("%w: unknown frame type %d", wire.ErrProtocol, frame.Type)
	}
}

func writeStream(sink io.Writer, payload []byte, dropped *bool) error {
	if *dropped {
		return nil
	}

	_, err := sink.Write(payload)
	if err != nil {
		*dropped = true

		return fmt.Errorf("%w: %w", errSink, err)
	}

	return nil
}

// watchCancel sends a cancel frame if ctx ends before the command does, and
// closes the connection if that frame goes unanswered.
//
// The cancel alone is only as good as the worker: it asks a shim that is still
// listening to stop, which is the ordinary case. A worker that has wedged — a
// stalled disk, a hung child, a network still holding the TCP connection open
// with nothing moving over it — answers nothing, and the reads it is being
// waited on are synchronous ReadFulls with no deadline. So timeout:, fail_fast,
// race: and Ctrl-C would each end nothing at all: the step would sit on that
// read for as long as the machine stayed up.
func (s *session) watchCancel(ctx context.Context, op uint32) func() {
	return s.watch(ctx, &op)
}

// watchTransfer is the same watchdog for a tree crossing the wire, which has
// no cancel frame to send: the shim is mid-transfer, not mid-command. A
// transfer that stalls parks the build just as thoroughly as a command that
// does — the upload's acknowledgement and every fetch are reads with the same
// absent deadline.
func (s *session) watchTransfer(ctx context.Context) func() {
	return s.watch(ctx, nil)
}

// watch ends a blocked read once ctx does.
//
// Closing the reader is what makes the read return. Same bound and the same
// reasoning as a local command's WaitDelay: ask first, and if the far end has
// stopped listening, stop waiting on it.
func (s *session) watch(ctx context.Context, cancelOp *uint32) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	// Captured now rather than read in the goroutine: close() nils the field
	// under the mutex, and this runs without it.
	tr := s.transport

	go func() {
		defer close(stopped)

		select {
		case <-ctx.Done():
			if cancelOp != nil {
				// Best effort: a worker already gone cannot be told to stop,
				// and the read side will report that first. Through the
				// session's writer, not the raw encoder: the docker relay put
				// other goroutines on this encoder, and wire.Encoder stamps a
				// shared header and payload buffer.
				_ = s.writeFrame(wire.Frame{Type: wire.FrameCancel, Op: *cancelOp})
			}
		case <-done:
			return
		}

		grace := time.NewTimer(shell.CancelWaitDelay)
		defer grace.Stop()

		select {
		case <-done:
		case <-grace.C:
			if tr != nil && tr.interrupt != nil {
				tr.interrupt()
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}
