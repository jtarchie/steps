// Package shell runs pipeline-defined commands via `sh -c`, either on the
// host (HostRunner) or inside a container (DockerRunner, see docker.go).
package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs pipeline-defined commands, either on the host or inside a
// container, against the working directory it was constructed with (see
// NewRunner) — binding cwd at construction, rather than taking it on every
// call, means a runner reused across many calls (an agent conversation's
// repeated run_shell calls, a task's fix-loop re-runs) resolves/validates
// its working directory once, not once per call.
type Runner interface {
	// Run streams stdout/stderr live; any nonzero exit is a Go error.
	Run(ctx context.Context, command string) error
	// RunCapture captures stdout while also streaming stderr live; any
	// nonzero exit is a Go error.
	RunCapture(ctx context.Context, command string) ([]byte, error)
	// RunCaptureFull captures stdout/stderr separately and never streams;
	// a normal nonzero exit is reported as data (exitCode), not an error —
	// only a failure to start the command returns a non-nil error.
	RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error)
	// RunCaptureFullLimited behaves exactly like RunCaptureFull, except each
	// of stdout/stderr is capped at maxBytes while the command is running —
	// not just truncated afterward — so a runaway command's output never
	// fully buffers into memory before being discarded (or, with spillDir
	// set, before being streamed to disk). When spillDir is "", a stream that
	// exceeds maxBytes gets a trailing "... [truncated N bytes]" marker,
	// matching internal/agent's truncateToolOutput format, and the overflow
	// is dropped. When spillDir is a directory path, a stream that exceeds
	// maxBytes is instead written in full to a new file under spillDir (up
	// to spillMaxBytes, beyond which the file itself is marked truncated the
	// same way), and the returned string is a short pointer message naming
	// the file, its size, and a head preview — see spillWriter.
	RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error)
	// RunCaptureFullLimitedStreamed behaves exactly like RunCaptureFullLimited,
	// except stdout/stderr are ALSO streamed live (prefixed, when WithLabel
	// was used) while being captured — unlike every other RunCaptureFull*
	// variant, which never streams. Used only by run_shell/custom tools
	// (internal/agent's shellToolResult): a model-directed command was
	// previously invisible until the agent's final text response, which this
	// fixes. RunCaptureFull/RunCaptureFullLimited stay silent for their other
	// callers (a when: guard, an assert-mode task's own command) where the
	// command's output isn't meant to be user-facing on its own.
	RunCaptureFullLimitedStreamed(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error)
	// WithLabel returns a copy of the runner that prefixes every line of
	// whatever it streams live (Run, RunCapture's stderr,
	// RunCaptureFullLimitedStreamed) with "[label] ". A zero-value/unset
	// label (the default from NewRunner) streams unprefixed, byte-identical
	// to before this method existed.
	WithLabel(label string) Runner
	// Close releases whatever the runner holds open — for a DockerRunner,
	// the step's container. Every caller of NewRunner must call it (see
	// CloseRunner), including on the failure path: it is what keeps a step
	// from stranding a container. A HostRunner has nothing to release and
	// returns nil. Closing a runner that never ran a command is valid, as is
	// closing twice; a copy made by WithLabel shares one underlying session,
	// so closing either closes both.
	Close() error
}

// CloseRunner is a best-effort cleanup helper for the deferred Close calls
// every NewRunner caller owes: a cleanup failure must never mask the original
// error already being returned, so it is only logged. Mirrors
// internal/workspace's CloseBuild/CloseSpace precedent.
func CloseRunner(r Runner, label string) {
	err := r.Close()
	if err != nil {
		slog.Error("shell.runner_close", "label", label, "error", err)
	}
}

