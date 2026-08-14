package agent

// Everything an agent step needs resolved and materialized before a single
// token is spent: the invocation (after failover), the workspace, the prompt,
// the tool set, and the spill directory its oversized tool output lands in.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/workspace"
)

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

	// Under --keep-workspace the step's directory survives for a postmortem,
	// and the spilled tool outputs are exactly what the kept transcript's
	// pointer messages name — deleting them would keep the scene and throw
	// away the evidence. A spill dir that would have ridden into an artifact
	// is already gone by now (removeSpillDirIfCaptured, before Capture).
	kept := workspace.Kept(p.space)

	workspace.CloseSpace(p.space, stepLabel)

	if !kept {
		p.removeSpillDir()
	}
}

// removeSpillDir deletes the step's tool-output spill directory.
func (p preparedAgentStep) removeSpillDir() {
	if p.spillDir != "" {
		_ = os.RemoveAll(p.spillDir)
	}
}

// removeSpillDirIfCaptured deletes the spill directory when — and only when —
// leaving it would put scratch tool output inside a captured artifact.
//
// The spill dir has to live inside the agent's working directory: read_file is
// confined there, and reading spilled output back is the entire point of
// spilling rather than truncating. Usually that is the step directory's own
// root, which Capture never copies. But dir: may point INSIDE a declared
// output (`dir: built`, `outputs: [built]`), and then the spill dir sits in
// the very tree that is about to be captured — so it goes, before Capture, on
// the success path.
//
// Everywhere else it survives until close, which honors --keep-workspace: a
// kept failed step is being kept for a postmortem, and the spilled files are
// exactly what its transcript's pointer messages name.
func (p preparedAgentStep) removeSpillDirIfCaptured() {
	if p.spillDir == "" || !p.spillDirWouldBeCaptured() {
		return
	}

	p.removeSpillDir()
}

// spillDirWouldBeCaptured reports whether the spill directory lies inside one
// of the step's declared output directories.
func (p preparedAgentStep) spillDirWouldBeCaptured() bool {
	for _, out := range p.step.Outputs {
		outDir := filepath.Join(p.space.Dir(), out)

		rel, err := filepath.Rel(outDir, p.spillDir)
		if err != nil {
			continue
		}

		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

// prepareAgentStep resolves step's agent, materializes its (isolated or
// shared) working directory, and builds the tools/LLM client it'll run
// with. On error, any workspace.StepSpace already created is closed before
// returning so the caller never has to.
func prepareAgentStep(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (preparedAgentStep, error) {
	primary, ri, err := resolveWithFailover(cfg, step)
	if err != nil {
		return preparedAgentStep{}, err
	}

	// The mapping is nil except for a cell of a collecting matrix, where each
	// output is captured under the cell's own coordinates — see
	// config.CollectedOutputMapping.
	space, err := bw.TaskSpace(ctx, step.Agent, step.InputNames(), step.Outputs,
		nil, config.CollectedOutputMapping(step.Outputs, nil, step.OutputSubdir))
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
	tools, closers, err := buildAgentTools(ctx, config.WithResolvedMCPCwd(cfg, dir), ri.ToolSpecs, ri.Image)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	verdictTool, err := injectVerdictTool(step.VerdictNames(), step.NoteRequired, tools)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: ri.Image, Cwd: dir, Env: ri.Env, User: ri.User, Network: ri.Network,
		Privileged: ri.Privileged, CPUShares: ri.Limits.CPUShares(), MemoryBytes: ri.Limits.MemoryBytes()})
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

	contextBlocks, err := prepareContextBlocks(dir, ri.ContextPaths, ri.MaxContextBytes, tools.decls)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)
		closeAll(closers)

		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	conv := agentConversation{
		system:        buildSystemMessage(ri.Persona, dir),
		prompt:        step.Prompt,
		contextBlocks: contextBlocks,
		upstream:      upstreamBlocks(ctx, step),
		env:           toolEnv{dir: dir, runner: runner, spillDir: spillDir},
		tools:         tools,
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
		verdictTool:          verdictTool,
		compactAfterTokens:   ri.CompactAfterTokens,
		usage:                &stepUsage{name: ri.AgentName, budget: ri.BudgetTokens, delegateFraction: cfg.DelegateBudgetFraction(ri.AgentName)},
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

	effective, err = primary.WithSource(source, agent)
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

// prepareContextBlocks loads context_paths files and validates that read_file
// is declared when context paths are present. Extracted from prepareAgentStep
// to keep its cyclomatic complexity under the linter budget.
func prepareContextBlocks(dir string, paths []string, limit int, decls *genai.Tool) ([]contextBlock, error) {
	blocks, err := loadContextBlocks(dir, paths, limit)
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
