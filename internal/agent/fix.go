package agent

import (
	"context"
	"fmt"
	"os"
	"slices"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
)

// defaultFixPrompt is used when a task's fix: supplies no prompt of its own.
// %q is the task name (also the name of the injected rerun tool). The
// captured failure output is appended after this.
// The stop condition is the stated failure, not exit 0: a task's success
// criteria are its assert: when it declares one, so a command can exit 0 and
// still have failed the step. An agent told to "repeat until it passes" reads
// the rerun tool's exit_code, sees 0, and stops having repaired nothing.
const defaultFixPrompt = `A command that must pass has just failed; the failure and its output are below. Investigate the working directory, make the smallest change that resolves the stated failure, then call the %q tool to re-run the command. Note that a zero exit code is not by itself success — the failure named below is what has to be resolved. Repeat until it is, then reply with a brief summary and stop.`

// buildFixMessages assembles the fix conversation's user turns: the fix:'s own
// messages: (or the default), with the captured failure output appended to the
// first — it is the thing the model is being asked to resolve, so it belongs
// in the turn that opens the work rather than the one that closes it.
//
// One turn per entry, exactly as a step's messages: means. They were joined
// with a blank line into a single turn, which is the difference between asking
// a model to do a thing and then check it, and asking it to do both at once —
// the ordering a list of messages exists to buy.
func buildFixMessages(fix *config.FixSpec, rt config.ResolvedTask, failureOutput, spillDir string) []string {
	messages := slices.Clone(fix.Messages)
	if len(messages) == 0 {
		messages = []string{fmt.Sprintf(defaultFixPrompt, rt.Name)}
	}

	messages[0] += "\n\n--- failure output ---\n" + spillOrTruncate(failureOutput, spillDir)

	return messages
}

// RunFix invokes a failed task's fix: agent. It reuses the normal
// agent-invocation resolution (tool grant, dials, attempts, max_turns) by
// projecting the FixSpec onto an agent Step, then injects the parent task as
// a zero-arg rerun tool — the task's own run: command (never its fix:, so a
// rerun can't recurse), exposed under the task's name — and seeds the
// conversation with the captured failure output. It does no merkle/store
// recording: the enclosing task step records the overall outcome, and the
// task's re-run (not the model's word) is the verdict.
//
// The fix conversation's run_shell/custom tools and the injected rerun tool
// all execute under rt.Image — the failing task's own image, never the fix
// agent's own Agent.Image (config.validateFixAgentImages rejects a fix
// agent that sets one, at LoadConfig time, precisely because it can never
// take effect here). The premise of the fix loop is reproducing and
// resolving the exact failure the task hit; running under a different image
// than the one that produced (and re-verifies) the failure would make the
// loop incoherent.
func RunFix(
	ctx context.Context, cfg *config.Config, jobName string, stepIndex int,
	rt config.ResolvedTask, st *store.Store, failureOutput, workspaceDir string,
) error {
	fix := rt.Fix

	// Project the fix spec onto an agent Step so ResolveAgentInvocation can
	// resolve grants/dials/limits exactly as it does for a real agent step.
	ri, err := cfg.ResolveAgentInvocation(config.Step{
		Agent:    fix.Agent,
		Messages: fix.Messages,
		Dir:      fix.Dir,
		Tools:    fix.Tools,
		Attempts: fix.Attempts,
		Timeout:  fix.Timeout,
	})
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", fix.Agent, err)
	}

	dir, err := resolveAgentDir(workspaceDir, fix.Dir)
	if err != nil {
		return err
	}

	// Expand "no tools granted means all built-ins" before appending, so the
	// injected task tool doesn't accidentally suppress the default built-ins.
	baseTools := ri.ToolSpecs
	if len(baseTools) == 0 {
		baseTools = config.DefaultAgentToolSpecs()
	}

	taskTool := config.ToolSpec{
		Name:        rt.Name,
		Description: fmt.Sprintf("Re-run the %q task's command. Returns exit_code, stdout, stderr.", rt.Name),
		Run:         rt.Run,
	}
	toolSpecs := append(append([]config.ToolSpec{}, baseTools...), taskTool)

	// A fix agent's grant may not include sub-agent tools
	// (validateFixAgentSubAgents), but it may include MCP tools — cfg is
	// required whenever that's possible, not just for signature parity.
	tools, closers, err := buildAgentTools(ctx, config.WithResolvedMCPCwd(cfg, dir), toolSpecs, rt.Image)
	if err != nil {
		return err
	}
	defer closeAll(closers)

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: dir, Env: rt.Env, User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()})
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", fix.Agent, err)
	}

	runner = runner.WithLabel(fix.Agent)
	defer shell.CloseRunner(runner, fix.Agent)

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return err
	}

	spillDir := newToolOutputSpillDir(dir, fix.Agent)
	if spillDir != "" {
		defer func() { _ = os.RemoveAll(spillDir) }()
	}

	messages := buildFixMessages(fix, rt, failureOutput, spillDir)

	contextBlocks, err := loadContextBlocks(dir, ri.ContextPaths, ri.MaxContextBytes)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", fix.Agent, err)
	}

	conv := agentConversation{
		system:        buildSystemMessage(ri.Persona, dir),
		messages:      messages,
		contextBlocks: contextBlocks,
		env:           toolEnv{dir: dir, runner: runner, spillDir: spillDir, ask: askContext(st, jobName, fix.Agent)},
		tools:         tools,
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
		compactAfterTokens:   ri.CompactAfterTokens,
		usage:                &stepUsage{name: ri.AgentName, budget: ri.BudgetTokens, delegateFraction: cfg.DelegateBudgetFraction(ri.AgentName)},
		// Give the conversation its live identity, matching RunStep/RunHook:
		// a fix agent is nested inside the task step that failed, so its
		// tool calls are attributable the same way instead of publishing (and
		// logging) nowhere.
		recorder: &transcriptRecorder{live: liveContext{
			bus: events.FromContext(ctx), runID: events.RunID(ctx), stepID: events.StepID(ctx), job: jobName, stepIndex: stepIndex, stepName: fix.Agent,
		}},
	}
	// RunFix runs the conversation once and never fails over (see
	// failover.go's doc comment on the scope boundary), so it owns
	// conv.usage's whole lifetime itself — runConversationLoop no longer
	// calls finish() on a caller's behalf.
	defer conv.usage.finish()

	llm := newAgentLLM(ri, apiKey)

	timeout := agentTimeout(ri.Timeout)

	// Keep the latest attempt's result either way, same as runPrepared: on
	// success it's the fix agent's own account of what it did (the
	// defaultFixPrompt asks it to "reply with a brief summary and stop");
	// on a turn-exhausted/failed attempt it's still the most useful partial
	// answer available. The caller (runFixTask, internal/pipeline/pipeline.go)
	// already printed "task %q failed ...; invoking fix agent %q" before this
	// call, so this only needs the response body, not another "agent:" banner.
	result, err := runOneConversation(ctx, ri, llm, conv, timeout)
	printAgentResponse(result)

	if err != nil {
		return fmt.Errorf("fix agent conversation: %w", err)
	}

	return nil
}