// NewRunner returns a DockerRunner scoped to image and cwd, or a HostRunner
// when image is empty — the single decision point every shell-out caller
// (task, agent, resource) funnels through. For a DockerRunner, cwd is
// resolved and validated once here (see resolveMountPath); an empty cwd is
// valid (resource check: has none) and mounts nothing. HostRunner never
// errors — cwd needs no resolution for host execution. The returned runner
// has no label (see WithLabel) until a caller that wants prefixed output
// opts in explicitly.
func NewRunner(image, cwd string) (Runner, error) {
	if image == "" {
		return HostRunner{cwd: cwd}, nil
	}

	var resolvedCwd string

	if cwd != "" {
		var err error

		resolvedCwd, err = resolveMountPath(cwd)
		if err != nil {
			return nil, fmt.Errorf("resolve working directory %q: %w", cwd, err)
		}
	}

	return DockerRunner{
		Image:   image,
		session: &dockerSession{image: image, resolvedCwd: resolvedCwd},
	}, nil
}

// captureWriter accumulates one stdout/stderr stream from a running command
// into a final string — used as cmd.Stdout/cmd.Stderr directly, so bytes are
// bounded (or not) as they arrive, never after the fact.
type captureWriter interface {
	io.Writer
	result() string
}

// spillMaxBytes bounds how much of an overflowing stream spillWriter will
// write to disk — a disk-exhaustion guard against a runaway model-directed
// command, the same reason boundedWriter caps memory. A file that hits this
// cap gets the same trailing "... [truncated N bytes]" marker a dropped
// boundedWriter overflow gets, just applied to the file instead of memory.
const spillMaxBytes = 10 << 20 // 10 MiB

// SpillPreviewBytes is how much of a spilled stream's head is echoed inline
// in the pointer message, so the model has some immediate signal without
// having to open the file first. Exported so internal/agent's one-shot spill
// helper (for MCP/sub-agent/handoff/fix output, which already holds its full
// content as a string rather than streaming it) uses the same preview size as
// spillWriter's own streaming spill path — one pointer-message shape across
// every spilled-output site, not two that can drift apart.
const SpillPreviewBytes = 2000

// newCaptureWriter returns an unboundedWriter when maxBytes <= 0 (matching
// every Run/RunCapture/RunCaptureFull caller's historical unbounded-buffer
// behavior exactly). Otherwise it returns a boundedWriter (truncate and
// drop the overflow) when spillDir is "", or a spillWriter (stream the
// overflow to a file under spillDir) when spillDir is set — both used only
// by RunCaptureFullLimited.
func newCaptureWriter(maxBytes int, spillDir string) captureWriter {
	if maxBytes <= 0 {
		return &unboundedWriter{}
	}

	if spillDir == "" {
		return &boundedWriter{max: maxBytes}
	}

	return &spillWriter{max: maxBytes, dir: spillDir}
}

// unboundedWriter is a plain, uncapped capture.
type unboundedWriter struct {
	buf bytes.Buffer
}

func (w *unboundedWriter) Write(p []byte) (int, error) { return w.buf.Write(p) } //nolint:wrapcheck // bytes.Buffer.Write never errors

func (w *unboundedWriter) result() string { return w.buf.String() }

// boundedWriter caps retained bytes at max while still tracking every byte
// offered, so result can report the true overflow via a trailing marker
// matching internal/agent's truncateToolOutput format. It never errors or
// short-writes (io.Writer's contract, and what exec.Cmd's output copiers
// require): overflow bytes are silently discarded, not rejected.
type boundedWriter struct {
	buf   bytes.Buffer
	max   int
	total int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	n := len(p) // the real input length: io.Writer requires this back even when we retain less
	w.total += n

	if remaining := w.max - w.buf.Len(); remaining > 0 {
		if remaining < len(p) {
			p = p[:remaining]
		}

		w.buf.Write(p)
	}

	return n, nil
}

func (w *boundedWriter) result() string {
	s := w.buf.String()

	if overflow := w.total - w.buf.Len(); overflow > 0 {
		s += fmt.Sprintf("\n... [truncated %d bytes]", overflow)
	}

	return s
}

