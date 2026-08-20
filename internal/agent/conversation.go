package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/outcome"
)

// agentGenParams holds the generation dials an agent configures. Unset
// fields (nil pointers, zero maxTokens, empty reasoning) are left off the
// request so the model's own defaults apply.
type agentGenParams struct {
	temperature *float64
	topP        *float64
	maxTokens   int
	reasoning   string // "", "low", "medium", or "high"
}

//nolint:gochecknoglobals // static, read-only lookup table
var reasoningLevels = map[string]genai.ThinkingLevel{
	"low":    genai.ThinkingLevelLow,
	"medium": genai.ThinkingLevelMedium,
	"high":   genai.ThinkingLevelHigh,
}

// applyTo sets the configured dials on a genai generation config.
func (p agentGenParams) applyTo(cfg *genai.GenerateContentConfig) {
	if p.temperature != nil {
		t := float32(*p.temperature)
		cfg.Temperature = &t
	}

	if p.topP != nil {
		t := float32(*p.topP)
		cfg.TopP = &t
	}

	if p.maxTokens > 0 {
		tokens := min(p.maxTokens, math.MaxInt32)
		cfg.MaxOutputTokens = int32(tokens) //nolint:gosec // clamped to MaxInt32 on the line above
	}

	if level, ok := reasoningLevels[p.reasoning]; ok {
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: level}
	}
}

// recordedToolCall is one tool invocation the model requested during a
// conversation, captured in the order it was requested. args holds the
// MODEL-authored arguments as they arrived, before any max_calls: budget
// check or args: pinning (see mergePinnedArgs) — so an assert.tool_calls
// check judges what the model actually chose, and a machine-pinned value is
// deliberately not matchable.
//
// Every requested call is recorded, including one later rejected by a
// max_calls: budget: the model did make that call (and saw the rejection as
// data), so it is part of the trajectory its behavior should be judged on.
type recordedToolCall struct {
	name string
	args map[string]any
	// ok reports whether the call actually succeeded, per the same shapes
	// requiredCallSucceeded reads. Backfilled once the turn's results are in
	// (see markTrajectoryResults), so it is false for a call that failed or
	// was rejected by a max_calls: budget without ever running. Trajectory
	// CONSUMERS differ on whether they want it: assert.tool_calls judges what
	// the model chose, success or not, while a handoff note's computed
	// files-touched section must list only what genuinely happened.
	ok bool
}

// conversationResult is one completed attempt's output: the model's final
// text, the number of turns it took, the ordered trajectory of tool calls it
// made, and — for a verdict agent — the verdict it emitted. Returned (rather
// than accumulated in the caller) so each attempt of a retry reports only its
// own calls.
type conversationResult struct {
	text       string
	turns      int
	trajectory []recordedToolCall
	// wrappedUp records that this answer came from the final tool-less turn
	// after the budget ran out, rather than from the model deciding it was
	// done. A degraded answer that does not say it is degraded reads exactly
	// like a confident one.
	wrappedUp bool
	// model names the model that actually served this conversation, set ONLY
	// when it was a fallback rather than the agent's configured primary. It is
	// recorded with the result so a quality dip caused by an outage can be
	// traced afterwards instead of looking like a normal run.
	model string
	// verdict is the choice from the last SUCCESSFUL call to the synthesized
	// verdict tool (see agentConversation.verdictTool); "" when the step
	// declares no verdicts or the model never emitted one. internal/pipeline
	// routes on it.
	verdict string
	// note is the optional free-text note attached to that same successful
	// verdict call (see buildVerdictTool); "" when there was none. It travels
	// with the verdict it accompanied — a step that declared context: {
	// from: { <this step>: note|full } } is handed it (see upstream.go).
	note string
	// transcript is the full ordered exchange — model text, tool calls,
	// results, nested sub-agent traces — attached on every exit path.
	// Persisted to node_transcripts (see saveAgentTranscript), never into
	// nodes.result, which stays bounded to the trajectory.
	//
	// Attached by runAgentConversation on the hosted path, and by
	// runCLIConversation on the delegated one, which never enters this loop
	// and reads the child's own stream instead (clistream.go).
	transcript []transcriptEvent
	// checkpoint is everything this attempt would have to hand the next
	// source for a swap to CONTINUE the conversation rather than restart it.
	// Set on every return path, success or failure alike. It is internal
	// plumbing, not part of what a step records — see resumeCheckpoint.
	checkpoint resumeCheckpoint
}

