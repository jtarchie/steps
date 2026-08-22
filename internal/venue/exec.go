package venue

// One command, from the frame that asks for it to the frame that ends it.

import (
	"context"
	"fmt"

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

	for {
		frame, err := s.read()
		if err != nil {
			return wire.Exit{}, err
		}

		if frame.Op != op {
			return wire.Exit{}, fmt.Errorf("%w: a type %d frame for operation %d arrived during operation %d",
				wire.ErrProtocol, frame.Type, frame.Op, op)
		}

		exit, done, err := deliver(frame, sinks)
		if err != nil {
			return wire.Exit{}, err
		}

		if done {
			return exit, nil
		}
	}
}

// deliver routes one of a running command's frames, reporting whether it ended
// the command.
func deliver(frame wire.Frame, sinks outputSinks) (wire.Exit, bool, error) {
	switch frame.Type {
	case wire.FrameStdout:
		_, err := sinks.stdout.Write(frame.Payload)
		if err != nil {
			return wire.Exit{}, false, fmt.Errorf("writing a command's output: %w", err)
		}

		return wire.Exit{}, false, nil
	case wire.FrameStderr:
		_, err := sinks.stderr.Write(frame.Payload)
		if err != nil {
			return wire.Exit{}, false, fmt.Errorf("writing a command's output: %w", err)
		}

		return wire.Exit{}, false, nil
	case wire.FrameExit:
		var exit wire.Exit

		err := decode(frame, &exit)
		if err != nil {
			return wire.Exit{}, false, err
		}

		return exit, true, nil
	case wire.FrameHello, wire.FrameHelloOK, wire.FrameUpload, wire.FrameExec,
		wire.FrameFetch, wire.FrameData, wire.FrameEnd, wire.FrameCancel,
		wire.FrameError, wire.FrameBye:
		return wire.Exit{}, false, fmt.Errorf("%w: a type %d frame interrupted a command", wire.ErrProtocol, frame.Type)
	default:
		return wire.Exit{}, false, fmt.Errorf("%w: unknown frame type %d", wire.ErrProtocol, frame.Type)
	}
}

// watchCancel sends a cancel frame if ctx ends before the command does.
func (s *session) watchCancel(ctx context.Context, op uint32) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		defer close(stopped)

		select {
		case <-ctx.Done():
			// Best effort: a worker already gone cannot be told to stop, and
			// the read side will report that first.
			_ = s.encoder.Write(wire.Frame{Type: wire.FrameCancel, Op: op})
		case <-done:
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}
