// Package pipeline orchestrates a job's plan: resolving/fetching get steps,
// running task/put/agent steps in order, and recording each step's outcome
// so later runs can skip unchanged work (see internal/merkle).
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// nodeRecord converts a plan merkle.Node into the shape store.RecordNode
// persists, keeping the store package free of a dependency on merkle's Node
// type.
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

// RunJob executes job's plan steps in order. pinned applies to any `get`
// step's version selection. provider materializes every build/step
// workspace the run needs — see workspace.Provider; when cfg.Workspace is
// nil, provider is the shared, single-mutable-directory implementation.
//
// Before executing anything, it statically validates every task/agent/put
// step's declared inputs (see workspace.ValidateArtifactFlow — always runs, even
// under --force) and plans every chain the job's steps could resolve to
// (resolving get versions but running nothing), checking the store for chains
// that already succeeded with identical content so that already-run
// get/task work can be skipped entirely. put steps are never skipped — see
// runSteps. skipCache (--force) bypasses only the chain-skip planning and
// re-runs everything, though results are still recorded as usual.
func RunJob(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, provider workspace.Provider, st *store.Store, skipCache bool) error {
	slog.Info("job.run", "job", job.Name, "steps", len(job.Plan))

	err := workspace.ValidateArtifactFlow(cfg, job)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}

	if cfg.UsesImages() {
		err = shell.ValidateDocker(ctx)
		if err != nil {
			return fmt.Errorf("job %q: image: configured but docker is unavailable: %w", job.Name, err)
		}
	}

	bw, err := provider.NewBuild(ctx, job.Name)
	if err != nil {
		return fmt.Errorf("job %q: %w", job.Name, err)
	}
	defer workspace.CloseBuild(bw, job.Name)

	// Carry an execution log through this invocation so a job's assert.execution
	// can self-verify what ran (plan steps and hooks). The dispatch points and
	// runHookStep append to it; nothing outside pipeline touches it.
	log := &execLog{}
	ctx = withExecLog(ctx, log)

	// Everything from here on has a workspace to run job-level hooks in, so
	// funnel planning and execution into one outcome and dispatch the job's
	// hooks around it. Pre-workspace failures above fire no hooks — the build
	// never started (matching Concourse). Job hooks fire on every invocation,
	// cached or not; they are never hashed or skipped.
	runErr := runJobPlan(ctx, cfg, job, pinned, provider, bw, st, skipCache)

	scope := hookScope{cfg: cfg, jobName: job.Name, label: fmt.Sprintf("job %q", job.Name), bw: bw}

	finalErr := runHooks(ctx, scope, job.Hooks, runErr)

	// A job assert.execution is the final word: it runs after hooks so the log
	// includes them. A match clears whatever the plan/hooks produced (so a
	// fixture of deliberately-failing tasks can be green); a mismatch fails the
	// job regardless, and is never itself cleared.
	if job.Assert != nil && len(job.Assert.Execution) > 0 {
		assertErr := checkExecution(fmt.Sprintf("job %q", job.Name), job.Assert.Execution, log.snapshot())
		if assertErr != nil {
			return assertErr
		}

		finalErr = nil
	}

	if finalErr != nil {
		return finalErr
	}

	slog.Info("job.done", "job", job.Name)

	return nil
}