// resumeCheckpoint is a conversation's position, carried from one source to
// the next when the mid-run fallback: cascade (failover.go) swaps sources.
//
// It is ONE struct rather than a set of parallel resume*/end* field pairs
// because every field here was added the same way: something turned out not
// to survive a swap, and the fix was another pair threaded through
// agentConversation, conversationResult, seedResumeState, every
// conversationResult literal in runConversationLoop, and failover.go's copy
// block. Each omission was a real bug — a required tool re-firing its side
// effect, a budgeted tool getting a fresh allowance per source, a turn
// ceiling multiplying by the length of the fallback list. Grouping them means
// the next thing a conversation must remember is added in one place and
// carried by construction, instead of relying on five call sites being
// updated together.
type resumeCheckpoint struct {
	// contents is the request's accumulated message history. Seeding a
	// request with it verbatim (see buildAgentRequest) is what makes a swap a
	// resume rather than a restart.
	contents []*genai.Content
	// satisfied and callCounts are required-tool and max_calls: bookkeeping.
	// The message history alone says WHAT already happened, not which
	// required tools that satisfies or how much of each budget it spent:
	// without these a resumed attempt would force an already-satisfied
	// required tool to fire its side effect again, and hand a budgeted tool a
	// fresh allowance on every source the cascade tries.
	satisfied  map[string]bool
	callCounts map[string]int
	// trajectory is the calls made so far, so assert.tool_calls and the
	// recorded audit trail describe the whole step rather than whichever
	// source happened to finish it.
	trajectory []recordedToolCall
	// verdict and note are the last successful verdict-tool choice and its
	// note, so an infrastructure hiccup cannot lose a decision the model had
	// already made.
	verdict string
	note    string
	// turnsSpent is how many turns the conversation has already used, across
	// every source so far. max_turns: is a ceiling on the STEP, so each
	// source continues the count instead of restarting it — otherwise a
	// declared cap of 30 permits 30 turns per fallback entry. This mirrors
	// what cli.go's session rejoin already does for a CLI source.
	turnsSpent int
	// summary and stalled are maybeCompact's running state (compaction.go).
	// Without them a swap re-initializes the running summary to empty, so the
	// fallback summarizes a history that already CONTAINS a summary with no
	// prior-summary anchor — compounding the loss — and re-pays for a
	// summarization the primary already proved impossible.
	summary string
	stalled bool
}

// agentConversation is one runnable attempt's inputs.
type agentConversation struct {
	system        string
	prompt        string
	contextBlocks []contextBlock
	// upstream is what named earlier steps decided, delivered as synthetic
	// read_step results because this step declared context: { from: ... }.
	// See upstream.go.
	upstream []contextBlock
	env      toolEnv
	tools    agentTools
	params   agentGenParams
	maxTurns int
	// toolChoiceStringOnly forces a required tool call via the string
	// tool_choice: "required" instead of a named function object — see
	// forceRequiredTool. Set from config.ResolvedInvocation.StringOnlyToolChoice.
	toolChoiceStringOnly bool
	// verdictTool is the name of the synthesized required verdict tool when the
	// step declares verdicts:, else "". A successful call to it records the
	// chosen verdict into conversationResult.verdict.
	verdictTool string
	// compactAfterTokens caps req.Contents' estimated size before older turns
	// are summarized away and replaced by a running summary — see maybeCompact
	// in compaction.go. 0 disables compaction entirely: the turn loop then
	// behaves exactly as it did before this field existed.
	compactAfterTokens int
	// usage tallies the provider's own reported token counts across this
	// conversation and enforces the agent's per-invocation budget: (see
	// usage.go). Never nil — a step with no budget still counts, because the
	// report is worth having on its own.
	usage *stepUsage
	// recorder captures this conversation's transcript and publishes it live.
	// nil means runAgentConversation makes its own, which records but
	// publishes nowhere — the shape a test or a plain terminal run gets. A
	// sub-agent is handed a child recorder so its work is visible while it
	// happens (see transcriptRecorder.childRecorder).
	recorder *transcriptRecorder
	// resume, when set, continues a conversation a previous source got part
	// way through instead of starting a fresh one — see resumeCheckpoint for
	// what has to travel and why. nil on a first attempt, which is the
	// fresh-start behavior this had before resuming existed.
	resume *resumeCheckpoint
}

// syntheticToolExchange builds the call/result message pair that makes
// content look like the answer to a tool the model already called: an
// assistant turn requesting name(args), then the matching result.
//
// It is how anything reaches a conversation without costing a turn or
// depending on the model choosing to ask — context_paths files and an
// upstream step's decision both arrive this way.
func syntheticToolExchange(callID, name string, args map[string]any, content string) []*genai.Content {
	return []*genai.Content{
		{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: callID, Name: name, Args: args}}},
		},
		{
			Role: genai.RoleUser,
			Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{
				ID:       callID,
				Name:     name,
				Response: map[string]any{"content": content},
			}}},
		},
	}
}

