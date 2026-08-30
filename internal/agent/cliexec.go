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

// cliProcess is one CLI attempt, however it is being run: a child of this
// process on the host, or a container on the daemon.
//
// An interface rather than an *exec.Cmd for both, because the container is no
// longer a subprocess to wire up — it is a container this code talks to over
// the engine API, and the only things the caller ever did with the command
// were start it, read its transcript, and wait. Those are the three methods.
type cliProcess interface {
	// Start begins the run and returns the stream its transcript arrives on.
	Start(ctx context.Context, stdin io.Reader, stderr io.Writer) (io.Reader, error)
	// Wait ends it, reporting a nonzero exit as an error — the same shape
	// exec.Cmd has, so the caller needs one branch and not two.
	Wait(ctx context.Context) error
	// Close releases whatever the run holds, on every exit path including the
	// ones that never reach Wait.
	Close()
}

// buildCLIProcess constructs the run for one CLI attempt: the binary itself on
// the host, or a container when the step resolved an image. Everything the
// caller does afterwards is identical either way — only what the process IS
// differs.
//
// The second return is the container's name, empty on the host path. It is
// what makes the run RECLAIMABLE: nothing this end does stops a container, so
// a caller whose context is cancelled can only tear it down by name.
func buildCLIProcess(
	ctx context.Context, prepared preparedAgentStep, binary string, args []string, stepHome string,
) (cliProcess, string, error) {
	if prepared.ri.Image == "" {
		cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary comes from the static cliProviders table
		cmd.Dir = prepared.conv.env.dir
		cmd.Env = cliEnv(prepared.ri)

		return &hostCLIProcess{cmd: cmd}, "", nil
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

	// The api_key_env: value crosses under the CLI's own name, carried by
	// value. It used to be forwarded value-free so the secret stayed out of
	// the docker client's argv; there is no argv now, and the value travels in
	// a request body that no process list shows.
	//
	// Conditioned on the variable actually being EXPORTED, not merely named.
	// Passing the name alone would hand the container whatever
	// ANTHROPIC_API_KEY this process happens to have — so a pipeline naming an
	// unset api_key_env: would silently authenticate with the operator's
	// personal key instead of failing. The host path cannot do that
	// (shell.HostEnv's allowlist excludes it), and the two must agree.
	if key := lookupCLIKey(prepared.ri); key != "" {
		spec.ExtraEnv[cliAPIKeyEnv] = key
	}

	return &containerCLIProcess{spec: spec}, name, nil
}

// hostCLIProcess runs the CLI as a child of this process.
type hostCLIProcess struct {
	cmd *exec.Cmd
}

func (h *hostCLIProcess) Start(_ context.Context, stdin io.Reader, stderr io.Writer) (io.Reader, error) {
	h.cmd.Stdin = stdin
	h.cmd.Stderr = stderr
	h.cmd.WaitDelay = cliWaitDelay

	stdout, err := h.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("cli stdout: %w", err)
	}

	err = h.cmd.Start()
	if err != nil {
		return nil, fmt.Errorf("starting %s: %w", h.cmd.Path, err)
	}

	return stdout, nil
}

// Close is a no-op: a child process owns nothing this end has to release.
func (h *hostCLIProcess) Close() {}

func (h *hostCLIProcess) Wait(context.Context) error {
	err := h.cmd.Wait()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// containerCLIProcess runs the CLI in a container on the daemon.
type containerCLIProcess struct {
	spec shell.DockerRunSpec
	run  *shell.ForegroundRun
}

func (c *containerCLIProcess) Start(ctx context.Context, stdin io.Reader, stderr io.Writer) (io.Reader, error) {
	run, err := shell.StartForeground(ctx, c.spec, stdin, stderr)
	if err != nil {
		return nil, fmt.Errorf("starting the cli container: %w", err)
	}

	c.run = run

	return run.Stdout, nil
}

func (c *containerCLIProcess) Wait(ctx context.Context) error {
	err := c.run.Wait(ctx)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// Close releases what the run holds, for the paths that never reach Wait.
func (c *containerCLIProcess) Close() {
	if c.run != nil {
		c.run.Close()
	}
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

	process, container, err := buildCLIProcess(ctx, prepared, binary, args, plan.home)
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
	defer process.Close()

	if container != "" {
		defer shell.RemoveContainer(context.WithoutCancel(ctx), container)
	}

	stdout, err := process.Start(ctx,
		strings.NewReader(plan.prompt),
		&cliStderrLogger{agent: prepared.ri.AgentName})
	if err != nil {
		return cliRunResult{}, fmt.Errorf("agent %q: %w", prepared.ri.AgentName, err)
	}

	// Parsed as it arrives, not buffered whole: a step that times out
	// mid-conversation still has the trajectory of what it managed to do.
	// The step's own recorder, so every turn the child takes is published as
	// it happens under the step that spawned it. It is set for every agent
	// step in RunStep, whichever path runs — this one just never used it.
	run, parseErr := parseCLIStream(stdout, prepared.conv.recorder)

	// Drain whatever is left before waiting. A parse that stopped early (an
	// over-long line) leaves the child writing into a pipe nobody reads, and
	// waiting would then block on a process blocked on us until the step
	// timeout expired.
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	waitErr := process.Wait(ctx)

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
	//
	// What the child is handed is the step's REMAINDER, not its declared
	// ceiling: budget: usd bounds the step, and a retry that started over with
	// the full figure would let three attempts spend three budgets. See
	// remainingCLIBudget.
	if plan.budgetUSD != unlimitedBudget {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(plan.budgetUSD, 'f', -1, 64))
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
	env := append(shell.HostEnv(), cliToolTimeoutEnv(ri)...)

	if ri.APIKeyEnv == "" {
		return env
	}

	if key := os.Getenv(ri.APIKeyEnv); key != "" {
		env = append(env, "ANTHROPIC_API_KEY="+key)
	}

	return env
}

// cliToolTimeoutEnv widens the CHILD's own MCP tool-call deadline to cover a
// parked question.
//
// A bridged call blocks until it returns, and ask_user blocks for as long as
// the pipeline said a person may be waited on. The bridge's own HTTP side is
// fine — no write deadline — but the binding constraint is the CLI's tool-call
// timeout, which is its default and not ours: without this, a parked question
// on a CLI agent dies at whatever the CLI decided rather than at the deadline
// the pipeline declared, and the model is told its question failed while a
// person is still looking at it.
//
// Only widened, never narrowed: an operator who set MCP_TOOL_TIMEOUT already
// keeps it, since a value they chose deliberately is not this function's to
// overrule.
func cliToolTimeoutEnv(ri config.ResolvedInvocation) []string {
	if os.Getenv(cliMCPToolTimeoutEnv) != "" || !grantsAskUserSpec(ri.ToolSpecs) {
		return nil
	}

	// A margin over the wait itself, so the deadline that fires is ask_user's
	// own — which resolves to the declared default: — rather than the child's,
	// which resolves to nothing anybody declared.
	budget := askUserWait(ri.ToolSpecs) + cliToolTimeoutMargin

	return []string{fmt.Sprintf("%s=%d", cliMCPToolTimeoutEnv, budget.Milliseconds())}
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
