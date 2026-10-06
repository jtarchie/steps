package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// The hottest write there is: one autocommit per event, from the sink's one
// goroutine. What it costs is what every step of every build pays.
func BenchmarkAppendRunEvent(b *testing.B) {
	ctx := context.Background()

	st, err := OpenStore(filepath.Join(b.TempDir(), "state.db"), "bench")
	if err != nil {
		b.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	err = st.StartRun(ctx, "run-1", "build", "/tmp/ws", "")
	if err != nil {
		b.Fatal(err)
	}

	row := store.RunEventRow{RunID: "run-1", Type: "step_output_chunk", StepName: "compile", StepKind: "task", Text: "line of output", At: time.Now()}

	b.ResetTimer()

	for range b.N {
		err = st.AppendRunEvent(ctx, row)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The hottest periodic write: a check re-reporting its whole window every
// poll, with nothing in it new.
func BenchmarkRecordVersionsSteadyState(b *testing.B) {
	ctx := context.Background()

	st, err := OpenStore(filepath.Join(b.TempDir(), "state.db"), "bench")
	if err != nil {
		b.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	versions := make([]map[string]any, 1000)
	for i := range versions {
		versions[i] = map[string]any{"ref": fmt.Sprintf("%040d", i)}
	}

	_, err = st.RecordVersions(ctx, "repo", versions, 0)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()

	for range b.N {
		added, err := st.RecordVersions(ctx, "repo", versions, 0)
		if err != nil || added != 0 {
			b.Fatalf("added %d, %v", added, err)
		}
	}
}
