package store

// Minting a run is not the same act as continuing one.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// TestStartRunRefusesAnIdSomeRunAlreadyHas is the loud half of the fix for a
// silent one.
//
// StartRun used to be an upsert on a GLOBAL primary key: a second run minting
// an id some run already held did not fail and did not insert — it took the
// existing row over, flipping a finished run back to running and repointing
// its workspace, while the row kept the OLD job_name and the OLD pipeline_id.
// Every child row of the new run then hung off a record describing a
// different run of a different job.
//
// It cannot be driven through run(): ids are random, so the collision is a
// probability rather than an input. This is the highest layer that can state
// it as a fact.
func TestStartRunRefusesAnIdSomeRunAlreadyHas(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.StartRun(ctx, "COLLIDE1", "build", "/tmp/first")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.FinishRun(ctx, "COLLIDE1", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = store.StartRun(ctx, "COLLIDE1", "deploy", "/tmp/second")
	if !errors.Is(err, ErrRunExists) {
		t.Fatalf("minting a colliding id returned %v, want ErrRunExists", err)
	}

	// And it did not take the first run over on its way to failing.
	run, err := store.FindRun(ctx, "COLLIDE1")
	if err != nil {
		t.Fatalf("FindRun: %v", err)
	}

	if run.Status != "succeeded" {
		t.Errorf("the first run is now %q — a refused mint still flipped it back to running", run.Status)
	}

	if run.JobName != "build" || run.Workspace != "/tmp/first" {
		t.Errorf("the first run now reads job %q workspace %q, want its own", run.JobName, run.Workspace)
	}
}

// TestStartRunRefusesAnIdHeldByAnotherPipeline: one state file may hold
// several pipelines and runs.id is global across all of them, so the refusal
// has to be global too. A pipeline-scoped check would hand the second
// pipeline a row belonging to the first.
func TestStartRunRefusesAnIdHeldByAnotherPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web, err := OpenStore(path, "web")
	if err != nil {
		t.Fatalf("OpenStore web: %v", err)
	}

	defer func() { _ = web.Close() }()

	infra, err := OpenStore(path, "infra")
	if err != nil {
		t.Fatalf("OpenStore infra: %v", err)
	}

	defer func() { _ = infra.Close() }()

	err = web.StartRun(ctx, "SHARED01", "build", "/tmp/web")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = infra.StartRun(ctx, "SHARED01", "build", "/tmp/infra")
	if !errors.Is(err, ErrRunExists) {
		t.Fatalf("minting an id another pipeline holds returned %v, want ErrRunExists", err)
	}
}

// TestResumeRunNeedsARunToResume: continuing a run is an UPDATE, so it must
// refuse to invent the row it was told to continue. The upsert could not tell
// the two apart, which is what let a mint pass as a resume.
func TestResumeRunNeedsARunToResume(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.ResumeRun(ctx, "MISSING1", "/tmp/ws")
	if !errors.Is(err, ErrNoSuchRun) {
		t.Fatalf("resuming a run that does not exist returned %v, want ErrNoSuchRun", err)
	}

	err = store.StartRun(ctx, "REAL0001", "build", "/tmp/first")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.FinishRun(ctx, "REAL0001", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// A resume puts the run back in flight and repoints it at the build this
	// attempt is using — which is the whole of what the upsert was doing that
	// was legitimate.
	err = store.ResumeRun(ctx, "REAL0001", "/tmp/second")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	run, err := store.FindRun(ctx, "REAL0001")
	if err != nil {
		t.Fatalf("FindRun: %v", err)
	}

	if run.Status != "running" || run.Workspace != "/tmp/second" {
		t.Errorf("resumed run reads %q at %q, want running at the new workspace", run.Status, run.Workspace)
	}

	if run.JobName != "build" {
		t.Errorf("resuming rewrote job_name to %q", run.JobName)
	}
}

// TestResumeRunIsScopedToItsPipeline holds the repo rule on the new query: a
// run id names a row in ONE pipeline, and reaching another pipeline's run
// would put a foreign row back in flight under this pipeline's name.
func TestResumeRunIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web, err := OpenStore(path, "web")
	if err != nil {
		t.Fatalf("OpenStore web: %v", err)
	}

	defer func() { _ = web.Close() }()

	infra, err := OpenStore(path, "infra")
	if err != nil {
		t.Fatalf("OpenStore infra: %v", err)
	}

	defer func() { _ = infra.Close() }()

	err = web.StartRun(ctx, "WEBRUN01", "build", "/tmp/web")
	if err != nil {
		t.Fatalf("StartRun web: %v", err)
	}

	err = infra.ResumeRun(ctx, "WEBRUN01", "/tmp/infra")
	if !errors.Is(err, ErrNoSuchRun) {
		t.Fatalf("resuming another pipeline's run returned %v, want ErrNoSuchRun", err)
	}
}
