package agent

// Spawning the CLI: the argument vector that IS the permission boundary, the
// host-vs-container command, and the environment each gets.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// cliContainerHome is the container-side path a containerized CLI gets as its
// $HOME. A fixed path of our own rather than the image's user home: the
// container may run as a uid the image never heard of (see
// shell.defaultContainerUser's Linux default), so no image-defined home can be
// assumed writable.
const cliContainerHome = "/steps-home"

// cliStepHomeMode is the permission the containerized $HOME and its .claude
// subdirectory get.
//
// 0777 rather than the 0700 a private directory would normally take, because
// the process that must write here is NOT necessarily the user that created
// it: user: can name any uid, and the Linux default is the host uid:gid,
// neither of which this process can chown to without privileges it does not
// have. A 0700 directory owned by the steps user is unwritable by a
// container running as anyone else, and the CLI then fails trying to write
// its own transcript.
//
// The exposure is bounded and short: the directory is a fresh per-step temp
// dir removed when the step ends, holding only what the CLI writes there.
// The credentials file mounted into it is a separate host path with its own
// (unchanged, 0600) permissions — this mode does not touch it.
const cliStepHomeMode = 0o777

// newCLIStepHome creates the host directory a containerized CLI gets as its
// $HOME, with the .claude subdirectory ALREADY created. Pre-creating it
// host-side is load-bearing: docker creates missing bind-mount targets as
// root, and a root-owned .claude would be unwritable by the process that has
// to write its transcript into it. A directory made here arrives with a mode
// the container's uid can use, whatever that uid turns out to be.
//
// Note nothing seeds a ~/.claude.json (onboarding/trust state) into it: the
// CLI is always invoked with --print (see cliArgs), and the trust dialog is
// documented as skipped in non-interactive mode. If --print ever stopped
// being passed, a fresh $HOME would block on an interactive prompt inside a
// container nothing can answer — that connection is why this comment exists.
func newCLIStepHome() (string, error) {
	home, err := os.MkdirTemp("", "steps-cli-home-*")
	if err != nil {
		return "", fmt.Errorf("creating cli home: %w", err)
	}

	// Explicitly chmod: MkdirTemp always makes 0700, and Mkdir's mode is
	// masked by the process umask, so neither reaches the mode on its own.
	for _, dir := range []string{home, filepath.Join(home, ".claude")} {
		err = os.MkdirAll(dir, cliStepHomeMode)
		if err == nil {
			err = os.Chmod(dir, cliStepHomeMode)
		}

		if err != nil {
			_ = os.RemoveAll(home)

			return "", fmt.Errorf("creating cli home: %w", err)
		}
	}

	return home, nil
}