// runJobPlan plans (unless skipCache) and runs a job's steps, returning the
// aggregate outcome that job-level hooks dispatch on. A planning failure
// classifies as errored (job on_error); a step failure carries whatever
// classification its producing site marked it with.
func runJobPlan(ctx context.Context, cfg *config.Config, job *config.Job, pinned map[string]string, provider workspace.Provider, bw workspace.BuildWorkspace, st *store.Store, skipCache bool) error {
	skippable := map[string]bool{}

	// cache is scoped to this one RunJob invocation (never shared across
	// concurrent invocations — see resource.NewCache) and threaded into both
	// the plan-time and run-time get-step resolution below, so a get step's
	// check command runs at most once per job run instead of once during
	// planning and again during execution.
	cache := rsrc.NewCache()

	if !skipCache {
		chains, err := merkle.PlanChains(ctx, cfg, job.Name, job.Plan, pinned, cache)
		if err != nil {
			return fmt.Errorf("job %q: planning: %w", job.Name, err)
		}

		skippable, err = buildSkippableIndex(ctx, st, job.Name, chains)
		if err != nil {
			return fmt.Errorf("job %q: %w", job.Name, err)
		}
	}

	return runSteps(ctx, cfg, job.Name, job.Plan, pinned, provider, bw, st, skippable, "", false, cache)
}

// computeChainSkippable reports, per chain, whether it's already covered by a
// prior succeeded job_runs row — batched into one query (per
// Store.HasSucceededBatch) instead of one per chain. An Unskippable chain is
// never even asked about; it stays false.
func computeChainSkippable(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) ([]bool, error) {
	chainSkippable := make([]bool, len(chains))

	toCheck := make([]string, 0, len(chains))

	for _, chain := range chains {
		if !chain.Unskippable {
			toCheck = append(toCheck, chain.RootHash)
		}
	}

	succeeded, err := st.HasSucceededBatch(ctx, jobName, toCheck)
	if err != nil {
		return nil, fmt.Errorf("has succeeded batch: %w", err)
	}

	for i, chain := range chains {
		if !chain.Unskippable {
			chainSkippable[i] = succeeded[chain.RootHash]
		}
	}

	return chainSkippable, nil
}

// buildSkippableIndex returns, for every node hash reachable across chains,
// whether every leaf merkle.Chain passing through it is already covered by a
// prior succeeded job_runs row. Any Unskippable chain (contains a put or
// agent step) is forced non-skippable everywhere along it — those steps
// (and everything feeding them) must always run. A node hash shared by
// multiple chains is skippable only if ALL chains through it are skippable
// (AND-rollup), which correctly forces get/task ancestors of an
// unskippable branch to execute even if a sibling branch is independently
// skippable.
func buildSkippableIndex(ctx context.Context, st *store.Store, jobName string, chains []merkle.Chain) (map[string]bool, error) {
	chainSkippable, err := computeChainSkippable(ctx, st, jobName, chains)
	if err != nil {
		return nil, fmt.Errorf("job %q: %w", jobName, err)
	}

	nodeChains := map[string][]int{}

	for i, chain := range chains {
		for _, node := range chain.Nodes {
			nodeChains[node.Hash] = append(nodeChains[node.Hash], i)
		}
	}

	skippable := make(map[string]bool, len(nodeChains))

	for hash, idxs := range nodeChains {
		all := true

		for _, idx := range idxs {
			if !chainSkippable[idx] {
				all = false

				break
			}
		}

		skippable[hash] = all
	}

	return skippable, nil
}

// stepDisposition is what happened to one non-get step, distinguishing the
// two very different reasons a step might not have executed.
type stepDisposition int

const (
	// stepRan: the step executed; advance parentHash to its node.
	stepRan stepDisposition = iota
	// stepGuardSkipped: the step's when: guard was false. Only THIS step is
	// skipped — the plan continues with the next one, and parentHash does not
	// advance (no node was produced).
	stepGuardSkipped
	// stepChainSkipped: the step's hash matched an already-succeeded chain, so
	// everything downstream of it also already succeeded. The whole remaining
	// plan is skipped.
	stepChainSkipped
)

// nonGetOutcome is what dispatchNonGetStep/runNonGetStep report about a
// step's routing-relevant outcome, beyond its hash/disposition/error: the
// verdict applyRouting keys on, and — for an agent step only — the verdict's
// note and this step's own run (see agent.PreviousRun), both of which
// runSteps threads into a routed-to successor's Handoff. Its zero value is
// exactly right for task/put steps and any non-stepRan disposition.
type nonGetOutcome struct {
	verdict  string
	note     string
	previous *agent.PreviousRun
}

