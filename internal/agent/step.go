package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// agentStepTimeout bounds one agent step's total wall-clock time (across
// all attempts and turns). config.ResolveAgentInvocation's MaxTurns bounds
// the number of turns, but a single hung endpoint could otherwise block
// indefinitely — the OpenAI client sets no default request timeout and
// relies entirely on ctx. 30 minutes: a 30-turn conversation on a slow or
// reasoning-heavy model routinely outlives the previous 10, and a step
// that wants a tighter leash sets timeout: itself.
const agentStepTimeout = 30 * time.Minute

// agentTimeout resolves the per-attempt conversation deadline. It returns
// noAgentDeadline when the step asked for none — an explicit timeout: 0,
// which is the only way to opt out of the implicit ceiling above, since an
// EMPTY timeout: on an agent step means "the default", not "no limit".
//
// A parse error can't happen for a validated config (config.validateTimeouts
// rejects it at LoadConfig), so an unexpected one falls back to the default
// rather than failing the run — and deliberately not to "no deadline", which
// would turn a typo into an unbounded step.
func agentTimeout(riTimeout string) time.Duration {
	if riTimeout == "" {
		return agentStepTimeout
	}

	parsed, err := config.ParseTimeout(riTimeout)
	if err != nil {
		return agentStepTimeout
	}

	if parsed == 0 {
		return noAgentDeadline
	}

	return parsed
}

// noAgentDeadline is what agentTimeout returns for timeout: 0. Callers must
// skip context.WithTimeout entirely rather than pass it along — a zero
// duration there expires immediately, which is the opposite of what was
// asked for.
const noAgentDeadline time.Duration = 0

// withAgentDeadline applies a resolved agent deadline to ctx, or leaves ctx
// alone when the step opted out. It exists so the three attempt loops
// (failover, cli, fix) cannot each get the noAgentDeadline check subtly
// different.
func withAgentDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout == noAgentDeadline {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, timeout)
}

// nodeRecord converts a plan merkle.Node into the shape store.RecordNode
// persists, keeping the store package free of a dependency on merkle's Node
// type. Mirrors internal/pipeline's own nodeRecord — both are small enough
// that duplicating the conversion is simpler than adding a cross-package
// edge between merkle and store just for this shuffle.
func nodeRecord(n merkle.Node) store.NodeRecord {
	return store.NodeRecord{
		Hash:       n.Hash,
		ParentHash: n.ParentHash,
		Kind:       string(n.Kind),
		StepIndex:  n.StepIndex,
		Resource:   n.Resource,
		Content:    n.Content,
	}
}

// resolveAgentDir joins and validates a step's working directory.
func resolveAgentDir(workspaceDir, stepDir string) (string, error) {
	dir := workspaceDir
	if stepDir != "" {
		dir = filepath.Join(workspaceDir, stepDir)
	}

	_, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("working directory %q: %w", dir, err)
	}

	return dir, nil
}

// recordAgentFailure records a failed agent step the same way the
// pipeline's task/put steps do — best-effort, errors ignored, since a
// failure to record must not mask the original error being returned to the
// caller. The recorded status reflects the classified outcome (failed vs
// errored vs aborted), and the write uses a detached context so an aborted
// step's outcome still persists rather than being dropped by the canceled ctx.
//
// res is whatever the conversation produced before it failed. Recording it
// (rather than the nil this used to store) is the whole point: a failed agent
// step is exactly the one you need to reconstruct afterwards, and its response
// and tool calls were being thrown away.
func recordAgentFailure(ctx context.Context, st *store.Store, node merkle.Node, jobName string, res conversationResult, runErr error) {
	status := string(outcome.Classify(ctx, runErr))
	recCtx := context.WithoutCancel(ctx)
	_ = st.RecordNode(recCtx, nodeRecord(node), jobName, status, agentResultRecord(res), runErr)
	_ = st.RecordJobRun(recCtx, jobName, node.Hash, status, runErr)
}

// StepOutcome is what RunStep reports about a completed agent step, beyond
// any error: the merkle hash to use as the next step's parentHash, the
// verdict (if any) internal/pipeline routes on, that verdict's note, and the
// model's final response text — recorded under this step's own name for a
// later step that declared context: { from: { <this step>: full } } to read
// (see upstream.go). Response is populated whenever the conversation
// produced a result at all — including on a failure/assert-failure path — and
// stays "" only when the step never got as far as running a conversation
// (e.g. workspace materialization failed).
type StepOutcome struct {
	Hash     string
	Verdict  string
	Note     string
	Response string
	// Cached reports that this step's declared outputs were restored from an
	// earlier run rather than produced by a conversation — so the caller
	// publishes a skip, and no model was paid.
	Cached bool
}