// buildAgentRequest builds a fresh LLM request (system + user prompt + tools
// + dials). When conv.contextBlocks is non-empty, synthetic read_file tool
// call/response messages are prepended before the user prompt so the model
// sees the context as if it had called read_file itself. A fresh request is
// built per attempt so a retry starts from a clean conversation rather than
// the grown Contents of a failed attempt.
func buildAgentRequest(conv agentConversation) *model.LLMRequest {
	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: conv.system}}},
		Tools:             []*genai.Tool{conv.tools.decls},
	}
	conv.params.applyTo(cfg)

	if conv.resume != nil && conv.resume.contents != nil {
		return &model.LLMRequest{Contents: conv.resume.contents, Config: cfg}
	}

	contents := make([]*genai.Content, 0, 3+(len(conv.contextBlocks)+len(conv.upstream))*2)

	// The decisions this step asked upstream steps for come first: they are
	// what happened BEFORE this step, and the context_paths files below are
	// what this step was handed to work on.
	for i, block := range conv.upstream {
		contents = append(contents, syntheticToolExchange(
			fmt.Sprintf("upstream_%d", i), readStepToolName, map[string]any{"step": block.path}, block.content)...)
	}

	for i, block := range conv.contextBlocks {
		contents = append(contents, syntheticToolExchange(
			fmt.Sprintf("ctx_%d", i), "read_file", map[string]any{"path": block.path}, block.content)...)
	}

	// User prompt comes after any injected context.
	contents = append(contents, &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: conv.prompt}},
	})

	return &model.LLMRequest{
		Contents: contents,
		Config:   cfg,
	}
}

// runAgentConversation runs one conversation: an initial system+user message,
// then up to conv.maxTurns request/tool-execute/append round trips,
// terminating when the model responds with no tool calls while every required
// tool has succeeded.
//
// It runs ONCE per step. Nothing restarts it: attempts: retries the failing
// REQUEST (see requests.go), so a transient fault is absorbed without losing a
// single accumulated turn. A tool failure, required or not, is handed back
// into this same conversation as data and is never a reason to abort. That
// also retires a real hazard — a restart used to re-invoke tool calls whose
// side effects had already happened (posting a PR review, say), because the
// workspace survived the restart while the memory did not.
//
// If the model tries to stop without every required tool having succeeded
// (exit_code 0 — a failed call doesn't count, and the model already saw why
// it failed via the appended tool response), its next turn is constrained
// via the provider's tool_choice (see forceRequiredTool) to a function call
// for one unsatisfied tool — a hard API-level constraint the model cannot
// decline, not a text reminder it could still ignore. One unsatisfied tool
// is forced per turn (repeated as needed for more than one), still bounded
// by maxTurns: a provider that doesn't honor tool_choice, or a model that
// keeps failing the call anyway, still hits the turn cap rather than
// looping forever.
//
// conv.toolChoiceStringOnly swaps that named-function force for the generic
// tool_choice: "required" string form — some OpenAI-compat local servers
// (e.g. LM Studio) 400 on the named-object form entirely. The generic form
// only guarantees *some* tool call, not the missing one specifically, so a
// model can still stall on the wrong tool until maxTurns; that's the same
// safety bound already documented above.
//
// When conv.compactAfterTokens > 0, maybeCompact (compaction.go) runs at the
// start of every turn, before generateOnce: once req.Contents' estimated size
// crosses that budget, older turns are summarized via the agent's own model
// and replaced by the summary, so a long conversation doesn't grow without
// bound or overflow the model's real context window. A summarization failure
// is logged and the turn proceeds with req.Contents unchanged — the same
// failure-is-data treatment every other tool/transport failure in this loop
// gets, never a reason to abort the attempt. compactAfterTokens == 0 skips
// this entirely — an agent reaches that only via an explicit
// compact_after_tokens: 0, since config.ResolveAgentInvocation otherwise
// resolves an unset field to a nonzero default (see
// defaultCompactAfterTokens), unlike every other value-gated field in this
// package.
func runAgentConversation(ctx context.Context, llm model.LLM, conv agentConversation) (conversationResult, error) {
	// The recorder is attached here, not inside the loop, so every exit path
	// — success, transport error, loop detection, turn exhaustion — carries
	// whatever was captured up to that point. It rides in conv.env because
	// the env already reaches every toolImpl (see toolEnv.transcript).
	rec := conv.recorder
	if rec == nil {
		rec = &transcriptRecorder{}
	}

	conv.env.transcript = rec

	res, err := runConversationLoop(ctx, llm, conv)
	res.transcript = rec.events

	return res, err
}

