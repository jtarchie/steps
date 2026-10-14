package storetest

// agent_usage: what a step's model calls cost, accumulated per attempt.

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestRunCostTotalsKeepsUnpricedApartFromFree: a run whose every step went
// unpriced has no dollar total at all, and one with some priced steps says
// how many were not.
func (s suite) TestRunCostTotalsKeepsUnpricedApartFromFree(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")
	priced := 0.25

	for _, spend := range []struct {
		run, hash string
		cost      *float64
		tokens    int
	}{
		{"mixed", hashOf(1), &priced, 100},
		{"mixed", hashOf(2), nil, 50},
		{"unpriced", hashOf(3), nil, 10},
	} {
		ensureRun(ctx, t, st, spend.run, "review")
		mustRecordNode(t, st, "review", spend.hash)

		err := st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: spend.run, JobName: "review", NodeHash: spend.hash, Total: spend.tokens, CostUSD: spend.cost,
		})
		if err != nil {
			t.Fatalf("RecordAgentUsage: %v", err)
		}
	}

	totals, err := st.RunCostTotals(ctx, 0)
	if err != nil || len(totals) != 2 {
		t.Fatalf("RunCostTotals = %+v, %v; want two runs", totals, err)
	}

	// Newest first: the unpriced run's usage was recorded last.
	assertTotals(t, totals[0], store.RunTotals{RunID: "unpriced", Tokens: 10, Steps: 1, Unpriced: 1})
	assertTotals(t, totals[1], store.RunTotals{RunID: "mixed", Tokens: 150, Steps: 2, Unpriced: 1, CostUSD: &priced})
}

func assertTotals(t *testing.T, got, want store.RunTotals) {
	t.Helper()

	gotCost, wantCost := got.CostUSD, want.CostUSD
	got.CostUSD, want.CostUSD = nil, nil

	if got != want || (gotCost == nil) != (wantCost == nil) || (gotCost != nil && *gotCost != *wantCost) {
		t.Errorf("run totals %+v (cost %v); want %+v (cost %v) — an unpriced run has no cost at all, not $0", got, gotCost, want, wantCost)
	}
}