// spillWriter buffers up to max bytes in head, exactly like boundedWriter,
// but on overflow streams the FULL stream (head plus everything after,
// itself capped at spillMaxBytes to bound disk use) to a new file under dir
// instead of dropping it. result() then returns a short pointer message —
// the file's path, the stream's true total size, and a head preview —
// rather than the raw content, so a caller (an agent's run_shell/custom
// tool) can hand the model something it can act on (grep/read the file)
// instead of silently losing whatever was truncated.
//
// It never errors or short-writes, matching boundedWriter's io.Writer
// contract; if creating or writing the spill file fails partway through, it
// degrades to boundedWriter's drop-the-overflow behavior and notes the
// failure in result().
type spillWriter struct {
	max       int
	dir       string
	total     int
	head      bytes.Buffer
	file      *os.File
	filePath  string
	fileBytes int
	spillErr  error
}

func (w *spillWriter) Write(p []byte) (int, error) {
	n := len(p) // the real input length: io.Writer requires this back even when we retain/write less
	w.total += n

	p = w.bufferHead(p)

	if w.file == nil && w.spillErr == nil && w.total > w.max {
		w.beginSpill()
	}

	if w.file != nil && len(p) > 0 {
		w.writeToFile(p)
	}

	return n, nil
}

// bufferHead appends as much of p as still fits within max bytes of head
// (a no-op once head is already full or spilling has begun/failed) and
// returns whatever of p didn't fit, for the caller to spill instead.
func (w *spillWriter) bufferHead(p []byte) []byte {
	if w.file != nil || w.spillErr != nil {
		return p
	}

	remaining := w.max - w.head.Len()
	if remaining <= 0 {
		return p
	}

	toBuffer := p
	if remaining < len(toBuffer) {
		toBuffer = toBuffer[:remaining]
	}

	w.head.Write(toBuffer)

	return p[len(toBuffer):]
}

// beginSpill creates the spill file and flushes the already-buffered head
// (the first max bytes of the stream) to it, called exactly once per
// spillWriter, the first time a Write pushes total past max. A failure here
// (can't create/write the file — a full or read-only spill dir) is recorded
// in spillErr rather than returned: spillWriter, like boundedWriter, must
// never turn an io.Writer failure into a broken command capture — it just
// degrades to boundedWriter's drop-the-overflow behavior instead.
func (w *spillWriter) beginSpill() {
	f, err := os.CreateTemp(w.dir, "output-*.txt")
	if err != nil {
		w.spillErr = fmt.Errorf("create spill file: %w", err)

		return
	}

	_, err = f.Write(w.head.Bytes())
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name()) // don't leave a half-created spill file behind on the degrade path

		w.spillErr = fmt.Errorf("write spill file: %w", err)

		return
	}

	w.file = f
	w.filePath = f.Name()
	w.fileBytes = w.head.Len()
}

// writeToFile appends p to the already-open spill file, capping the file's
// total size at spillMaxBytes — a disk-exhaustion guard, mirroring why
// boundedWriter caps memory. Bytes beyond the cap are silently dropped, like
// boundedWriter's overflow; result reports that drop via the trailing marker.
func (w *spillWriter) writeToFile(p []byte) {
	if w.spillErr != nil || w.fileBytes >= spillMaxBytes {
		return
	}

	remaining := spillMaxBytes - w.fileBytes
	if remaining < len(p) {
		p = p[:remaining]
	}

	if len(p) == 0 {
		return
	}

	_, err := w.file.Write(p)
	if err != nil {
		w.spillErr = err

		return
	}

	w.fileBytes += len(p)
}

func (w *spillWriter) result() string {
	if w.file == nil {
		return w.resultFromHead()
	}

	return w.resultFromFile()
}

// resultFromHead is result() when Write never crossed max (nothing spilled)
// or spilling itself failed (spillErr set) — the exact same shape
// boundedWriter.result returns, plus a note when a spill was attempted and
// failed.
func (w *spillWriter) resultFromHead() string {
	s := w.head.String()

	overflow := w.total - w.head.Len()
	if overflow <= 0 {
		return s
	}

	s += fmt.Sprintf("\n... [truncated %d bytes]", overflow)

	if w.spillErr != nil {
		s += fmt.Sprintf(" (could not save full output: %s)", w.spillErr)
	}

	return s
}

