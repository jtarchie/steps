// Package hostenv is the environment a host-executed command is allowed to
// see, and how long a cancelled one is waited on. Stdlib only, split out of
// internal/shell so the shim applies the same trust boundary on a worker
// without linking shell's docker engine client.
package hostenv

import (
	"os"
	"strings"
	"time"
)

// allowlist is the fixed set of environment variable names a
// host-executed command (resource check/in/out, task run:, an agent's
// run_shell/custom tools, an mcp_servers: stdio server's subprocess) is
// allowed to see. Everything else the steps process itself was started with
// — most importantly every configured agent's api_key_env secret and any
// other credential an operator happens to have exported (cloud credentials,
// tokens, etc.) — is deliberately not passed through: DockerRunner already
// starts every containerized command from the image's own env with no host
// variables at all (see shell's docker.go), and this brings the default host path to
// the same trust boundary instead of silently handing every pipeline-defined
// command, and by extension any LLM directing run_shell/a custom tool/a
// stdio MCP server, read access to the operator's full environment.
//
// A host-executed command that relies on any other exported variable —
// including SSH_AUTH_SOCK for git-over-ssh — opts it back in by name via
// the pipeline's env: (see With). Build metadata (see shell.BuildMetadata) is
// added on top of this baseline. SSH_AUTH_SOCK is deliberately not
// in the baseline: the socket grants signing with every key the operator's
// agent holds, which is a credential capability, not plumbing.
//
//nolint:gochecknoglobals // static, read-only allowlist
var allowlist = map[string]bool{
	"PATH": true,
	"HOME": true,
	// Locale/terminal — affect command output formatting, not secrets.
	"LANG": true, "LC_ALL": true, "LC_CTYPE": true, "LC_MESSAGES": true, "TERM": true,
	// Temp/user identity — needed by common CLI tools (mktemp, git, ssh).
	"TMPDIR": true, "TMP": true, "TEMP": true, "USER": true, "LOGNAME": true, "SHELL": true,
	// Proxy configuration — operational routing, not credentials, and
	// commonly required in restricted network environments.
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// Env returns the subset of the current process's environment allowed
// to reach a host-executed command (see allowlist), in os.Environ's
// "KEY=VALUE" form so it can be assigned directly to exec.Cmd.Env. Exported
// so internal/mcp's stdio transport can apply the same trust boundary to an
// mcp_servers: subprocess.
func Env() []string {
	return With(nil)
}

// WithValues is Env plus variables whose values are supplied
// rather than resolved from this process's environment — the shape a venue
// needs, where the pipeline's env: was resolved on the orchestrator and the
// command runs on a machine that never had those variables set.
//
// The baseline still comes from THIS process, deliberately: PATH, HOME and
// TMPDIR belong to the machine the command runs on, and carrying the
// orchestrator's across would be wrong in the cases where it differed at all.
// Only the explicitly named values travel.
func WithValues(values map[string]string) []string {
	baseline := With(nil)
	env := make([]string, 0, len(baseline)+len(values))

	// Supplied values FIRST, baseline after, because os/exec keeps the last
	// duplicate. Appending them the other way round meant naming any
	// allowlisted variable in env: — PATH, HOME, TMPDIR, USER, SHELL, LANG —
	// replaced the worker's with the orchestrator's, and a macOS HOME or
	// TMPDIR names directories a Linux worker does not have, so mktemp, git
	// and ssh break on them.
	//
	// Silent, too: naming a baseline variable is a genuine no-op on the local
	// path, where both values come from one machine. It only misbehaves once a
	// step is placed.
	//
	// A variable outside the baseline — which is what env: is for — is
	// unaffected: nothing later shadows it.
	for name, value := range values {
		env = append(env, name+"="+value)
	}

	return append(env, baseline...)
}

// With is Env plus the variables a pipeline's env: named
// explicitly (see shell.NewRunner). A named variable that isn't set in the steps
// process's environment contributes nothing rather than an empty value: the
// two are different to a command that tests for presence, and inventing an
// empty one would turn "the operator forgot to export it" into a silent
// misconfiguration instead of the command's own clear failure.
func With(extra []string) []string {
	opted := make(map[string]bool, len(extra))
	for _, name := range extra {
		opted[name] = true
	}

	full := os.Environ()
	allowed := make([]string, 0, len(full))

	for _, kv := range full {
		key, _, ok := strings.Cut(kv, "=")
		if ok && (allowlist[key] || opted[key]) {
			allowed = append(allowed, kv)
		}
	}

	return allowed
}

// CancelWaitDelay bounds how long a cancelled command is waited on after its
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
const CancelWaitDelay = 2 * time.Second
