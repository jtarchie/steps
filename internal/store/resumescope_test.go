package store

// The resume index across the several pipelines one state file may hold.

import (
	"context"
	"path/filepath"
	"testing"
)

// TestCompletedRunStepsAreScopedToTheirPipeline pins the read --resume trusts.
//
// run_steps carries no pipeline column of its own, so this read reaches the
// pipeline only through runs. Unscoped, a pipeline asking about a run id it
// does not own reads the OWNER's finished steps as its own and skips work it
// never did — which for the resume index means skipping work, not just
// misreporting it.
//
// Defense in depth since StartRun stopped upserting: a run id now names a row
// in exactly one pipeline, so no honest resume can ask this question. The
// predicate stays because the repo rule is categorical about it.
func TestCompletedRunStepsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web := mustOpenPipeline(t, path, "web")
	infra := mustOpenPipeline(t, path, "infra")

	const shared = "SHARED01"

	err := web.StartRun(ctx, shared, "build", "/tmp/web", "")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = web.RecordRunStep(ctx, shared, 0, "compile")
	if err != nil {
		t.Fatalf("RecordRunStep web: %v", err)
	}

	// infra records no run of its own: runs.id is global and StartRun refuses
	// an id another pipeline holds. What is being asked is narrower and is the
	// query's own contract — infra reading a run id it does not own must see
	// nothing, rather than the other pipeline's finished steps.
	done, err := infra.CompletedRunSteps(ctx, shared)
	if err != nil {
		t.Fatalf("CompletedRunSteps infra: %v", err)
	}

	if len(done) != 0 {
		t.Fatalf("infra sees %d completed steps of a run it never finished: %v", len(done), done)
	}
}