// runSteps executes steps in order. A `get` step fans out: for each version
// it selects (a single version normally, or every version returned by
// check when version:every is set), that version triggers its own build
// of the remainder of the plan — see runTriggeredBuild. It always
// terminates this loop, since it delegates the rest of the plan to its
// triggered build(s). A `task`/`put`/`agent` step is handled by
// runNonGetStep; `put`/`agent` steps are never looked up in skippable and
// always execute.
func runSteps(
	ctx context.Context, cfg *config.Config, jobName string, steps []config.Step, pinned map[string]string,
	provider workspace.Provider, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool,
	parentHash string, chainUnskippable bool, cache *rsrc.Cache,
) error {
	// visits counts how many times each step index has executed this
	// invocation, bounding a to:-driven backward loop. It's per-runSteps-call,
	// so each triggered build (and each version under get: version: every, which
	// re-enters via a fresh runSteps) gets its own independent max_visits budget.
	visits := map[int]int{}

	// pending is the Handoff a routed transition builds for whichever step
	// index it targets — consumed (and cleared) the moment that step is next
	// dispatched, whether or not its handoff: actually uses it. nil means "no
	// pending transition into the next step" (a straight fall-through, or the
	// very first step), which is the overwhelmingly common case and costs
	// nothing extra.
	var pending *agent.Handoff

	// A manual index loop (not range) so a to: transition can set the next
	// index — forward to skip ahead, or backward to loop. Without any to:, the
	// default nextIndex of i+1 reproduces today's straight-line behavior exactly.
	for i := 0; i < len(steps); {
		step := steps[i]

		if step.Get != "" {
			return runGetStep(ctx, cfg, jobName, i, step, steps[i+1:], pinned, provider, st, skippable, parentHash, chainUnskippable, cache)
		}

		newParentHash, disposition, no, err := runNonGetStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash, handoffFor(step, pending))

		if disposition == stepRan {
			visits[i]++ // count executions before resolveTransition reads visits[i]
		}

		// A routed transition consumes err (a to.failure route means the job
		// doesn't also fail); exhaustion of a backward loop is a job failure.
		nextIndex, routedKey, err, exhaustedErr := applyRouting(ctx, steps, i, step, disposition, no.verdict, err, visits)
		if exhaustedErr != nil {
			return exhaustedErr // routed to the job's on_failure hook
		}

		if err != nil {
			return err
		}

		if disposition == stepChainSkipped {
			reportChainSkipped(jobName, steps[i+1:])

			return nil
		}

		chainUnskippable, err = foldStepUnskippable(cfg, step, chainUnskippable)
		if err != nil {
			return err
		}

		if disposition == stepGuardSkipped {
			// The transition that landed here (if any) already happened; the
			// guard merely declined to run the step it targeted. Consume it
			// rather than letting a stale pending leak into whatever runs next.
			pending = nil
			i = nextIndex // a guard-skip never routes; nextIndex is still i+1

			continue
		}

		// Only advance parentHash when the step produced a node hash. A FAILED
		// step returns "" — today that never surfaces (a failure returns
		// immediately) but a to.failure route consumes the error and continues,
		// so without this guard the routed target and every failed loop
		// iteration would inherit parentHash="" and collide onto one nodes row.
		// Keeping the incoming parentHash threads each iteration distinctly.
		if newParentHash != "" {
			parentHash = newParentHash
		}

		pending = nextPendingHandoff(jobName, step, steps, routedKey, no, visits, nextIndex)

		i = nextIndex
	}

	return recordChainSucceeded(ctx, st, jobName, parentHash, chainUnskippable)
}

