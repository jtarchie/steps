package venue

// The six Runner methods, over one wire operation.
//
// They differ only in what this end does with the bytes — stream them, keep
// them, bound them — and in whether a nonzero exit is an error or a fact.
// DockerRunner already proved that shape: five public methods over one private
// dockerExec. Anything else would be six chances for the remote path to
// disagree with the local one about what a command did.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// runner executes a step's commands on a worker. It holds the session by
// pointer so a WithLabel copy shares one worker rather than opening a second.
type runner struct {
	label   string
	session *session
}

// plan is what this end does with one command's output.
type plan struct {
	streamStdout bool
	streamStderr bool
	capture      bool
	maxBytes     int
	spillDir     string
}

// Run streams live and treats any nonzero exit as an error.
func (r runner) Run(ctx context.Context, command string) error {
	_, _, err := r.execute(ctx, command, plan{streamStdout: true, streamStderr: true})

	return err
}

// RunStreamedCapture is Run, keeping what it streamed.
func (r runner) RunStreamedCapture(ctx context.Context, command string, maxBytes int) (string, string, error) {
	return r.execute(ctx, command, plan{streamStdout: true, streamStderr: true, capture: true, maxBytes: maxBytes})
}

// RunCapture keeps stdout and streams stderr live.
func (r runner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	stdout, _, err := r.execute(ctx, command, plan{streamStderr: true, capture: true})
	if err != nil {
		return nil, err
	}

	return []byte(stdout), nil
}

// RunCaptureFull reports a nonzero exit as data.
func (r runner) RunCaptureFull(ctx context.Context, command string) (string, string, int, error) {
	return r.executeFull(ctx, command, plan{capture: true})
}

