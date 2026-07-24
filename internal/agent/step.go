package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// agentStepTimeout bounds one agent step's total wall-clock time (across
// all attempts and turns). config.ResolveAgentInvocation's MaxTurns bounds
// the number of turns, but a single hung endpoint could otherwise block
// indefinitely — the OpenAI client sets no default request timeout and
// relies entirely on ctx.
const agentStepTimeout = 10 * time.Minute

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
func recordAgentFailure(ctx context.Context, st *store.Store, node merkle.Node, jobName string, runErr error) {
	status := string(outcome.Classify(ctx, runErr))
	recCtx := context.WithoutCancel(ctx)
	_ = st.RecordNode(recCtx, nodeRecord(node), jobName, status, nil, runErr)
	_ = st.RecordJobRun(recCtx, jobName, node.Hash, status, runErr)
}

// preparedAgentStep is RunStep's resolved-and-materialized preamble:
// everything needed to run the conversation, split out so RunStep itself
// only sequences hash/run/capture/record and stays within the linter's
// complexity budget.
type preparedAgentStep struct {
	ri       config.ResolvedInvocation
	space    workspace.StepSpace
	conv     agentConversation
	llm      model.LLM
	closers  []io.Closer // MCP connections opened for this step's granted tools — see buildAgentTools
	spillDir string      // run_shell/custom tool output spill dir — see toolEnv.spillDir; "" if it couldn't be created
}

// close releases everything prepareAgentStep opened for this step: its
// workspace space, any tool connections (MCP), and its spill dir (removed,
// not just closed — it may hold files a large run_shell/custom tool output
// spilled to; see toolEnv.spillDir). Ignores errors — this is always
// deferred right after a successful prepareAgentStep, with the step's real
// outcome already determined by the time it runs.
func (p preparedAgentStep) close(stepLabel string) {
	workspace.CloseSpace(p.space, stepLabel)
	closeAll(p.closers)

	if p.spillDir != "" {
		_ = os.RemoveAll(p.spillDir)
	}
}

// prepareAgentStep resolves step's agent, materializes its (isolated or
// shared) working directory, and builds the tools/LLM client it'll run
// with. On error, any workspace.StepSpace already created is closed before
// returning so the caller never has to. handoff is the transition context
// (see Handoff) to seed the conversation with when step.Handoff enables it —
// nil on a step's first/unrouted execution, or when the caller (RunHook)
// never participates in routing at all.
func prepareAgentStep(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace, handoff *Handoff) (preparedAgentStep, error) {
	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	space, err := bw.TaskSpace(ctx, step.Agent, step.InputNames(), step.Outputs, nil, nil)
	if err != nil {
		return preparedAgentStep{}, fmt.Errorf("workspace: %w", err)
	}

	dir, err := resolveAgentDir(space.Dir(), step.Dir)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	decls, registry, closers, err := buildAgentTools(ctx, cfg, ri.ToolSpecs, ri.Image)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	required := requiredToolNames(ri.ToolSpecs)

	verdictTool, err := injectSynthesizedTools(step, handoff, decls, registry, required)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	runner, err := shell.NewRunner(ri.Image, dir)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, err
	}

	spillDir := newToolOutputSpillDir(dir, step.Agent)

	conv := agentConversation{
		system: buildSystemMessage(ri.Persona, dir),
		prompt: promptWithHandoff(step.Prompt, step.Handoff, handoff),
		env:    toolEnv{dir: dir, runner: runner, spillDir: spillDir},
		tools:  agentTools{decls: decls, registry: registry, required: required, maxCalls: maxCallsByName(ri.ToolSpecs)},
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
		verdictTool:          verdictTool,
	}

	return preparedAgentStep{
		ri: ri, space: space, conv: conv, llm: newAgentLLM(ri, apiKey),
		closers: closers, spillDir: spillDir,
	}, nil
}

// toolOutputSpillDirName is the fixed name of the subdirectory a
// run_shell/custom tool's oversized output is spilled into (see
// newToolOutputSpillDir). config.artifactNamePattern requires a declared
// input/output name to start with a letter or digit, so this leading '.'
// can never collide with a real one.
const toolOutputSpillDirName = ".steps-agent-out"

