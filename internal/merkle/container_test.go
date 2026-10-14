package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// A block is as cacheable as its branches, at any depth.
func TestABlockIsAsCacheableAsItsBranches(t *testing.T) {
	t.Parallel()

	task := config.Step{Task: "t", Run: "true"}

	for name, test := range map[string]struct {
		block       config.Step
		unskippable bool
	}{
		"tasks":            {block: config.Step{InParallel: &config.InParallel{Steps: []config.Step{task, task}}}},
		"nested tasks":     {block: config.Step{Do: []config.Step{{Race: &config.Race{Steps: []config.Step{task, task}}}}}},
		"a put branch":     {block: config.Step{InParallel: &config.InParallel{Steps: []config.Step{task, {Put: "out"}}}}, unskippable: true},
		"a nested agent":   {block: config.Step{Do: []config.Step{{Do: []config.Step{{Agent: "a"}}}}}, unskippable: true},
		"a get branch":     {block: config.Step{InParallel: &config.InParallel{Steps: []config.Step{{Get: "repo"}}}}, unskippable: true},
		"a guarded branch": {block: config.Step{Do: []config.Step{{Task: "t", Run: "true", When: &config.WhenSpec{Run: "true"}}}}, unskippable: true},
		"a try":            {block: config.Step{Try: &task}, unskippable: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reason, err := UncacheableReason(&config.Config{}, test.block)
			if err != nil {
				t.Fatal(err)
			}

			if got := reason != ""; got != test.unskippable {
				t.Errorf("unskippable = %v, want %v", got, test.unskippable)
			}
		})
	}
}
