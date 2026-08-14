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