// newToolOutputSpillDir creates a fresh subdirectory of dir — the step's
// working directory — that a run_shell/custom tool's oversized output can be
// spilled to for this one step (see toolEnv.spillDir), removed by
// preparedAgentStep.close when the step ends.
//
// It must live inside dir, rather than a top-level os.MkdirTemp: read_file
// and list_dir are confined to dir (resolveAgentPath), so a spill directory
// outside it would be reachable only via run_shell (the one tool with no
// path confinement of its own) — defeating the point of spilling large
// output to a file the model can then read back in slices.
//
// Creation failure degrades to "" rather than failing the step — spilling is
// a usability improvement over the shell layer's older truncate-and-drop
// behavior, not something a step should abort over (e.g. a read-only
// workspace would otherwise turn an unrelated pipeline failure into an agent
// step failure).
func newToolOutputSpillDir(dir, stepLabel string) string {
	spillDir := filepath.Join(dir, toolOutputSpillDirName)

	err := os.MkdirAll(spillDir, 0o750)
	if err != nil {
		slog.Warn("agent.spill_dir", "agent", stepLabel, "error", err)

		return ""
	}

	return spillDir
}

// StepOutcome is what RunStep reports about a completed agent step, beyond
// any error: the merkle hash to use as the next step's parentHash, the
// verdict (if any) internal/pipeline routes on, that verdict's note, and
// this step's own run packaged as a PreviousRun for a possible routed-to
// successor to pull via its previous_run tool. Previous is populated
// whenever the conversation produced a result at all — including on a
// failure/assert-failure path, so a to.failure-routed successor can still
// pull a failed run's partial text/trajectory; it stays nil only when the
// step never got as far as running a conversation (e.g. workspace
// materialization failed).
type StepOutcome struct {
	Hash     string
	Verdict  string
	Note     string
	Previous *PreviousRun
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
// resolved attempt count. handoff carries the transition context (see
// Handoff) when step was entered via a to:/verdicts: route into a
// handoff:-enabled step; nil otherwise. internal/pipeline routes the plan
// on the returned StepOutcome.Verdict and threads StepOutcome.Previous/Note
// into the next step's Handoff when this step itself routes somewhere.
func RunStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *Handoff) (StepOutcome, error) {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, handoff)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}
	defer prepared.close(step.Agent)

	content, err := merkle.AgentContentMap(cfg, step, prepared.ri)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindAgent, content, parentHash)
	if err != nil {
		return StepOutcome{}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "agent", "agent", step.Agent)

	fmt.Printf("agent: %s\n", step.Agent)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAgent, StepIndex: i, Resource: prepared.ri.AgentName, Content: content}

	res, err := runPrepared(ctx, prepared)
	printAgentResponse(res)

	previous := &PreviousRun{
		Agent: step.Agent, Response: res.text, Verdict: res.verdict, Note: res.note,
		Turns: res.turns, Trajectory: exportTrajectory(res.trajectory),
	}

	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, err)

		// A failed run emitted no clean verdict; the pipeline routes it via
		// to["failure"] (or fails the job).
		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = assertAgentResponse(step.Assert, res)
	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, err)

		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = prepared.space.Capture(ctx)
	if err != nil {
		wrapped := fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
		recordAgentFailure(ctx, st, node, jobName, wrapped)

		return StepOutcome{Previous: previous}, wrapped
	}

	result := map[string]any{"response": res.text, "turns": res.turns}
	if res.verdict != "" {
		result["verdict"] = res.verdict
	}

	if res.note != "" {
		result["note"] = res.note
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", result, nil)
	if err != nil {
		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return StepOutcome{Hash: hash, Verdict: res.verdict, Note: res.note, Previous: previous}, nil
}

// assertAgentResponse checks an agent step's assert (stdout and/or
// tool_calls — an agent has no exit code) against what the conversation
// produced. Every field that is set must pass; a mismatch on any is a
// task-level failure so the step fails and its on_failure hook fires. A nil
// assert, or one with neither field set, is a no-op.
func assertAgentResponse(assert *config.Assert, res conversationResult) error {
	if assert == nil {
		return nil
	}

	if assert.Stdout != nil && !strings.Contains(res.text, *assert.Stdout) {
		//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
		return outcome.Fail(fmt.Errorf("assert.stdout: response does not contain %q", *assert.Stdout))
	}

	if len(assert.ToolCalls) > 0 {
		err := matchToolCallTrajectory(assert.ToolCalls, res.trajectory)
		if err != nil {
			//nolint:wrapcheck // outcome.Fail is the intended failure marker, not an opaque external error
			return outcome.Fail(err)
		}
	}

	return nil
}

// matchToolCallTrajectory reports whether want appears, in order, as a
// SUBSEQUENCE of got: every expected call must be matched, in the given
// order, but any number of unexpected calls may appear before, between, or
// after them. Each expected entry matches a call with the same name whose
// arguments are a SUPERSET of the entry's args — every listed key must be
// present with an equal value, extra actual arguments ignored. Both rules are
// ported from secret-agent's eval matcher (internal/eval), which this
// feature's semantics deliberately mirror.
//
// On failure the error names the first expected call that could not be
// matched and prints the observed trajectory, so a fixture failure is
// debuggable without re-running with verbose logging.
func matchToolCallTrajectory(want []config.ExpectedToolCall, got []recordedToolCall) error {
	next := 0

	for _, expected := range want {
		matched := false

		for ; next < len(got); next++ {
			if toolCallMatches(expected, got[next]) {
				next++
				matched = true

				break
			}
		}

		if !matched {
			return fmt.Errorf("assert.tool_calls: no call matching %s after the previously matched calls; got %s",
				describeExpectedCall(expected), describeTrajectory(got))
		}
	}

	return nil
}

// toolCallMatches reports whether one observed call satisfies one expected
// entry: same name, and every expected argument present with an equal value.
// Values compare as strings via fmt.Sprint, since a tool's arguments are
// rendered into its run: template as strings regardless of the JSON type the
// model emitted.
func toolCallMatches(want config.ExpectedToolCall, got recordedToolCall) bool {
	if want.Name != got.name {
		return false
	}

	for key, wantValue := range want.Args {
		actual, present := got.args[key]
		if !present || fmt.Sprint(actual) != wantValue {
			return false
		}
	}

	return true
}

func describeExpectedCall(want config.ExpectedToolCall) string {
	if len(want.Args) == 0 {
		return fmt.Sprintf("%q", want.Name)
	}

	keys := make([]string, 0, len(want.Args))
	for key := range want.Args {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%q", key, want.Args[key]))
	}

	return fmt.Sprintf("%q with %s", want.Name, strings.Join(pairs, " "))
}

// describeTrajectory renders the observed calls for a mismatch message, names
// only — argument values can be large (a whole review body) and the name
// sequence is what makes an ordering mismatch legible.
func describeTrajectory(got []recordedToolCall) string {
	if len(got) == 0 {
		return "(no tool calls)"
	}

	names := make([]string, len(got))
	for i, call := range got {
		names[i] = call.name
	}

	return "[" + strings.Join(names, ", ") + "]"
}

// runPrepared runs the (already resolved and materialized) conversation under
// the agent step timeout, retrying the whole conversation up to the resolved
// attempt count. Shared by RunStep and RunHook so a hook agent runs the exact
// same conversation machinery, minus the merkle/store recording RunStep does.
func runPrepared(ctx context.Context, prepared preparedAgentStep) (conversationResult, error) {
	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	var result conversationResult

	err := retry.Do(agentCtx, prepared.ri.Attempts, func(attempt int) error {
		// withAttempt keeps a retry off the provider instance the previous
		// attempt may have just failed against (see composeSessionID).
		res, runErr := runAgentConversation(withAttempt(agentCtx, attempt), prepared.llm, prepared.conv)
		// Keep the latest attempt's result either way: on success it's the
		// answer, and on failure its turns/trajectory describe the attempt
		// that actually failed.
		result = res

		return runErr
	})

	return result, err //nolint:wrapcheck // callers (RunStep/RunHook) wrap with step context
}

// RunHook runs an agent step as a hook: it resolves, materializes, runs the
// conversation, and captures declared outputs exactly like RunStep, but
// records no merkle node or job_run — the enclosing step/job records the
// aggregate outcome (the same no-record contract as RunFix). A returned error
// is already outcome-marked where appropriate (see runAgentConversation), so
// the caller's hook classification works unchanged.
func RunHook(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) error {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, nil)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}
	defer prepared.close(step.Agent)

	fmt.Printf("agent: %s\n", step.Agent)

	res, err := runPrepared(ctx, prepared)
	printAgentResponse(res)

	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	err = prepared.space.Capture(ctx)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	return nil
}
