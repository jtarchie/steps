package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// intPtr builds a pointer dial the way YAML would, so a test can tell
// "attempts: 0" from an omitted attempts: (see config's dials.go).
func intPtr(v int) *int { return &v }

// TestStepCacheable pins which steps may have their outputs reused. Every
// false case is a step whose observable result is MORE than its declared
// artifacts, which a cache hit does not restore.
func TestStepCacheable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	out := []string{"built"}

	cacheable := map[string]config.Step{
		"a task with outputs":   {Task: "work", Run: "make", Outputs: out},
		"an agent with outputs": {Agent: "reviewer", Prompt: "review", Outputs: out},
		"a task behind a when:": {Task: "work", Run: "make", Outputs: out, When: &config.WhenSpec{Run: "test -f go.mod"}},
		"a task with an assert": {Task: "work", Run: "make", Outputs: out, Assert: &config.Assert{}},
		"a task with attempts":  {Task: "work", Run: "make", Outputs: out, Attempts: intPtr(3)},
		"an agent with tools":   {Agent: "reviewer", Prompt: "review", Outputs: out, Tools: []config.ToolSpec{{Name: "read_file"}}},
	}

	for name, step := range cacheable {
		if !StepCacheable(cfg, step) {
			t.Errorf("%s: not cacheable, want cacheable", name)
		}
	}

	uncacheable := map[string]config.Step{
		"volatile":         {Task: "work", Run: "make", Outputs: out, Volatile: true},
		"a volatile agent": {Agent: "reviewer", Prompt: "review", Outputs: out, Volatile: true},
		"a put":            {Put: "results"},
		"a get":            {Get: "repo"},
		"a routing task":   {Task: "work", Run: "make", Outputs: out, To: map[string]string{"failure": "cleanup"}},
		"a verdict agent":  {Agent: "reviewer", Prompt: "review", Outputs: out, Verdicts: []config.VerdictRoute{{Name: "approve"}}},
		"a context: from reader": {Agent: "reviewer", Prompt: "review", Outputs: out, Context: &config.ContextSpec{
			From: map[string]config.FromLevel{"classifier": config.FromVerdict},
		}},
		"a hooked task": {Task: "work", Run: "make", Outputs: out, Hooks: config.Hooks{OnSuccess: &config.Step{Task: "notify", Run: "echo"}}},
		"a do block":    {Do: []config.Step{{Task: "work", Run: "make"}}},

		// A hit restores declared outputs and nothing else, so a step that
		// declares none has nothing to reuse — skipping it would just drop
		// whatever its run: actually did.
		"a task with no outputs":   {Task: "notify", Run: "curl -X POST https://hooks.example/deploy"},
		"an agent with no outputs": {Agent: "reviewer", Prompt: "review"},

		// Both spellings of an across: cell. The templated one is why
		// OutputSubdir is checked and not only Label: config.nameCell leaves
		// Label empty when the author's own template names the cell.
		"an across cell":          {Task: "work", Run: "make", Outputs: out, Label: "work [os:linux]", OutputSubdir: "linux"},
		"a templated across cell": {Task: "work", Run: "make", Outputs: out, OutputSubdir: "linux"},
	}

	for name, step := range uncacheable {
		if StepCacheable(cfg, step) {
			t.Errorf("%s: cacheable, want NOT cacheable", name)
		}
	}
}

// TestStepCacheableRejectsAFixTask: a fix: agent runs only when the command
// failed, so a cache entry records exactly the case that never needed it.
func TestStepCacheableRejectsAFixTask(t *testing.T) {
	t.Parallel()

	out := []string{"report"}

	cfg := &config.Config{
		Tasks: []config.Task{{
			Name: "lint", Run: "golangci-lint run", Outputs: out,
			Fix: &config.FixSpec{Agent: "fixer", Prompt: "fix it"},
		}},
	}

	if StepCacheable(cfg, config.Step{Task: "lint"}) {
		t.Error("a task with a fix: agent is cacheable, want NOT cacheable")
	}

	// The same task without the fix: is, so the rejection is the fix: and not
	// something incidental about resolving through tasks:.
	plain := &config.Config{Tasks: []config.Task{{Name: "lint", Run: "golangci-lint run", Outputs: out}}}
	if !StepCacheable(plain, config.Step{Task: "lint"}) {
		t.Error("a plain tasks:-resolved task is not cacheable, want cacheable")
	}

	// The outputs that make it cacheable may come from the tasks: entry rather
	// than the step, which is the whole reason this check resolves first.
	noOutputs := &config.Config{Tasks: []config.Task{{Name: "lint", Run: "golangci-lint run"}}}
	if StepCacheable(noOutputs, config.Step{Task: "lint"}) {
		t.Error("a tasks:-resolved task with no outputs is cacheable, want NOT cacheable")
	}
}