// buildCLICommand constructs the subprocess for one CLI attempt: the binary
// itself on the host, or a `docker run` of it when the step resolved an
// image. Everything the caller wires afterwards (stdin, stdout parsing,
// stderr, WaitDelay) is identical either way — only what the process IS
// differs.
func buildCLICommand(
	ctx context.Context, prepared preparedAgentStep, binary string, args []string, stepHome string,
) (*exec.Cmd, string, error) {
	if prepared.ri.Image == "" {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary comes from the static cliProviders table
		cmd.Dir = prepared.conv.env.dir
		cmd.Env = cliEnv(prepared.ri)

		return cmd, "", nil
	}

	resolvedCwd, err := shell.ResolveMountPath(prepared.conv.env.dir)
	if err != nil {
		return nil, "", fmt.Errorf("agent %q: resolving workspace for container: %w", prepared.ri.AgentName, err)
	}

	// Named so it can always be reclaimed. Killing the docker CLIENT does
	// nothing to the container it started, so a step that times out could
	// otherwise leave the CLI running — still spending, and still writing
	// into the bind-mounted workspace the next step is about to read. A name
	// this process generated is one it can `docker rm -f` knowing nothing
	// else could own it.
	name, err := shell.NewContainerName()
	if err != nil {
		return nil, "", fmt.Errorf("agent %q: %w", prepared.ri.AgentName, err)
	}

	spec := shell.DockerRunSpec{
		Image:       prepared.ri.Image,
		Name:        name,
		Argv:        append([]string{binary}, args...),
		ResolvedCwd: resolvedCwd,
		EnvNames:    prepared.ri.Env,
		User:        prepared.ri.User,
		Network:     prepared.ri.Network,
		Privileged:  prepared.ri.Privileged,
		CPUShares:   prepared.ri.Limits.CPUShares(),
		MemoryBytes: prepared.ri.Limits.MemoryBytes(),
		ExtraMounts: []shell.Mount{{HostPath: stepHome, ContainerPath: cliContainerHome}},
		// A literal -e HOME=value is fine precisely because a path is not a
		// secret — the value-free `-e NAME` convention exists to keep secret
		// VALUES out of the docker client's argv.
		ExtraEnv: map[string]string{"HOME": cliContainerHome},
	}

	// A subscription login lives at ~/.claude/.credentials.json on Linux (on
	// macOS it lives in the Keychain and this file simply does not exist).
	// Mount exactly that one file, read-only: the rest of the operator's
	// ~/.claude (history, transcripts, settings) is not the container's
	// business, and read-only bounds a hostile image to reading the one token
	// it was deliberately given. The cost: a token refresh cannot write back
	// through the mount, so an expired token heals on the next host-side use,
	// not here.
	if credentials, ok := hostCLICredentials(); ok {
		spec.ExtraMounts = append(spec.ExtraMounts, shell.Mount{
			HostPath:      credentials,
			ContainerPath: cliContainerHome + "/.claude/.credentials.json",
			ReadOnly:      true,
		})
	}

	// The api_key_env: value crosses under the CLI's own name, forwarded
	// value-free (`-e ANTHROPIC_API_KEY`) — the docker client's environment
	// carries it, its argv never does. The client otherwise inherits this
	// process's environment, which is what makes the pipeline env: names in
	// EnvNames resolvable at all (matching how the session container's docker
	// client behaves).
	//
	// Both halves are conditioned on the variable actually being EXPORTED,
	// not merely named. Forwarding the name alone would hand the container
	// whatever ANTHROPIC_API_KEY this process happens to have — so a pipeline
	// naming an unset api_key_env: would silently authenticate with the
	// operator's personal key instead of failing. The host path cannot do
	// that (shell.HostEnv's allowlist excludes it), and the two must agree.
	env := os.Environ()

	if key := lookupCLIKey(prepared.ri); key != "" {
		spec.EnvNames = append(append([]string{}, spec.EnvNames...), cliAPIKeyEnv)
		env = append(env, cliAPIKeyEnv+"="+key)
	}

	//nolint:gosec // running a pipeline-defined image is the point; the image is load-validated and sits after "--"
	cmd := exec.CommandContext(ctx, "docker", shell.DockerRunArgv(spec)...)
	cmd.Env = env

	// A canceled context kills the docker client, which does NOT stop the
	// container. SIGTERM first, mirroring shell.dockerCommand, so the client
	// can detach cleanly; the container itself is torn down by name, by
	// execCLI's deferred RemoveContainer.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }

	return cmd, name, nil
}

// cliAPIKeyEnv is the variable the claude CLI reads its key from, whatever
// the pipeline chose to call its own.
//
//nolint:gosec // an environment variable NAME, not a credential
const cliAPIKeyEnv = "ANTHROPIC_API_KEY"

// lookupCLIKey returns the value of the pipeline's api_key_env:, or "" when
// none was named or the named variable is not exported.
func lookupCLIKey(ri config.ResolvedInvocation) string {
	if ri.APIKeyEnv == "" {
		return ""
	}

	return os.Getenv(ri.APIKeyEnv)
}

// hostCLICredentials reports the host path of the CLI's on-disk credentials
// file, if there is one.
func hostCLICredentials() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	path := filepath.Join(home, ".claude", ".credentials.json")

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return "", false
	}

	return path, true
}

