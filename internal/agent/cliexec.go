package agent

// Spawning the CLI: the argument vector that IS the permission boundary, and
// the environment it gets.
//
// The CLI process is ALWAYS a host subprocess of this one — even when the
// step resolved an image: for its tools. That is the issue #100 design: a
// hosted agent is a brain whose hands are steps' own tool implementations,
// executing wherever steps decides (host, or a container against whatever
// daemon internal/shell resolves); a CLI agent is now the same shape. `image:`
// on a CLI agent step places the TOOLS — toolEnv.runner, built by
// prepareAgentStep exactly as it is for a hosted agent — never the CLI
// process itself. That single move is what deletes the credentials
// bind-mount, the macOS Keychain asymmetry, the container $HOME, and the
// host.docker.internal reach analysis: the bridge is loopback, full stop,
// because nothing dialling it is ever anywhere but this host.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// cliAPIKeyEnv is the variable the claude CLI reads its key from, whatever
// the pipeline chose to call its own.
//
//nolint:gosec // an environment variable NAME, not a credential
const cliAPIKeyEnv = "ANTHROPIC_API_KEY"

// execCLI spawns the CLI and reads its transcript off stdout as it runs.
//
// It wraps its own cancellable context around ctx so an attestation failure
// (parseCLIStream disagreeing with expected — see cliattest.go) can KILL the
// child immediately, before the rest of the stream is drained: the point of
// the kill is to stop a child this process no longer trusts from acting
// further, not merely to stop reading what it says.
func execCLI(
	ctx context.Context,
	prepared preparedAgentStep,
	mcpConfig string,
	plan cliAttempt,
	expected []string,
) (cliRunResult, error) {
	binary := config.CLIBinary(prepared.ri.CLI)
	args := cliArgs(prepared, mcpConfig, plan)

	slog.Debug("agent.cli.exec", "agent", prepared.ri.AgentName, "binary", binary, "args", args,
		"dir", prepared.conv.env.dir)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, args...) //nolint:gosec // binary comes from the static cliProviders table
	cmd.Dir = prepared.conv.env.dir
	cmd.Env = cliEnv(prepared.ri)
	cmd.Stdin = strings.NewReader(plan.prompt)
	cmd.Stderr = &cliStderrLogger{agent: prepared.ri.AgentName}
	cmd.WaitDelay = cliWaitDelay

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return cliRunResult{}, fmt.Errorf("cli stdout: %w", err)
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
	run, parseErr := parseCLIStream(stdout, prepared.conv.recorder, expected)

	// An attestation failure kills the child right here, before Wait: the
	// child's own natives are gone and its tool surface is exactly what the
	// bridge served, but the kill is what turns that claim into an enforced
	// one for the calls the child has not made yet.
	if errors.Is(parseErr, errCLIToolSurface) {
		cancel()
	}

	// Drain whatever is left before waiting. A parse that stopped early (an
	// over-long line, an attestation kill) leaves the child writing into a
	// pipe nobody reads, and waiting would then block on a process blocked on
	// us until the step timeout expired.
	if parseErr != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}

	waitErr := cmd.Wait()

	switch {
	case errors.Is(parseErr, errCLIToolSurface):
		return run, fmt.Errorf("agent %q: %w", prepared.ri.AgentName, parseErr)

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
func cliArgs(prepared preparedAgentStep, mcpConfig string, plan cliAttempt) []string {
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

	// The one generation dial a CLI does take. The others (temperature,
	// top_p, max_tokens) are refused at load because there is no request for
	// steps to shape; reasoning depth is different — the CLI exposes it as a
	// session-level flag, so the value the pipeline wrote binds. steps'
	// accepted set (low/medium/high) is a subset of the CLI's, so no
	// translation is needed; a value the CLI does not know would be its
	// error to report, and validReasoningEfforts means it cannot arrive.
	if prepared.ri.ReasoningEffort != "" {
		args = append(args, "--effort", prepared.ri.ReasoningEffort)
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

	// --tools "" unconditionally: the CLI's own natives are never the
	// surface, whatever this build ships (see the file header). Verified —
	// `--tools ""` reports an empty session tool list — so this, not a
	// per-grant computation, IS the whole of the CLI-side fence; the
	// stream-json init event asserts it held (see cliattest.go).
	args = append(args, "--tools", "")

	// --allowedTools names every granted tool under the bridge's namespace —
	// there are no natives left to also name here.
	args = append(args, "--allowedTools", strings.Join(cliToolPermissions(prepared.conv), ","))

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

// cliToolPermissions translates a step's grant into the bridged names the
// child may use without being asked (--allowedTools) — every declared tool,
// under the bridge's namespace, since --tools "" leaves no native to also
// name. The same list is what an attempt expects the CLI's own init event to
// report back (see cliattest.go): one written grant, one enforced surface.
func cliToolPermissions(conv agentConversation) []string {
	allowed := make([]string, 0, len(conv.tools.decls.FunctionDeclarations))

	for _, decl := range conv.tools.decls.FunctionDeclarations {
		if decl == nil {
			continue
		}

		allowed = append(allowed, bridgedToolName(decl.Name))
	}

	// Sorted so the command line is stable run to run — a permission boundary
	// that reorders itself is one nobody can diff.
	sort.Strings(allowed)

	return allowed
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
		env = append(env, cliAPIKeyEnv+"="+key)
	}

	return env
}

// cliUnboundedToolTimeout is the ceiling cliToolTimeoutEnv gives the child's
// per-tool-call deadline when the step itself declared timeout: 0 (no
// deadline of its own). MCP_TOOL_TIMEOUT cannot be left unset — see below —
// so an uncapped step still needs a number; the real bound in that case is
// whatever the enclosing job imposes, not this one, so this is deliberately
// generous rather than a considered ceiling of its own.
const cliUnboundedToolTimeout = 24 * time.Hour

// cliToolTimeoutEnv widens the CHILD's own MCP tool-call deadline.
//
// EVERY tool call is now a bridged MCP call (issue #100: `--tools ""` leaves
// no native path), so this no longer widens only for a parked ask_user
// question — an ordinary run_shell building a project would otherwise die at
// the CLI's own default per-call deadline, where before it ran under the
// CLI's native Bash timeout instead. The bound is max(the step's own
// timeout:, a parked ask_user's wait plus a margin) plus a further margin, so
// the deadline that actually fires in the ordinary case is the step's own
// (enforced by this process's context, not the child's), and in the
// ask_user case is ask_user's — never the child's un-widened default.
//
// An operator's own value is FORWARDED rather than deferred to, which is not
// the same thing and was the bug: MCP_TOOL_TIMEOUT is not on shell.HostEnv's
// allowlist, so it never reaches the child on its own. Returning nil because
// the operator had set one meant the child ran with no value at all — the
// exact failure this function exists to prevent, in precisely the case where
// somebody had thought about it.
func cliToolTimeoutEnv(ri config.ResolvedInvocation) []string {
	if operator := os.Getenv(cliMCPToolTimeoutEnv); operator != "" {
		return []string{cliMCPToolTimeoutEnv + "=" + operator}
	}

	bound := agentTimeout(ri.Timeout)
	if bound == noAgentDeadline {
		bound = cliUnboundedToolTimeout
	}

	// A margin over the wait itself, so the deadline that fires is ask_user's
	// own — which resolves to the declared default: — rather than the child's,
	// which resolves to nothing anybody declared.
	if ask := askUserWait(ri.ToolSpecs) + cliToolTimeoutMargin; ask > bound {
		bound = ask
	}

	return []string{fmt.Sprintf("%s=%d", cliMCPToolTimeoutEnv, (bound + cliToolTimeoutMargin).Milliseconds())}
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
