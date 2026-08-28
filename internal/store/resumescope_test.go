package store

// The resume index across the several pipelines one state file may hold.

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCompletedRunStepsAreScopedToTheirPipeline pins the read --resume trusts.
//
// Run ids are minted without a uniqueness check, so two pipelines sharing a
// state file can hold the same id — and run_steps carries no pipeline column
// to tell them apart. Unscoped, the second pipeline's resume reads the first's
// finished steps as its own and skips work it never did.
func TestCompletedRunStepsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web := mustOpenPipeline(t, path, "web")
	infra := mustOpenPipeline(t, path, "infra")

	const shared = "SHARED01"

	err := web.StartRun(ctx, shared, "build", "/tmp/web")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.RecordRunStep(ctx, shared, 0, "compile")
	if err != nil {
		t.Fatalf("RecordRunStep web: %v", err)
	}

	err = infra.StartRun(ctx, shared, "build", "/tmp/infra")
	if err != nil {
		t.Fatalf("StartRun infra: %v", err)
	}

	done, err := infra.CompletedRunSteps(ctx, shared)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(done) != 0 {
		t.Fatalf("infra sees %d completed steps of a run it never finished: %v", len(done), done)
	}
}
