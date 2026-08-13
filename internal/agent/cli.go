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
	"crypto/rand"
	"errors"
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
// (see requests.go). A CLI's conversation lives in a subprocess, so the
// equivalent is to RESUME it: every attempt after the first reconnects to the
// same session rather than starting the task over.
//
// That distinction is the whole point, and it is not a cost optimization.
// `attempts:` used to restart an agent's conversation on the hosted path too,
// and was deliberately removed (see requests.go and commit "attempts: meant
// two different things on task vs agent"): the workspace survived a restart
// but the memory did not, so a retried attempt inherited its own half-finished
// edits with no recollection of making them. A CLI agent has exactly that
// problem — more of it, since it edits more — so it gets exactly that fix.
//
// Only INFRASTRUCTURE failures are retried either way: a CLI that ran and
// decided the task failed gets its answer respected, not re-rolled.
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

	// Minted here, not read from the CLI's own report: a session id we chose
	// is one we can resume without parsing for it, and one we can clean up
	// afterwards knowing no other run could own that name.
	session, err := newCLISessionID()
	if err != nil {
		return conversationResult{}, fmt.Errorf("agent %q: %w", prepared.ri.AgentName, err)
	}

	// A containerized step gets a private $HOME for the CLI (see
	// newCLIStepHome), created once per STEP rather than per attempt: a
	// retried attempt resumes the same session, and the transcript it resumes
	// lives in that home. Removing the directory afterwards is the
	// containerized equivalent of cleanupCLISession — the transcript never
	// touches the host's own ~/.claude at all.
	var stepHome string

	if prepared.ri.Image != "" {
		stepHome, err = newCLIStepHome()
		if err != nil {
			return conversationResult{}, fmt.Errorf("agent %q: %w", prepared.ri.AgentName, err)
		}

		defer func() {
			removeErr := os.RemoveAll(stepHome)
			if removeErr != nil {
				slog.Debug("agent.cli.step_home_cleanup_failed", "path", stepHome, "error", removeErr)
			}
		}()
	} else {
		defer cleanupCLISession(session)
	}

	// One state for the whole step, not one per attempt: the attempts share a
	// conversation now, so what an attempt observed stays true for the ones
	// after it. See cliStepState.
	state := newCLIStepState()

	var lastErr error

	runErr := retry.Do(ctx, prepared.ri.Attempts, func(attempt int) error {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		plan := cliAttempt{
			session: session,
			resume:  attempt > 0,
			home:    stepHome,
			// The turn budget is per STEP, not per attempt — the hosted path
			// counts turns in one conversation that request retries never
			// reset, and a resumed session is the same conversation. The CLI
			// counts turns per invocation, so the remainder is ours to track.
			maxTurns: prepared.ri.MaxTurns - state.turns,
		}

		// A resumed attempt is NOT re-sent the task: the session already holds
		// the prompt, the recap and the context blocks, and repeating them
		// invites redoing finished work.
		if plan.resume {
			plan.prompt = cliContinuationPrompt(state, prepared)
		} else {
			plan.prompt = renderCLIPrompt(prepared.conv)
		}

		if plan.maxTurns <= 0 {
			// Carrying lastErr matters: the budget ran out because of whatever
			// failed a moment ago, and reporting only the ceiling would hide
			// the thing actually worth investigating.
			return retry.Stop(outcome.Fail(fmt.Errorf("agent %q: exhausted its %d-turn budget across %d attempt(s); last failure: %w",
				prepared.ri.AgentName, prepared.ri.MaxTurns, attempt, lastErr)))
		}

		attemptErr := runCLIAttempt(attemptCtx, prepared, runtime, plan, state)
		lastErr = attemptErr

		if attemptErr != nil {
			slog.Warn("agent.cli.attempt_failed",
				"agent", prepared.ri.AgentName,
				"cli", prepared.ri.CLI,
				"attempt", attempt+1,
				"of", prepared.ri.Attempts,
				"turns_spent", state.turns,
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

	return state.result(prepared.ri.ModelName), runErr
}

// cliAttempt is one invocation's session state: which conversation it joins,
// whether it is starting or continuing it, what it may still spend, and what
// it is told.
type cliAttempt struct {
	session  string
	resume   bool
	maxTurns int
	prompt   string
	// home is the per-step directory a containerized CLI gets as its $HOME
	// (empty on the host path). Per-step, not per-attempt: a resumed attempt
	// needs the transcript the previous one wrote there.
	home string
}

// cliStepState is everything a step has observed across its attempts.
//
// It accumulates because the attempts share one conversation. Under the old
// restart semantics a per-attempt view was right — attempt 2 redid the work,
// so attempt 1's verdict was stale. Resuming inverts that, exactly as it
// inverted the session-identity reasoning in #20: a verdict the model emitted
// before the process died is still THIS conversation's verdict, and a resumed
// model that can see it already called the tool has no reason to call it
// again. Discarding it would fail the step for an obligation it met.
//
// The same holds for the trajectory: a resumed CLI reports only its new
// events, so the calls a step actually made are the concatenation, not the
// last slice.
type cliStepState struct {
	text       string
	turns      int
	trajectory []recordedToolCall
	verdict    string
	note       string
	satisfied  map[string]bool
}

func newCLIStepState() *cliStepState {
	return &cliStepState{satisfied: map[string]bool{}}
}

// absorb folds one attempt's observations in.
func (s *cliStepState) absorb(run cliRunResult, bridge *cliBridge) {
	verdict, note, satisfied, bridgeCalls := bridge.observed()

	s.turns += run.turns
	s.trajectory = append(s.trajectory, mergeCLITrajectory(run.trajectory, bridgeCalls)...)

	// Last non-empty wins for the answer and the decision: a later attempt
	// speaks for the conversation, but a crashed one that said nothing must
	// not erase what an earlier one said.
	if run.text != "" {
		s.text = run.text
	}

	if verdict != "" {
		s.verdict, s.note = verdict, note
	}

	for name := range satisfied {
		s.satisfied[name] = true
	}
}

// result is what the step reports, however many attempts it took.
func (s *cliStepState) result(modelName string) conversationResult {
	return conversationResult{
		text:       s.text,
		turns:      s.turns,
		trajectory: s.trajectory,
		model:      modelName,
		verdict:    s.verdict,
		note:       s.note,
	}
}

// cliContinuationPrompt is what a resumed attempt is told.
//
// It names what went wrong and nothing else. This is deliberately NOT the
// prompt-text workaround the hosted path's restart semantics needed: there the
// model had lost its memory and had to be talked around the resulting
// incoherence, whereas here the conversation is intact and this is simply the
// one fact the model cannot see — the failure happened outside its transcript.
func cliContinuationPrompt(state *cliStepState, prepared preparedAgentStep) string {
	var out strings.Builder

	out.WriteString("Your previous attempt did not finish. ")

	if len(state.trajectory) > 0 {
		fmt.Fprintf(&out, "You had made %d tool call(s) before it stopped. ", len(state.trajectory))
	}

	out.WriteString("Your conversation and your working directory are both intact. " +
		"Continue from where you stopped — do not start the task over, and do not repeat work you have already done.")

	if names := prepared.step.VerdictNames(); len(names) > 0 {
		fmt.Fprintf(&out, " When you are done, call the %s tool with one of: %s.",
			verdictToolName, strings.Join(names, ", "))
	}

	return out.String()
}

// newCLISessionID mints a random RFC 4122 version 4 UUID, the form
// --session-id requires.
func newCLISessionID() (string, error) {
	var buf [16]byte

	_, err := rand.Read(buf[:])
	if err != nil {
		return "", fmt.Errorf("generating a cli session id: %w", err)
	}

	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}

// cleanupCLISession removes the transcript the CLI persisted for this step.
//
// Persistence has to stay on — it is what makes a retry able to resume — but a
// pipeline should not silently accumulate a dead session file per agent step
// in the operator's home directory, which is what happened before this
// existed. Matching on the session id we minted rather than on a derived
// directory name keeps the deletion precise: the only file that can match is
// one this step created, whatever layout the CLI uses around it.
//
// Best effort throughout. A step that did its work must not fail over tidying,
// and a CLI that stores transcripts somewhere else simply leaves nothing to
// find.
func cleanupCLISession(session string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}

	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", session+".jsonl"))
	if err != nil {
		return
	}

	for _, match := range matches {
		err := os.Remove(match)
		if err != nil {
			slog.Debug("agent.cli.session_cleanup_failed", "path", match, "error", err)
		}
	}
}

// runCLIAttempt is one subprocess: bridge up, process run, transcript read,
// obligations checked. What it observed goes into state rather than into a
// return value, so an attempt that fails before producing anything cannot
// erase what earlier attempts of the same conversation already established.
//
// The bridge itself is still per-attempt — it has to be, since each one binds
// its own port — but its captures are not.
func runCLIAttempt(
	ctx context.Context,
	prepared preparedAgentStep,
	runtime cliRuntime,
	plan cliAttempt,
	state *cliStepState,
) error {
	bridge, err := newCLIBridge(ctx, prepared.conv, nativeToolNames(prepared.conv, runtime), cliBridgeReach(prepared.ri))
	if err != nil {
		return err
	}

	defer func() { _ = bridge.Close(ctx) }()

	mcpConfig, err := bridge.writeConfig()
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(mcpConfig) }()

	run, runErr := execCLI(ctx, prepared, runtime, mcpConfig, plan)

	state.absorb(run, bridge)
	prepared.conv.usage.addTokens(run.inputTokens, run.outputTokens)

	if runErr != nil {
		return runErr
	}

	// Checked against the STEP's satisfied set, not this attempt's: a verdict
	// emitted before an earlier attempt died still counts, and the resumed
	// model can see it already called the tool.
	return checkCLIObligations(prepared, run, state.satisfied)
}

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
			prepared.ri.AgentName, verdictToolName, strings.Join(prepared.step.VerdictNames(), ", ")))
	}

	//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
	return outcome.Fail(fmt.Errorf("agent %q: finished without a successful call to required tool %q",
		prepared.ri.AgentName, missing[0]))
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
		"--max-turns", strconv.Itoa(plan.maxTurns),
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
// On the HTTP path the upstream steps' decisions and the context_paths files
// arrive as synthetic tool exchanges — messages fabricated into a transcript
// this package owns. There is no transcript to fabricate into here, so the
// same content is prepended to the prompt instead, in the same order.
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
// On the host path that is deliberately all it asks. The HTTP probe sends a
// real request because an endpoint can be reachable and still reject the
// model or the key; a host CLI has no equivalent failure that a cheap check
// would catch, and spawning one to find out would put a process launch in the
// path of every `steps watch` poll. A CLI that is installed but broken fails
// at the step, with the CLI's own error, which is a better message than a
// probe would synthesize.
//
// A CONTAINERIZED CLI inverts that trade, so it gets two more checks — see
// probeCLIImage and probeCLICredentials for why each earns its cost.
func probeCLI(ctx context.Context, ri config.ResolvedInvocation, timeout time.Duration) error {
	binary := config.CLIBinary(ri.CLI)
	if binary == "" {
		return fmt.Errorf("agent %q: no runtime for cli %q", ri.AgentName, ri.CLI)
	}

	if ri.Image == "" {
		_, err := exec.LookPath(binary)
		if err != nil {
			return fmt.Errorf("agent %q: cli %q is not on PATH: %w", ri.AgentName, binary, err)
		}

		return nil
	}

	err := probeCLICredentials(ri)
	if err != nil {
		return err
	}

	return probeCLIImage(ctx, ri, binary, timeout)
}

