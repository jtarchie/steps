package storetest

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestStepBlobsRoundTrip is the index's whole contract: what a step's outputs
// digested to, by the key the step cache files them under, replaced wholesale
// on re-record.
func (s suite) TestStepBlobsRoundTrip(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	ctx := context.Background()

	got, err := st.StepBlobs(ctx, "unknown")
	if err != nil || len(got) != 0 {
		t.Fatalf("StepBlobs of an unknown key = %v, %v; want empty, nil", got, err)
	}

	err = st.RecordStepBlobs(ctx, "key-1", map[string]string{"out": "d1", "logs": "d2"})
	if err != nil {
		t.Fatalf("RecordStepBlobs: %v", err)
	}

	assertBlobs(t, st, "key-1", map[string]string{"out": "d1", "logs": "d2"})

	// A re-record replaces the entry: an output the step no longer declares
	// must not survive as a stale row.
	err = st.RecordStepBlobs(ctx, "key-1", map[string]string{"out": "d3"})
	if err != nil {
		t.Fatalf("RecordStepBlobs again: %v", err)
	}

	assertBlobs(t, st, "key-1", map[string]string{"out": "d3"})
}
func assertBlobs(t *testing.T, st store.Store, key string, want map[string]string) {
	t.Helper()

	got, err := st.StepBlobs(context.Background(), key)
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
func (s suite) TestStepBlobsAreScopedToThePipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	first := s.open(t, "app")

	t.Cleanup(func() { _ = first.Close() })

	second := s.open(t, "infra")

	t.Cleanup(func() { _ = second.Close() })

	err := first.RecordStepBlobs(ctx, "same-key", map[string]string{"out": "app-digest"})
	if err != nil {
		t.Fatalf("RecordStepBlobs: %v", err)
	}

	got, err := second.StepBlobs(ctx, "same-key")
	if err != nil || len(got) != 0 {
		t.Fatalf("the other pipeline sees %v, %v; want nothing", got, err)
	}
}
