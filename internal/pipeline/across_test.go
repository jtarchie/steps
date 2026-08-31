package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestCellBlockKindSpeaksTheAuthorsWord: a desugared parallelism: matrix
// reports as parallelism, a hand-written one as across.
func TestCellBlockKindSpeaksTheAuthorsWord(t *testing.T) {
	t.Parallel()

	across := config.Step{Task: "unit", Across: []config.AcrossVar{{Var: "shard", Values: []string{"a"}}}}
	if got := cellBlockKind(across); got != "across" {
		t.Errorf("hand-written matrix reports as %q, want across", got)
	}

	across.Parallelism = 3
	if got := cellBlockKind(across); got != "parallelism" {
		t.Errorf("desugared parallelism: matrix reports as %q, want parallelism", got)
	}
}
