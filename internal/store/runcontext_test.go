package store

import (
	"path/filepath"
	"testing"
)

// TestRunContextRoundTrip covers the read path: writes come back, ordered by
// key, and one run never sees another's facts.
func TestRunContextRoundTrip(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	mustSetContext(t, store, "run-1", "failure_cause", "flaky DNS", "investigator")
	mustSetContext(t, store, "run-1", "owner", "platform", "investigator")

	// A different run writing the same key must stay invisible to run-1.
	mustSetContext(t, store, "run-2", "failure_cause", "something else", "investigator")

	entries := mustRunContext(t, store, "run-1")

	// Ordered by key, so the recap a reader sees is stable across runs
	// regardless of the order the model wrote them in.
	if len(entries) != 2 || entries[0].Key != "failure_cause" || entries[1].Key != "owner" {
		t.Fatalf("entries = %+v, want failure_cause then owner", entries)
	}

	if entries[0].Value != "flaky DNS" {
		t.Errorf("failure_cause = %q, want %q", entries[0].Value, "flaky DNS")
	}

	if entries[0].WrittenBy != "investigator" {
		t.Errorf("written_by = %q, want investigator", entries[0].WrittenBy)
	}
}

// TestRunContextOverwriteReplaces proves a second write to one key replaces
// the first rather than accumulating two readable answers — the store says
// what is true now, and a step correcting an earlier fact must not leave the
// stale one beside it. Attribution follows the new value.
func TestRunContextOverwriteReplaces(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	mustSetContext(t, store, "run-1", "failure_cause", "flaky DNS", "investigator")
	mustSetContext(t, store, "run-1", "failure_cause", "expired cert", "fixer")

	entries := mustRunContext(t, store, "run-1")
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want 1 (an overwrite replaces, never appends)", entries)
	}

	if entries[0].Value != "expired cert" || entries[0].WrittenBy != "fixer" {
		t.Errorf("failure_cause = %+v, want the fixer's value", entries[0])
	}
}

func mustRunContext(t *testing.T, store *Store, runID string) []ContextEntry {
	t.Helper()

	entries, err := store.RunContext(t.Context(), runID)
	if err != nil {
		t.Fatalf("RunContext(%q): %v", runID, err)
	}

	return entries
}

// TestRunContextEmptyRun proves an unknown run reads as empty rather than
// erroring — every agent step asks, and most runs have nothing recorded.
func TestRunContextEmptyRun(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	entries, err := store.RunContext(t.Context(), "never-ran")
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("entries = %+v, want none", entries)
	}
}

func mustSetContext(t *testing.T, store *Store, runID, key, value, writtenBy string) {
	t.Helper()

	err := store.SetContext(t.Context(), runID, key, value, writtenBy)
	if err != nil {
		t.Fatalf("SetContext(%q, %q): %v", runID, key, err)
	}
}

// TestLayeredContextNearestScopeWins covers what a step inside a concurrent
// branch can see (#40.2).
//
// Writes go to the branch's own scope and reads used to come from the run
// alone, so a step in a branch could not see what an earlier step of the SAME
// branch had recorded — the facts only surfaced at the join, after the branch
// had finished. Two steps outside a block always saw each other, which is what
// made the asymmetry invisible until a matrix was nested in a branch.
func TestLayeredContextNearestScopeWins(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	mustSetContext(t, store, "run", "owner", "platform", "triage")
	mustSetContext(t, store, "run", "verdict", "unknown", "triage")
	mustSetContext(t, store, "run#0:reviewer", "verdict", "block", "reviewer")
	mustSetContext(t, store, "run#0:reviewer#1:cell", "finding", "expired cert", "cell")

	// A concurrent SIBLING's scope is on nobody else's chain, so its writes stay
	// invisible until the join lifts them.
	mustSetContext(t, store, "run#1:other", "verdict", "ship it", "other")

	entries, err := store.LayeredContext(t.Context(), []string{"run", "run#0:reviewer", "run#0:reviewer#1:cell"})
	if err != nil {
		t.Fatalf("LayeredContext: %v", err)
	}

	got := map[string]string{}
	for _, entry := range entries {
		got[entry.Key] = entry.Value
	}

	want := map[string]string{
		"owner":   "platform",     // only the run has it
		"verdict": "block",        // the branch shadows the run
		"finding": "expired cert", // the innermost scope contributes its own
	}

	if len(got) != len(want) {
		t.Fatalf("entries = %+v, want %d keys", entries, len(want))
	}

	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}

	// Ordered by key, like RunContext, or a rendered recap reads differently
	// every time for no reason.
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Key > entries[i].Key {
			t.Errorf("entries are not ordered by key: %+v", entries)

			break
		}
	}
}

// TestLayeredContextOnTheRunAlone pins the common case: no block, one scope,
// exactly what RunContext returns.
func TestLayeredContextOnTheRunAlone(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	defer func() { _ = store.Close() }()

	mustSetContext(t, store, "run", "owner", "platform", "triage")

	entries, err := store.LayeredContext(t.Context(), []string{"run"})
	if err != nil {
		t.Fatalf("LayeredContext: %v", err)
	}

	if len(entries) != 1 || entries[0].Key != "owner" || entries[0].Value != "platform" {
		t.Errorf("entries = %+v, want the run's single fact", entries)
	}
}
