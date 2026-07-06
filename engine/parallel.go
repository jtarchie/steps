package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

// runParallel forks the run into concurrent branches and joins them at a
// barrier. Each branch is a hermetic CHILD RUN of states from the same machine,
// driven by the unchanged single-cursor loop — so the parent's journal stays
// single-cursor (the fork is one state_entered + one handler_finished, shaped
// like a foreach state). Branch outputs are aggregated by label into
// {label: exitOutput, ...} (+ _failures under onBranchFailure: "collect"); the
// join is the fork state's successor, which reads the aggregate from flat scope.
//
// v1 runs branches SERIALLY (bounded concurrency lands in a later increment);
// crash-resume idempotency (reusing already-spawned children) lands with the
// ForkStarted journal event.
func (e *Engine) runParallel(ctx context.Context, m *machine.Machine, st *machine.State, runID string, rs *journal.RunState) (*HandlerResult, error) {
	// The pre-fork scope snapshot every branch sees — hermetic, like a foreach
	// item: no branch sees a sibling, only what existed before the fork.
	snapshot := cloneCtx(rs.Ctx)

	children, err := e.forkChildren(ctx, st, runID, rs)
	if err != nil {
		return nil, err
	}

	// Fork deadline: branches cannot outlive the run's wall-clock budget. The
	// parent loop checks limits.timeout only between states, but it is BLOCKED
	// here for the whole fork — without this, a hung branch runs unbounded.
	forkCtx, cancel := context.WithDeadline(ctx, rs.Started.Add(m.Limits.Timeout))
	defer cancel()

	results := e.runBranches(forkCtx, m, st, runID, children, snapshot, cancel)
	return e.aggregateParallel(st, results)
}

