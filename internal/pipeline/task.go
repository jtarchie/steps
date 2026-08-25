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
	"github.com/jtarchie/steps/internal/venue"
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

	// The venue retry wraps the attempts: loop rather than sitting inside it:
	// a worker reclaimed underneath the step is re-placed on a fresh machine
	// with the author's attempts: budget untouched. A new runner per venue
	// attempt, because the machine is a different one.
	err = withVenueRetry(ctx, step, func(ctx context.Context) error {
		// One runner for every attempt, not one per attempt. A retry is a
		// second go at the SAME workspace: locally that falls out of the
		// directory simply persisting, but a venue session owns a remote
		// scratch and uploads the tree when it opens, so a per-attempt
		// session would re-upload the ORIGINAL tree and lose whatever the
		// last attempt wrote outside its declared outputs. A task that marks
		// progress on disk to skip work it has already done would then pass
		// here and loop forever on a worker.
		runner, runnerErr := taskRunner(ctx, step, rt, space)
		if runnerErr != nil {
			return runnerErr
		}

		defer shell.CloseRunner(runner, rt.Name)

		return retryWithTimeout(ctx, step.Attempts, rt.Timeout, func(attempt, total int) {
			fmt.Printf("task: %s (attempt %d/%d)\n", executedStepName(step), attempt, total)
			logFrom(ctx).Info("job.task.attempt", "task", executedStepName(step), "attempt", attempt, "total_attempts", total)
		}, func(attemptCtx context.Context) error {
			return runTaskCommand(attemptCtx, cfg, runner, rt, space.Dir())
		})
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

// taskRunner builds the runner a task's attempts share.
func taskRunner(ctx context.Context, step config.Step, rt config.ResolvedTask, space workspace.StepSpace) (shell.Runner, error) {
	workspaceDir := space.Dir()

	// Fetch is the step's declared outputs: what a worker sends back after
	// every command, so the local tree matches before an assert: reads it.
	worker, err := workerFor(ctx, step)
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	//nolint:contextcheck // NewRunner takes no context; opening the artifact store reads only local config
	runner, err := venue.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: workspaceDir, Env: rt.Env, User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes(),
		Worker: worker, WorkerTag: placementTag(step), Fetch: rt.Outputs,
		ArtifactStore: artifactStoreFrom(ctx),
		// The same postmortem, on the machine that actually ran the step: a
		// worker's scratch is the remote half of the step directory, and a
		// flag whose whole purpose is having the files afterwards would stop
		// at the machine boundary without this. Asked of the workspace rather
		// than read from the flag, because the workspace is what actually
		// decided whether this step's directory survives.
		Keep: workspace.Kept(space)})
	if err != nil {
		return nil, fmt.Errorf("task %q: %w", rt.Name, err)
	}

	return runner.WithLabel(rt.Name), nil
}

// runTaskCommand runs a task's run: command. Without an assert: or fix:, it
// streams output live and any nonzero exit is a hard failure.
func runTaskCommand(ctx context.Context, cfg *config.Config, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
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
	publishOutputForCurrentStep(ctx, stdout, stderr)

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

// runFixTask runs a task with a fix: agent: capture the output, and when the
// run misses the step's own success criteria invoke the fix agent (seeded
// with that verdict and output, and given the task itself as a rerun tool),
// then re-run the command once and judge that. A run that already passes
// never constructs the agent.
//
// The trigger and the verdict are the same question — taskVerdict — asked
// before and after the repair. Asking the first one with a different rule is
// what let a declared fix: bind nothing: gated on the exit code alone, a task
// whose assert: was the real criterion got repaired when it had already
// passed (and went red when the repair worked), and got no repair at all when
// it failed while exiting 0.
func runFixTask(ctx context.Context, cfg *config.Config, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	failure := taskVerdict(rt, stdout, exitCode, workspaceDir)
	if failure == nil {
		return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir, "")
	}

	fmt.Printf("task %q failed (%s); invoking fix agent %q\n", rt.Name, failure, rt.Fix.Agent)

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

	err = agent.RunFix(ctx, cfg, jobName, stepIndex, rt, taskFailureOutput(failure, stdout, stderr, exitCode), workspaceDir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", rt.Fix.Agent, err)
	}

	// Verdict: re-run the command (its run:, not its fix:) and judge that.
	stdout, stderr, exitCode, err = runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir,
		fmt.Sprintf("still failing after fix agent %q", rt.Fix.Agent))
}

// taskVerdict judges a completed run by the step's own success criteria: its
// assert: when the task declared one, its exit code otherwise. nil means the
// run passed.
//
// Separate from finishTask because runFixTask has to ask the question before
// it publishes anything — a repair decided by a different rule than the one
// that grades the result is a repair aimed at the wrong target.
func taskVerdict(rt config.ResolvedTask, stdout string, exitCode int, workspaceDir string) error {
	if rt.Assert != nil {
		return assertMismatch(rt.Assert, stdout, exitCode, workspaceDir)
	}

	if exitCode != 0 {
		return fmt.Errorf("exit status %d", exitCode)
	}

	return nil
}

// finishTask publishes a completed run's output and turns the step's verdict
// on it into the step's error. The publish comes first — a mismatch names the
// expectation that failed and never the output that missed it, so suppressing
// the output on failure would hide exactly what the reader needs.
//
// attribution, when set, names what already tried and failed to make this run
// pass, so a step its fixer could not rescue does not read like a step that
// never had one. That distinction is the first thing an operator wants from a
// red build of a feature that costs money per invocation.
func finishTask(ctx context.Context, rt config.ResolvedTask, stdout, stderr string, exitCode int, workspaceDir, attribution string) error {
	publishOutputForCurrentStep(ctx, stdout, stderr)

	failure := taskVerdict(rt, stdout, exitCode, workspaceDir)
	if failure == nil {
		return nil
	}

	if attribution != "" {
		failure = fmt.Errorf("%s: %w", attribution, failure)
	}

	return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(failure))
}

// runAssertedTask runs rt.Run capturing its output, then judges it against
// rt.Assert. workspaceDir is where assert.files: entries are checked — the
// task's own working directory, read before the caller (executeTask) captures
// it into the artifact store.
func runAssertedTask(ctx context.Context, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runCaptured(ctx, runner, rt)
	if err != nil {
		return err
	}

	return finishTask(ctx, rt, stdout, stderr, exitCode, workspaceDir, "")
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

// taskFailureOutput formats a failed run's verdict, exit code and streams
// into the text seeded into the fix agent's prompt.
//
// The verdict is named separately from the exit code because with an assert:
// they are different facts: a command can exit 0 and still have failed the
// step, and a fixer told only the exit code would be repairing something the
// step never asked about.
func taskFailureOutput(reason error, stdout, stderr string, exitCode int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "failure: %s\n", reason)
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
