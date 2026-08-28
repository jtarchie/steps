package store

// agent_usage across the several pipelines one state file may hold.

import (
	"context"
	"path/filepath"
	"testing"
)

// mustOpenPipeline opens one named pipeline's handle on a shared state file.
func mustOpenPipeline(t *testing.T, path, name string) *Store {
	t.Helper()

	store, err := OpenStore(path, name)
	if err != nil {
		t.Fatalf("OpenStore %s: %v", name, err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}

// recordSpend records one agent step's node and what it spent.
func recordSpend(ctx context.Context, t *testing.T, store *Store, runID, jobName, stepName string, tokens int) {
	t.Helper()

	err := store.RecordNode(ctx, NodeRecord{
		Hash: hashOf(7), Kind: "agent", Resource: "review",
		Content: map[string]any{"prompt": "identical in both pipelines"},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	err = store.StartRun(ctx, runID, jobName, "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.RecordAgentUsage(ctx, AgentUsage{
		RunID: runID, StepIndex: 0, StepName: stepName, JobName: jobName,
		NodeHash: hashOf(7), ModelReq: "haiku",
		Prompt: tokens, Total: tokens, FinishReason: "stop",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}
}

func onlyUsage(ctx context.Context, t *testing.T, store *Store, runID string) AgentUsage {
	t.Helper()

	rows, err := store.RunUsage(ctx, runID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("run %s reported %d usage rows, want 1: %+v", runID, len(rows), rows)
	}

	return rows[0]
}

// TestAgentUsageKeyIsScopedToItsPipeline is the same collision run_placements
// had, on the table that holds money.
//
// merkle.HashNode folds kind, content and parent but NOT the pipeline, so two
// pipelines each with a job named build over a byte-identical agent step
// produce the same node hash; run ids are minted per pipeline and collide on
// their own terms. Keyed on (run_id, node_hash) alone the second pipeline's
// insert conflicts with the first's and takes the ACCUMULATING branch — which
// is correct for a retried step and catastrophic across pipelines: the tokens
// are added to a row belonging to someone else, so one pipeline is billed for
// work it never ran and the other reports nothing at all.
func TestAgentUsageKeyIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web := mustOpenPipeline(t, path, "web")
	infra := mustOpenPipeline(t, path, "infra")

	const (
		runID   = "SHARED01"
		jobName = "build"
	)

	recordSpend(ctx, t, web, runID, jobName, "web-review", 1_000)
	recordSpend(ctx, t, infra, runID, jobName, "infra-review", 7)

	got := onlyUsage(ctx, t, web, runID)
	if got.StepName != "web-review" {
		t.Errorf("web's run reports step %q — it is reading another pipeline's spend", got.StepName)
	}

	if got.Total != 1_000 {
		t.Errorf("web's run spent %d tokens, want 1000 — another pipeline's tokens were added to its row", got.Total)
	}

	got = onlyUsage(ctx, t, infra, runID)
	if got.StepName != "infra-review" {
		t.Errorf("infra's run reports step %q", got.StepName)
	}

	if got.Total != 7 {
		t.Errorf("infra's run spent %d tokens, want 7", got.Total)
	}
}