// execCLI spawns the CLI and reads its transcript off stdout as it runs.
func execCLI(
	ctx context.Context,
	prepared preparedAgentStep,
	runtime cliRuntime,
	mcpConfig string,
	plan cliAttempt,
) (cliRunResult, error) {
	binary := config.CLIBinary(prepared.ri.CLI)
	args := cliArgs(prepared, runtime, mcpConfig, plan)

	slog.Debug("agent.cli.exec", "agent", prepared.ri.AgentName, "binary", binary, "args", args,
		"dir", prepared.conv.env.dir, "image", prepared.ri.Image)

	cmd, container, err := buildCLICommand(ctx, prepared, binary, args, plan.home)
	if err != nil {
		return cliRunResult{}, err
	}

	// The container outlives its client, so removing it is this function's
	// job on EVERY exit path — a timeout, a cancel, a parse failure. The
	// context is stripped of cancellation for the same reason
	// shell.dockerSession.close builds its own: the cases where teardown
	// matters most are exactly the ones where the caller's context is
	// already dead. A normal exit has nothing to remove (--rm got there
	// first) and this is a no-op against an absent container.
	if container != "" {
		defer shell.RemoveContainer(context.WithoutCancel(ctx), container)
	}

	cmd.Stdin = strings.NewReader(plan.prompt)
	cmd.Stderr = &cliStderrLogger{agent: prepared.ri.AgentName}
	cmd.WaitDelay = cliWaitDelay

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cliRunResult{}, fmt.Errorf("agent %q: cli stdout: %w", prepared.ri.AgentName, err)
	}

	err = cmd.Start()
	if err != nil {
		return cliRunResult{}, fmt.Errorf("agent %q: starting %s: %w", prepared.ri.AgentName, binary, err)
	}

	// Parsed as it arrives, not buffered whole: a step that times out
	// mid-conversation still has the trajectory of what it managed to do.
	// The step's own recorder, so every turn the child takes is published as
	// it happens under the step that spawned it. It is set for every agent
	// step in RunStep, whichever path runs — this one just never used it.
	run, parseErr := parseCLIStream(stdout, prepared.conv.recorder)

	// Drain whatever is left before waiting. A parse that stopped early (an
	// over-long line) leaves the child writing into a pipe nobody reads, and
	// cmd.Wait would then block on a process blocked on us until the step
	// timeout expired.
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	waitErr := cmd.Wait()

	switch {
	case parseErr != nil:
		return run, fmt.Errorf("agent %q: reading %s output: %w", prepared.ri.AgentName, binary, parseErr)

	// A reported result outranks the exit status, and the order here is the
	// whole point. The CLI exits NONZERO when it reports a task failure (a
	// max-turns stop exits 1 with is_error), so checking waitErr first would
	// call every such run an infrastructure error: classified as errored
	// instead of failed, unroutable by failure:, and retried by attempts:
	// at full cost for a conclusion the CLI already reached. If it spoke for
	// itself, believe it — checkCLIObligations reads is_error from here.
	case run.sawResult:
		if waitErr != nil {
			slog.Debug("agent.cli.exit", "agent", prepared.ri.AgentName, "error", waitErr, "reported_error", run.isError)
		}

		return run, nil

	case waitErr != nil:
		return run, fmt.Errorf("agent %q: %s exited: %w", prepared.ri.AgentName, binary, waitErr)
	}

	return run, fmt.Errorf("agent %q: %s exited cleanly without reporting a result", prepared.ri.AgentName, binary)
}

