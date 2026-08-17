package store

import (
	"path/filepath"
	"testing"
)

// TestAgentUsageAccumulatesAcrossAttempts pins agent_usage as a LEDGER.
//
// A step can execute more than once against the same node hash — a to: route
// sending the plan back over it, or a resumed run re-running the step its
// predecessor died on. Replacing the counts reported the last attempt as the
// whole bill, so a resumed run seeded its budget from a fraction of what it
// had really spent and bought most of the allowance a second time.
func TestAgentUsageAccumulatesAcrossAttempts(t *testing.T) {
	t.Parallel()

	st, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	ctx := t.Context()

	mustRecordNode(t, st, "j", "same-hash")

	// agent_usage.run_id references runs(id) as well, so the run has to exist —
	// which in production it always does: usage is recorded from inside a step
	// of a run that StartRun already filed.
	err = st.StartRun(ctx, "r1", "j", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	for range 4 {
		err = st.RecordAgentUsage(ctx, AgentUsage{
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

	err = st.RecordAgentUsage(ctx, AgentUsage{
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
func TestAgentUsageCostSurvivesAnUnpricedAttempt(t *testing.T) {
	t.Parallel()

	st, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	ctx := t.Context()

	mustRecordNode(t, st, "j", "node")

	err = st.StartRun(ctx, "r1", "j", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	priced := 0.25

	for _, cost := range []*float64{&priced, nil} {
		err = st.RecordAgentUsage(ctx, AgentUsage{
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
func TestAgentUsageNeverPricedStaysNull(t *testing.T) {
	t.Parallel()

	st, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	ctx := t.Context()

	mustRecordNode(t, st, "j", "node")

	err = st.StartRun(ctx, "r1", "j", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		err = st.RecordAgentUsage(ctx, AgentUsage{
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
func assertAccumulatedRow(t *testing.T, row AgentUsage) {
	t.Helper()

	if row.Total != 800 || row.Prompt != 600 || row.Completion != 200 {
		t.Errorf("accumulated row = total %d prompt %d completion %d, want 800/600/200",
			row.Total, row.Prompt, row.Completion)
	}

	if row.FinishReason != "stop" || row.ModelServed != "m" {
		t.Errorf("descriptive fields = %q/%q, want the last attempt's", row.FinishReason, row.ModelServed)
	}
}
