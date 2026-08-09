package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
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

// agentTimeout resolves the per-attempt conversation deadline: the
// invocation's timeout: when it parses to a positive duration, otherwise the
// default agentStepTimeout. A parse error can't happen for a validated config
// (config.validateTimeouts rejects it at LoadConfig), so an unexpected one
// falls back to the default rather than failing the run.
func agentTimeout(riTimeout string) time.Duration {
	if riTimeout != "" {
		parsed, err := config.ParseTimeout(riTimeout)
		if err == nil && parsed > 0 {
			return parsed
		}
	}

	return agentStepTimeout
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

// preparedAgentStep is RunStep's resolved-and-materialized preamble:
// everything needed to run the conversation, split out so RunStep itself
// only sequences hash/run/capture/record and stays within the linter's
// complexity budget.
type preparedAgentStep struct {
	// step is the resolved step: identical to RunStep's own step parameter,
	// except a run-time prompt_file: {artifact, path} (see FileRef.Deferred)
	// has been read and folded into Prompt (with PromptFile cleared) by the
	// time this is returned. RunStep must hash THIS copy, not its own step
	// param, so merkle.AgentContentMap sees the loaded prompt text rather than
	// the file reference.
	step config.Step
	// ri is the invocation as it will RUN, after any preflight failover.
	ri config.ResolvedInvocation
	// primary is the invocation as CONFIGURED, before failover. The step
	// hashes against this one, so which source served a run is availability,
	// not content: a fallback firing cannot invalidate a cache entry.
	primary config.ResolvedInvocation
	space   workspace.StepSpace
	conv    agentConversation
	llm     model.LLM
	// closers holds everything opened for this step that must be released
	// before its directory goes away: the MCP connections buildAgentTools
	// opened, and the shell runner (whose container, under image:, bind-mounts
	// that very directory).
	closers  []io.Closer
	spillDir string // run_shell/custom tool output spill dir — see toolEnv.spillDir; "" if it couldn't be created
}

// close releases everything prepareAgentStep opened for this step: any tool
// connections (MCP) and the shell runner, then its workspace space, then its
// spill dir (removed, not just closed — it may hold files a large
// run_shell/custom tool output spilled to; see toolEnv.spillDir). Ignores
// errors — this is always deferred right after a successful
// prepareAgentStep, with the step's real outcome already determined by the
// time it runs.
//
// The closers go first, before the space: both a containerized runner and a
// stdio MCP server hold the step's directory (as a bind mount, as a working
// directory), and tearing the directory out from under either is a worse
// failure than the ordinary case this runs in.
func (p preparedAgentStep) close(stepLabel string) {
	closeAll(p.closers)
	workspace.CloseSpace(p.space, stepLabel)

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
// st is the run-context seam: both halves of the context store are derived
// from it here (the set_context writer and the recap read back), so callers
// pass one thing and a nil store simply means "no context store on this path".
func prepareAgentStep(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace, handoff *Handoff, st *store.Store) (preparedAgentStep, error) {
	primary, ri, err := resolveWithFailover(cfg, step)
	if err != nil {
		return preparedAgentStep{}, err
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

	step, err = prepareStepPrompt(space.Dir(), step)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	// A stdio MCP server with a relative cwd: is pointed at this step's own
	// working directory, so a language server can index the same
	// materialized input the agent's file tools read. Resolved here, where
	// dir is finally known, rather than at load time.
	decls, registry, closers, err := buildAgentTools(ctx, config.WithResolvedMCPCwd(cfg, dir), ri.ToolSpecs, ri.Image)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	required := requiredToolNames(ri.ToolSpecs)

	synthesized, err := injectSynthesizedTools(ctx, cfg, step,
		synthesisInputs{handoff: handoff, store: st, readScopes: ContextReadScopes(ctx), writeScope: ContextWriteScope(ctx)},
		decls, registry, required)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: ri.Image, Cwd: dir, Env: ri.Env, User: ri.User})
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	runner = runner.WithLabel(step.Agent)

	// Joins closers immediately, so every error path below tears the step's
	// container down through the same closeAll the success path uses.
	closers = append(closers, runner)

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, err
	}

	spillDir := newToolOutputSpillDir(dir, step.Agent)

	contextBlocks, err := prepareContextBlocks(dir, withHandoffNotePath(step, dir, ri.ContextPaths), decls)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	// Delivered notes are model-authored, and a fan-in delivers several at
	// once — the widest injection surface here, so each is fenced as data.
	contextBlocks = fenceNoteBlocks(contextBlocks)

	conv := agentConversation{
		system:        buildSystemMessage(ri.Persona, dir),
		prompt:        promptWithHandoff(step.Prompt, step.Handoff, handoff, spillDir),
		contextBlocks: contextBlocks,
		recap:         synthesized.recap,
		env:           toolEnv{dir: dir, runner: runner, spillDir: spillDir},
		tools:         agentTools{decls: decls, registry: registry, required: required, maxCalls: maxCallsByName(ri.ToolSpecs)},
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
		verdictTool:          synthesized.verdictTool,
		compactAfterTokens:   ri.CompactAfterTokens,
		usage:                &stepUsage{name: ri.AgentName, budget: ri.BudgetTokens},
	}

	return preparedAgentStep{
		step: step, ri: ri, primary: primary, space: space, conv: conv, llm: invocationLLM(ri, apiKey),
		closers: closers, spillDir: spillDir,
	}, nil
}