// cliArgs builds the CLI's command line. Kept pure and separate from spawning
// so what a grant translates to is directly assertable in a test — the
// argument vector IS the permission boundary.
func cliArgs(prepared preparedAgentStep, runtime cliRuntime, mcpConfig string, plan cliAttempt) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--model", prepared.ri.ModelName,
	}

	// An uncapped step passes no --max-turns at all rather than a large one:
	// the CLIs this drives impose no turn cap of their own, so omitting the
	// flag IS the uncapped spelling, and a number would be a ceiling the
	// pipeline never asked for.
	if plan.maxTurns != unlimitedTurns {
		args = append(args, "--max-turns", strconv.Itoa(plan.maxTurns))
	}

	// A retry rejoins the conversation instead of restarting the task. Session
	// flags are session-scoped, not sticky: every other flag below is re-read
	// on a resume too, which is what lets each attempt point at its own
	// freshly-bound bridge port.
	if plan.resume {
		args = append(args, "--resume", plan.session)
	} else {
		args = append(args, "--session-id", plan.session)
	}

	if persona := prepared.conv.system; persona != "" {
		args = append(args, "--append-system-prompt", persona)
	}

	// The CLI meters itself in dollars and can stop mid-conversation, which is
	// the one circuit breaker available across the process boundary — a token
	// count here only ever arrives after the spending is done.
	if prepared.ri.BudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(prepared.ri.BudgetUSD, 'f', -1, 64))
	}

	natives, allowed := cliToolPermissions(prepared.conv, runtime)

	// Two flags for two different questions, and the distinction is the whole
	// fence.
	//
	// --tools is the SURFACE: the CLI offers only the built-ins named here
	// (verified — `--tools Read` reports exactly ["Read"] and nothing else).
	// It is deny-by-default, which is why there is no list of things to deny:
	// a built-in this build has never heard of is withheld because it was
	// never named, rather than surviving because nobody remembered to add it.
	// An empty value means no built-ins at all.
	args = append(args, "--tools", strings.Join(natives, ","))

	// --allowedTools is PERMISSION. Read/Glob/Grep need none, but Bash, Write
	// and Edit are gated and would otherwise stall on a prompt nobody can
	// answer in non-interactive mode; the bridge's own tools need naming here
	// too, since --tools governs built-ins only.
	args = append(args, "--allowedTools", strings.Join(allowed, ","))

	// --strict-mcp-config is what makes the grant a limit rather than a
	// suggestion: without it the CLI would also load the user's own MCP
	// servers, handing the step tools the pipeline never granted.
	args = append(args, "--mcp-config", mcpConfig, "--strict-mcp-config")

	// A pipeline step is not a personal session. With no settings: declared
	// the subprocess loads NO configuration scopes — not the operator's
	// ~/.claude (settings, hooks, plugins, skills, output styles, which would
	// make the same pipeline behave differently per machine), and not the
	// repo's own .claude/ either: project config shaping an agent is a
	// capability the pipeline opts into with `settings: project`, checked in
	// and reviewable beside the pipeline.
	args = append(args, "--setting-sources", prepared.ri.CLISettings)

	return args
}

// cliToolPermissions translates a step's grant into the CLI's two axes:
// natives is the built-in surface (--tools), allowed is everything the child
// may use without being asked (--allowedTools), which is the natives plus the
// bridge's tools.
func cliToolPermissions(conv agentConversation, runtime cliRuntime) (natives, allowed []string) {
	for _, decl := range conv.tools.decls.FunctionDeclarations {
		if decl == nil {
			continue
		}

		// Provenance, not spelling: the natives table is keyed by builtin
		// name, and a custom tool may reuse one (see agentTools.builtins).
		if native, isNative := runtime.natives[decl.Name]; isNative && conv.tools.builtins[decl.Name] {
			natives = append(natives, native)

			// A web_fetch allow: list becomes per-domain permission entries
			// instead of one blanket grant, so the CLI enforces the same
			// fence the HTTP path's impl does.
			//
			// TWO rules per entry, and both are needed: the CLI's domain
			// matcher is exact where checkWebFetchHost is suffix-aware, so
			// `domain:h` alone denies api.h (which the hosted path allows)
			// and `domain:*.h` alone denies the apex. Emitting the pair is
			// what keeps one written fence from being two different fences.
			//
			// One divergence remains and is not closable from here: the CLI
			// matches the requested domain, not each redirect hop, so a hop
			// off an allowed host is its enforcement to make, not steps'.
			if decl.Name == config.WebFetchBuiltinName && len(conv.tools.webFetchAllow) > 0 {
				for _, host := range conv.tools.webFetchAllow {
					allowed = append(allowed,
						fmt.Sprintf("%s(domain:%s)", native, host),
						fmt.Sprintf("%s(domain:*.%s)", native, host))
				}

				continue
			}

			allowed = append(allowed, native)

			continue
		}

		// Everything else reaches the CLI through the bridge.
		allowed = append(allowed, bridgedToolName(decl.Name))
	}

	// Sorted so the command line is stable run to run — a permission boundary
	// that reorders itself is one nobody can diff.
	sort.Strings(natives)
	sort.Strings(allowed)

	return natives, allowed
}

