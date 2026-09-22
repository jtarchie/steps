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

// TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere: every other way a
// step reads what it produced on this machine. Each is one reason, and a
// deferred output would make each of them see an empty directory.
func TestDeferrableRefusesAStepThatReadsItsOwnOutputsHere(t *testing.T) {
	t.Parallel()

	base := config.ResolvedTask{Name: "unit", Outputs: []string{"out"}}

	for _, tc := range []struct {
		name string
		rt   config.ResolvedTask
		want bool
	}{
		{name: "an ordinary task", rt: base, want: true},
		{name: "an assert with no files", rt: withAssert(base, &config.Assert{Code: new(int)}), want: true},
		{name: "an assert on files", rt: withAssert(base, &config.Assert{Files: []string{"out/f"}}), want: false},
		{name: "a fix agent", rt: withFix(base), want: false},
	} {
		if got := deferrable(config.Step{}, tc.rt); got != tc.want {
			t.Errorf("%s: deferrable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func withAssert(rt config.ResolvedTask, assert *config.Assert) config.ResolvedTask {
	rt.Assert = assert

	return rt
}

func withFix(rt config.ResolvedTask) config.ResolvedTask {
	rt.Fix = &config.FixSpec{}

	return rt
}
