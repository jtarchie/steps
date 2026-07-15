package agent

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/retry"
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
func RunFix(ctx context.Context, cfg *config.Config, taskName, taskRun string, fix *config.FixSpec, failureOutput, workspaceDir string) error {
	// Project the fix spec onto an agent Step so ResolveAgentInvocation can
	// resolve grants/dials/limits exactly as it does for a real agent step.
	ri, err := cfg.ResolveAgentInvocation(config.Step{
		Agent:    fix.Agent,
		Prompt:   fix.Prompt,
		Dir:      fix.Dir,
		Tools:    fix.Tools,
		Attempts: fix.Attempts,
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
		Name:        taskName,
		Description: fmt.Sprintf("Re-run the %q task's command. Returns exit_code, stdout, stderr.", taskName),
		Run:         taskRun,
	}
	toolSpecs := append(append([]config.ToolSpec{}, baseTools...), taskTool)

	decls, registry, err := buildAgentTools(toolSpecs)
	if err != nil {
		return err
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return err
	}

	prompt := fix.Prompt
	if prompt == "" {
		prompt = fmt.Sprintf(defaultFixPrompt, taskName)
	}

	prompt += "\n\n--- failure output ---\n" + truncateToolOutput(failureOutput)

	conv := agentConversation{
		system: buildSystemMessage(ri.Persona, dir),
		prompt: prompt,
		dir:    dir,
		tools:  agentTools{decls: decls, registry: registry, required: requiredToolNames(toolSpecs)},
		params: agentGenParams{
			temperature: ri.Temperature,
			topP:        ri.TopP,
			maxTokens:   ri.MaxTokens,
			reasoning:   ri.ReasoningEffort,
		},
		maxTurns: ri.MaxTurns,
	}
	llm := newAgentLLM(ri.BaseURL, ri.ModelName, apiKey)

	agentCtx, cancel := context.WithTimeout(ctx, agentStepTimeout)
	defer cancel()

	err = retry.Do(agentCtx, ri.Attempts, func(_ int) error {
		_, _, runErr := runAgentConversation(agentCtx, llm, conv)

		return runErr
	})
	if err != nil {
		return fmt.Errorf("fix agent conversation: %w", err)
	}

	return nil
}