// nativeToolNames is the set of tool names the CLI serves itself, which the
// bridge must therefore NOT re-export — exporting both would offer the model
// the same capability twice under two names.
func nativeToolNames(conv agentConversation, runtime cliRuntime) map[string]bool {
	skip := map[string]bool{}

	for _, decl := range conv.tools.decls.FunctionDeclarations {
		if decl == nil {
			continue
		}

		// Same provenance rule as cliToolPermissions: a custom tool sharing a
		// builtin's name is NOT served by the CLI, so the bridge must serve
		// it or the model is offered a tool nothing runs.
		if _, isNative := runtime.natives[decl.Name]; isNative && conv.tools.builtins[decl.Name] {
			skip[decl.Name] = true
		}
	}

	return skip
}

// renderCLIPrompt assembles what goes in on stdin.
//
// On the HTTP path the upstream steps' decisions and the context_paths files
// arrive as synthetic tool exchanges — messages fabricated into a transcript
// this package owns, which is why the task has to precede them there. There
// is no transcript to fabricate into here and no roles to order, so the same
// content is prepended to the prompt as fenced blocks and the task text ends
// it, where the most recent instruction belongs.
func renderCLIPrompt(conv agentConversation) string {
	var out strings.Builder

	// The decisions this step asked upstream steps for come first, as they do
	// on the HTTP path: they are what happened BEFORE this step, and the
	// context_paths files below are what it works on. Already fenced by
	// upstreamBlocks, so this adds no second wrapper.
	for _, block := range conv.upstream {
		fmt.Fprintf(&out, "%s:\n%s\n\n", block.path, block.content)
	}

	// Fenced, one tag per block. On the hosted path this content arrives as a
	// read_file tool RESULT — a structural boundary the model reads as data.
	// There is no such boundary in a prompt, so concatenating a file straight
	// in would let "ignore previous instructions" inside somebody's README
	// read exactly like an operator instruction. The tag is drawn fresh
	// against the content so it cannot be closed early from inside.
	for _, block := range conv.contextBlocks {
		tag := freshFenceTag(block.content)
		fmt.Fprintf(&out, "%s:\n<%s>\n%s\n</%s>\n\n", block.path, tag, block.content, tag)
	}

	out.WriteString(conv.opening())

	return out.String()
}

// cliEnv is the subprocess environment: the same allowlisted host environment
// every shell tool gets — which carries HOME, so the CLI finds its own
// credentials and a subscription login works with no api_key_env at all —
// plus an explicitly configured key when the pipeline named one.
func cliEnv(ri config.ResolvedInvocation) []string {
	env := shell.HostEnv()

	if ri.APIKeyEnv == "" {
		return env
	}

	if key := os.Getenv(ri.APIKeyEnv); key != "" {
		env = append(env, "ANTHROPIC_API_KEY="+key)
	}

	return env
}

// cliStderrLogger turns the CLI's stderr into debug records line by line,
// rather than letting it interleave with the pipeline's own output. Modeled
// on internal/mcp's stdio server logger, and safe for the same reason: only
// os/exec's copy goroutine writes, and cmd.Wait synchronizes after it.
type cliStderrLogger struct {
	agent string
	buf   bytes.Buffer
}

func (w *cliStderrLogger) Write(p []byte) (int, error) {
	_, _ = w.buf.Write(p)

	for {
		idx := bytes.IndexByte(w.buf.Bytes(), '\n')
		if idx < 0 {
			break
		}

		line := w.buf.Next(idx + 1)
		slog.Debug("agent.cli.stderr", "agent", w.agent, "line", string(bytes.TrimRight(line, "\n")))
	}

	return len(p), nil
}
