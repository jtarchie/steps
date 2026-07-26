package agent

import (
	"context"
	"fmt"
	"os"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/retry"
	"github.com/jtarchie/steps/internal/shell"
)

// defaultFixPrompt is used when a task's fix: supplies no prompt of its own.
// %q is the task name (also the name of the injected rerun tool). The
// captured failure output is appended after this.
const defaultFixPrompt = `A command that must pass has just failed; its output is below. Investigate the working directory, make the smallest change that resolves the failure, then call the %q tool to re-run the command and confirm it passes. Repeat until it passes, then reply with a brief summary and stop.`

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
func RunFix(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, failureOutput, workspaceDir string) error {
	fix := rt.Fix

	// Project the fix spec onto an agent Step so ResolveAgentInvocation can
	// resolve grants/dials/limits exactly as it does for a real agent step.
	ri, err := cfg.ResolveAgentInvocation(config.Step{
		Agent:    fix.Agent,
		Prompt:   fix.Prompt,
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
	decls, registry, closers, err := buildAgentTools(ctx, cfg, toolSpecs, rt.Image)
	if err != nil {
		return err
	}
	defer closeAll(closers)

	runner, err := shell.NewRunner(rt.Image, dir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", fix.Agent, err)
	}

	runner = runner.WithLabel(fix.Agent)

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return err
	}

	spillDir := newToolOutputSpillDir(dir, fix.Agent)
	if spillDir != "" {
		defer func() { _ = os.RemoveAll(spillDir) }()
	}

	prompt := fix.Prompt
	if prompt == "" {
		prompt = fmt.Sprintf(defaultFixPrompt, rt.Name)
	}

	prompt += "\n\n--- failure output ---\n" + spillOrTruncate(failureOutput, spillDir)

	conv := agentConversation{
		system: buildSystemMessage(ri.Persona, dir),
		prompt: prompt,
		env:    toolEnv{dir: dir, runner: runner, spillDir: spillDir},
		tools:  agentTools{decls: decls, registry: registry, required: requiredToolNames(toolSpecs), maxCalls: maxCallsByName(toolSpecs)},
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns:             ri.MaxTurns,
		toolChoiceStringOnly: ri.StringOnlyToolChoice,
		compactAfterTokens:   ri.CompactAfterTokens,
	}
	llm := newAgentLLM(ri, apiKey)

	timeout := agentTimeout(ri.Timeout)

	// Keep the latest attempt's result either way, same as runPrepared: on
	// success it's the fix agent's own account of what it did (the
	// defaultFixPrompt asks it to "reply with a brief summary and stop");
	// on a turn-exhausted/failed attempt it's still the most useful partial
	// answer available. The caller (runFixTask, internal/pipeline/pipeline.go)
	// already printed "task %q failed ...; invoking fix agent %q" before this
	// call, so this only needs the response body, not another "agent:" banner.
	var result conversationResult

	err = retry.Do(ctx, ri.Attempts, func(attempt int) error {
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// withAttempt keeps a retry off the provider instance the previous
		// attempt may have just failed against (see composeSessionID).
		res, runErr := runAgentConversation(withAttempt(attemptCtx, attempt), llm, conv)
		result = res

		return runErr
	})
	printAgentResponse(result)

	if err != nil {
		return fmt.Errorf("fix agent conversation: %w", err)
	}

	return nil
}
