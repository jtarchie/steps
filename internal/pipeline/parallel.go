package pipeline

// The in_parallel: branch runner.
//
// A plan is otherwise strictly sequential, so independent work waits on itself:
// three downloads run one at a time, and one slow resource check stalls
// everything behind it. This is also the keystone the aggregation policies
// build on — across:, ensemble: and race: are all "run these branches, decide
// what their outcomes mean".

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// branchResult is one branch's outcome, kept with its index so the report
// reads in declaration order however the branches actually finished.
type branchResult struct {
	index int
	name  string
	err   error
}

// runParallelStep runs an in_parallel: block's branches concurrently and
// reports the block's outcome.
//
// The block FAILS when any branch fails. fail_fast decides only whether the
// siblings are cancelled or allowed to finish — never whether the failure
// counts. That distinction is the defect this step shipped with the first time
// it was written: with fail_fast: false a child failure was swallowed and a job
// containing a failing parallel step reported PASS.
func runParallelStep(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, parentHash string, handoff *agent.Handoff,
) (string, stepDisposition, nonGetOutcome, error) {
	content, err := merkle.ParallelNodeContent(cfg, step)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindParallel, content, parentHash)
	if err != nil {
		return "", stepRan, nonGetOutcome{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	branches := step.InParallel.Steps

	fmt.Printf("in_parallel: %d branches%s\n", len(branches), limitSuffix(step.InParallel.Limit))
	slog.Debug("job.step", "job", jobName, "index", i, "kind", "in_parallel", "branches", len(branches))

	results := runBranches(ctx, cfg, jobName, i, step, bw, st, hash, handoff)
	blockErr := combineBranchErrors(ctx, results)

	status := "succeeded"
	if blockErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindParallel,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), jobName, status, nil, blockErr)

	return hash, stepRan, nonGetOutcome{}, blockErr
}

// runBranches executes every branch, bounded by limit, and collects one result
// per branch.
//
// Each branch chains under the BLOCK's hash rather than under its siblings:
// concurrent branches have no order between them, so there is no sequence for
// one to be the parent of another. Caching is off inside the block (nil
// skippable) — a branch's own steps still record their nodes, but the block
// decides nothing about skipping.
func runBranches(
	ctx context.Context, cfg *config.Config, jobName string, i int, step config.Step,
	bw workspace.BuildWorkspace, st *store.Store, blockHash string, handoff *agent.Handoff,
) []branchResult {
	branches := step.InParallel.Steps
	results := make([]branchResult, len(branches))

	// fail_fast cancels the siblings of a failing branch. Derived from ctx so
	// an outer cancellation (a job abort) still reaches every branch.
	branchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		slot = newLimiter(step.InParallel.Limit, len(branches))
		logs = make([]*execLog, len(branches))
	)

	// Slots are acquired HERE, in the parent, rather than inside each
	// goroutine. That makes branches start in declaration order — under
	// `limit:` especially, "which two go first" is otherwise whichever
	// goroutines the scheduler happened to run, which is nothing a pipeline
	// author can reason about.
	for index := range branches {
		slot.acquire()

		// Under fail_fast a branch that has not started yet never should.
		if branchCtx.Err() != nil {
			slot.release()

			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer slot.release()

			branch := branches[index]
			results[index] = branchResult{index: index, name: executedStepName(branch)}

			// Each branch records into its own log, merged back in
			// declaration order below — branches finish in whatever order
			// they finish, and recording as they go would make
			// assert.execution nondeterministic.
			runCtx, branchLog := forkExecLog(branchCtx)
			logs[index] = branchLog

			_, _, _, err := runNonGetStep(runCtx, cfg, jobName, i, branch, bw, st, nil, blockHash, handoff)
			results[index].err = err

			if err != nil && step.InParallel.FailsFast() {
				cancel()
			}
		}()
	}

	wg.Wait()

	for _, branchLog := range logs {
		if branchLog != nil {
			mergeExecLog(ctx, branchLog)
		}
	}

	return results
}

// combineBranchErrors turns the branches' outcomes into the block's own.
//
// Every failure is reported, not just the first: a reader debugging a parallel
// block wants to know whether one branch broke or all of them did, and a
// truncated report hides exactly that. The block's classification follows the
// worst branch — an errored branch (infrastructure) outranks a failed one (the
// step said no), so on_error fires rather than on_failure.
func combineBranchErrors(ctx context.Context, results []branchResult) error {
	var (
		failures []error
		errored  bool
	)

	for _, result := range results {
		if result.err == nil {
			continue
		}

		failures = append(failures, fmt.Errorf("branch %d (%s): %w", result.index, result.name, result.err))

		if outcome.Classify(ctx, result.err) != outcome.Failed {
			errored = true
		}
	}

	if len(failures) == 0 {
		return nil
	}

	joined := errors.Join(failures...)
	if errored {
		return joined
	}

	// Every failure was a step-level one, so the block is a step-level failure
	// too and on_failure is the right hook.
	return outcome.Fail(joined) //nolint:wrapcheck // Fail only marks the classification; the joined error is already this package's
}

// limiter bounds how many branches are in flight. A zero or oversized limit
// means unbounded — the plan author asked for parallelism, so making them also
// choose a width would be a second decision for the common case.
type limiter struct{ slots chan struct{} }

func newLimiter(limit, branches int) *limiter {
	if limit <= 0 || limit >= branches {
		return &limiter{}
	}

	return &limiter{slots: make(chan struct{}, limit)}
}

func (l *limiter) acquire() {
	if l.slots != nil {
		l.slots <- struct{}{}
	}
}

func (l *limiter) release() {
	if l.slots != nil {
		<-l.slots
	}
}

func limitSuffix(limit int) string {
	if limit <= 0 {
		return ""
	}

	return fmt.Sprintf(" (limit %d)", limit)
}
