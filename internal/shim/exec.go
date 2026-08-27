package shim

// Running one command, the way HostRunner runs one.
//
// Every choice here is made to keep placement from changing meaning. The
// command string is handed to `sh -c` unaltered, the environment is filtered
// through the same allowlist, and a nonzero exit is reported as data rather
// than as a failure of the transfer — so a step that passes here passes there,
// and one that fails fails the same way.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// startExec begins a command and returns without waiting for it.
//
// Two things here are load-bearing and easy to get backwards.
//
// The command runs on its own goroutine, so the session keeps reading frames
// while it works. A shim that ran commands inline could not read a cancel
// until the command it was meant to stop had already finished — which fails
// silently, as a cancelled step that takes as long as whatever it started.
//
// The cancel is registered BEFORE that goroutine starts, not inside it.
// Registering from the goroutine loses a race the orchestrator can easily win:
// a cancel sent immediately after the exec — a step already past its deadline,
// a race: whose sibling already answered — arrives to find nothing registered
// and is dropped.
func (s *session) startExec(ctx context.Context, frame wire.Frame) error {
	if s.workdir == "" {
		return errUnopened
	}

	var request wire.Exec

	err := wire.DecodeJSON(frame, &request)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	s.beginCommand(frame.Op, cancel)

	s.running.Add(1)

	go func() {
		defer s.running.Done()
		defer cancel()

		exit := s.runCommand(runCtx, frame.Op, request)

		s.endCommand(frame.Op)

		sendErr := s.send(wire.FrameExit, frame.Op, exit)
		if sendErr != nil {
			// Nowhere left to report it: the session that would have carried
			// the message is the thing that failed. The orchestrator sees the
			// transport die, which is the truth.
			return
		}
	}()

	return nil
}

func (s *session) runCommand(ctx context.Context, op uint32, request wire.Exec) wire.Exit {
	// `sh -c`, exactly as HostRunner spells it. An embedded interpreter would
	// make a tagged step mean something subtly different from the same step
	// untagged — different builtins, different subshell semantics, different
	// $PATH resolution — and the difference would only ever show up on the
	// machine hardest to debug on.
	command := exec.CommandContext(ctx, "sh", "-c", request.Command) //nolint:gosec // running the pipeline's command is the whole job
	command.Dir = s.workdir
	command.Env = shell.HostEnvWithValues(request.Env)
	command.Stdout = streamWriter{session: s, op: op, frameType: wire.FrameStdout}
	command.Stderr = streamWriter{session: s, op: op, frameType: wire.FrameStderr}

	// Same bound and the same reason as HostRunner's: killing `sh -c "sleep 5;
	// echo done"` kills the shell, not the sleep it forked, and a surviving
	// grandchild still holds the output pipe.
	command.WaitDelay = shell.CancelWaitDelay

	err := command.Run()
	if err == nil {
		return wire.Exit{Started: true, Code: 0}
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Started and then failed, including killed by a signal, which
		// ExitCode reports as -1 — the same sentinel the orchestrator would
		// have seen for a local kill.
		return wire.Exit{Started: true, Code: exitErr.ExitCode()}
	}

	// Never launched: a bad working directory, no `sh`, a permission problem.
	// The orchestrator turns this into an infrastructure error rather than a
	// verdict, which is what stops an unreachable machine from reading as a
	// guard that said no.
	return wire.Exit{Started: false, Code: -1, Reason: err.Error()}
}

// beginCommand records what a cancel for op should stop.
func (s *session) beginCommand(op uint32, cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancels == nil {
		s.cancels = map[uint32]context.CancelFunc{}
	}

	s.cancels[op] = cancel
}

// endCommand clears the registration for op, and only that one.
//
// Per-operation for cancelRunning's reason: the frame loop keeps reading while
// a command runs, so a second exec can register before the first finishes. A
// single registration made each of these deregister the OTHER command, and a
// cancel aimed at it then found nothing and was dropped — timeout:, fail_fast
// and race: ending nothing while the step ran to completion on the worker.
func (s *session) endCommand(op uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.cancels, op)
}

// cancelRunning stops the command belonging to op, and only that one.
//
// The op check is the whole point: a cancel races the exit it was trying to
// prevent. Without it, a cancel sent for a command that has already finished
// arrives while the next command is running and kills that one instead —
// which looks exactly like a flaky step.
func (s *session) cancelRunning(op uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cancel, running := s.cancels[op]; running {
		cancel()
	}
}
