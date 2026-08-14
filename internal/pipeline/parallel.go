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

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/outcome"
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
func runParallelStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	content, err := merkle.ParallelNodeContent(r.cfg, step)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindParallel, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (in_parallel): %w", i, err)
	}

	branches := step.InParallel.Steps

	fmt.Printf("in_parallel: %d branches%s\n", len(branches), limitSuffix(step.InParallel.Limit))
	slog.Debug("job.step", "job", r.jobName, "index", i, "kind", "in_parallel", "branches", len(branches))

	results := runBranches(ctx, r, i, step, hash)
	blockErr := combineBranchErrors(ctx, results)

	status := "succeeded"
	if blockErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindParallel,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, status, nil, blockErr)

	return ran(hash), blockErr
}

// runBranches executes every branch, bounded by limit, and collects one result
// per branch.
//
// Each branch chains under the BLOCK's hash rather than under its siblings:
// concurrent branches have no order between them, so there is no sequence for
// one to be the parent of another. Caching is off inside the block (nil
// skippable) — a branch's own steps still record their nodes, but the block
// decides nothing about skipping.
func runBranches(ctx context.Context, r stepRunner, i int, step config.Step, blockHash string) []branchResult {
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
	// Identity is stamped here, in the parent, rather than inside the
	// goroutine: under fail_fast a branch may never start, and a zero-value
	// result would then wrongly claim index 0. Every branch now carries its
	// own index whether or not it ran.
	for index := range branches {
		results[index] = branchResult{index: index, name: executedStepName(branches[index])}
	}

	for index := range branches {
		slot.acquire()

		// Under fail_fast a branch that has not started yet never should.
		//
		// Nor should one the job no longer has time for: the plan walk checks
		// the deadline between STEPS, and this whole block is one of them, so
		// without asking here a job timeout: cannot bound a parallel fan-out
		// at all — the same hole across: had, closed the same way and with the
		// same helper. Asked after acquiring, so the answer reflects branches
		// that have actually finished.
		if branchCtx.Err() != nil || deadlineStopsFanOut(ctx, r.jobName, "in_parallel", "branches", index, len(branches)) {
			slot.release()

			break
		}

		wg.Add(1)

		go func() {
			defer wg.Done()
			defer slot.release()

			branch := branches[index]

			// Each branch records into its own log, merged back in
			// declaration order below — branches finish in whatever order
			// they finish, and recording as they go would make
			// assert.execution nondeterministic.
			runCtx, branchLog := forkExecLog(branchCtx)
			logs[index] = branchLog

			_, err := runNonGetStep(runCtx, r, i, branch, nil, blockHash)

			// A try: branch tolerates its own failure HERE, because the plan
			// walk never sees a branch — executeNonGetStep is where every
			// other try: is tolerated, and runNonGetStep is as far up as a
			// branch goes. Without this the identical wrapper was tolerated
			// in a serial plan and propagated inside a block, which is the
			// case try: is most often reached for: a best-effort notification
			// running alongside the work it reports on.
			//
			// Before the fail_fast check, so a tolerated failure does not
			// cancel its siblings either. It is not a failure any more.
			err = tolerateTryFailure(runCtx, r.jobName, branch, err)

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

// runRaceStep runs a race: block's branches concurrently and keeps whichever
// completes successfully first, cancelling the rest.
//
// "Successfully" means completed without error — NOT a judgment about output
// quality. A fast but mediocre answer still wins; gating on quality is a
// downstream assert:/verdicts: concern, and folding it in here would make the
// race's outcome depend on something no branch can observe about itself.
//
// Losing branches are cancelled, which stops only FUTURE work: a loser that
// already had a real-world side effect keeps it. That is why race: requires
// workspace isolation and is documented as safe for read/generate-only agents.
func runRaceStep(ctx context.Context, r stepRunner, i int, step config.Step, parentHash string) (stepResult, error) {
	content, err := merkle.RaceNodeContent(r.cfg, step)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindRace, content, parentHash)
	if err != nil {
		return stepResult{}, fmt.Errorf("step %d (race): %w", i, err)
	}

	branches := step.Race.Steps

	fmt.Printf("race: %d branches\n", len(branches))
	slog.Debug("job.step", "job", r.jobName, "index", i, "kind", "race", "branches", len(branches))

	winner, results := raceBranches(ctx, r, i, branches, hash)
	raceErr := raceOutcome(ctx, winner, results)

	status := "succeeded"
	if raceErr != nil {
		status = "failed"
	}

	node := merkle.Node{
		Hash: hash, ParentHash: parentHash, Kind: merkle.NodeKindRace,
		StepIndex: i, Resource: executedStepName(step), Content: content,
	}
	_ = r.st.RecordNode(context.WithoutCancel(ctx), nodeRecord(node), r.jobName, status, nil, raceErr)

	return ran(hash), raceErr
}

// raceBranches starts every branch and returns the index of the first to
// succeed (-1 if none did) along with every branch's result.
func raceBranches(ctx context.Context, r stepRunner, i int, branches []config.Step, blockHash string) (int, []branchResult) {
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winner  = -1
		results = make([]branchResult, len(branches))
		logs    = make([]*execLog, len(branches))
	)

	for index := range branches {
		wg.Add(1)

		go func() {
			defer wg.Done()

			branch := branches[index]
			results[index] = branchResult{index: index, name: executedStepName(branch)}

			runCtx, branchLog := forkExecLog(raceCtx)
			logs[index] = branchLog

			_, err := runNonGetStep(runCtx, r, i, branch, nil, blockHash)
			results[index].err = err

			if err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			// First success wins; a later one changes nothing, and cancelling
			// again is harmless.
			if winner < 0 {
				winner = index
			}

			cancel()
		}()
	}

	wg.Wait()

	// Only the winner's execution is recorded. A cancelled loser's partial
	// work is not something a fixture should have to predict, and its
	// artifacts are discarded.
	if winner >= 0 && logs[winner] != nil {
		mergeExecLog(ctx, logs[winner])
	}

	return winner, results
}

// raceOutcome reports the block's error: none when some branch won, and every
// branch's failure when none did.
func raceOutcome(ctx context.Context, winner int, results []branchResult) error {
	if winner >= 0 {
		return nil
	}

	return combineBranchErrors(ctx, results)
}
