package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
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
// caller.
func recordAgentFailure(ctx context.Context, st *store.Store, node merkle.Node, jobName string, runErr error) {
	_ = st.RecordNode(ctx, nodeRecord(node), jobName, "failed", nil, runErr)
	_ = st.RecordJobRun(ctx, jobName, node.Hash, "failed", runErr)
}

// preparedAgentStep is RunStep's resolved-and-materialized preamble:
// everything needed to run the conversation, split out so RunStep itself
// only sequences hash/run/capture/record and stays within the linter's
// complexity budget.
type preparedAgentStep struct {
	ri    config.ResolvedInvocation
	space workspace.StepSpace
	conv  agentConversation
	llm   model.LLM
}

// prepareAgentStep resolves step's agent, materializes its (isolated or
// shared) working directory, and builds the tools/LLM client it'll run
// with. On error, any workspace.StepSpace already created is closed before
// returning so the caller never has to.
func prepareAgentStep(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (preparedAgentStep, error) {
	ri, err := cfg.ResolveAgentInvocation(step)
	if err != nil {
		return preparedAgentStep{}, fmt.Errorf("agent %q: %w", step.Agent, err)
	}

	space, err := bw.TaskSpace(ctx, step.Agent, step.Inputs, step.Outputs)
	if err != nil {
		return preparedAgentStep{}, fmt.Errorf("workspace: %w", err)
	}

	dir, err := resolveAgentDir(space.Dir(), step.Dir)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	decls, registry, err := buildAgentTools(ri.ToolSpecs, ri.Image)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		workspace.CloseSpace(space, step.Agent)

		return preparedAgentStep{}, err
	}

	conv := agentConversation{
		system: buildSystemMessage(ri.Persona, dir),
		prompt: step.Prompt,
		env:    toolEnv{dir: dir, runner: shell.NewRunner(ri.Image)},
		tools:  agentTools{decls: decls, registry: registry, required: requiredToolNames(ri.ToolSpecs)},
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
	}

	return preparedAgentStep{ri: ri, space: space, conv: conv, llm: newAgentLLM(ri.BaseURL, ri.ModelName, apiKey)}, nil
}

// RunStep hashes step against parentHash (agent steps are never
// skippable) and runs it, retrying the whole conversation up to the
// resolved attempt count. It returns the hash to use as parentHash for the
// next step.
func RunStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string) (string, error) {
	prepared, err := prepareAgentStep(ctx, cfg, step, bw)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}
	defer workspace.CloseSpace(prepared.space, step.Agent)

	content := merkle.AgentContentMap(step, prepared.ri, cfg.Workspace)

	hash, err := merkle.HashNode(merkle.NodeKindAgent, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "agent", "agent", step.Agent)

	fmt.Printf("agent: %s\n", step.Agent)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindAgent, StepIndex: i, Resource: prepared.ri.AgentName, Content: content}

	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	var (
		finalContent string
		turnsUsed    int
	)

	err = retry.Do(agentCtx, prepared.ri.Attempts, func(_ int) error {
		answer, turns, runErr := runAgentConversation(agentCtx, prepared.llm, prepared.conv)
		turnsUsed = turns

		if runErr != nil {
			return runErr
		}

		finalContent = answer

		return nil
	})
	if err != nil {
		recordAgentFailure(ctx, st, node, jobName, err)

		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	err = prepared.space.Capture(ctx)
	if err != nil {
		wrapped := fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
		recordAgentFailure(ctx, st, node, jobName, wrapped)

		return "", wrapped
	}

	result := map[string]any{"response": finalContent, "turns": turnsUsed}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (agent %q): %w", i, step.Agent, err)
	}

	return hash, nil
}
