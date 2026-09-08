package sqlite

// The entry bound on the step-blob index, which is this driver's constant.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// TestStepBlobsEvictWholeEntriesByCount pins the bound and its shape: count,
// never age, and whole entries rather than stray rows — so pressure costs a
// re-run, not a half-answer.
func TestStepBlobsEvictWholeEntriesByCount(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	ctx := context.Background()

	for i := range stepBlobEntryCap + 1 {
		err := st.RecordStepBlobs(ctx, fmt.Sprintf("key-%d", i), map[string]string{"out": "d", "logs": "d2"})
		if err != nil {
			t.Fatalf("RecordStepBlobs %d: %v", i, err)
		}
	}

	oldest, err := st.StepBlobs(ctx, "key-0")
	if err != nil || len(oldest) != 0 {
		t.Fatalf("the oldest entry = %v, %v; want evicted", oldest, err)
	}

	newest, err := st.StepBlobs(ctx, fmt.Sprintf("key-%d", stepBlobEntryCap))
	if err != nil || len(newest) != 2 {
		t.Fatalf("the newest entry = %v, %v; want both rows intact", newest, err)
	}
}
