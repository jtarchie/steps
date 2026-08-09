package agent

// Running an agent step by delegating to a coding-agent CLI.
//
// A `source.model` of `@claude/sonnet` means the conversation is not steps' to
// drive: the CLI owns the turn loop, the tool calls, and the context window.
// What steps keeps is everything around it — the workspace the process runs
// in, the merkle hash that decides whether it runs at all, the timeout, the
// recorded trajectory, and the verdict that routes the job. That division is
// the whole design; see docs/agents-internals.md.
//
// The two halves of making it work are here and in clibridge.go. This file
// decides what the subprocess is allowed to do (which native tools, which
// bridged ones) and reads back what it did. The bridge is how the tools the
// pipeline actually granted reach a process that only speaks MCP.

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
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/shell"
)

// cliWaitDelay bounds how long a CLI gets to exit after its context is
// canceled before it is killed outright — the same grace internal/mcp gives a
// stdio server.
const cliWaitDelay = 5 * time.Second

// cliRuntime is how one coding-agent CLI is invoked: which of its native
// tools stand in for steps' built-ins, and which of its tools must be denied
// outright.
//
// The native mapping is the load-bearing part. A CLI is good at its own
// tools — they are what its model was trained against — so a grant of
// `read_file` becomes permission to use `Read` rather than a bridged
// reimplementation the CLI would use worse. The tradeoff is that the
// grant becomes a permission boundary expressed in the CLI's vocabulary, and
// steps' own path confinement no longer applies; the subprocess's working
// directory is the fence instead. (A grant including run_shell makes that
// distinction academic anyway — it does on the HTTP path too.)
type cliRuntime struct {
	// natives maps a steps built-in tool name to the CLI's equivalent.
	natives map[string]string
}

// cliRuntimes mirrors config.cliProviders — same keys, invocation details
// instead of load-time facts. TestCLIRuntimesCoverProviders keeps them in
// step, so half-adding a CLI fails the build rather than a run.
//
//nolint:gochecknoglobals // static, read-only lookup table
var cliRuntimes = map[string]cliRuntime{
	"claude": {
		natives: map[string]string{
			"read_file":    "Read",
			"list_dir":     "Glob",
			"run_shell":    "Bash",
			"write_file":   "Write",
			"edit_file":    "Edit",
			"search_files": "Grep",
		},
	},
}

// runCLIConversation runs a prepared agent step as a CLI subprocess, once per
// attempt, and returns the same conversationResult an HTTP conversation would
// have produced — so everything downstream (asserts, routing, recording,
// handoff) is unaware which one ran.
//
// attempts: means something different here than it does on the HTTP path,
// where it retries an individual request beneath a conversation that survives
// (see requests.go). A CLI's conversation is inside the subprocess and dies
// with it, so the only thing left to retry is the whole invocation. That is
// the honest reading of the knob for a process that cannot be resumed, and it
// is why only INFRASTRUCTURE failures are retried: a CLI that ran and decided
// the task failed gets its answer respected, not re-rolled.
func runCLIConversation(ctx context.Context, prepared preparedAgentStep, timeout time.Duration) (conversationResult, error) {
	runtime, known := cliRuntimes[prepared.ri.CLI]
	if !known {
		return conversationResult{}, fmt.Errorf("agent %q: no runtime for cli %q", prepared.ri.AgentName, prepared.ri.CLI)
	}

	// Bind this step's accounting to the job and report it however the step
	// ends — the hosted path gets both from runAgentConversation, which this
	// path never calls, so without them a CLI step's spend reached the step
	// counters and stopped there: invisible to a job budget: and missing from
	// the run's usage report.
	prepared.conv.usage = attachUsage(ctx, prepared.conv.usage)
	defer prepared.conv.usage.finish()

	var result conversationResult

	err := retry.Do(ctx, prepared.ri.Attempts, func(attempt int) error {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		var attemptErr error

		result, attemptErr = runCLIAttempt(attemptCtx, prepared, runtime)
		if attemptErr != nil {
			slog.Warn("agent.cli.attempt_failed",
				"agent", prepared.ri.AgentName,
				"cli", prepared.ri.CLI,
				"attempt", attempt+1,
				"of", prepared.ri.Attempts,
				"error", attemptErr)
		}

		// A step-level failure (the CLI reported the task failed, or finished
		// without the verdict the step requires) is an answer, not an outage:
		// retrying it would just pay twice for the same conclusion.
		var failure *outcome.Failure
		if errors.As(attemptErr, &failure) {
			return retry.Stop(attemptErr)
		}

		return retry.StopOnDeadline(ctx, attemptCtx, attemptErr)
	})

	//nolint:wrapcheck // retry.Do returns the attempt's own error, already agent-labeled
	return result, err
}