// invocationLLM builds the client an invocation talks to, or nil for a CLI
// source: the subprocess talks to the model itself, and everything a step
// assembled reaches it through the bridge instead of through a request (see
// cli.go).
func invocationLLM(ri config.ResolvedInvocation, apiKey string) model.LLM {
	if ri.CLI != "" {
		return nil
	}

	return newAgentLLM(ri, apiKey)
}

// resolveWithFailover resolves a step's agent twice over: as CONFIGURED, which
// is what the step hashes against, and as it will actually RUN, which is the
// same thing unless preflight failed the primary model over to a fallback.
//
// Keeping them apart is what lets an outage change where requests go without
// invalidating a single cache entry — which source served a run is
// availability, not content. Everything but the source is identical between
// the two: an outage changes where requests go, never what the agent is.
func resolveWithFailover(cfg *config.Config, step config.Step) (primary, effective config.ResolvedInvocation, err error) {
	primary, err = cfg.ResolveAgentInvocation(step)
	if err != nil {
		return primary, effective, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	source, ok := selectedSource(primary.AgentName)
	if !ok {
		return primary, primary, nil
	}

	agent, err := cfg.FindAgent(primary.AgentName)
	if err != nil {
		return primary, primary, nil //nolint:nilerr // it resolved a moment ago; not worth failing a step over
	}

	effective, err = primary.WithSource(source, agent.CompactAfterTokens)
	if err != nil {
		return primary, primary, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	return primary, effective, nil
}

// prepareStepPrompt resolves a run-time prompt_file: {artifact, path} (see
// resolveDeferredPrompt) out of the materialized workspace, when the step
// declares one. Extracted from prepareAgentStep (its branches would
// otherwise push the preparation flow over the complexity budget); behavior
// is unchanged.
func prepareStepPrompt(spaceDir string, step config.Step) (config.Step, error) {
	if !step.PromptFile.Deferred() {
		return step, nil
	}

	resolved, err := resolveDeferredPrompt(spaceDir, step)
	if err != nil {
		return config.Step{}, err
	}

	step.Prompt = resolved
	step.PromptFile = nil

	return step, nil
}

// resolveDeferredPrompt reads step's run-time prompt_file: {artifact, path}
// (see config.FileRef.Deferred) out of the step's own materialized working
// directory. Unlike a load-time include (resolved once, at LoadConfig, before
// merkle.PlanChains ever runs), this file lives inside an artifact a get step
// fetched, which does not exist until the step's own build workspace has
// materialized it — spaceDir is that build's step space root, where a
// declared input "repo" lands at spaceDir/repo (see workspace.StepSpace),
// independent of step.Dir (the agent's working directory, which may be a
// subdirectory of an input).
//
// The artifact's contents are untrusted — a fetched repo is whatever a PR
// author put there — so the path is confined and symlink-checked by
// resolveAgentPath, the same guard read_file/list_dir use against a
// model-supplied path.
func resolveDeferredPrompt(spaceDir string, step config.Step) (string, error) {
	if step.Prompt != "" {
		return "", fmt.Errorf("agent %q: prompt: and prompt_file: are mutually exclusive", step.Agent)
	}

	rel := filepath.Join(step.PromptFile.Artifact, step.PromptFile.Path)

	resolved, err := resolveAgentPath(spaceDir, rel)
	if err != nil {
		return "", fmt.Errorf("agent %q: prompt_file {artifact: %q, path: %q}: %w",
			step.Agent, step.PromptFile.Artifact, step.PromptFile.Path, err)
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // resolved and confined by resolveAgentPath against spaceDir, the step's own materialized working directory
	if err != nil {
		return "", fmt.Errorf("agent %q: prompt_file {artifact: %q, path: %q}: %w",
			step.Agent, step.PromptFile.Artifact, step.PromptFile.Path, err)
	}

	if strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("agent %q: prompt_file {artifact: %q, path: %q} is empty",
			step.Agent, step.PromptFile.Artifact, step.PromptFile.Path)
	}

	return string(data), nil
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
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, handoff, st)
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

	fmt.Printf("agent: %s%s\n", name, fallbackBanner(prepared))

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAgent, StepIndex: i, Resource: name, Content: content}

	res, err := runPrepared(ctx, prepared)
	res.model = fallbackModel(prepared)

	printAgentResponse(res)

	previous := &PreviousRun{
		Agent: step.Agent, Response: res.text, Verdict: res.verdict, Note: res.note,
		Turns: res.turns, Trajectory: exportTrajectory(res.trajectory),
	}

	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, res, err)

		// A failed run emitted no clean verdict; the pipeline routes it via
		// to["failure"] (or fails the job).
		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = assertAgentResponse(step.Assert, res)
	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, res, err)

		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = prepared.space.Capture(ctx)
	if err != nil {
		wrapped := fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
		recordAgentFailure(ctx, st, node, jobName, res, wrapped)

		return StepOutcome{Previous: previous}, wrapped
	}

	// Publish the handoff note only now, once the step has fully succeeded:
	// a failed run, a failed assert, or a failed capture leaves the previous
	// note (if any) in place rather than replacing it with one describing work
	// that did not stand.
	publishHandoffNote(prepared, jobName, res)

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", agentResultRecord(res), nil)
	if err != nil {
		return StepOutcome{Previous: previous}, fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return StepOutcome{Hash: hash, Verdict: res.verdict, Note: res.note, Previous: previous}, nil
}

