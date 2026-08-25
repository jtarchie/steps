package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestStepBlobsRoundTrip is the index's whole contract: what a step's outputs
// digested to, by the key the step cache files them under, replaced wholesale
// on re-record.
func TestStepBlobsRoundTrip(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := context.Background()

	got, err := store.StepBlobs(ctx, "unknown")
	if err != nil || len(got) != 0 {
		t.Fatalf("StepBlobs of an unknown key = %v, %v; want empty, nil", got, err)
	}

	err = store.RecordStepBlobs(ctx, "key-1", map[string]string{"out": "d1", "logs": "d2"})
	if err != nil {
		t.Fatalf("RecordStepBlobs: %v", err)
	}

	assertBlobs(t, store, "key-1", map[string]string{"out": "d1", "logs": "d2"})

	// A re-record replaces the entry: an output the step no longer declares
	// must not survive as a stale row.
	err = store.RecordStepBlobs(ctx, "key-1", map[string]string{"out": "d3"})
	if err != nil {
		t.Fatalf("RecordStepBlobs again: %v", err)
	}

	assertBlobs(t, store, "key-1", map[string]string{"out": "d3"})
}

func assertBlobs(t *testing.T, store *Store, key string, want map[string]string) {
	t.Helper()

	got, err := store.StepBlobs(context.Background(), key)
	if err != nil {
		t.Fatalf("StepBlobs(%q): %v", key, err)
	}

	if len(got) != len(want) {
		t.Fatalf("StepBlobs(%q) = %v, want %v", key, got, want)
	}

	for output, digest := range want {
		if got[output] != digest {
			t.Fatalf("StepBlobs(%q) = %v, want %v", key, got, want)
		}
	}
}

// TestStepBlobsAreScopedToThePipeline holds the standing rule: two pipelines
// sharing a state file never see each other's rows, even for an identical
// action key.
func TestStepBlobsAreScopedToThePipeline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "shared.db")
	ctx := context.Background()

	first, err := OpenStore(path, "app")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	second, err := OpenStore(path, "infra")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	err = first.RecordStepBlobs(ctx, "same-key", map[string]string{"out": "app-digest"})
	if err != nil {
		t.Fatalf("RecordStepBlobs: %v", err)
	}

	got, err := second.StepBlobs(ctx, "same-key")
	if err != nil || len(got) != 0 {
		t.Fatalf("the other pipeline sees %v, %v; want nothing", got, err)
	}
}

// TestStepBlobsEvictWholeEntriesByCount pins the bound and its shape: count,
// never age, and whole entries rather than stray rows — so pressure costs a
// re-run, not a half-answer.
func TestStepBlobsEvictWholeEntriesByCount(t *testing.T) {
	t.Parallel()

	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := context.Background()

	for i := range stepBlobEntryCap + 1 {
		err := store.RecordStepBlobs(ctx, fmt.Sprintf("key-%d", i), map[string]string{"out": "d", "logs": "d2"})
		if err != nil {
			t.Fatalf("RecordStepBlobs %d: %v", i, err)
		}
	}

	oldest, err := store.StepBlobs(ctx, "key-0")
	if err != nil || len(oldest) != 0 {
		t.Fatalf("the oldest entry = %v, %v; want evicted", oldest, err)
	}

	newest, err := store.StepBlobs(ctx, fmt.Sprintf("key-%d", stepBlobEntryCap))
	if err != nil || len(newest) != 2 {
		t.Fatalf("the newest entry = %v, %v; want both rows intact", newest, err)
	}
}