// seedResumeState builds runConversationLoop's per-conversation bookkeeping,
// seeded from a resumed attempt's checkpoint rather than starting empty, so a
// mid-run source swap continues the conversation instead of restarting it —
// see resumeCheckpoint for what each field prevents. A first attempt has no
// checkpoint, which yields exactly the fresh-start behavior this had before
// resuming existed.
func seedResumeState(conv agentConversation) resumeCheckpoint {
	state := resumeCheckpoint{}
	if conv.resume != nil {
		state = *conv.resume
	}

	if state.satisfied == nil {
		state.satisfied = make(map[string]bool, len(conv.tools.required))
	}

	if state.callCounts == nil {
		state.callCounts = make(map[string]int, len(conv.tools.maxCalls))
	}

	return state
}

// unlimitedTurns is remainingTurns' answer for a step that declared
// max_turns: 0. It is deliberately not a very large number: a sentinel the
// loop tests for cannot be reached by a long conversation, whereas a ceiling
// of math.MaxInt silently becomes a real one for anything that subtracts
// from it (the CLI path spends turns across attempts and does exactly that).
const unlimitedTurns = -1

// maxIgnoredForces is how many turns in a row the model may answer with text
// while a forced tool call goes unmade before the attempt is failed.
//
// It costs a capped step nothing but wasted turns: the verdict at the end of
// the old path was the same task-level failure naming the same unsatisfied
// tools, just reached thirty turns later. Five matches loopDetectionMaxRepeats
// on the same premise — a model that has declined an API-level constraint five
// consecutive times is not about to comply on the sixth — and any tool call at
// all resets it, so a model that stalls once and then complies is unaffected.
const maxIgnoredForces = 5

// remainingTurns is how many turns this source may still spend: the step's
// declared max_turns: less whatever earlier sources already used. Never
// negative for a capped step — a checkpoint at or past the ceiling yields 0,
// and the loop falls straight through to outOfTurns, which is the right
// answer for a step that has already spent its whole allowance.
func remainingTurns(conv agentConversation, spent int) int {
	if conv.maxTurns == 0 {
		return unlimitedTurns
	}

	return max(conv.maxTurns-spent, 0)
}

