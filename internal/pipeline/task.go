package pipeline

// task: — running a command, with the assert:/fix: variants of "did it work".

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/workspace"
)

// runTaskStep hashes step against parentHash and, unless that hash is
// skippable, runs it.
func runTaskStep(ctx context.Context, r stepRunner, i int, step config.Step, skippable map[string]bool, parentHash string) (stepResult, error) {
	rt, err := r.cfg.ResolveTask(step)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d: %w", i, err)
	}

	content, err := merkle.TaskNodeContent(r.cfg, step, rt)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTask, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	if skippable[hash] {
		// Every skip line names its reason — (chain), (when), (version: ...) —
		// and this one is the cache hit that triggers the (chain) lines below.
		fmt.Printf("skip: %s (cached)\n", rt.Name)
		logFrom(ctx).Info("job.skip", "task", rt.Name, "reason", "cached", "hash", hash)

		return stepResult{hash: parentHash, disposition: stepChainSkipped}, nil
	}

	logFrom(ctx).Debug("job.step", "task", rt.Name, "command", rt.Run)

	// The name the step is KNOWN by, which for an across: cell is its labelled
	// identity rather than the task it resolves through — so the run line, the
	// recorded node, and the skip line on the next run all say the same thing.
	name := executedStepName(step)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTask, StepIndex: i, Resource: name, Content: content}

	req, err := taskCacheRequest(content, step, rt)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	cached := lookupStepCache(ctx, r, step, req, name)
	if cached.Hit {
		err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", cached.NodeResult(), nil)
		if err != nil {
			return stepResult{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		}

		return stepResult{hash: hash, disposition: stepCacheHit}, nil
	}

	fmt.Printf("task: %s\n", name)

	err = executeTask(ctx, r.cfg, step, rt, r.bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		recordStepFailure(ctx, r, node, wrapped)

		return stepResult{}, wrapped
	}

	err = r.st.RecordNode(ctx, nodeRecord(node), r.jobName, "succeeded", nil, nil)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	// After the node is recorded, so a run that could not record its own
	// outcome does not leave behind an entry claiming the work is done.
	workspace.SaveStepCache(ctx, r.bw, cached.Key, req)

	return ran(hash), nil
}

// executeTask materializes a task's (isolated or shared) working directory,
// runs its command with retries and timeout, and captures its declared
// outputs — with no merkle/store recording. Shared by runTaskStep (which
// records the aggregate outcome) and hook execution (where the enclosing
// step/job records it).
func executeTask(ctx context.Context, cfg *config.Config, step config.Step, rt config.ResolvedTask, bw workspace.BuildWorkspace) error {
	// A cell of a collecting matrix captures each output under its own
	// coordinates (findings -> findings/alpha) instead of the plain name, so
	// N cells share one declared artifact without clobbering each other. An
	// ordinary step passes its mapping through unchanged.
	outputMapping := config.CollectedOutputMapping(rt.Outputs, rt.OutputMapping, step.OutputSubdir)

	space, err := bw.TaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs, rt.InputMapping, outputMapping)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}
	defer workspace.CloseSpace(space, rt.Name)

	// A shell command cannot be handed a synthetic tool result, so a task that
	// declared context: from: gets each demanded decision as a file it can
	// read (config.UpstreamPath). Written before the command runs, and only
	// for senders that have actually run.
	err = deliverUpstreamFiles(ctx, space.Dir(), step)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	err = retryWithTimeout(ctx, step.Attempts, rt.Timeout, func(attempt, total int) {
		fmt.Printf("task: %s (attempt %d/%d)\n", executedStepName(step), attempt, total)
		logFrom(ctx).Info("job.task.attempt", "task", executedStepName(step), "attempt", attempt, "total_attempts", total)
	}, func(attemptCtx context.Context) error {
		return runTaskCommand(attemptCtx, cfg, rt, space.Dir())
	})
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	err = space.Capture(ctx)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	return nil
}

// runTaskCommand runs a task's run: command. Without an assert: or fix:, it
// streams output live and any nonzero exit is a hard failure.
func runTaskCommand(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, workspaceDir string) error {
	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: workspaceDir, Env: rt.Env, User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()})
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name)
	defer shell.CloseRunner(runner, rt.Name)

	switch {
	case rt.Fix != nil:
		return runFixTask(ctx, cfg, runner, rt, workspaceDir)
	case rt.Assert != nil:
		return runAssertedTask(ctx, runner, rt, workspaceDir)
	}

	stdout, stderr, err := runner.RunStreamedCapture(ctx, rt.Run, maxPublishedOutputBytes)

	// Published whether or not the command succeeded. A failing step is the
	// one whose output is actually wanted, and nothing else carries it: the
	// error this returns is "command %q failed: exit status N", with no trace
	// of what the command said on its way out.
	publishOutputForCurrentStep(ctx, rt.Name, stdout, stderr)

	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, classifyRunError(ctx, err))
	}

	return nil
}

// classifyRunError decides what a failed command means. A canceled context
// (job abort) or per-attempt timeout propagates as-is; only a genuine nonzero
// exit is a task-level Failure. Order matters: a killed process is also an
// *exec.ExitError, so it would otherwise be misread as a real failure.
func classifyRunError(ctx context.Context, err error) error {
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return cancelErr //nolint:wrapcheck // the caller names the task; this only classifies
	}

	if shell.IsExitError(err) {
		return outcome.Fail(err) //nolint:wrapcheck // Fail only marks the classification
	}

	return err
}

