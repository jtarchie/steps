package pipeline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A failed walk forgets only the chains it could still have completed: a
// sibling chain that never passes through the walk's position — another
// version of a fanned-out get — keeps its success.
func TestAFailedWalkForgetsOnlyItsOwnChains(t *testing.T) {
	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	for _, leaf := range []string{"v1-leaf", "v2-leaf"} {
		err = st.RecordChainSucceeded(ctx, "build", leaf)
		if err != nil {
			t.Fatal(err)
		}
	}

	w := planWalk{
		stepRunner: stepRunner{jobName: "build", st: st},
		chains:     [][]string{{"get-v1", "v1-leaf"}, {"get-v2", "v2-leaf"}},
		parentHash: "get-v2",
	}

	succeeded := func() map[string]bool {
		t.Helper()

		got, err := st.HasSucceededBatch(ctx, "build", []string{"v1-leaf", "v2-leaf"})
		if err != nil {
			t.Fatal(err)
		}

		return got
	}

	w.forgetUnfinishedChains(ctx, nil)

	if got := succeeded(); !got["v1-leaf"] || !got["v2-leaf"] {
		t.Fatalf("after a walk that did not fail: succeeded = %v, want both kept", got)
	}

	w.forgetUnfinishedChains(ctx, errors.New("step failed"))

	if got := succeeded(); !got["v1-leaf"] || got["v2-leaf"] {
		t.Errorf("succeeded = %v, want only v1-leaf kept", got)
	}
}
