package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func taskHashWithWhen(t *testing.T, when *config.WhenSpec) string {
	t.Helper()

	cfg := &config.Config{}
	step := config.Step{Task: "work", Run: "make", When: when}

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

// TestWhenOmittedFromHashWhenUnset proves value-gating: a step with no when:
// hashes byte-identically to before this field existed.
func TestWhenOmittedFromHashWhenUnset(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	step := config.Step{Task: "work", Run: "make"}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	content, err := TaskNodeContent(cfg, step, rt)
	if err != nil {
		t.Fatalf("TaskNodeContent: %v", err)
	}

	if _, present := content["when"]; present {
		t.Error("an unset when: must not appear in the hashed content")
	}
}

// TestWhenBustsHash proves adding or changing a guard changes the step's
// hash — the guard decides whether the step executes, so it must invalidate
// the cache.
func TestWhenBustsHash(t *testing.T) {
	t.Parallel()

	unset := taskHashWithWhen(t, nil)
	guarded := taskHashWithWhen(t, &config.WhenSpec{Run: "test -f a"})
	changed := taskHashWithWhen(t, &config.WhenSpec{Run: "test -f b"})

	if unset == guarded {
		t.Error("adding a when: guard should change the hash")
	}

	if guarded == changed {
		t.Error("changing the guard command should change the hash")
	}
}

// TestWhenHashedOnEveryStepKind proves the guard folds into put and agent
// content too, not just task.
func TestWhenHashedOnEveryStepKind(t *testing.T) {
	t.Parallel()

	when := &config.WhenSpec{Run: "test -f gate"}

	t.Run("put", func(t *testing.T) {
		t.Parallel()

		cfg := &config.Config{}
		rt := config.ResourceType{Name: "rt"}

		with, err := PutNodeContent(cfg, config.Step{Put: "r", When: when}, rt, nil, nil, nil, nil, false)
		if err != nil {
			t.Fatal(err)
		}

		if with["when"] != "test -f gate" {
			t.Errorf("put content when = %#v, want the guard command", with["when"])
		}
	})

	t.Run("agent", func(t *testing.T) {
		t.Parallel()

		cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")
		step := config.Step{Agent: "reviewer", Messages: []string{"x"}, When: when}

		ri, err := cfg.ResolveAgentInvocation(step)
		if err != nil {
			t.Fatal(err)
		}

		content, err := AgentContentMap(cfg, step, ri)
		if err != nil {
			t.Fatal(err)
		}

		if content["when"] != "test -f gate" {
			t.Errorf("agent content when = %#v, want the guard command", content["when"])
		}
	})
}
