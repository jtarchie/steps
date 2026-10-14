package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/agent/llmclient"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
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
//
// The value itself lives in internal/config (DefaultAgentStepTimeout), not
// here, so internal/web can resolve the same effective deadline a running
// step is actually held to without importing internal/agent.
const agentStepTimeout = config.DefaultAgentStepTimeout

// agentTimeout resolves the per-attempt conversation deadline, delegating to
// config.ResolvedAgentTimeout (the empty/zero/parse-error rules are
// documented there — internal/web's job-page dials table and web countdown
// resolve a step's timeout the same way, off the same function).
// noAgentDeadline is what it returns for an explicit timeout: 0, the only
// way to opt out of the implicit ceiling above, since an EMPTY timeout: on
// an agent step means "the default", not "no limit".
func agentTimeout(riTimeout string) time.Duration {
	return config.ResolvedAgentTimeout(riTimeout)
}

// noAgentDeadline is what agentTimeout returns for timeout: 0. Callers must
// skip context.WithTimeout entirely rather than pass it along — a zero
// duration there expires immediately, which is the opposite of what was
// asked for.
const noAgentDeadline time.Duration = 0

// remainingOrNoDeadline reports how long is left until ctx's own deadline, or
// noAgentDeadline when ctx carries none — for a caller (subagent.go) that
// discloses a deadline it inherited rather than one it resolved itself, and
// so has no config.ResolvedAgentTimeout call of its own to fall back through.
//
// Floored at noAgentDeadline rather than returning a negative remainder: a
// delegation started right as (or just after) the parent conversation's
// deadline expires still finds ctx.Deadline's ok true with a past time, and
// disclosing "-50ms" is nonsensical when the request is already on its way
// to being cancelled outright.
func remainingOrNoDeadline(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return noAgentDeadline
	}

	if remaining := time.Until(deadline); remaining > 0 {
		return remaining
	}

	return noAgentDeadline
}

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

// StepStore is the part of the state database an agent step's conversation
// touches: its spend in the usage ledger, the questions it parks for a person,
// what its memory: scope keeps, and its transcript. Not the node it is recorded
// under, nor the skip index — internal/pipeline owns both for every step kind,
// and this type having no way to write either is what keeps it so.
type StepStore interface {
	store.Usage
	store.Questions
	store.Memories
	TranscriptStore
}

// TranscriptStore files a conversation's transcript under the node
// internal/pipeline recorded for it. The one store.Cache method an agent step
// needs, declared alone because the rest of that facet is pipeline's.
type TranscriptStore interface {
	SaveNodeTranscript(ctx context.Context, hash, transcript string) error
}

// PreparedStep is an agent step resolved and materialized, with its memory:
// loaded — everything internal/pipeline needs to key it — but whose
// conversation has not started. Pipeline hashes it, asks the step cache, and
// records its node; Run is the conversation and nothing around it.
type PreparedStep struct {
	prepared preparedAgentStep
	st       StepStore
	digest   string
}

// PrepareStep resolves, materializes and loads the memory of an agent step.
// The caller must Close what it returns.
func PrepareStep(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace, st StepStore, newRunner RunnerFactory) (*PreparedStep, error) {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, newRunner)
	if err != nil {
		return nil, err
	}

	digest, err := applyStepMemory(ctx, cfg, &prepared, st)
	if err != nil {
		prepared.close(step.Agent)

		return nil, err
	}

	return &PreparedStep{prepared: prepared, st: st, digest: digest}, nil
}

// Step is the resolved step — the copy to key on. When message_files: named a
// run-time artifact, its loaded text is in Messages here (see
// prepareStepPrompt) while the step the caller holds still has none;
// keying on the caller's copy would collide every such pipeline onto one
// node regardless of what the file said.
func (s *PreparedStep) Step() config.Step {
	return s.prepared.step
}

