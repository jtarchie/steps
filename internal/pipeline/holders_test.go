package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestDeferrableRefusesACollectingCell: a cell captures under its coordinates
// (findings/alpha), and the collected artifact is composed HERE — a tree left
// on the worker under that path is one no consumer of findings would find.
func TestDeferrableRefusesACollectingCell(t *testing.T) {
	t.Parallel()

	rt := config.ResolvedTask{Name: "cell", Outputs: []string{"findings"}}

	if !deferrable(config.Step{}, rt) {
		t.Fatal("an ordinary task's outputs should be deferrable")
	}

	if deferrable(config.Step{OutputSubdir: "alpha"}, rt) {
		t.Error("a collecting cell's outputs were deferred; the collection is a local reader")
	}
}
