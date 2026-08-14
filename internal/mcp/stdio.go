package mcp

import (
	"bytes"
	"context"
	"log/slog"
	"os/exec"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// stdioWaitDelay bounds how long exec.Cmd.Wait (called by the SDK's
// CommandTransport.Close, via session.Close) may block on the subprocess's
// stderr pipe after the process itself has exited. It is NOT the terminate
// grace period — that's CommandTransport.TerminateDuration (SDK default:
// 5s). Wiring cmd.Stderr to a non-*os.File writer (stderrLogger, below)
// makes os/exec create an internal pipe and copy goroutine, and Wait blocks
// until that copy sees EOF. A server like gopls spawns its own `go`
// subprocesses that inherit the stderr fd; if one survives its parent, the
// pipe never closes and Wait — i.e. Close, i.e. a step's own teardown —
// would hang forever without this.
//
// It has to be SHORTER than that terminate grace period, not equal to it.
// The SDK's Close escalates on a 5s timer — close stdin, wait, SIGTERM,
// wait, SIGKILL, wait — and then gives up with "unresponsive subprocess". A
// delay of 5s ties that final wait, so whether teardown of a forking server
// reports an error came down to which timer fired first, for a subprocess
// that had in fact already died. Two seconds always wins, and matches what
// internal/shell picked for the same job.
const stdioWaitDelay = 2 * time.Second

// commandTransport builds the SDK transport for a stdio (command:) server:
// an explicit argv subprocess (never a shell — Args is never passed through
// sh -c), spawned with the same filtered environment every other
// host-executed command in this codebase gets (see shell.HostEnv), and
// bound to ctx so it dies with the caller's step/call rather than
// outliving it.
func commandTransport(ctx context.Context, srv config.MCPServer) *sdkmcp.CommandTransport {
	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...) //nolint:gosec // running a pipeline-defined mcp server is this feature's purpose; explicit argv, never a shell
	cmd.Dir = srv.Cwd                                         // "" leaves the steps process's own cwd inherited
	cmd.Env = shell.HostEnv()                                 // same trust boundary as every other host-executed command
	cmd.Stderr = &stderrLogger{server: srv.Name}
	cmd.WaitDelay = stdioWaitDelay
	setProcessGroup(cmd)

	slog.Debug("mcp.stdio.spawn", "server", srv.Name, "command", srv.Command, "args", srv.Args, "cwd", srv.Cwd)

	return &sdkmcp.CommandTransport{Command: cmd}
}

// stderrLoggerMaxLine caps a stderrLogger's partial-line buffer so a server
// that emits a huge line with no newline can't grow it without bound.
const stderrLoggerMaxLine = 8 << 10

// stderrLogger turns a stdio mcp server's stderr into newline-delimited
// slog debug records tagged with the server name. Debug level, deliberately
// — command/output logging is opt-in (--log-level=debug) and off by
// default — but discarding stderr
// entirely, which is what the SDK does if cmd.Stderr is left unset, makes a
// server that fails to start (e.g. a missing dependency) undiagnosable.
//
// Only os/exec's single internal copy goroutine ever calls Write, and
// cmd.Wait (via Close) synchronizes after that goroutine finishes, so no
// locking is needed here.
type stderrLogger struct {
	server string
	buf    bytes.Buffer
}

func (w *stderrLogger) Write(p []byte) (int, error) {
	_, _ = w.buf.Write(p)

	for {
		idx := bytes.IndexByte(w.buf.Bytes(), '\n')
		if idx < 0 {
			break
		}

		line := w.buf.Next(idx + 1)
		slog.Debug("mcp.stdio.stderr", "server", w.server, "line", string(bytes.TrimRight(line, "\n")))
	}

	if w.buf.Len() > stderrLoggerMaxLine {
		slog.Debug("mcp.stdio.stderr", "server", w.server, "line", w.buf.String())
		w.buf.Reset()
	}

	return len(p), nil
}
