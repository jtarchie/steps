package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// keyProbes reaches tasks only through the types it reflects over, and a task's sandbox: arrives already resolved onto ResolvedTask, so this pins the task half by hand: set moves the key, unset leaves the content without the field.
func TestTaskSandboxMovesTheKeyOnlyWhenSet(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Tasks: []config.Task{{Name: "work", Run: "make"}}}
	step := config.Step{Task: "work"}

	keyOf := func(sandbox bool) (string, map[string]any) {
		cfg.Tasks[0].Sandbox = sandbox

		rt, err := cfg.ResolveTask(step)
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}

		content, err := TaskNodeContent(cfg, step, rt)
		if err != nil {
			t.Fatalf("TaskNodeContent: %v", err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return hash, content
	}

	fenced, _ := keyOf(true)
	open, content := keyOf(false)

	if fenced == open {
		t.Error("sandbox: true hashed the same as unset: a result produced unfenced would be reused for a fenced step")
	}

	if _, ok := content["sandbox"]; ok {
		t.Error("an unsandboxed task folds sandbox into its key, which re-runs every cached task")
	}
}

// keyProbes reflects over no Step, so the step's own sandbox: true is pinned here: it reaches the key through resolution, inline run: and referencing step alike.
func TestStepSandboxMovesTheKey(t *testing.T) {
	t.Parallel()

	fenced := true
	cfg := &config.Config{Tasks: []config.Task{{Name: "work", Run: "make"}}}

	keyOf := func(step config.Step) string {
		rt, err := cfg.ResolveTask(step)
		if err != nil {
			t.Fatalf("ResolveTask: %v", err)
		}

		content, err := TaskNodeContent(cfg, step, rt)
		if err != nil {
			t.Fatalf("TaskNodeContent: %v", err)
		}

		hash, err := HashNode(NodeKindTask, content, "")
		if err != nil {
			t.Fatalf("HashNode: %v", err)
		}

		return hash
	}

	if keyOf(config.Step{Task: "inline", Run: "make"}) == keyOf(config.Step{Task: "inline", Run: "make", Sandbox: &fenced}) {
		t.Error("an inline run:'s sandbox: true hashed the same as unset")
	}

	if keyOf(config.Step{Task: "work"}) == keyOf(config.Step{Task: "work", Sandbox: &fenced}) {
		t.Error("a step's sandbox: true over an unfenced entry hashed the same as unset")
	}
}
