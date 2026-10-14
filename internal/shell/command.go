package shell

import (
	"context"
	"github.com/jtarchie/steps/internal/redact"
	"io"
	"log/slog"

	"github.com/jtarchie/steps/internal/events"
)

// Command is one command and what to do with it: four independent settings
// every Runner honours the same way, so the combinations live in the helpers
// below rather than in a method per combination on every runner.
type Command struct {
	// Script runs through `sh -c`.
	Script string
	// Stdin wires this process's stdin through. Off, the command reads
	// /dev/null: a model-directed command inheriting an interactive stdin
	// (cat with no args, a tool prompting for input) blocks until the step's
	// timeout instead of getting EOF. An ssh:// worker cannot honour it — its
	// stdin carries the step's env, never the operator's terminal.
	Stdin bool
	// Stream sends the named streams live to events.Stdout/Stderr, prefixed
	// when WithLabel was used.
	Stream Streams
	// Capture keeps both streams in the Result. Limit caps each one while the
	// command RUNS, not afterwards, so a runaway command never fully buffers
	// before being discarded: zero is unbounded; past it the overflow is
	// dropped behind a "... [truncated N bytes]" marker (internal/agent's
	// truncateToolOutput format), or, with SpillDir set, written in full to a
	// file under SpillDir and replaced by a pointer message — see spillWriter.
	// The writer is always on this machine, wherever the command runs, so a
	// spill file is a path a later read_file resolves here.
	Capture  bool
	Limit    int
	SpillDir string
	// ExitAsData reports a nonzero exit, even a signal-killed one, in
	// Result.Code rather than as an error; only a command that never started
	// (no container, an unreachable worker, a bad cwd) is an error then, with
	// Result.Code -1. Off, any nonzero exit is an error whose chain satisfies
	// IsExitError, and the Result still carries whatever was captured.
	//
	// A caller that must tell "incomplete because ctx was cancelled" from an
	// ordinary exit code asks CanceledError afterwards: a killed process is
	// data here, by design.
	ExitAsData bool
}

// Streams names which of a command's output streams go out live.
type Streams uint8

// The streams a Command can stream live.
const (
	StreamStdout Streams = 1 << iota
	StreamStderr

	StreamBoth = StreamStdout | StreamStderr
)

// Result is what a command left behind.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Run streams both streams live with stdin attached; any nonzero exit is an
// error.
func Run(ctx context.Context, r Runner, script string) error {
	_, err := r.Exec(ctx, Command{Script: script, Stdin: true, Stream: StreamBoth})

	return err //nolint:wrapcheck // the runner's error is the command's
}

// RunCapture returns stdout while streaming stderr live, stdin attached; any
// nonzero exit is an error. The captured output is logged at debug on both
// outcomes, so a failing check/out command's output is not lost to the terse
// "exit status N".
func RunCapture(ctx context.Context, r Runner, script string) ([]byte, error) {
	res, err := r.Exec(ctx, Command{Script: script, Stdin: true, Stream: StreamStderr, Capture: true})
	if err != nil {
		return nil, err //nolint:wrapcheck // the runner's error is the command's
	}

	return []byte(res.Stdout), nil
}

// RunCaptureFull captures both streams unbounded and silently, with no stdin,
// reporting a nonzero exit as data — for a when: guard, whose output is not
// user-facing on its own.
func RunCaptureFull(ctx context.Context, r Runner, script string) (stdout, stderr string, exitCode int, err error) {
	return RunCaptureFullLimited(ctx, r, script, 0, "")
}

// RunCaptureFullLimited is RunCaptureFull bounded by maxBytes and spillDir;
// see Command.Limit.
func RunCaptureFullLimited(ctx context.Context, r Runner, script string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return captureFull(ctx, r, Command{Script: script, Capture: true, Limit: maxBytes, SpillDir: spillDir, ExitAsData: true})
}

// RunCaptureFullLimitedStreamed is RunCaptureFullLimited, also streaming both
// streams live: run_shell and custom tools, whose model-directed commands
// were otherwise invisible until the agent's final answer, and a task judged
// by assert:/fix:, which needs its output both live and in hand.
func RunCaptureFullLimitedStreamed(ctx context.Context, r Runner, script string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return captureFull(ctx, r, Command{Script: script, Stream: StreamBoth, Capture: true, Limit: maxBytes, SpillDir: spillDir, ExitAsData: true})
}

func captureFull(ctx context.Context, r Runner, cmd Command) (stdout, stderr string, exitCode int, err error) {
	res, err := r.Exec(ctx, cmd)

	return res.Stdout, res.Stderr, res.Code, err //nolint:wrapcheck // the runner's error is the command's
}

// Output is where one running command's two streams land on this end, built
// from a Command so every runner — and a venue running the command on another
// machine — streams, captures and bounds it identically.
type Output struct {
	Stdout, Stderr io.Writer

	stdout, stderr *Capture
	flushes        []func()
}

// Output wires the command's streams for this run.
func (c Command) Output(ctx context.Context, label string) *Output {
	o := &Output{}
	o.Stdout, o.stdout = o.sink(c, StreamStdout, label, events.Stdout(ctx))
	o.Stderr, o.stderr = o.sink(c, StreamStderr, label, events.Stderr(ctx))

	return o
}

func (o *Output) sink(c Command, which Streams, label string, dst io.Writer) (io.Writer, *Capture) {
	writers := make([]io.Writer, 0, 2)

	if c.Stream&which != 0 {
		live, flush := prefixedStream(label, dst)
		writers = append(writers, live)
		o.flushes = append(o.flushes, flush)
	}

	var capture *Capture
	if c.Capture {
		capture = NewCapture(c.Limit, c.SpillDir)
		writers = append(writers, capture)
	}

	switch len(writers) {
	case 0:
		return io.Discard, nil
	case 1:
		return writers[0], capture
	default:
		return io.MultiWriter(writers...), capture
	}
}

// Flush emits whatever partial line a live stream is still holding.
func (o *Output) Flush() {
	for _, flush := range o.flushes {
		flush()
	}
}

// Result is what was captured, with code.
func (o *Output) Result(code int) Result {
	res := Result{Code: code}

	if o.stdout != nil {
		res.Stdout = o.stdout.Result()
	}

	if o.stderr != nil {
		res.Stderr = o.stderr.Result()
	}

	return res
}

// logFinished records a finished command at debug. Output is included only
// when a failure would otherwise lose it: exit-as-error, stdout not streamed.
func logFinished(ctx context.Context, msg string, cmd Command, res Result, attrs ...any) {
	attrs = append(attrs, "command", redact.Command(cmd.Script), "exit_code", res.Code)

	if cmd.Capture && !cmd.ExitAsData && cmd.Stream&StreamStdout == 0 {
		attrs = append(attrs, "output_bytes", len(res.Stdout), "output", redact.Command(res.Stdout), "stderr", redact.Command(res.Stderr))
	}

	slog.DebugContext(ctx, msg, attrs...)
}