// runConversationLoop is runAgentConversation's body, split out so the
// transcript attaches once at the boundary instead of at each of the loop's
// several return sites.
func runConversationLoop(ctx context.Context, llm model.LLM, conv agentConversation) (conversationResult, error) {
	// conv.usage.finish() is NOT called here — a resumed attempt (see
	// failover.go) reuses the same *stepUsage across more than one call to
	// this function, and finish() is not safe to call twice on one pointer
	// (it adds this attempt's spend into the job total unconditionally, and
	// double-charges a delegating parent). Whichever caller owns a
	// conversation's whole lifetime — RunFix for its one-shot conversation,
	// runPreparedWithFailover for a step's whole (possibly multi-source)
	// sequence, subagent.go's preparedSubAgent.run for a delegation — calls
	// finish() exactly once, after that lifetime ends.
	conv.usage = attachUsage(ctx, conv.usage)

	// Publish this conversation's accumulator to its tools, so a sub-agent
	// call can size the child's allowance against what THIS invocation has
	// left. Set here rather than at env construction because attachUsage
	// above is what guarantees a non-nil accumulator to hand over.
	conv.env.usage = conv.usage

	req := buildAgentRequest(conv)
	state := seedResumeState(conv)
	turnsBefore := state.turnsSpent

	// result snapshots the conversation's position for whichever exit path is
	// taken next. turns counts the WHOLE step, across every source the
	// cascade has tried, so a resumed attempt reports (and is bounded by) the
	// total rather than its own share of it.
	result := func(text string, spentThisSource int) conversationResult {
		state.contents = req.Contents
		state.turnsSpent = turnsBefore + spentThisSource

		return conversationResult{text: text, turns: state.turnsSpent, trajectory: state.trajectory,
			verdict: state.verdict, note: state.note, checkpoint: state}
	}

	// detector is per-attempt: it watches for the model repeating one
	// identical tool interaction (same call, same result) until it is clearly
	// stuck, warns once, then fails the attempt — see loop.go.
	detector := newLoopDetector()

	// ignoredForces counts CONSECUTIVE turns where a required tool was
	// unsatisfied, tool_choice forced it, and the model answered with text
	// anyway. The detector cannot see these: it hashes tool INTERACTIONS, and
	// a turn with no tool call produces none — so this is the only thing
	// standing between a provider that disregards tool_choice and a loop that
	// never ends. max_turns used to be that backstop, which stopped being true
	// the moment max_turns: 0 became expressible.
	ignoredForces := 0

	// budget is fixed before the loop so the sentinel is compared, not
	// counted down: an uncapped conversation ends by answering, by the step's
	// deadline, by its token budget, or by loop detection — never here.
	budget := remainingTurns(conv, turnsBefore)

	turn := 0

	for ; budget == unlimitedTurns || turn < budget; turn++ {
		state.summary, state.stalled = maybeCompact(ctx, llm, req, conv, state.summary, state.stalled)

		// The budget is checked before the turn's tool calls run: a step that
		// has already blown its ceiling must not go on to have side effects.
		resp, err := conv.generateWithinBudget(ctx, llm, req)
		if err != nil {
			return result("", turn), err
		}

		req.Contents = append(req.Contents, resp.Content)

		calls, text := collectParts(resp.Content)
		conv.env.transcript.text(text)

		if len(calls) == 0 {
			if conv.finishOrForce(req, state.satisfied) {
				return result(text, turn+1), nil
			}

			ignoredForces++

			forceErr := conv.forcingIsGoingNowhere(ignoredForces, state.satisfied)
			if forceErr != nil {
				return result("", turn+1), forceErr
			}

			continue
		}

		ignoredForces = 0

		req.Config.ToolConfig = nil // clear any forcing from the prior turn — the model chooses freely again next time it tries to stop

		turnStart := len(state.trajectory)

		for _, call := range calls {
			state.trajectory = append(state.trajectory, recordedToolCall{name: call.Name, args: call.Args})
			conv.env.transcript.call(call.Name, call.Args)
		}

		parts := toolResponseParts(ctx, calls, conv.env, conv.tools.registry, conv.tools.maxCalls, state.callCounts)
		markTrajectoryResults(state.trajectory[turnStart:], parts)
		conv.env.transcript.results(parts)

		if choice, n := conv.trackToolResults(parts, state.satisfied); choice != "" {
			state.verdict, state.note = choice, n // last successful verdict (and its note) wins across turns
		}

		req.Contents = append(req.Contents, &genai.Content{
			Role:  genai.RoleUser,
			Parts: parts,
		})

		detectErr := detector.respond(req, calls, parts)
		if detectErr != nil {
			return result("", turn+1), detectErr
		}
	}

	// Both terminal outcomes here are task-level failures (the conversation
	// ran but didn't finish its task), not infrastructure errors — mark them
	// so hook dispatch classifies them as failed rather than errored. A
	// transport error from generateOnce above stays unwrapped → errored.
	exhausted := result("", turn)

	return conv.outOfTurns(ctx, llm, req, exhausted, state.satisfied)
}

// forcingIsGoingNowhere ends an attempt whose model has answered with text
// through maxIgnoredForces consecutive forced tool calls. nil means carry on.
//
// The failure is task-level, matching what the turn cap used to produce on
// this same path — the conversation ran and did not finish its task, which is
// not an infrastructure fault.
func (conv agentConversation) forcingIsGoingNowhere(ignored int, satisfied map[string]bool) error {
	if ignored < maxIgnoredForces {
		return nil
	}

	//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
	return outcome.Fail(fmt.Errorf(
		"agent ignored a forced tool call %d turns running; required tool(s) never succeeded: %s",
		ignored, strings.Join(unsatisfiedRequiredTools(conv.tools.required, satisfied), ", ")))
}

