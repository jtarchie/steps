package venue

// Whether a worker's scratch outlives the step.

import (
	"context"
	"os/exec"
	"testing"
)

// volumesMadeBy are the volumes a docker+ session made for itself: on a worker they hold the step's tree, the only copy of anything undeclared.
func volumesMadeBy(t *testing.T, runner any) []string {
	t.Helper()

	plus, ok := runner.(plusRunner)
	if !ok {
		t.Fatalf("runner is %T, want a docker+ runner", runner)
	}

	if len(plus.s.volumes) == 0 {
		t.Fatal("the session made no volumes — there is nothing for Keep to leave")
	}

	return plus.s.volumes
}

// TestVenueKeepLeavesTheWorkersScratch is --keep-workspace reaching the
// machine that actually ran the step.
//
// It was read from STEPS_KEEP_WORKSPACE, which kong only consults as a
// FALLBACK for the flag: someone who typed --keep-workspace set the struct
// field and never the variable, so the worker deleted its scratch anyway. The
// files the flag exists to leave behind stopped at the machine boundary — and
// on a worker they are the only copy, since only declared outputs come home.
func TestVenueKeepLeavesTheWorkersScratch(t *testing.T) {
	cwd := t.TempDir()

	spec := localWorker(t, cwd)
	spec.Keep = true

	runner, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	kept := volumesMadeBy(t, runner)

	err = runner.Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	for _, name := range kept {
		t.Cleanup(func() { _ = exec.CommandContext(context.Background(), "docker", "volume", "rm", "-f", name).Run() }) //nolint:gosec // a name the session made

		if !volumeExists(t, name) {
			t.Errorf("the worker removed %s under Keep — the postmortem the flag exists for has nothing to look at on the machine that ran the step", name)
		}
	}
}

// TestVenueRemovesTheWorkersScratchByDefault is the other half: a worker is
// somebody else's machine, and a step that did not ask to leave anything on it
// leaves nothing.
func TestVenueRemovesTheWorkersScratchByDefault(t *testing.T) {
	cwd := t.TempDir()

	runner := newLocalRunner(t, localWorker(t, cwd))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("running: %v", err)
	}

	made := volumesMadeBy(t, runner)

	// Explicitly, not via the cleanup: the volumes go on close, so the check has to come after it.
	err = runner.Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	for _, name := range made {
		if volumeExists(t, name) {
			t.Errorf("the worker kept %s — every step would accumulate a tree on somebody else's disk", name)
		}
	}
}