// probeCLIImage checks that the image actually contains the CLI. Unlike the
// host case, "installed but broken" is not the failure mode worth worrying
// about here — "the operator pointed image: at something that never had the
// CLI in it" is, and it is both easy to do and invisible until a step runs.
// The cost of asking is one short container start, paid once per (image, cli,
// model) per cache window rather than per poll.
//
// --pull=never is what keeps that cost bounded. RunJob pulls every image
// before it reaches preflight, so the image is already local; without the
// flag, an image that somehow is not would be pulled inside this probe's
// timeout, turning a slow download into "the image cannot run the cli" —
// blaming the image for the network. A genuinely absent image fails here
// with docker saying exactly that, which is the truth.
func probeCLIImage(ctx context.Context, ri config.ResolvedInvocation, binary string, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var errBuf bytes.Buffer

	//nolint:gosec // image is validated at load (no leading '-') and binary comes from the static cliProviders table
	cmd := exec.CommandContext(probeCtx, "docker", "run", "--rm", "--pull=never", "--", ri.Image, binary, "--version")
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("agent %q: image %q cannot run %q (%s): %w",
			ri.AgentName, ri.Image, binary, strings.TrimSpace(errBuf.String()), err)
	}

	return nil
}

// probeCLICredentials checks that a containerized CLI has some way to
// authenticate, because neither route is guaranteed to exist and the failure
// is otherwise reported from inside a container as whatever the CLI says
// about being logged out.
//
// Two routes, and which one is available is a property of the MACHINE, not of
// the pipeline — which is exactly why this is a preflight check and not a
// load-time one (a pipeline must not stop loading because it moved to a Mac).
// On Linux a subscription login leaves ~/.claude/.credentials.json, which the
// run mounts read-only. On macOS it lives in the Keychain, which cannot be
// mounted into a container at all, so there api_key_env: is the only route.
func probeCLICredentials(ri config.ResolvedInvocation) error {
	if _, ok := hostCLICredentials(); ok {
		return nil
	}

	if ri.APIKeyEnv != "" && os.Getenv(ri.APIKeyEnv) != "" {
		return nil
	}

	return fmt.Errorf("agent %q: a containerized cli agent has no way to authenticate: "+
		"there is no ~/.claude/.credentials.json to mount (on macOS the subscription login lives in the Keychain, "+
		"which a container cannot read) and source.api_key_env is %s",
		ri.AgentName, describeMissingKeyEnv(ri.APIKeyEnv))
}

// describeMissingKeyEnv distinguishes "you never named a variable" from "you
// named one that is not exported" — different mistakes with different fixes.
func describeMissingKeyEnv(name string) string {
	if name == "" {
		return "unset"
	}

	return fmt.Sprintf("%q, which is not exported", name)
}