// TestAgentUsageAccumulatesAcrossAttempts pins agent_usage as a LEDGER.
//
// A step can execute more than once against the same node hash — a to: route
// sending the plan back over it, or a resumed run re-running the step its
// predecessor died on. Replacing the counts reported the last attempt as the
// whole bill, so a resumed run seeded its budget from a fraction of what it
// had really spent and bought most of the allowance a second time.
func (s suite) TestAgentUsageAccumulatesAcrossAttempts(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := t.Context()

	mustRecordNode(t, st, "j", "same-hash")

	// agent_usage.run_id references runs(id) as well, so the run has to exist —
	// which in production it always does: usage is recorded from inside a step
	// of a run that StartRun already filed.
	err := st.StartRun(ctx, store.NewRun{ID: "r1", JobName: "j", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	for range 4 {
		err = st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: "r1", StepIndex: 0, StepName: "writer", JobName: "j",
			NodeHash: "same-hash", Prompt: 150, Completion: 50, Total: 200,
			ModelServed: "m", FinishReason: "stop",
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	spent, err := st.RunTokensSpent(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}

	if spent != 800 {
		t.Errorf("RunTokensSpent = %d after four 200-token attempts, want 800", spent)
	}

	// A different step of the same run is its own row, not folded into the one
	// above — the ledger is per node, summed per run.
	mustRecordNode(t, st, "j", "other-hash")

	err = st.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: "r1", StepIndex: 1, StepName: "editor", JobName: "j",
		NodeHash: "other-hash", Total: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.RunUsage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per node hash)", len(rows))
	}

	assertAccumulatedRow(t, rows[0])
}

// TestAgentUsageCostSurvivesAnUnpricedAttempt pins the NULL half of the ledger.
//
// cost_usd is nullable because unpriced and free are different answers, and the
// accumulation was a plain sum — so one attempt reporting a dollar figure and a
// second reporting none left the row NULL, discarding a cost that HAD been
// reported. The direction matters: it under-reports spend, on exactly the runs
// that retried.
func (s suite) TestAgentUsageCostSurvivesAnUnpricedAttempt(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := t.Context()

	mustRecordNode(t, st, "j", "node")

	err := st.StartRun(ctx, store.NewRun{ID: "r1", JobName: "j", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	priced := 0.25

	for _, cost := range []*float64{&priced, nil} {
		err = st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: "r1", StepName: "writer", JobName: "j", NodeHash: "node",
			Total: 100, CostUSD: cost,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.RunUsage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	if rows[0].CostUSD == nil {
		t.Fatal("cost_usd is NULL after a priced attempt and an unpriced one; the reported cost was erased")
	}

	if *rows[0].CostUSD != priced {
		t.Errorf("cost_usd = %v, want %v — an unpriced attempt must add nothing, not erase", *rows[0].CostUSD, priced)
	}
}

// TestAgentUsageNeverPricedStaysNull is the other half of the same distinction:
// a step no provider priced must read as unpriced, not as free. The fix for the
// erasure above would be a COALESCE to zero, which reports every unpriced run as
// $0.00 — see RunCostTotals, which counts unpriced steps precisely so a partial
// dollar total can be shown AS partial.
func (s suite) TestAgentUsageNeverPricedStaysNull(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")

	ctx := t.Context()

	mustRecordNode(t, st, "j", "node")

	err := st.StartRun(ctx, store.NewRun{ID: "r1", JobName: "j", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		err = st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: "r1", StepName: "editor", JobName: "j", NodeHash: "node", Total: 50,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	rows, err := st.RunUsage(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}

	if rows[0].CostUSD != nil {
		t.Errorf("a step nothing ever priced reports %v, want NULL", *rows[0].CostUSD)
	}
}

// assertAccumulatedRow checks the counts summed and the descriptive fields —
// which cannot be summed — took the newest attempt's value.
func assertAccumulatedRow(t *testing.T, row store.AgentUsage) {
	t.Helper()

	if row.Total != 800 || row.Prompt != 600 || row.Completion != 200 {
		t.Errorf("accumulated row = total %d prompt %d completion %d, want 800/600/200",
			row.Total, row.Prompt, row.Completion)
	}

	if row.FinishReason != "stop" || row.ModelServed != "m" {
		t.Errorf("descriptive fields = %q/%q, want the last attempt's", row.FinishReason, row.ModelServed)
	}
}

// ensureRun records the run these tests hang their rows on, reusing one that
// is already there.
//
// runs.id is a single GLOBAL primary key and StartRun refuses an id some run
// already holds, so a cross-pipeline collision cannot be built by starting the
// same run twice — that is the whole point of ErrRunExists. What CAN still
// exist is the shape these tests are about: one run row, and a second pipeline
// writing its own child rows against that run id. The child tables are what
// have to survive it.
func ensureRun(ctx context.Context, t *testing.T, st store.Store, runID, jobName string) {
	t.Helper()

	err := st.StartRun(ctx, store.NewRun{ID: runID, JobName: jobName, Workspace: "/tmp/ws"})
	if err != nil && !errors.Is(err, store.ErrRunExists) {
		t.Fatalf("StartRun: %v", err)
	}
}

// recordSpend records one agent step's node and what it spent.
func recordSpend(ctx context.Context, t *testing.T, st store.Store, runID, jobName, stepName string, tokens int) {
	t.Helper()

	err := st.RecordNode(ctx, store.NodeRecord{
		Hash: hashOf(7), Kind: "agent", Resource: "review",
		Content: map[string]any{"prompt": "identical in both pipelines"},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	ensureRun(ctx, t, st, runID, jobName)

	err = st.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: runID, StepIndex: 0, StepName: stepName, JobName: jobName,
		NodeHash: hashOf(7), ModelReq: "haiku",
		Prompt: tokens, Total: tokens, FinishReason: "stop",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}
}

func onlyUsage(ctx context.Context, t *testing.T, st store.Store, runID string) store.AgentUsage {
	t.Helper()

	rows, err := st.RunUsage(ctx, runID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("run %s reported %d usage rows, want 1: %+v", runID, len(rows), rows)
	}

	return rows[0]
}

// TestAgentUsageRecordsNodelessSpend: a hook's agent and a task's fix: agent
// spend inside a job and have no node. Each execution is its own row — there
// is no node to say two of them were one invocation — and every read counts
// them: the per-run rollup, the breakdown, and the ledger a resumed run seeds
// its job budget from.
func (s suite) TestAgentUsageRecordsNodelessSpend(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	st := s.open(t, "test")

	recordSpend(ctx, t, st, "r1", "j", "writer", 100)

	for _, spend := range []store.AgentUsage{
		{RunID: "r1", StepIndex: -1, StepName: "notifier", JobName: "j", Kind: store.UsageHook, Total: 20},
		{RunID: "r1", StepIndex: -1, StepName: "notifier", JobName: "j", Kind: store.UsageHook, Total: 30},
		{RunID: "r1", StepIndex: 0, StepName: "fixer", JobName: "j", Kind: store.UsageFix, Total: 40},
	} {
		err := st.RecordAgentUsage(ctx, spend)
		if err != nil {
			t.Fatalf("RecordAgentUsage(%s %s): %v", spend.Kind, spend.StepName, err)
		}
	}

	spent, err := st.RunTokensSpent(ctx, "r1")
	if err != nil || spent != 190 {
		t.Errorf("RunTokensSpent = %d, %v; want 190 — a resume would restore the hook and fix spend", spent, err)
	}

	assertUsageKinds(ctx, t, st, "r1")

	totals, err := st.RunCostTotals(ctx, 0)
	if err != nil || len(totals) != 1 {
		t.Fatalf("RunCostTotals = %+v, %v; want one run", totals, err)
	}

	assertTotals(t, totals[0], store.RunTotals{RunID: "r1", Tokens: 190, Steps: 4, Unpriced: 4})
}

// assertUsageKinds checks TestAgentUsageRecordsNodelessSpend's run reads back
// the step, two hook executions and the fix as rows of their own, the
// node-less ones with no node.
func assertUsageKinds(ctx context.Context, t *testing.T, st store.Store, runID string) {
	t.Helper()

	rows, err := st.RunUsage(ctx, runID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}

	kinds := map[string]int{}

	for _, row := range rows {
		kinds[row.Kind] += row.Total

		if row.Kind != store.UsageStep && row.NodeHash != "" {
			t.Errorf("%s row %q reads back node %q, want none", row.Kind, row.StepName, row.NodeHash)
		}
	}

	if len(rows) != 4 || kinds[store.UsageStep] != 100 || kinds[store.UsageHook] != 50 || kinds[store.UsageFix] != 40 {
		t.Errorf("RunUsage = %+v; want the step, two hook executions and the fix as their own rows", rows)
	}
}

// TestNodelessUsageIsReapedWithItsRun: with no node to cascade off, a hook's
// row has only its run — and retention must still take it.
func (s suite) TestNodelessUsageIsReapedWithItsRun(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	st := s.open(t, "test")

	for n, runID := range []string{"OLDRUN01", "NEWRUN01"} {
		build(ctx, t, st, "job", runID, n)

		err := st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: runID, StepIndex: -1, StepName: "notifier", JobName: "job", Kind: store.UsageHook, Total: 10,
		})
		if err != nil {
			t.Fatalf("RecordAgentUsage: %v", err)
		}
	}

	err := st.Prune(ctx, store.Retention{JobName: "job", Runs: 1}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	for runID, want := range map[string]int{"OLDRUN01": 0, "NEWRUN01": 1} {
		rows, err := st.RunUsage(ctx, runID)
		if err != nil || len(rows) != want {
			t.Errorf("run %s keeps %d usage rows (%v), want %d", runID, len(rows), err, want)
		}
	}
}