// resultFromFile is result() once spilling succeeded: it appends a
// truncation marker to the file if spillMaxBytes cut it short, closes it,
// and returns the pointer message the model actually sees in place of the
// raw content — via the same SpillPointerMessage format every other spilled-
// output path (internal/agent's one-shot spillOrTruncate) uses.
func (w *spillWriter) resultFromFile() string {
	overflow := w.total - w.fileBytes
	if overflow > 0 {
		_, err := fmt.Fprintf(w.file, "\n... [truncated %d bytes]", overflow)
		if err != nil {
			w.spillErr = err
		}
	}

	path := w.filePath

	_ = w.file.Close()

	preview := w.head.Bytes()
	if len(preview) > SpillPreviewBytes {
		preview = preview[:SpillPreviewBytes]
	}

	return SpillPointerMessage(w.total, path, preview)
}

// SpillPointerMessage renders the message a spilled-to-file tool result
// returns to the model in place of raw content: the true size, the file it
// was saved to, and a preview of its head. Shared by spillWriter (streaming
// command output, via resultFromFile) and internal/agent's one-shot
// spillOrTruncate (MCP tool text/structured content, a sub-agent's final
// answer, a previous_run response/note/trajectory arg, a fix loop's failure
// output — all of which already hold their full content as a string rather
// than streaming it), so every spilled-output path produces byte-identical
// wording rather than each inventing its own.
func SpillPointerMessage(totalBytes int, path string, preview []byte) string {
	msg := fmt.Sprintf(
		"<persistent_file>\nThe requested content was %d bytes. The content was saved to %s. Please read from it directly, obviously not the whole file. After this will be the first %s of the output.\n</persistent_file>",
		totalBytes, path, FormatBytes(len(preview)),
	)

	if len(preview) > 0 {
		msg += "\n\n" + string(preview)
	}

	return msg
}