// runFixTask runs a task with a fix: agent: capture the output, and on a
// nonzero exit invoke the fix agent (seeded with that output and given the
// task itself as a rerun tool), then re-run the command once. A green first
// run never constructs the agent.
//
// The re-run's output is what decides the step — via the task's assert: when
// it has one, and by its exit code otherwise. Repair is part of producing an
// outcome and assert: is the oracle over the outcome produced, which is the
// layering attempts: already has (only the final attempt is judged). Running
// them the other way round made a declared fix: bind nothing, silently.
func runFixTask(ctx context.Context, cfg *config.Config, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	if exitCode == 0 {
		return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir)
	}

	fmt.Printf("task %q failed (exit %d); invoking fix agent %q\n", rt.Name, exitCode, rt.Fix.Agent)

	// The fix agent is a dispatch point like any other, so it records — without
	// this a job's assert.execution reads [check] whether the fix ran or the
	// task passed first try, which is the one difference a fix: fixture exists
	// to pin. It lands BEFORE the task's own entry because entries are recorded
	// as each thing completes and the fix agent finishes inside the task step,
	// the same ordering that puts a step ahead of its hooks.
	recordExecution(ctx, rt.Fix.Agent)

	// The step that invoked it, so the fix conversation's own turns and tool
	// calls publish under it rather than nowhere (see agent.RunFix).
	jobName, stepIndex := currentStepRef(ctx)

	err = agent.RunFix(ctx, cfg, jobName, stepIndex, rt, taskFailureOutput(stdout, stderr, exitCode), workspaceDir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", rt.Fix.Agent, err)
	}

	// Verdict: re-run the command (its run:, not its fix:) and gate on it.
	stdout, stderr, exitCode, err = runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	if rt.Assert != nil {
		return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir)
	}

	publishOutputForCurrentStep(ctx, rt.Name, stdout, stderr)

	if exitCode != 0 {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(fmt.Errorf("still failing after fix agent %q (exit %d)", rt.Fix.Agent, exitCode)))
	}

	return nil
}

// finishTask publishes a completed run's output and turns it into the step's
// outcome: the assert's own mismatch when the task declared one, the exit code
// otherwise. The publish comes first either way — a mismatch names the
// expectation that failed and never the output that missed it, so suppressing
// the output on failure would hide exactly what the reader needs.
func finishTask(ctx context.Context, rt config.ResolvedTask, stdout, stderr string, exitCode int, workspaceDir string) error {
	publishOutputForCurrentStep(ctx, rt.Name, stdout, stderr)

	if rt.Assert == nil {
		if exitCode != 0 {
			return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(fmt.Errorf("exit status %d", exitCode)))
		}

		return nil
	}

	mismatch := assertMismatch(rt.Assert, stdout, exitCode, workspaceDir)
	if mismatch != nil {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(mismatch))
	}

	return nil
}

// runAssertedTask runs rt.Run capturing its output, then evaluates rt.Assert:
// a matching stdout substring and exit code make the task a success even on a
// non-zero exit; a mismatch is a task-level failure with a got-vs-want reason.
// workspaceDir is where assert.files: entries are checked — the task's own
// working directory, read before the caller (executeTask) captures it into
// the artifact store.
func runAssertedTask(ctx context.Context, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir)
}

// runCaptured runs rt.Run buffering both streams, echoes them (RunCaptureFull
// does not stream live the way Run does), and returns the exit code as data.
//
// A signal-killed process — from a canceled ctx — is reported as data too, not
// as err, so the cancellation is checked here rather than left for the caller
// to misread as a genuine nonzero verdict.
func runCaptured(ctx context.Context, runner shell.Runner, rt config.ResolvedTask) (stdout, stderr string, exitCode int, err error) {
	stdout, stderr, exitCode, err = runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return "", "", 0, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return "", "", 0, fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	if stdout != "" {
		fmt.Print(shell.PrefixLines(rt.Name, stdout))
	}

	if stderr != "" {
		fmt.Fprint(os.Stderr, shell.PrefixLines(rt.Name, stderr))
	}

	return stdout, stderr, exitCode, nil
}

// assertMismatch returns a reason when captured stdout/exit code/output files
// don't satisfy assert, or nil when they match. Code is exact; Stdout is a
// substring test; Files is checked against workspaceDir.
func assertMismatch(assert *config.Assert, stdout string, exitCode int, workspaceDir string) error {
	if assert.Code != nil && *assert.Code != exitCode {
		return fmt.Errorf("assert.code: want %d, got %d", *assert.Code, exitCode)
	}

	if assert.Stdout != nil && !strings.Contains(stdout, *assert.Stdout) {
		return fmt.Errorf("assert.stdout: output does not contain %q", *assert.Stdout)
	}

	//nolint:wrapcheck // the message is already an assert.* mismatch reason; wrapping would only repeat the field name
	return config.AssertFilesMismatch(assert.Files, workspaceDir)
}

// taskFailureOutput formats a failed run's exit code and streams into the
// text seeded into the fix agent's prompt.
func taskFailureOutput(stdout, stderr string, exitCode int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "exit code: %d\n", exitCode)

	if stdout != "" {
		b.WriteString("stdout:\n")
		b.WriteString(stdout)
		b.WriteString("\n")
	}

	if stderr != "" {
		b.WriteString("stderr:\n")
		b.WriteString(stderr)
		b.WriteString("\n")
	}

	return b.String()
}
