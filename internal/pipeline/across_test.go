package pipeline

import (
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// shardStep is a desugared parallelism: block the way LoadConfig leaves one:
// the across: matrix plus the marker.
func shardStep(n int) config.Step {
	return config.Step{
		Task:        "unit",
		Across:      []config.AcrossVar{{Var: "index", Values: []string{"1", "2"}}},
		Parallelism: &n,
	}
}

// TestCellBlockKindSpeaksTheAuthorsWord: a desugared parallelism: matrix
// reports as parallelism, a hand-written one as across.
func TestCellBlockKindSpeaksTheAuthorsWord(t *testing.T) {
	t.Parallel()

	across := config.Step{Task: "unit", Across: []config.AcrossVar{{Var: "shard", Values: []string{"a"}}}}
	if got := cellBlockKind(across); got != "across" {
		t.Errorf("hand-written matrix reports as %q, want across", got)
	}

	if got := cellBlockKind(shardStep(3)); got != "parallelism" {
		t.Errorf("desugared parallelism: matrix reports as %q, want parallelism", got)
	}
}

// TestStepKindNameSpeaksTheAuthorsWord holds the OTHER front end to the same
// word: the published step kind is what the web UI labels the block with, and
// an operator reading "parallelism: 3 cells" on the console must not find the
// same block filed under "across" in the run's events.
func TestStepKindNameSpeaksTheAuthorsWord(t *testing.T) {
	t.Parallel()

	if got := stepKindName(shardStep(3)); got != "parallelism" {
		t.Errorf("published kind = %q, want parallelism", got)
	}

	across := config.Step{Task: "unit", Across: []config.AcrossVar{{Var: "shard", Values: []string{"a"}}}}
	if got := stepKindName(across); got != "across" {
		t.Errorf("published kind = %q, want across for a hand-written matrix", got)
	}
}

// TestReportCellCountSpeaksTheAuthorsWord crosses the seam the helper tests
// above cannot: the line the run report actually prints. Not parallel — it
// swaps os.Stdout.
func TestReportCellCountSpeaksTheAuthorsWord(t *testing.T) { //nolint:paralleltest // swaps os.Stdout
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = write

	reportCellCount("j", 0, shardStep(2), 2)

	os.Stdout = orig

	_ = write.Close()

	buf := make([]byte, 4096)
	n, _ := read.Read(buf)

	if got := string(buf[:n]); !strings.Contains(got, "parallelism: 2 cells") {
		t.Errorf("run report printed %q, want it to contain %q", got, "parallelism: 2 cells")
	}
}
