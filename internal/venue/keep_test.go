package venue

// Whether a worker's scratch outlives the step.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// scratchOf finds the shim's scratch for a session opened against cwd. The
// shim names it after the step directory and this process, which is exactly
// what makes a leftover traceable.
func scratchOf(t *testing.T, cwd string) []string {
	t.Helper()

	pattern := filepath.Join(os.TempDir(), "steps-shim",
		fmt.Sprintf("%s-%d-*", filepath.Base(cwd), os.Getpid()))

	found, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("looking for the worker's scratch: %v", err)
	}

	return found
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

	err = runner.Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	kept := scratchOf(t, cwd)
	if len(kept) == 0 {
		t.Fatal("the worker removed its scratch under Keep — the postmortem the flag exists for has nothing to look at on the machine that ran the step")
	}

	for _, dir := range kept {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
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

	// Explicitly, not via the cleanup: the scratch is removed on the goodbye,
	// so the check has to come after it.
	err = runner.Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	if left := scratchOf(t, cwd); len(left) != 0 {
		t.Errorf("the worker kept %v — every step would accumulate a tree on somebody else's disk", left)
	}
}
