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
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/retry"
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
			// A known contract divergence, accepted deliberately: the CLI's
			// WebFetch takes url+prompt and answers with a model-written
			// summary, where the HTTP path's web_fetch returns the raw body.
			// The native tool is what the CLI's model was trained against,
			// which is the same reasoning as every other row here.
			"web_fetch": "WebFetch",
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
	// ends — the hosted path gets both from its own owning caller
	// (runPreparedWithFailover, RunFix, or subagent.go), never from
	// runConversationLoop itself, and this path calls none of those, so
	// without them a CLI step's spend reached the step counters and stopped
	// there: invisible to a job budget: and missing from the run's usage
	// report.
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

	// What the CLI reported about its own spend, folded in once the attempts
	// are done. Deferred rather than written per attempt because the figures
	// accumulate in state, and the last word on how a step finished belongs
	// to the step rather than to whichever attempt spoke last.
	defer func() { prepared.conv.usage.addCLIReport(state.cachedTokens, state.costUSD, state.finishReason) }()

	var (
		lastErr error
		// nudges counts the rounds spent telling the child its declared
		// assert.files: are missing — see nudgeCLIForMissingFiles.
		nudges int
		// spent counts CHILD INVOCATIONS across every round, which is what
		// the pooled budget is denominated in — see cliRoundAttempts.
		spent int
		// sent is the index of the message being asked. Every invocation
		// after the first rejoins the session, whatever the reason.
		sent int
	)

	attempts := func(attempt int) error {
		spent++

		attemptCtx, cancel := withAgentDeadline(ctx, timeout)
		defer cancel()

		plan := cliAttempt{
			session: session,
			// A nudge round rejoins the same conversation the same way a
			// retried attempt does. Nothing about the child's memory is
			// different; only the reason for speaking to it again is.
			resume: rejoiningCLISession(attempt, nudges, sent),
			home:   stepHome,
			// The turn budget is per STEP, not per attempt — the hosted path
			// counts turns in one conversation that request retries never
			// reset, and a resumed session is the same conversation. The CLI
			// counts turns per invocation, so the remainder is ours to track.
			// max_turns: 0 has no remainder to track: it passes the sentinel
			// through, and cliArgs omits the flag so the CLI applies its own
			// (absent) default.
			maxTurns: remainingCLITurns(prepared.ri.MaxTurns, state.turns),
		}

		plan.prompt = cliAttemptPrompt(plan.resume, attempt > 0, sent, state, prepared)

		if plan.outOfTurns() {
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

		return retry.StopOnDeadline(ctx, attemptCtx, outcome.FailOnDeadline(ctx, attemptCtx, attemptErr))
	}

	runErr := runCLIMessages(ctx, prepared, state, &nudges, &spent, &sent, attempts)

	// The conversation the child had, as the recorder captured it off the
	// stream. Attached however the step ended, like the hosted path attaches
	// its own — a step that died mid-task is the one whose trace is needed
	// most.
	return state.result(prepared.ri.ModelName, prepared.conv.recorder), runErr
}

// runCLIMessages asks the step's messages in turn, each one resuming the same
// session the last was answered in.
//
// A child that finished is woken for a new question exactly as it is woken for
// a missing file: the transcript, the working directory and the task are all
// intact, and it is told the one thing it has not heard. That is what makes
// this the same conversation rather than a second one — a CLI agent has no
// other way to be asked twice, since the child owns its own turn loop and
// exits when it is done.
//
// Two things reset at a message boundary and one deliberately does not.
//
// Required-tool satisfaction resets, for the reason the hosted path resets it:
// carrying it forward would let the child answer the last question without
// calling the verdict tool again, and the step would route on a decision made
// about something else.
//
// The invocation budget resets, because attempts: means "retries for this
// ask". Pooling it across messages would leave a child that died answering the
// third question with nothing left to retry on, having spent the allowance on
// the first.
//
// max_turns: does NOT reset: it bounds the whole conversation, exactly as it
// does on the hosted path, and remainingCLITurns already tracks it per step.
func runCLIMessages(
	ctx context.Context, prepared preparedAgentStep, state *cliStepState,
	nudges, spent, sent *int, attempts func(int) error,
) error {
	messages := prepared.conv.messages
	if len(messages) == 0 {
		messages = []string{""}
	}

	for i := range messages {
		*sent = i

		if i > 0 {
			*spent = 0
			*nudges = 0
			state.satisfied = map[string]bool{}
		}

		err := runCLIRounds(ctx, prepared, state, nudges, spent, i == len(messages)-1, attempts)
		if err != nil {
			return err
		}
	}

	return nil
}