// agentResultRecord builds the result map RunStep records for a succeeded
// agent step: always the response and turn count, plus whichever optional
// outcomes the run actually produced. Extracted from RunStep to keep its
// cyclomatic complexity under the linter budget.
func agentResultRecord(res conversationResult) map[string]any {
	result := map[string]any{"response": res.text, "turns": res.turns}

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

	if res.handoffNote != nil {
		result["handoff_note"] = res.handoffNote
	}

	return result
}

// maxRecordedArgBytes caps how much of a single tool argument is persisted.
// The trajectory is a record of what the agent did, not a copy of what it
// wrote: a write_file call's whole content would balloon nodes.result for no
// diagnostic gain, since the file itself is the artifact.
const maxRecordedArgBytes = 2048

// recordedTrajectory converts a run's tool calls into the plain shape stored
// in nodes.result. It is the persistence counterpart to exportTrajectory (see
// handoff.go), which shapes the same calls for the previous_run tool and drops
// the ok flag; here that flag is the point, since "it called write_file and it
// failed" is a different story from "it called write_file".
//
// The trajectory used to die with the process — it existed only in memory, for
// a routed-to successor and the handoff note's files-touched section. So the
// most useful question about an agent step ("what did it actually do?") had no
// answer once the run ended, and none at all for a step that failed.
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

