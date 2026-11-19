package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestCellHashExcludesWhatAStepCacheHitWould: a skipped cell lets the matrix carry on like a step cache hit does, so each result that is more than the cell's exit code keeps it running — and none of them moves its key.
func TestCellHashExcludesWhatAStepCacheHitWould(t *testing.T) {
	t.Parallel()

	fix := &config.FixSpec{Agent: "fixer", Messages: []string{"fix it"}}
	cfg := &config.Config{Tasks: []config.Task{{Name: "lint", Run: "golangci-lint run", Fix: fix}}}
	plain := config.Step{Task: "work", Run: "make", Inputs: config.Inputs()}

	for name, tc := range map[string]struct {
		cell      func(config.Step) config.Step
		cacheable bool
	}{
		"a plain task":  {func(s config.Step) config.Step { return s }, true},
		"volatile":      {func(s config.Step) config.Step { s.Volatile = true; return s }, false},
		"a when: guard": {func(s config.Step) config.Step { s.When = &config.WhenSpec{Run: "true"}; return s }, false},
		"a to: route":   {func(s config.Step) config.Step { s.To = map[string]string{"failure": "next"}; return s }, false},
		"a context: from reader": {func(s config.Step) config.Step {
			s.Context = &config.ContextSpec{From: map[string]config.FromLevel{"triager": config.FromVerdict}}
			return s
		}, false},
		"a hook": {func(s config.Step) config.Step {
			s.Hooks.OnSuccess = &config.Step{Task: "notify", Run: "echo"}
			return s
		}, false},
		"its own fix:":   {func(s config.Step) config.Step { s.Fix = fix; return s }, false},
		"a ((.:name))":   {func(s config.Step) config.Step { s.Run = "make ((.:tag))"; return s }, false},
		"a tasks: fix:":  {func(config.Step) config.Step { return config.Step{Task: "lint"} }, false},
		"an inline run:": {func(config.Step) config.Step { return config.Step{Task: "lint", Run: "golangci-lint run"} }, true},
	} {
		cell := tc.cell(plain)

		hash, cacheable, err := CellHash(cfg, cell, "parent")
		if err != nil {
			t.Fatalf("%s: CellHash: %v", name, err)
		}

		if cacheable != tc.cacheable {
			t.Errorf("%s: cacheable = %v, want %v", name, cacheable, tc.cacheable)
		}

		content, err := stepContentMap(cfg, cell)
		if err != nil {
			t.Fatalf("%s: content: %v", name, err)
		}

		want, err := HashNode(NodeKindTask, content, "parent")
		if err != nil {
			t.Fatalf("%s: HashNode: %v", name, err)
		}

		if hash != want {
			t.Errorf("%s: the cell's key is not its task node's hash", name)
		}
	}
}