// reportChainSkipped prints one "skip: <name> (chain)" line per step after a
// chain-skip point, matching the "skip: <name> (when)"/"skip: <name>
// (version: ...)" convention used elsewhere, so a cached rerun's transcript
// still names every downstream step instead of going silent after the
// triggering task's own "skip:" line. It only names steps from their config
// — it never resolves a get step's version or does any of the work runSteps
// is skipping, so it can't reintroduce the resource checks caching exists to
// avoid.
func reportChainSkipped(jobName string, steps []config.Step) {
	for _, step := range steps {
		name := executedStepName(step)
		if name == "" {
			name = step.Get // executedStepName covers task/put/agent only
		}

		if name == "" {
			continue
		}

		fmt.Printf("skip: %s (chain)\n", name)
		slog.Info("job.skip", "job", jobName, "step", name, "reason", "chain")
	}
}

// handoffFor returns pending when step's own handoff: enables something —
// the step is what actually consumes carried transition context — and nil
// otherwise, so a step without handoff: never sees a pending value even when
// one exists. Split out of runSteps to keep its cyclomatic complexity down.
func handoffFor(step config.Step, pending *agent.Handoff) *agent.Handoff {
	if step.Handoff != nil && step.Handoff.Enabled() {
		return pending
	}

	return nil
}

// nextPendingHandoff builds the Handoff a just-routed step hands to whichever
// step index it targeted, or nil when the step didn't route (routedKey ==
// ""). Split out of runSteps as a pure function so the carry's construction —
// which fields come from where — is unit-testable without a live agent.
// visits[nextIndex] is read BEFORE runSteps' next iteration would increment
// it, so Visit correctly previews "this will be the Nth execution of
// nextIndex".
func nextPendingHandoff(jobName string, step config.Step, steps []config.Step, routedKey string, no nonGetOutcome, visits map[int]int, nextIndex int) *agent.Handoff {
	if routedKey == "" {
		return nil
	}

	return &agent.Handoff{
		JobName:   jobName,
		FromStep:  executedStepName(step),
		RouteKey:  routedKey,
		Note:      no.note,
		Visit:     visits[nextIndex] + 1,
		MaxVisits: steps[nextIndex].MaxVisits,
		StepIndex: nextIndex,
		PlanLen:   len(steps),
		Previous:  no.previous,
	}
}

// runNonGetStep runs a task/put/agent step and dispatches its hooks around the
// outcome. A skipped (merkle-cached) step fires no hooks — it did not run, so
// there is nothing for its observers to react to. When a green step is failed
// by its own on_success/ensure hook, the true (failed) outcome is recorded so
// the store and skip-cache reflect it.
func runNonGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, handoff *agent.Handoff) (string, stepDisposition, nonGetOutcome, error) {
	hash, disposition, no, err := dispatchNonGetStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash, handoff)

	// Record what ran (not a cached chain, not a guard-skipped step) for a
	// job's assert.execution, before hooks so the order reads
	// [step, its hooks...].
	if disposition == stepRan {
		recordExecution(ctx, executedStepName(step))
	}

	// Neither kind of skip fires hooks: the step did not run, so it has no
	// outcome for its observers to react to.
	if disposition != stepRan || step.Hooks.Empty() {
		return hash, disposition, no, err
	}

	scope := hookScope{cfg: cfg, jobName: jobName, label: stepLabel(i, step), bw: bw}

	final := runHooks(ctx, scope, step.Hooks, err)
	if err == nil && final != nil {
		recCtx := context.WithoutCancel(ctx)
		_ = st.RecordJobRun(recCtx, jobName, hash, string(outcome.Failed), final)

		return "", stepRan, no, final
	}

	return hash, disposition, no, final
}

