package pipeline

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A runaway step cannot fill the database: past the bound the output stops and says how much it dropped. Written through to a real store and read back, because the bound is only worth anything in what the database ends up holding.
func TestAKeptOutputIsBounded(t *testing.T) {
	t.Parallel()

	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx := t.Context()

	err = st.StartRun(ctx, store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	const bound = 100_000

	kept := &keptOutputs{st: st, runID: "run-1", limit: bound, next: map[int64]int{}}
	out := &keptOutput{keptOutputs: kept, stepID: 7, n: kept.claim(7)}

	// Chunks that do not divide the bound, so the cut lands inside one.
	for range 400 {
		out.write(ctx, []byte(strings.Repeat("x\n", 500)))
	}

	if !out.finish(ctx) {
		t.Fatal("the store refused the output")
	}

	var whole strings.Builder

	found, err := st.StepOutput(ctx, "run-1", 7, out.n, func(chunk []byte) error {
		whole.Write(chunk)

		return nil
	})
	if err != nil || !found {
		t.Fatalf("StepOutput: found=%v err=%v", found, err)
	}

	head, trailer, cut := strings.Cut(whole.String(), "\n... [")

	switch {
	case !cut:
		t.Fatalf("the output does not say it was cut (%d bytes)", whole.Len())
	case head != strings.Repeat("x\n", bound/2):
		t.Errorf("the output kept %d bytes before its trailer, want exactly the %d-byte bound of what was printed", len(head), bound)
	case !strings.Contains(trailer, "300000 further bytes"):
		t.Errorf("the trailer does not count what was dropped: %q", trailer)
	}
}