// Primary is the invocation as CONFIGURED, before any preflight failover —
// the one to key on, so which source serves a run is availability rather than
// content and a fallback firing cannot invalidate a cache entry.
func (s *PreparedStep) Primary() config.ResolvedInvocation {
	return s.prepared.primary
}

// MemoryDigest covers what the step's memory: scope holds as it starts, or ""
// when it declares no memory:. Part of the key: the same message with
// different memory is a different step (see applyStepMemory).
func (s *PreparedStep) MemoryDigest() string {
	return s.digest
}

// Close releases everything PrepareStep opened, the step's directory last.
func (s *PreparedStep) Close() {
	s.prepared.close(s.prepared.step.Agent)
}

// Capture files the step's declared outputs. After a successful Run only: the
// spill directory is removed first when it would otherwise ride into an
// artifact (see removeSpillDirIfCaptured).
func (s *PreparedStep) Capture(ctx context.Context) error {
	s.prepared.removeSpillDirIfCaptured()

	return s.prepared.space.Capture(ctx) //nolint:wrapcheck // the caller adds the step's own context
}

// StepRun is where a conversation belongs: the job and plan position it runs
// at, the name it is known by, and the node internal/pipeline recorded for it
// — which its usage and transcript rows reference.
type StepRun struct {
	JobName  string
	Index    int
	Name     string
	NodeHash string
}

// StepResult is what a conversation produced. Record is what its node records
// whatever the outcome, and Response is set whenever the conversation ran at
// all. Verdict and Note are set only on success: a failed run emitted no clean
// verdict, and internal/pipeline routes it via to["failure"] instead.
type StepResult struct {
	Record   map[string]any
	Response string
	Verdict  string
	Note     string
}

// Run runs the conversation and checks its assert:. Its usage and transcript
// are saved on every outcome, before the error is returned, so a step that
// FAILED still records what it spent and said — a failed agent step is often
// the expensive one, and its transcript is the one that gets read. Both writes
// are best-effort and detached (see saveAgentUsage, saveAgentTranscript), and
// both reference run.NodeHash, which is why the caller records that node
// before calling this.
func (s *PreparedStep) Run(ctx context.Context, run StepRun) (StepResult, error) {
	prepared := &s.prepared

	noteFallback(ctx, run.Name, *prepared)

	bindRun(ctx, prepared, s.st, run.JobName, run.Index, run.Name)

	stepStarted := time.Now()

	res, err := runAndAnnounceFailover(ctx, run.Name, prepared)

	printAgentResponse(ctx, res)

	saveAgentUsage(ctx, s.st, saveUsageArgs{
		jobName: run.JobName, stepIndex: run.Index, stepName: run.Name, kind: store.UsageStep, nodeHash: run.NodeHash,
		modelRequested: prepared.primary.ModelName,
		usage:          prepared.conv.usage.persistedSnapshot(),
		duration:       time.Since(stepStarted),
	})

	saveAgentTranscript(ctx, s.st, run.NodeHash, run.JobName, res)

	out := StepResult{Record: agentResultRecord(res), Response: res.text}
	if err != nil {
		return out, err
	}

	err = assertAgentResponse(prepared.step.Assert, res, prepared.space.Dir())
	if err != nil {
		return out, err
	}

	out.Verdict, out.Note = res.verdict, res.note

	return out, nil
}

// printAgentResponse echoes an agent step's conversation result to the
// terminal — the model's final text, then its verdict and note (if the step
// declares verdicts:) — matching printTaskOutput's precedent of always
// echoing a step's real output (internal/pipeline/pipeline.go's
// printTaskOutput). Called for both a successful run and a turn-exhausted/
// failed one, since runPrepared populates res either way (see runPrepared's
// own doc comment) and a failed attempt's partial text/note is exactly what
// a human needs to see to know what to do next.
func printAgentResponse(ctx context.Context, res conversationResult) {
	out := events.Stdout(ctx)

	text := strings.TrimSpace(res.text)
	if text != "" {
		_, _ = fmt.Fprintln(out, text)
	}

	if res.verdict != "" {
		_, _ = fmt.Fprintf(out, "verdict: %s\n", res.verdict)
	}

	if res.note != "" {
		_, _ = fmt.Fprintf(out, "note: %s\n", res.note)
	}
}