// dispatchNonGetStep dispatches a task/put/agent step — the three kinds that,
// unlike get, run in place and return a single new parentHash rather than
// fanning out or delegating the remainder of the plan. It first evaluates the
// step's when: guard (see evaluateStepGuard): a false guard skips only this
// step. stepChainSkipped is only ever returned for a cache-matched task step;
// put/agent steps are never chain-skippable. handoff is threaded straight
// into agent.RunStep — see runSteps' pending carry — and ignored by task/put.
func dispatchNonGetStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string, handoff *agent.Handoff) (string, stepDisposition, nonGetOutcome, error) {
	shouldRun, err := evaluateStepGuard(ctx, cfg, step, bw)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (when): %w", i, err)
	}

	if !shouldRun {
		fmt.Printf("skip: %s (when)\n", executedStepName(step))
		slog.Info("job.skip", "job", jobName, "index", i, "reason", "when", "step", executedStepName(step))

		return parentHash, stepGuardSkipped, nonGetOutcome{}, nil
	}

	kind, ok := step.Kind()
	if !ok {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}

	switch kind { //nolint:exhaustive // default covers config.StepKindGet — dispatchNonGetStep is only called for non-get steps
	case config.StepKindTask:
		hash, disposition, err := runTaskStep(ctx, cfg, jobName, i, step, bw, st, skippable, parentHash)

		return hash, disposition, nonGetOutcome{}, err
	case config.StepKindPut:
		hash, err := runPutStep(ctx, cfg, jobName, i, step, bw, st, parentHash)

		return hash, stepRan, nonGetOutcome{}, err
	case config.StepKindAgent:
		stepOut, err := agent.RunStep(ctx, cfg, jobName, i, step, bw, st, parentHash, handoff)
		no := nonGetOutcome{verdict: stepOut.Verdict, note: stepOut.Note, previous: stepOut.Previous}

		if err != nil {
			return "", stepRan, no, fmt.Errorf("agent step: %w", err)
		}

		return stepOut.Hash, stepRan, no, nil
	default: // config.StepKindGet — dispatchNonGetStep is only called for non-get steps
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d: unrecognized step (must be get, task, put, or agent)", i)
	}
}

// recordChainSucceeded records the leaf of a fully-executed chain as
// succeeded, unless it contains a put or agent step (those chains are
// never skippable, so recording job_runs for them would be unused).
func recordChainSucceeded(ctx context.Context, st *store.Store, jobName, rootHash string, chainUnskippable bool) error {
	if chainUnskippable {
		return nil
	}

	err := st.RecordJobRun(ctx, jobName, rootHash, "succeeded", nil)
	if err != nil {
		return fmt.Errorf("job %q: %w", jobName, err)
	}

	return nil
}

// runGetStep resolves and (unless skippable) fetches step's resource
// version(s), then runs the remainder of the plan for each — see
// runTriggeredBuild. It always terminates the calling runSteps loop, since
// a get step delegates the rest of the plan to its triggered build(s).
func runGetStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, remainder []config.Step,
	pinned map[string]string, provider workspace.Provider, st *store.Store, skippable map[string]bool,
	parentHash string, chainUnskippable bool, cache *rsrc.Cache,
) error {
	resource, resourceType, versions, err := cache.ResolveVersionsCached(ctx, cfg, step, pinned)
	if err != nil {
		return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "get", "resource", step.Get, "versions", len(versions))

	for _, version := range versions {
		content, err := merkle.GetNodeContent(cfg, step, *resourceType, resource.Source, version)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		hash, err := merkle.HashNode(merkle.NodeKindGet, content, parentHash)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}

		if skippable[hash] {
			fmt.Printf("skip: %s (version: %v)\n", resource.Name, version)
			slog.Info("job.skip", "job", jobName, "index", i, "kind", "get", "resource", resource.Name, "hash", hash)

			continue
		}

		node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindGet, StepIndex: i, Resource: resource.Name, Content: content}

		err = runTriggeredBuild(ctx, cfg, jobName, i, step, *resource, *resourceType, version, remainder, pinned, provider, st, skippable, node, chainUnskippable, cache)
		if err != nil {
			return fmt.Errorf("step %d (get %q): %w", i, step.Get, err)
		}
	}

	return nil
}