// RunCaptureFullLimited is RunCaptureFull with each stream bounded.
func (r runner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (string, string, int, error) {
	return r.executeFull(ctx, command, plan{capture: true, maxBytes: maxBytes, spillDir: spillDir})
}

// RunCaptureFullLimitedStreamed is RunCaptureFullLimited that also streams.
func (r runner) RunCaptureFullLimitedStreamed(ctx context.Context, command string, maxBytes int, spillDir string) (string, string, int, error) {
	return r.executeFull(ctx, command, plan{
		streamStdout: true, streamStderr: true, capture: true, maxBytes: maxBytes, spillDir: spillDir,
	})
}

// WithLabel is entirely local: it changes how this end prints what the worker
// sent, and nothing crosses the wire.
func (r runner) WithLabel(label string) shell.Runner {
	r.label = label

	return r
}

// Close releases the worker.
func (r runner) Close() error { return r.session.close() }

// ReclaimedBy reports whether the worker a runner used said it was definitely
// going away, and what it said.
//
// Asked of a finished runner, because the two-minute window means a step
// often succeeds on a machine that is being reclaimed — and nothing about
// that success says so. A caller holding the machine for later steps needs to
// know to let it go.
//
// A runner that is not placed answers false, so a caller need not ask whether
// it is talking to a venue.
func ReclaimedBy(runner shell.Runner) (string, bool) {
	placed, ok := runner.(interface{ reclaimed() (string, bool) })
	if !ok {
		return "", false
	}

	return placed.reclaimed()
}

// reclaimed is ReclaimedBy's implementation, unexported so the question is
// asked through the package function rather than by type-asserting a shape.
func (r runner) reclaimed() (string, bool) { return r.session.reclaimedBy() }

// execute is the error-returning half: a nonzero exit becomes a Go error, as
// it does for Run, RunStreamedCapture and RunCapture.
func (r runner) execute(ctx context.Context, command string, p plan) (string, string, error) {
	stdout, stderr, runErr := r.exchange(ctx, command, p)
	if runErr != nil {
		return stdout, stderr, fmt.Errorf("command %q failed: %w", command, shell.WrapIfCanceled(ctx, runErr))
	}

	return stdout, stderr, nil
}

// executeFull is the exit-as-data half. It returns an error only when the
// command never ran, which is the distinction every RunCaptureFull* caller
// depends on — a guard that could not run is not a guard that said no.
func (r runner) executeFull(ctx context.Context, command string, p plan) (string, string, int, error) {
	stdout, stderr, runErr := r.exchange(ctx, command, p)
	if runErr == nil {
		return stdout, stderr, 0, nil
	}

	if !shell.IsExitError(runErr) {
		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	return stdout, stderr, exitCodeOf(runErr), nil
}

// exchange runs one command and returns its streams, plus nil, a
// *shell.ExitError, or an infrastructure error.
func (r runner) exchange(ctx context.Context, command string, p plan) (outText, errText string, err error) {
	// Every failure out of this call is re-read once the work is done: a
	// worker that announced its own end turns an infrastructure failure into
	// ErrEvicted, which the pipeline retries without spending the author's
	// attempts: budget. Nothing else about the classification changes — an
	// ExitError is still the command's own verdict, drained or not.
	defer func() { err = r.asEviction(err) }()

	err = r.session.ensure(ctx)
	if err != nil {
		return "", "", err
	}

	stdout, stderr, sinks := r.sinks(p)

	exit, err := r.session.run(ctx, command, sinks)

	sinks.flush()

	if err != nil {
		return stdout.result(), stderr.result(), err
	}

	// Before returning, not at capture time: an assert: on this step is
	// checked against the local tree the moment this call returns.
	err = r.session.fetch(ctx)
	if err != nil {
		return stdout.result(), stderr.result(), err
	}

	return stdout.result(), stderr.result(), r.runError(command, exit)
}

// asEviction re-reads a failure as an eviction when the worker had already
// said it was definitely going away.
//
// The line is what the command's exit code MEANS. A command that ran and
// chose a nonzero status said something about the step, and a machine
// disappearing afterwards does not unsay it — re-running there would repeat
// work whose answer was already given. But a reclaimed instance runs its
// shutdown, init signals the command, and the shim reports that as an exit
// with code -1 (see internal/shim's exec: a signalled command reports the
// same sentinel os/exec uses locally). That is the machine ending the
// command, not the command answering, and it is the SHAPE A REAL EVICTION
// USUALLY TAKES — a session dying with no exit frame at all is the rarer
// case. So a signalled exit on a worker under a reclamation notice is
// infrastructure; every other exit stays the step's own verdict.
func (r runner) asEviction(err error) error {
	if err == nil {
		return nil
	}

	reason, reclaimed := r.session.reclaimedBy()
	if !reclaimed {
		return err
	}

	if shell.IsExitError(err) && exitCodeOf(err) != shell.SignalledExitCode {
		return err
	}

	if reason == "" {
		reason = "no reason given"
	}

	return fmt.Errorf("%w (%s): %w", ErrEvicted, reason, err)
}

// runError turns the worker's answer into the error shape the pipeline reads.
func (r runner) runError(command string, exit wire.Exit) error {
	if !exit.Started {
		// Deliberately NOT a shell.ExitError. guard.go turns a plain error into
		// "the guard command could not run" and fails the step; an ExitError
		// here would let an unreachable worker read as a guard that answered
		// no, and silently skip the work it was gating.
		return fmt.Errorf("worker %q: command %q never started: %s", r.session.worker, command, exit.Reason)
	}

	if exit.Code == 0 {
		return nil
	}

	return &shell.ExitError{Command: command, Venue: r.session.worker.String(), Code: exit.Code}
}

// stream is one of a command's two output streams on this end.
type stream struct {
	capture *shell.Capture
	writer  io.Writer
	flush   func()
}

func (s stream) result() string {
	if s.capture == nil {
		return ""
	}

	return s.capture.Result()
}

// sinks builds the two streams a plan asks for.
func (r runner) sinks(p plan) (stdout, stderr stream, out outputSinks) {
	stdout = r.stream(p.streamStdout, p.capture, p.maxBytes, p.spillDir, os.Stdout)
	stderr = r.stream(p.streamStderr, p.capture, p.maxBytes, p.spillDir, os.Stderr)

	return stdout, stderr, outputSinks{stdout: stdout.writer, stderr: stderr.writer, flushes: []func(){stdout.flush, stderr.flush}}
}

func (r runner) stream(live, capture bool, maxBytes int, spillDir string, dst io.Writer) stream {
	s := stream{flush: func() {}}

	writers := make([]io.Writer, 0, 2)

	if live {
		w, flush := shell.NewPrefixedStream(r.label, dst)
		writers = append(writers, w)
		s.flush = flush
	}

	if capture {
		s.capture = shell.NewCapture(maxBytes, spillDir)
		writers = append(writers, s.capture)
	}

	switch len(writers) {
	case 0:
		s.writer = io.Discard
	case 1:
		s.writer = writers[0]
	default:
		s.writer = io.MultiWriter(writers...)
	}

	return s
}

// outputSinks is where a running command's frames land.
type outputSinks struct {
	stdout  io.Writer
	stderr  io.Writer
	flushes []func()
}

func (o outputSinks) flush() {
	for _, flush := range o.flushes {
		flush()
	}
}

// exitCodeOf reads the status back off an error this package produced.
func exitCodeOf(err error) int {
	var exitErr *shell.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}

	return -1
}