// prepareContextBlocks loads context_paths files and validates that read_file
// is declared when context paths are present. Extracted from prepareAgentStep
// to keep its cyclomatic complexity under the linter budget.
func prepareContextBlocks(dir string, paths []string, decls *genai.Tool) ([]contextBlock, error) {
	blocks, err := loadContextBlocks(dir, paths)
	if err != nil {
		return nil, err
	}

	if len(blocks) > 0 && !hasReadFileDecl(decls) {
		return nil, errors.New("context_paths requires read_file in the tool grant")
	}

	return blocks, nil
}

// hasReadFileDecl reports whether decls declares the read_file tool.
func hasReadFileDecl(decls *genai.Tool) bool {
	for _, decl := range decls.FunctionDeclarations {
		if decl.Name == "read_file" {
			return true
		}
	}

	return false
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
// its timeout (step.Timeout if set, otherwise the default agentStepTimeout).
// Shared by RunStep and RunHook so a hook agent runs the exact same
// conversation machinery, minus the merkle/store recording RunStep does.
//
// The timeout now bounds the conversation outright rather than each of several
// restarts of it, which is what `timeout:` always read as. See
// docs/attempts-timeout.md.
func runPrepared(ctx context.Context, prepared preparedAgentStep) (conversationResult, error) {
	timeout := agentTimeout(prepared.ri.Timeout)

	// A CLI source delegates the conversation to a subprocess instead of
	// driving it here (see cli.go). Branching at this one choke point is what
	// gives RunStep, RunHook, and every routed re-entry the CLI path for free.
	if prepared.ri.CLI != "" {
		return runCLIConversation(ctx, prepared, timeout)
	}

	return runOneConversation(ctx, prepared.ri, prepared.llm, prepared.conv, timeout)
}

// runOneConversation runs a conversation under its timeout and, on failure,
// reports the provider requests it really spent.
//
// It runs the conversation ONCE. attempts: retries the failing request, down
// in requests.go, which is why nothing here loops: a restart discarded every
// accumulated turn and re-billed the whole conversation to re-ask a question
// the transport had already retried and given up on. Shared by runPrepared and
// RunFix so both spend attempts: on the same thing.
func runOneConversation(
	ctx context.Context,
	ri config.ResolvedInvocation,
	llm model.LLM,
	conv agentConversation,
	timeout time.Duration,
) (conversationResult, error) {
	convCtx, cancel := context.WithTimeout(ctx, timeout)
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

	return result, err
}

// RunHook runs an agent step as a hook: it resolves, materializes, runs the
// conversation, and captures declared outputs exactly like RunStep, but
// records no merkle node or job_run — the enclosing step/job records the
// aggregate outcome (the same no-record contract as RunFix). A returned error
// is already outcome-marked where appropriate (see runAgentConversation), so
// the caller's hook classification works unchanged.
func RunHook(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) error {
	// nil writer: a hook cannot declare context: (validateContextSteps rejects
	// it), so there is never a set_context tool to serve here.
	prepared, err := prepareAgentStep(ctx, cfg, step, bw, nil, nil)
	if err != nil {
		return fmt.Errorf("agent %q: %w", step.Agent, err)
	}
	defer prepared.close(step.Agent)

	fmt.Printf("agent: %s%s\n", step.Agent, fallbackBanner(prepared))

	res, err := runPrepared(ctx, prepared)
	res.model = fallbackModel(prepared)

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