// outOfTurns decides what a conversation that used every turn is worth.
func (conv agentConversation) outOfTurns(
	ctx context.Context, llm model.LLM, req *model.LLMRequest,
	exhausted conversationResult, satisfied map[string]bool,
) (conversationResult, error) {
	missing := unsatisfiedRequiredTools(conv.tools.required, satisfied)
	if len(missing) > 0 {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return exhausted, outcome.Fail(fmt.Errorf("agent exceeded %d turns; required tool(s) never succeeded: %s", conv.maxTurns, strings.Join(missing, ", ")))
	}

	// One more request, with the tools taken away, telling the model to answer
	// from what it already has.
	//
	// Without this a spent budget DESTROYS the work rather than ending it: a
	// model that was still reading on its last turn returns no text at all, and
	// a step that had done twelve turns of useful investigation fails with
	// nothing to show. That is the same mistake the block budget was built to
	// avoid one level up — stop starting new work, keep what finished — and it
	// is worse here, because nothing downstream can salvage an empty answer.
	//
	// Withheld tools rather than a polite request to stop: a model that has
	// spent every turn calling tools has already demonstrated it will not stop
	// when asked.
	text, err := conv.answerWithoutTools(ctx, llm, req)

	// The wrap-up request FAILING is not the model declining to answer, and the
	// two must not collapse into one message and one class. A 503 on this last
	// request, or a token ceiling breached by it, is infrastructure — it has to
	// stay unmarked so it classifies as `errored` and fires on_error, exactly
	// as the identical failure would one turn earlier inside the loop. Wrapping
	// it in outcome.Fail below (which is what "err == nil &&" used to fall
	// through to) reported a provider outage as a task-level failure, and threw
	// away the cause with it.
	if err != nil {
		slog.Warn("agent.turns_exhausted_wrapup_failed", "max_turns", conv.maxTurns, "error", err)

		return exhausted, fmt.Errorf("agent exceeded %d turns, and the request asking it to answer from what it had failed: %w", conv.maxTurns, err)
	}

	if strings.TrimSpace(text) != "" {
		slog.Warn("agent.turns_exhausted_answered", "max_turns", conv.maxTurns,
			"detail", "the turn budget ran out; the model answered from what it had rather than finishing on its own")

		exhausted.text = text
		exhausted.wrappedUp = true
		conv.env.transcript.text(text)

		return exhausted, nil
	}

	//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
	return exhausted, outcome.Fail(fmt.Errorf("agent exceeded %d turns without a final response, and produced none when asked to answer from what it had", conv.maxTurns))
}

// answerWithoutTools makes the final request of a conversation whose budget is
// spent: no tools offered, and an instruction to answer now.
//
// The request is a copy in everything that matters — the tool grant and any
// forcing are cleared on the live request, but the conversation is over, so
// nothing reads them again.
func (conv agentConversation) answerWithoutTools(ctx context.Context, llm model.LLM, req *model.LLMRequest) (string, error) {
	req.Contents = append(req.Contents, &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{Text: "You have used your entire turn budget, and no further tool calls are " +
			"possible. Answer now, using only what you have already gathered. If your " +
			"investigation was incomplete, give your best answer and say plainly which " +
			"parts you could not verify."}},
	})

	req.Config.Tools = nil
	req.Config.ToolConfig = nil

	resp, err := conv.generateWithinBudget(ctx, llm, req)
	if err != nil {
		return "", err
	}

	_, text := collectParts(resp.Content)

	return text, nil
}

// trackToolResults marks required tools satisfied for this turn's results and
// returns the verdict (and its accompanying note, if any) a successful
// verdict-tool call carried — both "" if none this turn. satisfied is
// mutated in place; the returned verdict/note let the caller keep "last
// successful verdict wins" across turns.
func (conv agentConversation) trackToolResults(parts []*genai.Part, satisfied map[string]bool) (verdict, note string) {
	for _, part := range parts {
		name := part.FunctionResponse.Name

		if conv.tools.required[name] && requiredCallSucceeded(part.FunctionResponse.Response) {
			satisfied[name] = true
		}

		if conv.verdictTool != "" && name == conv.verdictTool {
			if choice, ok := part.FunctionResponse.Response["verdict"].(string); ok && choice != "" {
				verdict = choice
				note, _ = part.FunctionResponse.Response["note"].(string)
			}
		}
	}

	return verdict, note
}

// forceRequiredTool constrains req's next generateOnce call to a tool call,
// via the provider's tool_choice mechanism (see genaiopenai's ToolConfig →
// tool_choice mapping) — the model cannot decline or reply with plain text
// on that turn. Cleared once any tool call comes back (see
// runAgentConversation) so later turns aren't stuck forced to the same
// name.
//
// stringOnly == false (the default) names name specifically, mapping to
// OpenAI's named tool_choice object — the precise force. stringOnly == true
// leaves AllowedFunctionNames empty, which genaiopenai instead maps to the
// generic string tool_choice: "required" — some OpenAI-compat local servers
// (e.g. LM Studio) reject the named-object form outright.
func forceRequiredTool(req *model.LLMRequest, name string, stringOnly bool) {
	fcc := &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}
	if !stringOnly {
		fcc.AllowedFunctionNames = []string{name}
	}

	req.Config.ToolConfig = &genai.ToolConfig{FunctionCallingConfig: fcc}
}