// runCLIRounds spends the step's attempts:, and then keeps waking the child
// for as long as the files its step declared are missing.
//
// The hosted path can interrupt a model mid-conversation because it owns the
// turn loop; here the child owns it, so the equivalent moment is the one
// after it exits. Resuming its session is what makes that the same thing
// rather than a restart — the transcript, the working directory and the task
// are all still there, and the child is told the one fact it could not see
// (cliContinuationPrompt already says exactly that of a failed attempt).
//
// nudges is the caller's, because the attempt closure reads it to decide
// whether it is opening the session or rejoining it.
func runCLIRounds(
	ctx context.Context, prepared preparedAgentStep, state *cliStepState,
	nudges, spent *int, last bool, attempts func(int) error,
) error {
	for ; ; *nudges++ {
		err := retry.Do(ctx, cliRoundAttempts(prepared.ri.Attempts, *spent), attempts)

		// Files are owed by the STEP, so a message with another after it is
		// not the moment to ask for them: the question that produces the file
		// may be the one still unasked.
		if err != nil || !last || !nudgeCLIForMissingFiles(prepared, state, *nudges) {
			//nolint:wrapcheck // the attempt already wrapped and classified its own failure
			return err
		}
	}
}

// rejoiningCLISession reports whether this invocation continues a session
// rather than opening one.
//
// Three reasons to wake a child that is already there, and the child cannot
// tell them apart from the flag alone: the last attempt died, the step still
// owes files, or there is another message to put to it. What differs is only
// what it is told (see cliAttemptPrompt); that it is the same conversation is
// true in every case.
func rejoiningCLISession(attempt, nudges, sent int) bool {
	return attempt > 0 || nudges > 0 || sent > 0
}

// cliRoundAttempts is what one round may spend out of a budget the whole step
// shares.
//
// The two limits answer different questions — attempts: retries a child that
// DIED, a nudge round wakes one that finished owing files — and nothing stops
// a round from doing both, so handing each round a fresh attempts: budget
// MULTIPLIES them: attempts: 3 against five nudges is eighteen real
// invocations of a model, each paid for, under a promise of five chances.
// They pool instead, at attempts + maxFilesNudges for the step, which leaves
// the common attempts: 1 case exactly where it was and stops a retry taken in
// one round from being handed back in the next.
//
// Never below 1: a round that has arrived here is a round that is going to
// run, and zero would make retry.Do's meaning the caller's problem.
func cliRoundAttempts(attempts, spent int) int {
	return max(min(attempts, attempts+maxFilesNudges-spent), 1)
}

// cliAttemptPrompt is what one invocation is told.
//
// A resumed attempt is NOT re-sent the task: the session already holds the
// prompt, the recap and the context blocks, and repeating them invites
// redoing finished work. It is told the one fact it cannot see instead —
// which is the missing file when there is one, and otherwise that its
// predecessor died.
//
// retrying separates the two reasons a session is rejoined, because they are
// not interchangeable: a nudge round wakes a child that FINISHED, so the
// missing file is the whole message; a retry wakes one that DIED, and telling
// it only about the file drops both "continue, do not start over" and the
// verdict-tool reminder cliContinuationPrompt carries. A retry that also owes
// files gets both.
//
// The unmet set is recomputed here rather than captured when the round
// opened: an attempt that wrote one of two declared files and then died
// should be told about the one still missing, not both.
func cliAttemptPrompt(resume, retrying bool, sent int, state *cliStepState, prepared preparedAgentStep) string {
	if !resume {
		return renderCLIPrompt(prepared.conv)
	}

	// A message the child has not been asked yet outranks both other reasons
	// to rejoin: it did not die, and whatever it still owes belongs to the
	// step rather than to this question. Only the message text is sent — the
	// session already holds the task, the context blocks and the upstream
	// decisions, and re-sending them is what invites redoing finished work.
	if next, ok := pendingCLIMessage(prepared, sent, state); ok {
		return next
	}

	unmet := prepared.conv.expect.unmet()

	if retrying {
		prompt := cliContinuationPrompt(state, prepared)
		if len(unmet) > 0 {
			prompt += " " + prepared.conv.expect.nudge(unmet)
		}

		return prompt
	}

	if len(unmet) > 0 {
		return prepared.conv.expect.nudge(unmet)
	}

	return cliContinuationPrompt(state, prepared)
}