// runTaskStep hashes step against parentHash and, unless that hash is
// skippable, runs it. It returns the hash to use as parentHash for the
// next step (unchanged, along with skipped=true, when skipped).
func runTaskStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, skippable map[string]bool, parentHash string) (string, stepDisposition, error) {
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d: %w", i, err)
	}

	content, err := merkle.TaskNodeContent(cfg, step, rt)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTask, content, parentHash)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	if skippable[hash] {
		fmt.Printf("skip: %s\n", rt.Name)
		slog.Info("job.skip", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "hash", hash)

		return parentHash, stepChainSkipped, nil
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "task", "task", rt.Name, "run", rt.Run)

	fmt.Printf("task: %s\n", rt.Name)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindTask, StepIndex: i, Resource: rt.Name, Content: content}

	err = executeTask(ctx, cfg, rt, bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
		recordStepFailure(ctx, st, node, jobName, wrapped)

		return "", stepRan, wrapped
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return "", stepRan, fmt.Errorf("step %d (task %q): %w", i, rt.Name, err)
	}

	return hash, stepRan, nil
}

// executeTask materializes a task's (isolated or shared) working directory,
// runs its command, and captures its declared outputs — with no merkle/store
// recording. Shared by runTaskStep (which records the aggregate outcome) and
// hook execution (where the enclosing step/job records it).
func executeTask(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, bw workspace.BuildWorkspace) error {
	space, err := bw.TaskSpace(ctx, rt.Name, rt.Inputs, rt.Outputs)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}
	defer workspace.CloseSpace(space, rt.Name)

	err = runTaskCommand(ctx, cfg, rt, space.Dir())
	if err != nil {
		return err
	}

	err = space.Capture(ctx)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	return nil
}

// recordStepFailure records a step's failed node and job_run, classifying the
// outcome (failed vs errored vs aborted) and writing under a detached context
// so an aborted step's outcome still persists rather than being dropped by the
// canceled context. Best-effort: recording errors are ignored so they can't
// mask the original error returned to the caller.
func recordStepFailure(ctx context.Context, st *store.Store, node merkle.Node, jobName string, err error) {
	status := string(outcome.Classify(ctx, err))
	recCtx := context.WithoutCancel(ctx)
	_ = st.RecordNode(recCtx, nodeRecord(node), jobName, status, nil, err)
	_ = st.RecordJobRun(recCtx, jobName, node.Hash, status, err)
}

// runTaskCommand runs a task's run: command. Without a fix:, it streams
// output live and any nonzero exit is a hard failure (unchanged behavior).
// With a fix:, it captures output instead, and on a nonzero exit invokes the
// fix agent — seeded with that output and given the task itself as a rerun
// tool — then re-runs the command once; that re-run's exit code is the
// verdict. A green run never constructs the agent.
func runTaskCommand(ctx context.Context, cfg *config.Config, rt config.ResolvedTask, workspaceDir string) error {
	runner, err := shell.NewRunner(rt.Image, workspaceDir)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	switch {
	case rt.Assert != nil:
		return runAssertedTask(ctx, runner, rt)
	case rt.Fix != nil:
		return runFixTask(ctx, cfg, runner, rt, workspaceDir)
	default:
		err := runner.Run(ctx, rt.Run)
		if err != nil {
			if shell.IsExitError(err) {
				err = outcome.Fail(err)
			}

			return fmt.Errorf("task %q: %w", rt.Name, err)
		}

		return nil
	}
}

