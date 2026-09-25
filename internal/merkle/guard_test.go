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

// TestWhenInputsKeyedByName proves guard inputs move the key on every step
// kind, and that a guard without them keys exactly as before they existed.
func TestWhenInputsKeyedByName(t *testing.T) {
	t.Parallel()

	plain := &config.WhenSpec{Run: "test -f gate"}
	named := &config.WhenSpec{Run: "test -f gate", Inputs: []string{"answer"}}

	if taskHashWithWhen(t, plain) == taskHashWithWhen(t, named) {
		t.Error("adding guard inputs should change a task's hash")
	}

	if taskHashWithWhen(t, named) == taskHashWithWhen(t, &config.WhenSpec{Run: "test -f gate", Inputs: []string{"other"}}) {
		t.Error("renaming a guard input should change a task's hash")
	}

	t.Run("put", func(t *testing.T) {
		t.Parallel()

		content := func(when *config.WhenSpec) any {
			got, err := PutNodeContent(&config.Config{}, config.Step{Put: "r", When: when}, config.ResourceType{Name: "rt"}, nil, nil, nil, nil, false)
			if err != nil {
				t.Fatal(err)
			}

			return got["when_inputs"]
		}

		if content(plain) != nil || content(named) == nil {
			t.Error("guard inputs should fold into a put's content only when set")
		}
	})

	t.Run("agent", func(t *testing.T) {
		t.Parallel()

		cfg := agentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "")

		content := func(when *config.WhenSpec) any {
			step := config.Step{Agent: "reviewer", Messages: []string{"x"}, When: when}

			ri, err := cfg.ResolveAgentInvocation(step)
			if err != nil {
				t.Fatal(err)
			}

			got, err := AgentContentMap(cfg, step, ri)
			if err != nil {
				t.Fatal(err)
			}

			return got["when_inputs"]
		}

		if content(plain) != nil || content(named) == nil {
			t.Error("guard inputs should fold into an agent's content only when set")
		}
	})
}

// TestWhenInputsOmittedWhenEmpty is the value-gating half: nil and [] alike
// leave a guarded step's key as it was before guard inputs existed.
func TestWhenInputsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	for _, when := range []*config.WhenSpec{{Run: "test -f gate"}, {Run: "test -f gate", Inputs: []string{}}} {
		content := withWhen(config.Step{When: when}, map[string]any{})
		if _, present := content["when_inputs"]; present {
			t.Errorf("guard inputs %#v must not appear in the hashed content", when.Inputs)
		}
	}
}
