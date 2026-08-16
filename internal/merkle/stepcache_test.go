package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestStepCacheable pins which steps may have their outputs reused. Every
// false case is a step whose observable result is MORE than its declared
// artifacts, which a cache hit does not restore.
func TestStepCacheable(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	cacheable := map[string]config.Step{
		"a plain task":             {Task: "work", Run: "make"},
		"a plain agent":            {Agent: "reviewer", Prompt: "review"},
		"a task with outputs":      {Task: "work", Run: "make", Outputs: []string{"built"}},
		"a task behind a when:":    {Task: "work", Run: "make", When: &config.WhenSpec{Run: "test -f go.mod"}},
		"a task with an assert":    {Task: "work", Run: "make", Assert: &config.Assert{}},
		"a task with attempts":     {Task: "work", Run: "make", Attempts: 3},
		"an agent with a tool set": {Agent: "reviewer", Prompt: "review", Tools: []config.ToolSpec{{Name: "read_file"}}},
	}

	for name, step := range cacheable {
		if !StepCacheable(cfg, step) {
			t.Errorf("%s: not cacheable, want cacheable", name)
		}
	}

	uncacheable := map[string]config.Step{
		"volatile":         {Task: "work", Run: "make", Volatile: true},
		"a volatile agent": {Agent: "reviewer", Prompt: "review", Volatile: true},
		"a put":            {Put: "results"},
		"a get":            {Get: "repo"},
		"a routing task":   {Task: "work", Run: "make", To: map[string]string{"failure": "cleanup"}},
		"a verdict agent":  {Agent: "reviewer", Prompt: "review", Verdicts: []config.VerdictRoute{{Name: "approve"}}},
		"a context: from reader": {Agent: "reviewer", Prompt: "review", Context: &config.ContextSpec{
			From: map[string]config.FromLevel{"classifier": config.FromVerdict},
		}},
		"a hooked task":  {Task: "work", Run: "make", Hooks: config.Hooks{OnSuccess: &config.Step{Task: "notify", Run: "echo"}}},
		"an across cell": {Task: "work", Run: "make", Label: "work [os:linux]"},
		"a do block":     {Do: []config.Step{{Task: "work", Run: "make"}}},
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

	cfg := &config.Config{
		Tasks: []config.Task{{Name: "lint", Run: "golangci-lint run", Fix: &config.FixSpec{Agent: "fixer", Prompt: "fix it"}}},
	}

	if StepCacheable(cfg, config.Step{Task: "lint"}) {
		t.Error("a task with a fix: agent is cacheable, want NOT cacheable")
	}

	// The same task without the fix: is, so the rejection is the fix: and not
	// something incidental about resolving through tasks:.
	plain := &config.Config{Tasks: []config.Task{{Name: "lint", Run: "golangci-lint run"}}}
	if !StepCacheable(plain, config.Step{Task: "lint"}) {
		t.Error("a plain tasks:-resolved task is not cacheable, want cacheable")
	}
}