// runFixTask runs a task with a fix: agent: capture the output, and on a
// nonzero exit invoke the fix agent (seeded with that output and given the
// task itself as a rerun tool), then re-run the command once — that re-run's
// exit code is the verdict. A green first run never constructs the agent.
func runFixTask(ctx context.Context, cfg *config.Config, runner shell.Runner, rt config.ResolvedTask, workspaceDir string) error {
	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	// RunCaptureFull reports even a signal-killed process (e.g. from a
	// canceled ctx) as data, not err — checked before treating exitCode as a
	// genuine verdict, so a run interrupted by shutdown isn't misread as a
	// real failure worth invoking the fix agent over.
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(stdout, stderr)

	if exitCode == 0 {
		return nil
	}

	fmt.Printf("task %q failed (exit %d); invoking fix agent %q\n", rt.Name, exitCode, rt.Fix.Agent)

	err = agent.RunFix(ctx, cfg, rt, taskFailureOutput(stdout, stderr, exitCode), workspaceDir)
	if err != nil {
		return fmt.Errorf("fix agent %q: %w", rt.Fix.Agent, err)
	}

	// Verdict: re-run the command (its run:, not its fix:) and gate on it.
	stdout, stderr, exitCode, err = runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	cancelErr = shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(stdout, stderr)

	if exitCode != 0 {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(fmt.Errorf("still failing after fix agent %q (exit %d)", rt.Fix.Agent, exitCode)))
	}

	return nil
}

// runAssertedTask runs rt.Run capturing its output, then evaluates rt.Assert:
// a matching stdout substring and exit code make the task a success even on a
// non-zero exit; a mismatch is a task-level failure with a got-vs-want reason.
// assert takes over the success determination, so a task's fix: is not
// consulted when an assert is present.
func runAssertedTask(ctx context.Context, runner shell.Runner, rt config.ResolvedTask) error {
	stdout, stderr, exitCode, err := runner.RunCaptureFull(ctx, rt.Run)
	if err != nil {
		return fmt.Errorf("task %q: %w", rt.Name, err)
	}

	// RunCaptureFull reports even a signal-killed process (e.g. from a
	// canceled ctx) as data, not err — checked before evaluating the assert,
	// so a run interrupted by shutdown isn't misread as a genuine mismatch.
	cancelErr := shell.CanceledError(ctx)
	if cancelErr != nil {
		return fmt.Errorf("task %q: %w", rt.Name, cancelErr)
	}

	printTaskOutput(stdout, stderr)

	mismatch := assertMismatch(rt.Assert, stdout, exitCode)
	if mismatch != nil {
		return fmt.Errorf("task %q: %w", rt.Name, outcome.Fail(mismatch))
	}

	return nil
}

// assertMismatch returns a reason when captured stdout/exit code don't satisfy
// assert, or nil when they match. Code is exact; Stdout is a substring test.
func assertMismatch(assert *config.Assert, stdout string, exitCode int) error {
	if assert.Code != nil && *assert.Code != exitCode {
		return fmt.Errorf("assert.code: want %d, got %d", *assert.Code, exitCode)
	}

	if assert.Stdout != nil && !strings.Contains(stdout, *assert.Stdout) {
		return fmt.Errorf("assert.stdout: output does not contain %q", *assert.Stdout)
	}

	return nil
}