// requiredCallSucceeded reports whether a tool's FunctionResponse data
// represents success, per toolResponseParts' own documented failure shapes
// (a nonzero exit_code, or an {"error": ...} result): shell-backed tools
// (run_shell, custom tools) succeed iff exit_code == 0 — shellToolResult
// always sets exit_code on anything that actually ran, so its absence means
// the command never ran at all, also not success. MCP tools (the other
// required:-capable kind — see config.validateMCPToolFields) carry no
// exit_code at all; for those, absence of an "error" key is success (see
// mcpToolImpl's {"structured_content", "content"} vs {"error"} shapes).
func requiredCallSucceeded(resp map[string]any) bool {
	if code, ok := resp["exit_code"].(int); ok {
		return code == 0
	}

	_, hasError := resp["error"]

	return !hasError
}

// unsatisfiedRequiredTools returns a sorted list of names present in
// required but not yet in satisfied, or nil if none remain. A tool stays
// unsatisfied across as many failed calls as it takes — see
// requiredCallSucceeded — so the model can keep retrying it in-session
// instead of the whole attempt being aborted.
func unsatisfiedRequiredTools(required, satisfied map[string]bool) []string {
	missing := make([]string, 0, len(required))

	for name := range required {
		if !satisfied[name] {
			missing = append(missing, name)
		}
	}

	sort.Strings(missing)

	return missing
}

// generateOnce drains llm.GenerateContent's iterator for the non-streaming
// case, which yields exactly one (response, error) pair.
func generateOnce(ctx context.Context, llm model.LLM, req *model.LLMRequest) (*model.LLMResponse, error) {
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, fmt.Errorf("agent: generate content: %w", err)
		}

		if resp == nil || resp.Content == nil {
			return nil, errors.New("agent: model returned an empty response")
		}

		return resp, nil
	}

	return nil, errors.New("agent: model returned no response")
}

// generateWithinBudget makes one model call and folds its reported token usage
// into this step's and the job's totals, failing the conversation when either
// ceiling is breached.
func (conv agentConversation) generateWithinBudget(ctx context.Context, llm model.LLM, req *model.LLMRequest) (*model.LLMResponse, error) {
	resp, err := generateOnce(ctx, llm, req)
	if err != nil {
		return nil, err
	}

	if conv.usage.record(resp) {
		return nil, conv.usage.exceededError()
	}

	return resp, nil
}

// attachUsage binds a conversation's token accounting to the job it runs in.
// A nil counter is defaulted rather than required, so a caller that only cares
// about the conversation (every unit test in this package) needn't build one.
func attachUsage(ctx context.Context, usage *stepUsage) *stepUsage {
	if usage == nil {
		usage = &stepUsage{}
	}

	usage.run = RunUsageFrom(ctx)

	return usage
}

// finishOrForce handles a turn in which the model requested no tools. It
// reports true when the conversation is genuinely done — every required tool
// has succeeded — and otherwise constrains the next turn to the first
// still-unsatisfied required tool and reports false.
func (conv agentConversation) finishOrForce(req *model.LLMRequest, satisfied map[string]bool) bool {
	missing := unsatisfiedRequiredTools(conv.tools.required, satisfied)
	if len(missing) == 0 {
		return true
	}

	forceRequiredTool(req, missing[0], conv.toolChoiceStringOnly)

	return false
}

// collectParts splits a model turn into the tool calls it requested and the
// plain (non-thought) text it emitted.
func collectParts(content *genai.Content) (calls []*genai.FunctionCall, text string) {
	var b strings.Builder

	for _, part := range content.Parts {
		switch {
		case part.FunctionCall != nil:
			calls = append(calls, part.FunctionCall)
		case part.Text != "" && !part.Thought:
			b.WriteString(part.Text)
		}
	}

	return calls, b.String()
}

// markTrajectoryResults backfills the ok flag on one turn's freshly-recorded
// calls from that turn's results. turn and parts are index-aligned:
// toolResponseParts appends exactly one part per call, in order. A length
// mismatch would mean that contract changed, so it degrades to leaving every
// call marked unsuccessful rather than pairing a call with the wrong result.
func markTrajectoryResults(turn []recordedToolCall, parts []*genai.Part) {
	if len(turn) != len(parts) {
		return
	}

	for i, part := range parts {
		if part.FunctionResponse == nil {
			continue
		}

		turn[i].ok = requiredCallSucceeded(part.FunctionResponse.Response)
	}
}

// toolResponseParts executes each requested tool and packages the results as
// FunctionResponse parts to feed back on the next turn. No call's failure
// (a nonzero exit_code, or an {"error": ...} result) stops any other call in
// the turn, or the conversation — every failure is just data the model sees
// on its next turn and can react to. maxCalls/callCounts enforce each
// budgeted tool's max_calls: — see executeBudgetedTool.
func toolResponseParts(ctx context.Context, calls []*genai.FunctionCall, env toolEnv, registry map[string]toolImpl, maxCalls, callCounts map[string]int) []*genai.Part {
	parts := make([]*genai.Part, 0, len(calls))

	for _, call := range calls {
		response := executeBudgetedTool(ctx, call, env, registry, maxCalls, callCounts)

		parts = append(parts, &genai.Part{
			FunctionResponse: &genai.FunctionResponse{ID: call.ID, Name: call.Name, Response: response},
		})
	}

	return parts
}