// FormatBytes renders n as a human-readable size (bytes, KB, or MB with one
// decimal place) for the spill pointer message. Exported so internal/agent's
// one-shot spill helper renders the same units/precision SpillPointerMessage
// itself uses.
func FormatBytes(n int) string {
	const (
		kb = 1024
		mb = kb * 1024
	)

	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// HostRunner runs commands directly on the host via `sh -c`, with cwd as
// its working directory.
type HostRunner struct {
	cwd   string
	label string
}

// WithLabel returns a copy of h that prefixes its live-streamed output —
// see the Runner interface doc.
func (h HostRunner) WithLabel(label string) Runner {
	h.label = label

	return h
}

// Close satisfies Runner. Host execution holds nothing open.
func (HostRunner) Close() error { return nil }

// hostEnvAllowlist is the fixed set of environment variable names a
// host-executed command (resource check/in/out, task run:, an agent's
// run_shell/custom tools, an mcp_servers: stdio server's subprocess) is
// allowed to see. Everything else the steps process itself was started with
// — most importantly every configured agent's api_key_env secret and any
// other credential an operator happens to have exported (cloud credentials,
// tokens, etc.) — is deliberately not passed through: DockerRunner already
// starts every containerized command from the image's own env with no host
// variables at all (see docker.go), and this brings the default host path to
// the same trust boundary instead of silently handing every pipeline-defined
// command, and by extension any LLM directing run_shell/a custom tool/a
// stdio MCP server, read access to the operator's full environment.
//
// This is a real (if narrow) behavior change: a host-executed command that
// previously relied on some other exported variable (GOFLAGS, an assumed
// GOPATH override, a custom cache directory, ...) for legitimate,
// non-secret configuration will no longer see it. There is currently no
// pass-through mechanism for a pipeline to opt a specific variable back in;
// that would need its own config surface (and merkle-hash implications)
// rather than belonging to this fix.
//
//nolint:gochecknoglobals // static, read-only allowlist
var hostEnvAllowlist = map[string]bool{
	"PATH": true,
	"HOME": true,
	// Locale/terminal — affect command output formatting, not secrets.
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "LC_MESSAGES": true, "TERM": true,
	// Temp/user identity — needed by common CLI tools (mktemp, git, ssh).
	"TMPDIR": true, "TMP": true, "TEMP": true, "USER": true, "LOGNAME": true, "SHELL": true,
	// SSH agent forwarding — needed for git-over-ssh, not a secret itself
	// (it's a socket path; the actual key material never touches env).
	"SSH_AUTH_SOCK": true,
	// Proxy configuration — operational routing, not credentials, and
	// commonly required in restricted network environments.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// HostEnv returns the subset of the current process's environment allowed
// to reach a host-executed command (see hostEnvAllowlist), in os.Environ's
// "KEY=VALUE" form so it can be assigned directly to exec.Cmd.Env. Exported
// so internal/mcp's stdio transport can apply the same trust boundary to an
// mcp_servers: subprocess.
func HostEnv() []string {
	full := os.Environ()
	allowed := make([]string, 0, len(full))

	for _, kv := range full {
		key, _, ok := strings.Cut(kv, "=")
		if ok && hostEnvAllowlist[key] {
			allowed = append(allowed, kv)
		}
	}

	return allowed
}

// wrapIfCanceled ensures err's chain satisfies errors.Is(_, ctx.Err()) when
// ctx was canceled by the time the command producing err finished. Go's
// os/exec only reliably surfaces a canceled context in cmd.Run's own
// returned error when the process happened to exit 0 despite cancellation
// (see exec.Cmd.Wait's watchCtx handling) — the far more common case (the
// process dies non-zero, or is killed by the cancellation itself) leaves no
// trace of *why* in Go's own error. Callers that need to tell "this command
// was interrupted by shutdown" apart from "this command just failed on its
// own" must go through this rather than trusting err alone. Only call this
// when err is already non-nil — it must never turn a successful command into
// a failure just because ctx happens to be canceled by the time it returns.
func wrapIfCanceled(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return err
	}

	return fmt.Errorf("%w: %w", ctxErr, err)
}

// CanceledError returns a non-nil error wrapping ctx.Err() when ctx is
// canceled, or nil otherwise. RunCaptureFull/RunCaptureFullLimited
// deliberately report even a signal-killed process as data (a normal
// nonzero exit), not a Go error, so their own err return can't be used to
// detect "this result may be incomplete/unreliable because ctx was
// canceled while the command ran." A caller that needs that distinction —
// to tell a shutdown-interrupted step apart from a step that genuinely
// failed on its own — calls CanceledError immediately after such a call
// returns, instead.
func CanceledError(ctx context.Context) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil
	}

	return fmt.Errorf("context canceled: %w", ctxErr)
}

// exitCodeOf extracts a process's exit code from cmd.Run's error (0 for a
// nil error). A signal-killed process (e.g. one cut off by a context
// timeout) is also an *exec.ExitError, and Go reports its ExitCode() as -1 —
// the same sentinel a never-started process would need. So -1 here is
// ambiguous by itself; callers that must tell "ran but was killed" apart
// from "never started at all" use processStarted, not this return value.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}

	return -1
}

// processStarted reports whether cmd.Run's error indicates the process
// actually started — and then exited nonzero or was killed by a signal — as
// opposed to never starting at all (bad cwd, permission error, missing
// interpreter). Both cases can produce exitCodeOf(err) == -1, so this is the
// only reliable way to distinguish them.
func processStarted(err error) bool {
	if err == nil {
		return true
	}

	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}