// printAgentResponse echoes an agent step's conversation result to the
// terminal — the model's final text, then its verdict and note (if the step
// declares verdicts:) — matching printTaskOutput's precedent of always
// echoing a step's real output (internal/pipeline/pipeline.go's
// printTaskOutput). Called for both a successful run and a turn-exhausted/
// failed one, since runPrepared populates res either way (see runPrepared's
// own doc comment) and a failed attempt's partial text/note is exactly what
// a human needs to see to know what to do next.
func printAgentResponse(res conversationResult) {
	text := strings.TrimSpace(res.text)
	if text != "" {
		fmt.Println(text)
	}

	if res.verdict != "" {
		fmt.Printf("verdict: %s\n", res.verdict)
	}

	if res.note != "" {
		fmt.Printf("note: %s\n", res.note)
	}
}

// RunStep hashes step against parentHash (agent steps are never
// skippable) and runs it, retrying the whole conversation up to the
// resolved attempt count. internal/pipeline routes the plan on the returned
// StepOutcome.Verdict.
func RunStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string) (StepOutcome, error) {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}
	defer prepared.close(step.Agent)

	// Hash prepared.step, not step: when step.PromptFile named a run-time
	// artifact file, prepared.step.Prompt is the file's loaded text (see
	// resolveDeferredPrompt) while step.Prompt is still empty — hashing step
	// here would hash an empty prompt for every such pipeline, colliding all
	// of them onto the same node regardless of what the file actually said.
	content, err := merkle.AgentContentMap(cfg, prepared.step, prepared.primary)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindAgent, content, parentHash)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "agent", "agent", step.Agent)

	// The name this step is KNOWN by: an across: cell reports and records under
	// its labelled identity rather than the agent it resolves through, so two
	// cells of one matrix are finally tellable apart in a run.
	name := step.DisplayName()

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAgent, StepIndex: i, Resource: name, Content: content}

	// Before the "agent:" line, so a reused step announces itself as reused
	// rather than appearing to start a conversation it never has.
	cached, out, err := reuseAgentStep(ctx, cfg, prepared, content, bw, st, node, jobName, name)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	if cached.Hit {
		return out, nil
	}

	fmt.Printf("agent: %s%s\n", name, fallbackBanner(prepared))

	// Give the conversation its live identity, so every turn it takes is
	// publishable as belonging to this run, job, and step. Set here rather
	// than in prepareAgentStep because only RunStep knows the plan index —
	// a hook or fix conversation has none, and publishes nothing.
	prepared.conv.recorder = &transcriptRecorder{live: liveContext{
		bus: events.FromContext(ctx), runID: events.RunID(ctx), stepID: events.StepID(ctx), job: jobName, stepIndex: i, stepName: name,
	}}

	// The node is recorded BEFORE the conversation, as running, because the
	// usage and transcript rows written below reference it: agent_usage.node_hash
	// and node_transcripts.hash are foreign keys into nodes, which is what lets
	// retention delete a node and have its spend record and its transcript go
	// with it rather than survive as rows nothing can reach.
	//
	// Every path below re-records the node with its real status, since
	// RecordNode upserts. What a placeholder therefore leaves behind is a node
	// still marked running only when the PROCESS died mid-conversation — which
	// is true, and more than the nothing-at-all that was recorded before.
	//
	// It cannot be a cache hit in that state: HasNodeSucceeded asks for
	// succeeded, and job_runs — the chain-level index — is not written here.
	//
	// Best-effort, like the usage and transcript writes it exists to enable: a
	// bookkeeping row must never be what fails a step that would otherwise run.
	// A failure here is not silent — those two writes log their own warning when
	// the reference they need is missing — and the success path below records the
	// node again with a returned error.
	//
	// On a DETACHED context, matching the two writes it enables (saveAgentUsage
	// and saveAgentTranscript both use context.WithoutCancel). Written on the
	// ambient ctx it was useless in the one case that matters: a step killed by
	// timeout: or Ctrl-C has a cancelled ctx, so the placeholder never landed,
	// and both later writes then failed the foreign keys they had just gained —
	// losing the tokens an aborted agent step spent and its transcript, on
	// precisely the runs worth investigating.
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, "running", nil, nil)

	stepStarted := time.Now()

	res, err := runAndAnnounceFailover(ctx, name, &prepared)

	printAgentResponse(res)

	// Before the error branches below, so a step that FAILED still records
	// what it spent. A failed agent step is often the expensive one, and
	// leaving it out would under-report exactly the runs worth investigating —
	// the same reason stepUsage.finish rolls a failed step into the job total.
	saveAgentUsage(ctx, st, saveUsageArgs{
		jobName: jobName, stepIndex: i, stepName: name, nodeHash: hash,
		modelRequested: prepared.primary.ModelName,
		usage:          prepared.conv.usage.persistedSnapshot(),
		duration:       time.Since(stepStarted),
	})

	// One call site covers every outcome — success, run failure, assert
	// failure, capture failure — because a failed step's transcript is the one
	// that gets read. Best-effort by design (see saveAgentTranscript).
	saveAgentTranscript(ctx, st, hash, jobName, res)

	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, res, err)

		// A failed run emitted no clean verdict; the pipeline routes it via
		// to["failure"] (or fails the job).
		return StepOutcome{Response: res.text}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = assertAgentResponse(step.Assert, res, prepared.space.Dir())
	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, res, err)

		return StepOutcome{Response: res.text}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	prepared.removeSpillDirIfCaptured()

	err = prepared.space.Capture(ctx)
	if err != nil {
		wrapped := fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
		recordAgentFailure(ctx, st, node, jobName, res, wrapped)

		return StepOutcome{Response: res.text}, wrapped
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", agentResultRecord(res), nil)
	if err != nil {
		return StepOutcome{Response: res.text}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	// After the node is recorded, so a run that could not record its own
	// outcome does not leave an entry behind claiming the work is done.
	workspace.SaveStepCache(ctx, bw, cached.Key, cached.request)

	return StepOutcome{Hash: hash, Verdict: res.verdict, Note: res.note, Response: res.text}, nil
}

// stepCacheLookup is what the cache decided about this step, carrying the
// request alongside so the store call after a successful run files exactly the
// outputs the lookup asked about.
type stepCacheLookup struct {
	workspace.StepCacheResult

	request workspace.StepCacheRequest
}

// reuseAgentStep performs the lookup and, on a hit, records the step's node as
// succeeded — everything RunStep would otherwise do inline, lifted out to keep
// it inside the complexity budget. The returned StepOutcome is meaningful only
// when the lookup hit.
func reuseAgentStep(
	ctx context.Context, cfg *config.Config, prepared preparedAgentStep, content map[string]any,
	bw workspace.BuildWorkspace, st *store.Store, node merkle.Node, jobName, name string,
) (stepCacheLookup, StepOutcome, error) {
	cached, err := lookupStepCache(ctx, cfg, prepared, content, bw, jobName, name)
	if err != nil || !cached.Hit {
		return cached, StepOutcome{}, err
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", cached.NodeResult(), nil)
	if err != nil {
		return cached, StepOutcome{}, fmt.Errorf("%w", err)
	}

	return cached, StepOutcome{Hash: node.Hash, Cached: true}, nil
}

// lookupStepCache reports whether this agent step's declared outputs were
// already produced by an earlier run over the same input bytes, restoring them
// when they were.
//
// It runs AFTER prepareAgentStep rather than before, because the step's own
// content — and so its key — is not knowable until then: a prompt_file: naming
// a run-time artifact is only loaded once the step's workspace exists. The cost
// of a hit is therefore one materialized workspace and whatever tool servers
// the grant starts, which is real, but is not what an agent step costs.
func lookupStepCache(
	ctx context.Context, cfg *config.Config, prepared preparedAgentStep,
	content map[string]any, bw workspace.BuildWorkspace, jobName, name string,
) (stepCacheLookup, error) {
	if !merkle.StepCacheable(cfg, prepared.step) {
		return stepCacheLookup{}, nil
	}

	// Hashed with NO parent: what identifies the work is this step's content
	// and the bytes it reads, not the chain that led to it. See
	// internal/pipeline's stepCacheRequest.
	contentHash, err := merkle.HashNode(merkle.NodeKindAgent, content, "")
	if err != nil {
		return stepCacheLookup{}, fmt.Errorf("step cache key: %w", err)
	}

	step := prepared.step
	req := workspace.StepCacheRequest{
		ContentHash:   contentHash,
		Inputs:        step.InputNames(),
		Outputs:       step.Outputs,
		OutputMapping: config.CollectedOutputMapping(step.Outputs, nil, step.OutputSubdir),
	}

	res := workspace.LookupStepCache(ctx, bw, req)
	if res.Hit {
		fmt.Printf("skip: %s (reused)\n", name)
		slog.Info("job.skip", "job", jobName, "step", name, "reason", "reused", "key", res.Key)
	}

	return stepCacheLookup{StepCacheResult: res, request: req}, nil
}

// agentResultRecord builds the result map RunStep records for a succeeded
// agent step: always the response and turn count, plus whichever optional
// outcomes the run actually produced. Extracted from RunStep to keep its
// cyclomatic complexity under the linter budget.
func agentResultRecord(res conversationResult) map[string]any {
	result := map[string]any{"response": res.text, "turns": res.turns}

	if res.wrappedUp {
		// The same reason fallback_model is recorded: an answer produced
		// against a spent budget is not the answer the step would have given,
		// and afterwards it is indistinguishable from a confident one unless
		// the record says so.
		result["wrapped_up"] = true
	}

	if res.model != "" {
		// Recorded only when a fallback served the run: a run's output has to
		// carry which model produced it, or an outage-driven quality dip is
		// indistinguishable afterwards from a normal run.
		result["fallback_model"] = res.model
	}

	if trajectory := recordedTrajectory(res.trajectory); len(trajectory) > 0 {
		result["trajectory"] = trajectory
	}

	if res.verdict != "" {
		result["verdict"] = res.verdict
	}

	if res.note != "" {
		result["note"] = res.note
	}

	return result
}

// maxRecordedArgBytes caps how much of a single tool argument is persisted.
// The trajectory is a record of what the agent did, not a copy of what it
// wrote: a write_file call's whole content would balloon nodes.result for no
// diagnostic gain, since the file itself is the artifact. 16KB keeps most
// real arguments (an edit_file old/new pair, a run_shell script) whole for
// the replay/audit reader; only bulk file bodies get cut.
const maxRecordedArgBytes = 16_384

// recordedTrajectory converts a run's tool calls into the plain shape stored
// in nodes.result — "it called write_file and it failed" is a different story
// from "it called write_file", so the ok flag is the point.
//
// The trajectory used to die with the process — it existed only in memory. So
// the most useful question about an agent step ("what did it actually do?")
// had no answer once the run ended, and none at all for a step that failed.
func recordedTrajectory(calls []recordedToolCall) []map[string]any {
	if len(calls) == 0 {
		return nil
	}

	out := make([]map[string]any, 0, len(calls))

	for _, call := range calls {
		out = append(out, map[string]any{
			"name": call.name,
			"args": truncateArgs(call.args),
			"ok":   call.ok,
		})
	}

	return out
}

// truncateArgs copies args with over-long string values elided, so one
// enormous argument can't dominate a node's recorded result.
func truncateArgs(args map[string]any) map[string]any {
	if len(args) == 0 {
		return nil
	}

	out := make(map[string]any, len(args))

	for key, value := range args {
		text, isString := value.(string)
		if isString && len(text) > maxRecordedArgBytes {
			out[key] = text[:maxRecordedArgBytes] + "…(truncated)"

			continue
		}

		out[key] = value
	}

	return out
}

// runOneConversation runs a conversation under its timeout and, on failure,
// reports the provider requests it really spent.
//
// It runs the conversation ONCE. attempts: retries the failing request, down
// in requests.go, which is why nothing here loops: a restart discarded every
// accumulated turn and re-billed the whole conversation to re-ask a question
// the transport had already retried and given up on. Shared by
// runPreparedWithFailover (failover.go, one call per source it tries) and
// RunFix so all spend attempts: on the same thing.
func runOneConversation(
	ctx context.Context,
	ri config.ResolvedInvocation,
	llm model.LLM,
	conv agentConversation,
	timeout time.Duration,
) (conversationResult, error) {
	convCtx, cancel := withAgentDeadline(ctx, timeout)
	defer cancel()

	logCompactionBudget(ri)

	requests := &requestCounter{}

	result, err := runAgentConversation(withRequestCounter(convCtx, requests), llm, conv)
	if err != nil {
		slog.Warn("agent.conversation_failed",
			"agent", ri.AgentName,
			"provider_requests", requests.Total(),
			"error", err)
	}

	// The failover cascade passes noAgentDeadline (its cascadeCtx owns the
	// step's budget and marks at its own return), so this fires only for the
	// callers that hand a real timeout in: RunFix, and the fix path's hooks.
	return result, outcome.FailOnDeadline(ctx, convCtx, err) //nolint:wrapcheck // classification wrapper; the step caller adds its own context
}

// RunHook runs an agent step as a hook: it resolves, materializes, runs the
// conversation, evaluates its assert, and captures declared outputs exactly
// like RunStep, but records no merkle node or job_run — the enclosing
// step/job records the aggregate outcome (the same no-record contract as
// RunFix). A returned error is already outcome-marked where appropriate (see
// runAgentConversation), so the caller's hook classification works unchanged.
//
// The assert runs here for the same reason a task hook's does (see
// internal/pipeline's executeTask -> runAssertedTask): config.validateAsserts
// deliberately walks hook steps, so an assert on an on_failure: agent is
// checked and rejected at load like any other. Evaluating it at load and then
// never running it made that promise a lie — the hook reported success on a
// mismatch its own assert existed to catch.
func RunHook(ctx context.Context, cfg *config.Config, jobName string, step config.Step, bw workspace.BuildWorkspace) error {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}
	defer prepared.close(step.Agent)

	fmt.Printf("agent: %s%s\n", step.Agent, fallbackBanner(prepared))

	// Give the conversation its live identity, matching RunStep (see its own
	// comment above): a hook's conversation is nested inside a job that DOES
	// have one, so its tool calls and any mid-run failover it triggers are
	// attributable the same way a plan step's are, instead of publishing (and
	// logging) nowhere. stepIndex is -1: a hook is not a plan position.
	prepared.conv.recorder = &transcriptRecorder{live: liveContext{
		bus: events.FromContext(ctx), runID: events.RunID(ctx), stepID: events.StepID(ctx), job: jobName, stepIndex: -1, stepName: step.DisplayName(),
	}}

	res, err := runAndAnnounceFailover(ctx, step.Agent, &prepared)

	printAgentResponse(res)

	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	// Before Capture, matching RunStep's order: assert.files reads the step's
	// own working directory, which is what the conversation just wrote into.
	err = assertAgentResponse(step.Assert, res, prepared.space.Dir())
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	prepared.removeSpillDirIfCaptured()

	err = prepared.space.Capture(ctx)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	return nil
}

// fallbackBanner annotates a step's own output line when it is running on a
// fallback model, so the difference is visible where the run is being read
// rather than only in a log line that scrolled past at startup.
//
// Visibility is the requirement, not a nicety: a fallback can produce
// meaningfully different output, and a quality dip caused by an outage that
// looks identical to a normal run is one nobody investigates.
func fallbackBanner(prepared preparedAgentStep) string {
	model := fallbackModel(prepared)
	if model == "" {
		return ""
	}

	return fmt.Sprintf(" (fallback: %s — %s is unavailable)", model, prepared.primary.ModelName)
}

// fallbackModel names the model serving this step when it is not the
// configured one, else "".
func fallbackModel(prepared preparedAgentStep) string {
	same := prepared.ri.ModelName == prepared.primary.ModelName &&
		prepared.ri.BaseURL == prepared.primary.BaseURL &&
		// A failover between a CLI and a hosted provider can leave the model
		// name identical while changing everything about how the step runs.
		prepared.ri.CLI == prepared.primary.CLI

	if same {
		return ""
	}

	return prepared.ri.ModelName
}

// runAndAnnounceFailover runs prepared's conversation via the mid-run
// cascade, updates *prepared to whichever source actually served it (so the
// caller's later fallbackModel/agentResultRecord calls see it), and prints
// the step's own visible note if the source that served it is a mid-run swap
// the pre-run fallbackBanner couldn't have already announced. Shared by
// RunStep and RunHook so both report a served-by source the same way.
//
// Preflight's own failover is already covered by that pre-run banner, since
// prepared.ri reflects preflight's pick before either caller ever prints it
// — but the mid-run cascade (failover.go) can swap sources AFTER the banner
// printed, and without the note here the step's own output line stays
// silent about it, leaving only the agent.failover log line and the
// recorded fallback_model to say so — one of the three channels
// docs/agents.md's "Loudly visible" promises.
func runAndAnnounceFailover(ctx context.Context, name string, prepared *preparedAgentStep) (conversationResult, error) {
	res, served, err := runPreparedWithFailover(ctx, *prepared)

	// Both halves of the served source, not just its invocation: leaving llm
	// pointing at the source the cascade abandoned would make the struct
	// describe two different sources at once, and anything later reusing it
	// would send requests to the dead one while every log line and the
	// recorded fallback_model named the live one.
	prepared.ri = served.ri
	prepared.llm = served.llm
	res.model = fallbackModel(*prepared)

	// Announced on the fact of the swap rather than on a change of model
	// name: two fallback: entries may legitimately resolve to the same model
	// and endpoint, and comparing names would go silent on exactly that
	// configuration while agent.failover still logged the swap.
	if served.swapped {
		fmt.Printf("agent: %s failed over mid-run to %s\n", name, served.ri.ModelName)
	}

	return res, err
}