// pendingCLIMessage is the message this invocation is opening, when it is one
// the child has not seen.
//
// Every invocation for message N after the first is a retry of that same
// message, and a retry must not re-ask it — the child either died mid-answer,
// in which case it is told to continue, or it owes a file. The distinction is
// whether anything has been absorbed since the boundary, which turns is the
// only per-invocation counter that survives it.
func pendingCLIMessage(prepared preparedAgentStep, sent int, state *cliStepState) (string, bool) {
	if sent <= 0 || sent >= len(prepared.conv.messages) {
		return "", false
	}

	if state.asked >= sent {
		return "", false
	}

	state.asked = sent

	return prepared.conv.messages[sent], true
}

// nudgeCLIForMissingFiles reports whether the child should be woken again
// because its step's assert.files: are still missing.
//
// False when there is nothing missing, when the allowance is spent, or when
// the step has no turns left to spend on another round — after which
// assertAgentResponse reports the mismatch exactly as it would have anyway.
// The nudge buys the model chances; it never changes the verdict on them.
func nudgeCLIForMissingFiles(prepared preparedAgentStep, state *cliStepState, nudges int) bool {
	unmet := prepared.conv.expect.unmet()
	if len(unmet) == 0 || nudges >= maxFilesNudges {
		return false
	}

	if remainingCLITurns(prepared.ri.MaxTurns, state.turns) == 0 {
		return false
	}

	slog.Info("agent.cli.files_nudge",
		"agent", prepared.ri.AgentName,
		"cli", prepared.ri.CLI,
		"round", nudges+1,
		"of", maxFilesNudges,
		"unmet", strings.Join(unmet, "; "))

	return true
}

// remainingCLITurns is the CLI path's share of the step's turn budget, with
// max_turns: 0 carried through as the sentinel rather than turned into a
// number — subtracting spent turns from "no cap" is how an uncapped step
// would have acquired one.
//
// An overspent budget floors at 0 rather than going negative, because the
// sentinel IS a negative number and the two must never collide: the CLI
// reports num_turns in its own units (see clistream.go), so a capped step can
// legitimately come back having spent MORE than its ceiling, and a bare
// subtraction lands on exactly -1 often enough to matter. That value would
// read as "uncapped" and hand the next attempt no --max-turns at all — the
// precise opposite of the exhausted budget it represents.
func remainingCLITurns(maxTurns, spent int) int {
	if maxTurns == 0 {
		return unlimitedTurns
	}

	return max(maxTurns-spent, 0)
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

// outOfTurns reports whether earlier attempts have spent the step's whole
// turn budget. An uncapped step never has, which is why the sentinel is
// tested before the number: unlimitedTurns is negative, and would otherwise
// read as the most exhausted budget of all.
func (p cliAttempt) outOfTurns() bool {
	return p.maxTurns != unlimitedTurns && p.maxTurns <= 0
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
	// asked is the highest message index already put to the child, so a retry
	// of that message resumes it rather than asking it twice.
	asked        int
	text         string
	turns        int
	trajectory   []recordedToolCall
	verdict      string
	note         string
	satisfied    map[string]bool
	cachedTokens int
	costUSD      float64
	finishReason string
}

func newCLIStepState() *cliStepState {
	return &cliStepState{satisfied: map[string]bool{}}
}

// absorb folds one attempt's observations in.
func (s *cliStepState) absorb(run cliRunResult, bridge *cliBridge) {
	verdict, note, satisfied, bridgeCalls := bridge.observed()

	s.turns += run.turns
	s.trajectory = append(s.trajectory, mergeCLITrajectory(run.trajectory, bridgeCalls)...)

	// Accumulated like everything else here: the attempts share one
	// conversation, so what a resumed run spent adds to what the first one
	// did rather than replacing it.
	s.cachedTokens += run.cachedTokens
	s.costUSD += run.costUSD

	// Last non-empty wins, matching the answer and the verdict below: a
	// crashed attempt reports no subtype and must not erase how an earlier
	// one finished.
	if run.errSubtype != "" {
		s.finishReason = run.errSubtype
	}

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
func (s *cliStepState) result(modelName string, rec *transcriptRecorder) conversationResult {
	return conversationResult{
		text:       s.text,
		turns:      s.turns,
		trajectory: s.trajectory,
		model:      modelName,
		verdict:    s.verdict,
		note:       s.note,
		transcript: rec.recorded(),
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
