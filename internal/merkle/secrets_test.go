package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// keyProbes does not reach tasks, so this pins the task half by hand: the reference moves the key, a step's own map replaces the task's, and with none the content has no secrets field (the value-gating promise).
func TestTaskSecretsKeyByReference(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Tasks: []config.Task{{Name: "work", Run: "make"}}}

	keyOf := func(taskSecrets, stepSecrets map[string]string) (string, map[string]any) {
		cfg.Tasks[0].Secrets = taskSecrets
		step := config.Step{Task: "work", Secrets: stepSecrets}

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

	none, content := keyOf(nil, nil)
	if _, present := content["secrets"]; present {
		t.Errorf("a task without secrets: hashes a secrets field: %v", content)
	}

	ci, _ := keyOf(map[string]string{"PUBLISH_AS": "((ci:a))"}, nil)
	other, _ := keyOf(map[string]string{"PUBLISH_AS": "((ci:b))"}, nil)
	overridden, _ := keyOf(map[string]string{"PUBLISH_AS": "((ci:a))"}, map[string]string{"PUBLISH_AS": "((ci:b))"})

	if ci == none || ci == other {
		t.Error("a task's secrets: reference does not move its key")
	}

	if overridden != other {
		t.Error("a step's secrets: does not replace its task's")
	}
}