// printTaskOutput echoes a captured task run's streams to the terminal, so a
// fix-enabled task's output is still visible (RunShellCaptureFull buffers
// rather than streaming live the way RunShell does).
func printTaskOutput(stdout, stderr string) {
	if stdout != "" {
		fmt.Print(stdout)
	}

	if stderr != "" {
		fmt.Fprint(os.Stderr, stderr)
	}
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

// runPutStep hashes and always runs step (put steps are never skipped),
// returning the hash to use as parentHash for the next step.
func runPutStep(ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, bw workspace.BuildWorkspace, st *store.Store, parentHash string) (string, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	content, err := merkle.PutNodeContent(cfg, step, *resourceType, resource.Source, step.Params, step.Inputs)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindPut, content, parentHash)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	slog.Debug("job.step", "job", jobName, "index", i, "kind", "put", "resource", step.Put)

	fmt.Printf("put: %s\n", step.Put)

	node := merkle.Node{Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindPut, StepIndex: i, Resource: resource.Name, Content: content}

	result, err := executePut(ctx, cfg, step, bw)
	if err != nil {
		wrapped := fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
		recordStepFailure(ctx, st, node, jobName, wrapped)

		return "", wrapped
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", result, nil)
	if err != nil {
		return "", fmt.Errorf("step %d (put %q): %w", i, step.Put, err)
	}

	return hash, nil
}

// executePut materializes a put step's input view, runs its resource's out:
// command, and returns the produced version — with no merkle/store recording.
// Shared by runPutStep (which records) and hook execution (which does not; a
// put hook's result version is discarded). A nonzero out: exit is marked as a
// task-level failure so hook dispatch classifies it as failed; a resource
// lookup or workspace error stays unmarked → errored.
func executePut(ctx context.Context, cfg *config.Config, step config.Step, bw workspace.BuildWorkspace) (map[string]any, error) {
	resource, err := cfg.FindResource(step.Put)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	resourceType, err := cfg.FindResourceType(resource.Type)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	space, err := bw.PutSpace(ctx, step.Put, step.Inputs)
	if err != nil {
		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}
	defer workspace.CloseSpace(space, step.Put)

	result, err := rsrc.RunOut(ctx, cfg, *resourceType, resource.Source, step.Params, space.Dir())
	if err != nil {
		if shell.IsExitError(err) {
			err = outcome.Fail(err)
		}

		return nil, fmt.Errorf("put %q: %w", step.Put, err)
	}

	return result, nil
}

// runTriggeredBuild runs the build that a single resource version triggers:
// per Concourse's model, the version triggering a get is what starts a
// build, and every build gets its own isolated working directory. So this
// creates a fresh workspace for just this version, fetches the version
// into it, runs the remainder of the plan inside it, and tears the
// workspace down afterward — never sharing it with any other triggered
// build, including sibling versions fanned out by version:every.
func runTriggeredBuild(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step, resource config.Resource,
	resourceType config.ResourceType, version map[string]any, remainder []config.Step, pinned map[string]string,
	provider workspace.Provider, st *store.Store, skippable map[string]bool, node merkle.Node,
	chainUnskippable bool, cache *rsrc.Cache,
) error {
	bw, err := provider.NewBuild(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create workspace for %q: %w", resource.Name, err)
	}

	defer workspace.CloseBuild(bw, resource.Name)

	recordExecution(ctx, resource.Name)

	err = fetchGetStep(ctx, cfg, resource, resourceType, version, bw)

	// Get-step hooks fire once per triggered build, in that build's own
	// workspace, observing the fetch outcome. A fetch failure (or a hook that
	// fails an otherwise-green fetch) fails this build.
	if !step.Hooks.Empty() {
		scope := hookScope{cfg: cfg, jobName: jobName, label: stepLabel(i, step), bw: bw}
		err = runHooks(ctx, scope, step.Hooks, err)
	}

	if err != nil {
		recordStepFailure(ctx, st, node, jobName, err)

		return err
	}

	err = st.RecordNode(ctx, nodeRecord(node), jobName, "succeeded", nil, nil)
	if err != nil {
		return fmt.Errorf("could not record node %q: %w", node.Hash, err)
	}

	return runSteps(ctx, cfg, jobName, remainder, pinned, provider, bw, st, skippable, node.Hash, chainUnskippable, cache)
}

// fetchGetStep places one version of a resource into bw's resource
// directory for resource.Name.
func fetchGetStep(ctx context.Context, cfg *config.Config, resource config.Resource, resourceType config.ResourceType, version map[string]any, bw workspace.BuildWorkspace) error {
	fmt.Printf("get: %s (version: %v)\n", resource.Name, version)

	destDir, err := bw.ResourceDir(ctx, resource.Name)
	if err != nil {
		return fmt.Errorf("could not create resource dir for %q: %w", resource.Name, err)
	}

	err = rsrc.RunIn(ctx, cfg, resourceType, resource.Source, version, destDir)
	if err != nil {
		if shell.IsExitError(err) {
			err = outcome.Fail(err)
		}

		return fmt.Errorf("could not fetch resource %q: %w", resource.Name, err)
	}

	return nil
}