// runCLIAttempt is one subprocess: bridge up, process run, transcript read,
// obligations checked.
func runCLIAttempt(ctx context.Context, prepared preparedAgentStep, runtime cliRuntime) (conversationResult, error) {
	// A fresh bridge per attempt, so a verdict captured by an attempt that
	// then crashed cannot be mistaken for this one's.
	bridge, err := newCLIBridge(ctx, prepared.conv, nativeToolNames(prepared.conv, runtime))
	if err != nil {
		return conversationResult{}, err
	}

	defer func() { _ = bridge.Close(ctx) }()

	mcpConfig, err := bridge.writeConfig()
	if err != nil {
		return conversationResult{}, err
	}

	defer func() { _ = os.Remove(mcpConfig) }()

	run, runErr := execCLI(ctx, prepared, runtime, mcpConfig)

	verdict, note, handoffNote, satisfied, bridgeCalls := bridge.observed()

	result := conversationResult{
		text:        run.text,
		turns:       run.turns,
		trajectory:  mergeCLITrajectory(run.trajectory, bridgeCalls),
		model:       prepared.ri.ModelName,
		verdict:     verdict,
		note:        note,
		handoffNote: handoffNote,
	}

	prepared.conv.usage.addTokens(run.inputTokens, run.outputTokens)

	if runErr != nil {
		return result, runErr
	}

	return result, checkCLIObligations(prepared, run, satisfied)
}

// execCLI spawns the CLI and reads its transcript off stdout as it runs.
func execCLI(ctx context.Context, prepared preparedAgentStep, runtime cliRuntime, mcpConfig string) (cliRunResult, error) {
	binary := config.CLIBinary(prepared.ri.CLI)
	args := cliArgs(prepared, runtime, mcpConfig)

	slog.Debug("agent.cli.exec", "agent", prepared.ri.AgentName, "binary", binary, "args", args, "dir", prepared.conv.env.dir)

	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary comes from the static cliProviders table
	cmd.Dir = prepared.conv.env.dir
	cmd.Stdin = strings.NewReader(renderCLIPrompt(prepared.conv))
	cmd.Env = cliEnv(prepared.ri)
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
	run, parseErr := parseCLIStream(stdout)

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

// checkCLIObligations turns what the CLI reported, plus what the bridge saw,
// into the step's outcome.
//
// The verdict check is the one that matters. On the HTTP path a required tool
// is forced through tool_choice, which does not exist here — so the
// enforcement moves to the exit: a step that declared verdicts: and got none
// failed, full stop. That is stricter than prompting for one and hoping, and
// it is what lets `to:` routing mean the same thing either way.
func checkCLIObligations(prepared preparedAgentStep, run cliRunResult, satisfied map[string]bool) error {
	if run.isError {
		// The CLI's own sentence first ("Reached maximum number of turns (1)"),
		// then its machine-readable subtype, then a generic line. An operator
		// reading a failed step should not have to look up error_max_turns.
		reason := run.errMessage
		if reason == "" {
			reason = run.errSubtype
		}

		if reason == "" {
			reason = "the cli reported the task as failed"
		}

		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("agent %q: %s", prepared.ri.AgentName, reason))
	}

	// The first unsatisfied obligation is the one reported: they are sorted,
	// and a step that skipped two required tools is not more informative than
	// one that skipped one.
	missing := unsatisfiedRequiredTools(prepared.conv.tools.required, satisfied)
	if len(missing) == 0 {
		return nil
	}

	if missing[0] == prepared.conv.verdictTool {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("agent %q: finished without calling the %s tool; declared verdicts: %s",
			prepared.ri.AgentName, verdictToolName, strings.Join(prepared.step.Verdicts, ", ")))
	}

	//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
	return outcome.Fail(fmt.Errorf("agent %q: finished without a successful call to required tool %q",
		prepared.ri.AgentName, missing[0]))
}