// Run runs command via `sh -c command` with h.cwd as its working directory,
// streaming stdout/stderr live to the terminal, prefixed per line when
// WithLabel was used.
func (h HostRunner) Run(ctx context.Context, command string) error {
	slog.Debug("shell.run", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.WaitDelay = cancelWaitDelay
	cmd.Dir = h.cwd
	cmd.Env = HostEnv()
	cmd.Stdin = os.Stdin

	stdoutW, flushStdout := prefixedStream(h.label, os.Stdout)
	stderrW, flushStderr := prefixedStream(h.label, os.Stderr)
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW

	err := cmd.Run()
	flushStdout()
	flushStderr()

	slog.Debug("shell.run", "command", command, "cwd", h.cwd, "exit_code", exitCodeOf(err))

	if err != nil {
		return fmt.Errorf("command %q failed: %w", command, wrapIfCanceled(ctx, err))
	}

	return nil
}

// RunCapture runs command via `sh -c command` with h.cwd as its working
// directory, capturing stdout and stderr while also streaming stderr live,
// prefixed per line when WithLabel was used. The captured output is logged
// (at debug level) on both success and failure, so a failing check/out
// command's output is available for debugging — previously it was discarded
// the moment the command exited nonzero, leaving only the terse "exit status
// N" from the wrapped error.
func (h HostRunner) RunCapture(ctx context.Context, command string) ([]byte, error) {
	slog.Debug("shell.capture", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.WaitDelay = cancelWaitDelay
	cmd.Dir = h.cwd
	cmd.Env = HostEnv()
	cmd.Stdin = os.Stdin

	var outBuf, errBuf bytes.Buffer

	stderrW, flushStderr := prefixedStream(h.label, os.Stderr)
	cmd.Stdout = &outBuf
	cmd.Stderr = io.MultiWriter(stderrW, &errBuf)

	err := cmd.Run()
	flushStderr()

	slog.Debug("shell.capture", "command", command, "cwd", h.cwd, "exit_code", exitCodeOf(err),
		"output_bytes", outBuf.Len(), "output", outBuf.String(), "stderr", errBuf.String())

	if err != nil {
		return nil, fmt.Errorf("command %q failed: %w", command, wrapIfCanceled(ctx, err))
	}

	return outBuf.Bytes(), nil
}

// RunCaptureFull runs command via `sh -c command` with h.cwd as its working
// directory, capturing stdout and stderr separately. Unlike Run/RunCapture
// (where any nonzero exit fails the step), a normal nonzero exit is reported
// as data via exitCode rather than a Go error — callers that need a
// command's failure to be observable data (e.g. an agent step's tool
// results) rather than a hard abort use this instead. Only a failure to
// start the process (not the process's own exit code, even a signal-killed
// one — e.g. from a canceled ctx) returns a non-nil error. A caller that
// needs to tell "this result may be incomplete because ctx was canceled
// while the command ran" apart from an ordinary exit code checks ctx.Err()
// itself (or CanceledError) after this returns, rather than relying on err —
// see runFixTask/runAssertedTask/evaluateStepGuard.
//
// Unlike Run/RunCapture, stdin is /dev/null, not the parent's: this runs
// non-interactive, model-generated commands, and inheriting an interactive
// stdin risks a command (cat with no args, a tool prompting for input)
// blocking until the step's timeout instead of getting EOF.
func (h HostRunner) RunCaptureFull(ctx context.Context, command string) (stdout, stderr string, exitCode int, err error) {
	return h.runCaptureFull(ctx, command, 0, "", false)
}

// RunCaptureFullLimited is RunCaptureFull with each stream capped at
// maxBytes (and, with spillDir set, overflow streamed to disk instead of
// dropped) while the command runs — see the Runner interface doc.
func (h HostRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return h.runCaptureFull(ctx, command, maxBytes, spillDir, false)
}

// RunCaptureFullLimitedStreamed is RunCaptureFullLimited, additionally
// streaming both stdout/stderr live (prefixed, when WithLabel was used) —
// see the Runner interface doc.
func (h HostRunner) RunCaptureFullLimitedStreamed(ctx context.Context, command string, maxBytes int, spillDir string) (stdout, stderr string, exitCode int, err error) {
	return h.runCaptureFull(ctx, command, maxBytes, spillDir, true)
}

// runCaptureFull is the shared implementation behind RunCaptureFull (maxBytes
// 0, meaning unbounded — byte-identical to before RunCaptureFullLimited
// existed), RunCaptureFullLimited (maxBytes > 0), and
// RunCaptureFullLimitedStreamed (stream true, tees both captured streams to
// os.Stdout/os.Stderr live).
func (h HostRunner) runCaptureFull(ctx context.Context, command string, maxBytes int, spillDir string, stream bool) (stdout, stderr string, exitCode int, err error) {
	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd)

	cmd := exec.CommandContext(ctx, "sh", "-c", command) //nolint:gosec // executing pipeline-defined commands is this tool's entire purpose
	cmd.WaitDelay = cancelWaitDelay
	cmd.Dir = h.cwd
	cmd.Env = HostEnv()
	cmd.Stdin = nil

	outWriter := newCaptureWriter(maxBytes, spillDir)
	errWriter := newCaptureWriter(maxBytes, spillDir)

	flushStdout, flushStderr := func() {}, func() {}

	if stream {
		var stdoutW, stderrW io.Writer

		stdoutW, flushStdout = prefixedStream(h.label, os.Stdout)
		stderrW, flushStderr = prefixedStream(h.label, os.Stderr)
		cmd.Stdout = io.MultiWriter(stdoutW, outWriter)
		cmd.Stderr = io.MultiWriter(stderrW, errWriter)
	} else {
		cmd.Stdout = outWriter
		cmd.Stderr = errWriter
	}

	runErr := cmd.Run()
	flushStdout()
	flushStderr()

	if !processStarted(runErr) {
		return "", "", -1, fmt.Errorf("command %q failed to start: %w", command, runErr)
	}

	code := exitCodeOf(runErr)

	slog.Debug("shell.capture_full", "command", command, "cwd", h.cwd, "exit_code", code)

	return outWriter.result(), errWriter.result(), code, nil
}

// IsExitError reports whether err (however wrapped) stems from a process that
// started and then exited nonzero or was killed by a signal — as opposed to an
// infrastructure failure to run it at all. Callers use it to tell a task-level
// failure (a command's own nonzero exit) apart from an errored one so hooks
// dispatch correctly; a resource check's failure, by contrast, is left
// unwrapped so it classifies as errored.
func IsExitError(err error) bool {
	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}

// RunShell runs command on the host via `sh -c command` with cwd as its
// working directory, streaming stdout/stderr live. Equivalent to
// HostRunner{cwd: cwd}.Run; kept as a package-level function for callers
// that never need containerized execution.
func RunShell(ctx context.Context, command, cwd string) error {
	return HostRunner{cwd: cwd}.Run(ctx, command)
}

// RunShellCapture runs command on the host via `sh -c command`, capturing
// stdout. Equivalent to HostRunner{cwd: cwd}.RunCapture.
func RunShellCapture(ctx context.Context, command, cwd string) ([]byte, error) {
	return HostRunner{cwd: cwd}.RunCapture(ctx, command)
}

// RunShellCaptureFull runs command on the host via `sh -c command`,
// capturing stdout/stderr separately and reporting a normal nonzero exit as
// data. Equivalent to HostRunner{cwd: cwd}.RunCaptureFull.
func RunShellCaptureFull(ctx context.Context, command, cwd string) (stdout, stderr string, exitCode int, err error) {
	return HostRunner{cwd: cwd}.RunCaptureFull(ctx, command)
}

// cancelWaitDelay bounds how long a cancelled command is waited on after its
// own process has been killed.
//
// Killing `sh -c "sleep 5; echo done"` kills the shell, not the `sleep` it
// forked — and a surviving grandchild still holds the stdout pipe, so
// cmd.Wait would block on the I/O copy until that grandchild exits on its own.
// A cancelled step would then take as long as whatever it started, which
// defeats every feature built on cancellation: fail_fast, race:, and Ctrl-C.
//
// ponytail: the complete fix is a process group per command (Setpgid, then
// kill the negative pid), which reaps grandchildren too. That is unix-only and
// wants build-tagged files; this bounds the damage portably in one line until
// something needs the difference between "2 seconds late" and "immediate".
const cancelWaitDelay = 2 * time.Second