// runBranches drives the fork's children with bounded concurrency (serial under
// mock so scripted queues stay deterministic — the foreach rule). Under the
// "fail" policy, the first failing branch cancels its siblings' in-flight work
// via the shared fork context; "collect" lets them all run to completion.
func (e *Engine) runBranches(ctx context.Context, m *machine.Machine, st *machine.State, runID string, children []journal.ChildRef, snapshot map[string]any, cancel context.CancelFunc) []branchOutcome {
	results := make([]branchOutcome, len(children))
	failFast := st.Parallel.OnBranchFailure != "collect"

	drive := func(i int, c journal.ChildRef) {
		results[i] = e.runBranch(ctx, m, runID, c, snapshot)
		if failFast && results[i].err != nil {
			cancel() // stop siblings' in-flight LLM calls
		}
	}

	concurrency := st.Parallel.Concurrency
	if concurrency < 1 || e.Mock != nil {
		concurrency = 1
	}
	if concurrency == 1 {
		for i, c := range children {
			drive(i, c)
		}
		return results
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	for i, c := range children {
		wg.Add(1)
		go func(i int, c journal.ChildRef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			drive(i, c)
		}(i, c)
	}
	wg.Wait()
	return results
}

// forkChildren returns the fork's pinned branch children, minting them on first
// execution and reusing them on resume. The ordering is the idempotency rule:
// the parent's intent to fork (fork_started, carrying the child run IDs) is
// durable BEFORE any child row exists, so a crash between mint and create
// reuses the same IDs on resume — no orphans, no duplicate side effects.
func (e *Engine) forkChildren(ctx context.Context, st *machine.State, runID string, rs *journal.RunState) ([]journal.ChildRef, error) {
	key := journal.ForkKey(st.Name, rs.Visits[st.Name])
	if existing := rs.Forks[key]; len(existing) > 0 {
		return existing, nil // resume: reattach to the pinned children
	}

	children := make([]journal.ChildRef, len(st.Parallel.Branches))
	for i, b := range st.Parallel.Branches {
		children[i] = journal.ChildRef{Label: b.Label, RunID: newRunID(), Entry: b.Entry}
	}
	err := e.append(ctx, runID, journal.ForkStarted, map[string]any{
		"state":    st.Name,
		"visit":    rs.Visits[st.Name],
		"children": children,
	})
	if err != nil {
		return nil, err
	}
	if rs.Forks == nil {
		// Start builds RunState in-memory; only Fold pre-seeds this map.
		rs.Forks = map[string][]journal.ChildRef{}
	}
	rs.Forks[key] = children
	return children, nil
}

// branchOutcome is one branch's result: its label, exit output, cumulative
// usage, and terminal error (nil on success).
type branchOutcome struct {
	label  string
	output map[string]any
	usage  journal.Usage
	err    error
}

// runBranch drives a single branch's child run to a terminal, creating and
// seeding it on first execution and reattaching to it on resume. Creation is
// idempotent: a child that already exists (from a pre-crash attempt) is driven,
// not recreated — b1-done returns instantly, b2-mid-agent resumes, b3-unstarted
// runs fresh.
func (e *Engine) runBranch(ctx context.Context, m *machine.Machine, parentID string, c journal.ChildRef, snapshot map[string]any) branchOutcome {
	existing, _ := e.Store.GetRun(ctx, c.RunID)
	if existing == nil {
		err := e.Store.CreateRun(ctx, &journal.Run{
			ID:           c.RunID,
			Machine:      m.Name,
			Hash:         m.Hash,
			Source:       m.Source,
			Assets:       m.Assets,
			Status:       journal.StatusRunning,
			CurrentState: c.Entry,
			ParentRunID:  parentID,
		})
		if err != nil {
			return branchOutcome{label: c.Label, err: fmt.Errorf("branch %q: creating child run: %w", c.Label, err)}
		}
		err = e.append(ctx, c.RunID, journal.RunStarted, map[string]any{
			"machine":      m.Name,
			"machine_hash": m.Hash,
			"input":        snapshot,
			"initial":      c.Entry,
		})
		if err != nil {
			return branchOutcome{label: c.Label, err: fmt.Errorf("branch %q: %w", c.Label, err)}
		}
	}

	res, err := e.driveRun(ctx, m, c.RunID)
	if err != nil {
		// A cancelled/errored branch must not linger as a running zombie.
		// Best-effort with cancellation stripped (the fork's ctx may be cancelled).
		_ = e.Store.UpdateRun(context.WithoutCancel(ctx), c.RunID, journal.StatusFailed, c.Entry)
		return branchOutcome{label: c.Label, err: fmt.Errorf("branch %q: %w", c.Label, err)}
	}
	switch res.Status {
	case journal.StatusFailed:
		usage := journal.Usage{}
		if res.State != nil {
			usage = res.State.Usage
		}
		return branchOutcome{label: c.Label, usage: usage, err: fmt.Errorf("branch %q failed at %q", c.Label, res.Terminal)}
	case journal.StatusParked:
		// Validation forbids gates in a branch; a park here is a broken machine.
		return branchOutcome{label: c.Label, err: fmt.Errorf("branch %q parked — a branch cannot park", c.Label)}
	}

	out, err := e.branchExitOutput(ctx, c.RunID)
	if err != nil {
		return branchOutcome{label: c.Label, err: fmt.Errorf("branch %q: %w", c.Label, err)}
	}
	usage := journal.Usage{}
	if res.State != nil {
		usage = res.State.Usage
	}
	return branchOutcome{label: c.Label, output: out, usage: usage}
}

// branchExitOutput reads a finished branch's result: the output of the last
// state to conclude before the terminal (its exit). One re-read of the child
// journal keeps the exit rule ("the state that routed to done") in one place.
func (e *Engine) branchExitOutput(ctx context.Context, childID string) (map[string]any, error) {
	events, err := e.Store.Events(ctx, childID)
	if err != nil {
		return nil, fmt.Errorf("reading branch journal: %w", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != journal.HandlerFinished {
			continue
		}
		if out, ok := events[i].Data["output"].(map[string]any); ok {
			return out, nil
		}
		return map[string]any{}, nil
	}
	return map[string]any{}, nil
}

// aggregateParallel folds branch outcomes into the fork's handler result. Under
// "fail" the first failed branch fails the fork (routed by the fork state's
// catch:); under "collect" every branch runs and failures land in _failures for
// the join to guard on. Mirrors aggregateForEachResults' fail/skip split.
func (e *Engine) aggregateParallel(st *machine.State, results []branchOutcome) (*HandlerResult, error) {
	output := map[string]any{}
	var usage journal.Usage
	var failures []any
	for _, r := range results {
		usage.Add(r.usage)
		if r.err != nil {
			if st.Parallel.OnBranchFailure != "collect" {
				return nil, r.err
			}
			e.Listener.Warn("parallel branch failed", "state", st.Name, "branch", r.label, "error", r.err.Error())
			failures = append(failures, map[string]any{"label": r.label, "error": r.err.Error()})
			continue
		}
		output[r.label] = r.output
	}
	if len(failures) > 0 {
		output["_failures"] = failures
	}
	return &HandlerResult{Output: output, Usage: usage}, nil
}

// driveRun drives a child run from wherever its journal left off to a terminal
// (or park). It is the single-cursor loop applied to a sub-run: a fresh branch
// child (journal is just run_started) runs from its entry; a resumed child
// continues from its own InFlight. Side effects are journaled and never
// replayed — the branch inherits every durability guarantee for free.
func (e *Engine) driveRun(ctx context.Context, m *machine.Machine, childID string) (*Result, error) {
	events, err := e.Store.Events(ctx, childID)
	if err != nil {
		return nil, fmt.Errorf("loading branch journal for %s: %w", childID, err)
	}
	rs := journal.Fold(events)
	if rs.Finished {
		return &Result{RunID: childID, Status: rs.Status, Terminal: rs.Current, State: rs}, nil
	}
	if rs.Started.IsZero() {
		rs.Started = time.Now()
	}
	err = e.Store.UpdateRun(ctx, childID, journal.StatusRunning, rs.Current)
	if err != nil {
		return nil, fmt.Errorf("updating branch run %s: %w", childID, err)
	}
	return e.loop(ctx, m, childID, rs, rs.InFlight, nil)
}

// cloneCtx shallow-copies a run's ctx map: branches only append their own
// outputs, so a top-level copy isolates the parent from a branch's writes.
func cloneCtx(ctx map[string]any) map[string]any {
	out := make(map[string]any, len(ctx))
	for k, v := range ctx {
		out[k] = v
	}
	return out
}