// cliArgs builds the CLI's command line. Kept pure and separate from spawning
// so what a grant translates to is directly assertable in a test — the
// argument vector IS the permission boundary.
func cliArgs(prepared preparedAgentStep, runtime cliRuntime, mcpConfig string) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
		"--model", prepared.ri.ModelName,
		"--max-turns", strconv.Itoa(prepared.ri.MaxTurns),
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

	// A pipeline step is not a personal session. Without this the subprocess
	// inherits the operator's whole ~/.claude — settings, hooks, plugins,
	// skills, output styles — so the same pipeline behaves differently per
	// machine and a user hook can fire inside a step that never declared one.
	// Project scope stays: it is checked in beside the pipeline, reviewable,
	// and identical everywhere the repo goes.
	args = append(args, "--setting-sources", "project")

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

		if native, isNative := runtime.natives[decl.Name]; isNative {
			natives = append(natives, native)
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

		if _, isNative := runtime.natives[decl.Name]; isNative {
			skip[decl.Name] = true
		}
	}

	return skip
}

// renderCLIPrompt assembles what goes in on stdin.
//
// On the HTTP path the run-context recap and context_paths files arrive as
// synthetic tool exchanges — messages fabricated into a transcript this
// package owns. There is no transcript to fabricate into here, so the same
// content is prepended to the prompt instead, in the same order, already
// fenced as untrusted data by prepareContextBlocks.
func renderCLIPrompt(conv agentConversation) string {
	var out strings.Builder

	if conv.recap != "" {
		out.WriteString(conv.recap)
		out.WriteString("\n\n")
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

	out.WriteString(conv.prompt)

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

// mergeCLITrajectory combines what the CLI reported calling with what the
// bridge saw called. The CLI's own stream is authoritative for order and for
// its native tools; the bridge contributes the ARGUMENTS of bridged calls,
// which the stream reports too — so the stream alone would do, except when it
// was truncated. Bridged calls the stream never mentioned are appended, since
// a call the parent executed definitely happened.
func mergeCLITrajectory(streamed, bridged []recordedToolCall) []recordedToolCall {
	seen := map[string]int{}
	for _, call := range streamed {
		seen[call.name]++
	}

	// A fresh slice rather than an alias of streamed: appending to the
	// caller's slice would write bridge-only calls into run.trajectory's
	// spare capacity, so the two records would stop being independent.
	merged := make([]recordedToolCall, len(streamed), len(streamed)+len(bridged))
	copy(merged, streamed)

	for _, call := range bridged {
		prefixed := bridgedToolName(call.name)
		if seen[prefixed] > 0 {
			seen[prefixed]--

			continue
		}

		merged = append(merged, recordedToolCall{name: prefixed, args: call.args, ok: call.ok})
	}

	return merged
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

// probeCLI answers preflight's question for a CLI target: is the binary
// there?
//
// That is deliberately all it asks. The HTTP probe sends a real request
// because an endpoint can be reachable and still reject the model or the key;
// a CLI has no equivalent failure that a cheap check would catch, and
// spawning one to find out would put a process launch in the path of every
// `steps watch` poll. A CLI that is installed but broken fails at the step,
// with the CLI's own error, which is a better message than a probe would
// synthesize.
func probeCLI(ri config.ResolvedInvocation) error {
	binary := config.CLIBinary(ri.CLI)
	if binary == "" {
		return fmt.Errorf("agent %q: no runtime for cli %q", ri.AgentName, ri.CLI)
	}

	_, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("agent %q: cli %q is not on PATH: %w", ri.AgentName, binary, err)
	}

	return nil
}