// executeBudgetedTool enforces call.Name's max_calls: budget (if any) before
// dispatching to executeAgentTool: once callCounts[call.Name] has reached its
// budget, the call is rejected as ordinary tool-result data — never executed,
// since the budget's whole point is bounding side effects — so the model sees
// the rejection and can react on its next turn instead of the attempt
// aborting. A successful dispatch increments the counter; a rejected one does
// not (it's already at the ceiling). Tools absent from maxCalls are
// unlimited.
//
// Every call (budget-rejected or dispatched) is bracketed by a debug-level
// "agent.tool_call"/"agent.tool_result" log pair — the args a call was made
// with, and how it finished (duration, error/exit_code, plus a size-capped
// preview of the rest of the result via debugToolResultPreview — the gap
// that once made a model silently abandoning a granted tool for a worse
// fallback undiagnosable without an out-of-band probe). Debug logging is
// opt-in, so this is silent
// unless an operator has already asked to see command/output content at
// that level; call.Args/the result can hold a model-authored value (e.g. a
// custom tool's args, or write_file's content), no different from the full
// command/output internal/shell already logs at this level.
func executeBudgetedTool(ctx context.Context, call *genai.FunctionCall, env toolEnv, registry map[string]toolImpl, maxCalls, callCounts map[string]int) map[string]any {
	// Which run/job/step/depth this call belongs to — the identity every
	// nested call (sub-agent, MCP, custom tool) shares with its enclosing
	// conversation, read from the recorder rather than threaded as
	// parameters. Zero-valued outside a conversation (tests, direct calls).
	live := env.transcript.liveIdentity()

	slog.Debug("agent.tool_call", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"args", call.Args)

	start := time.Now()

	var response map[string]any

	if budget, ok := maxCalls[call.Name]; ok && callCounts[call.Name] >= budget {
		response = map[string]any{"error": fmt.Sprintf("%s: call budget (%d) exhausted for this attempt", call.Name, budget)}
	} else {
		response = executeAgentTool(ctx, call, env, registry)
		callCounts[call.Name]++
	}

	slog.Debug("agent.tool_result", "tool", call.Name, "id", call.ID,
		"run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex, "depth", live.depth,
		"duration", time.Since(start), "error", response["error"], "exit_code", response["exit_code"],
		"result", lazyToolResultPreview{response: response})

	return response
}

// lazyToolResultPreview defers rendering a tool result until a handler
// actually formats the record.
//
// Debug logging is off by default, and this line runs on every tool call of
// every turn — building a filtered copy of the map and marshaling it (which
// for a read_file result means touching up to maxReadFileBytes) purely to
// produce a string nothing will print is work proportional to the result, on
// the hot path, in the common case. slog calls LogValue only when the record
// survives level filtering.
type lazyToolResultPreview struct {
	response map[string]any
}

func (p lazyToolResultPreview) LogValue() slog.Value {
	return slog.StringValue(debugToolResultPreview(p.response))
}

// debugToolResultPreviewBytes caps how much of a tool's result content
// appears in the agent.tool_result debug log line: large enough to see
// what a tool actually returned, small enough that a big read_file/
// go_search result doesn't flood --log-level debug output. Independent of
// maxToolOutputBytes (internal/agent/tools.go), which caps what the MODEL
// sees, not what an operator's log line shows.
const debugToolResultPreviewBytes = 500

// debugToolResultPreview renders response as a size-capped preview for the
// agent.tool_result debug log, mirroring truncateToolOutput's
// "... [truncated N bytes]" marker (internal/agent/tools.go). error/
// exit_code are already their own log fields (see executeBudgetedTool), so
// they're excluded here to avoid duplicating them in the preview.
func debugToolResultPreview(response map[string]any) string {
	preview := make(map[string]any, len(response))

	for k, v := range response {
		if k == "error" || k == "exit_code" {
			continue
		}

		preview[k] = v
	}

	b, err := json.Marshal(preview)
	if err != nil {
		return fmt.Sprintf("<unrenderable: %v>", err)
	}

	s := string(b)
	if len(s) <= debugToolResultPreviewBytes {
		return s
	}

	return s[:debugToolResultPreviewBytes] + fmt.Sprintf("... [truncated %d bytes]", len(s)-debugToolResultPreviewBytes)
}
