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
