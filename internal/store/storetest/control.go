package storetest

import (
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// What a `steps pipeline` verb does to the database, as a caller of the contract sees it.

// TestPauseIsTheWholePipelinesBreaker: it survives a re-read, so a drain in another goroutine reads the same answer the verb wrote.
func (s suite) TestPauseIsTheWholePipelinesBreaker(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	paused, err := st.Paused(ctx)
	if err != nil {
		t.Fatalf("Paused: %v", err)
	}

	if paused {
		t.Fatal("a new pipeline is paused")
	}

	err = st.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	paused, err = st.Paused(ctx)
	if err != nil || !paused {
		t.Fatalf("Paused after Pause = (%v, %v), want true", paused, err)
	}

	err = st.Unpause(ctx)
	if err != nil {
		t.Fatalf("Unpause: %v", err)
	}

	paused, err = st.Paused(ctx)
	if err != nil || paused {
		t.Fatalf("Paused after Unpause = (%v, %v), want false", paused, err)
	}
}

// TestPauseAllLeavesEachPipelinesOwnPause: thrown through one handle it stops every pipeline in the database, and lifting it returns each to the pause it had — one paused on purpose before stays paused.
func (s suite) TestPauseAllLeavesEachPipelinesOwnPause(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	held := s.open(t, "held")
	running := s.open(t, "running")

	err := held.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	// Twice, because a second press must not fail on the row the first wrote.
	for range 2 {
		err = held.PauseAll(ctx)
		if err != nil {
			t.Fatalf("PauseAll: %v", err)
		}
	}

	pausedAs(t, running, "running under PauseAll", true, false, true)
	pausedAs(t, held, "held under PauseAll", true, true, true)

	// Its own unpause is still its own: the daemon-wide pause holds it regardless.
	err = held.Unpause(ctx)
	if err != nil {
		t.Fatalf("Unpause: %v", err)
	}

	pausedAs(t, held, "held unpaused under PauseAll", true, false, true)

	err = held.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	err = running.UnpauseAll(ctx)
	if err != nil {
		t.Fatalf("UnpauseAll: %v", err)
	}

	pausedAs(t, running, "running after UnpauseAll", false, false, false)
	pausedAs(t, held, "held after UnpauseAll", true, true, false)
}

func pausedAs(t *testing.T, st store.Store, what string, paused, own, all bool) {
	t.Helper()

	ctx := ctxFor(t)

	gotPaused, err := st.Paused(ctx)
	if err != nil || gotPaused != paused {
		t.Errorf("%s: Paused = (%v, %v), want %v", what, gotPaused, err, paused)
	}

	gotOwn, err := st.PipelinePaused(ctx)
	if err != nil || gotOwn != own {
		t.Errorf("%s: PipelinePaused = (%v, %v), want %v", what, gotOwn, err, own)
	}

	gotAll, err := st.AllPaused(ctx)
	if err != nil || gotAll != all {
		t.Errorf("%s: AllPaused = (%v, %v), want %v", what, gotAll, err, all)
	}
}

// TestRenameKeepsTheHistoryUnderTheNewName: every scoped row reaches the pipeline by id, so a rename is one UPDATE rather than a new identity with empty state.
func (s suite) TestRenameKeepsTheHistoryUnderTheNewName(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "test")

	seedRuns(t, st, "build", "run-one")

	err := st.FinishRun(ctx, "run-one", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = st.Rename(ctx, "renamed")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	held := pipelineNames(t, st)
	if slices.Contains(held, "test") {
		t.Error("the old name is still in the file, so a rename minted a second pipeline")
	}

	if !slices.Contains(held, "renamed") {
		t.Fatalf("no pipeline is called renamed after a rename: %v", held)
	}

	// The run is the renamed pipeline's, which is the whole point of the identity being a row rather than a string.
	runs, err := st.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "run-one" {
		t.Errorf("after a rename the pipeline reports %+v, want the run it recorded before", runs)
	}
}

// TestDeleteForgetsThePipelineAndItsHistory: destroy is one DELETE and everything scoped to the pipeline goes with it, including the revision the RESTRICT would otherwise hold.
func (s suite) TestDeleteForgetsThePipelineAndItsHistory(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	doomed := s.open(t, "test")
	bystander := s.open(t, "other")

	setRevision(t, doomed, "sha-one", 1, map[string]string{"ci/build.sh": "echo hi\n"})

	err := doomed.StartRun(ctx, store.NewRun{ID: "run-one", JobName: "build", Workspace: "/tmp/ws", ConfigSHA: "sha-one"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	seedRuns(t, bystander, "build", "run-two")

	err = doomed.Delete(ctx)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if slices.Contains(pipelineNames(t, bystander), "test") {
		t.Error("the deleted pipeline is still in the file")
	}

	// The neighbour keeps everything, which is what makes destroy safe in a shared state file.
	runs, err := bystander.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "run-two" {
		t.Errorf("the neighbouring pipeline reports %+v, want the run it recorded", runs)
	}
}