// agentResultRecord builds the result map an agent step's node records:
// always the response and turn count, plus whichever optional outcomes the run
// actually produced.
func agentResultRecord(res conversationResult) map[string]any {
	result := map[string]any{"response": res.text, "turns": res.turns}

	if res.wrappedUp {
		// The same reason fallback_model is recorded: an answer produced
		// against a spent budget is not the answer the step would have given,
		// and afterwards it is indistinguishable from a confident one unless
		// the record says so.
		result["wrapped_up"] = true
	}

	// Written only when set, like wrapped_up, so a step that never compacts
	// records the bytes it always did.
	if res.compactions > 0 {
		result["compactions"] = res.compactions
	}

	if res.compactionStalled {
		result["compaction_stalled"] = true
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
// in llmclient/requests.go, which is why nothing here loops: a restart discarded every
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

	logCompactionBudget(ctx, ri)

	requests := &llmclient.RequestCounter{}

	result, err := runAgentConversation(llmclient.WithRequestCounter(convCtx, requests), llm, conv)
	if err != nil {
		slog.WarnContext(ctx, "agent.conversation_failed",
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
// like a plan step, but records no merkle node or job_run — the enclosing
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
func RunHook(ctx context.Context, cfg *config.Config, jobName string, step config.Step, bw workspace.BuildWorkspace, st StepStore, newRunner RunnerFactory) error {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, newRunner)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}
	defer prepared.close(step.Agent)

	events.Note(ctx, events.NoteInfo, "agent: "+step.Agent+fallbackBanner(prepared))

	// A hook's conversation is nested inside a job that DOES have a live
	// identity, so its tool calls and any mid-run failover it triggers are
	// attributable the same way a plan step's are, instead of publishing (and
	// logging) nowhere. stepIndex is -1: a hook is not a plan position.
	bindRun(ctx, &prepared, st, jobName, -1, step.DisplayName())

	started := time.Now()

	res, err := runAndAnnounceFailover(ctx, step.Agent, &prepared)

	printAgentResponse(ctx, res)

	saveAgentUsage(ctx, st, saveUsageArgs{
		jobName: jobName, stepIndex: -1, stepName: step.DisplayName(), kind: store.UsageHook,
		modelRequested: prepared.primary.ModelName,
		usage:          prepared.conv.usage.persistedSnapshot(),
		duration:       time.Since(started),
	})

	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	// Before Capture, matching a plan step's order: assert.files reads the step's
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

// bindRun says where a question the conversation asks is recorded, and under
// which job, then gives the conversation its live identity, so every turn it
// takes is publishable as belonging to this run, job, and step. Not done in
// prepareAgentStep because only PreparedStep.Run and RunHook hold the store handle,
// the job's name and the step's position; a fix conversation has none of
// them, and publishes nothing.
func bindRun(ctx context.Context, p *preparedAgentStep, st StepStore, jobName string, stepIndex int, stepName string) {
	p.conv.env.ask.st = st
	p.conv.env.ask.jobName = jobName

	p.conv.recorder = &transcriptRecorder{live: liveContext{
		bus: events.FromContext(ctx), runID: events.RunID(ctx), stepID: events.StepID(ctx), job: jobName, stepIndex: stepIndex, stepName: stepName,
	}}
}

// noteFallback warns when a step is served by a fallback model; the step's start line already names the agent.
func noteFallback(ctx context.Context, name string, prepared preparedAgentStep) {
	if banner := fallbackBanner(prepared); banner != "" {
		events.Note(ctx, events.NoteWarn, "agent: "+name+banner)
	}
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
// PreparedStep.Run and RunHook so both report a served-by source the same way.
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
		events.Note(ctx, events.NoteWarn, "agent: "+name+" failed over mid-run to "+served.ri.ModelName)
	}

	return res, err
}
