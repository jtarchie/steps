package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// TestResolveToolSpecPinnedArgsExcludedFromSchema proves a pinned key never
// appears in the schema shown to the model, even though the run: template
// still references it.
func TestResolveToolSpecPinnedArgsExcludedFromSchema(t *testing.T) {
	t.Parallel()

	spec := config.ToolSpec{
		Name: "post_review",
		Run:  "gh pr review --repo {{ .args.repo }} --{{ .args.action }}",
		Args: map[string]string{"repo": "jtarchie/ci"},
	}

	decl, _, _, err := resolveToolSpec(context.Background(), nil, spec, builtinAgentTools(""))
	if err != nil {
		t.Fatalf("resolveToolSpec: %v", err)
	}

	if _, present := decl.Parameters.Properties["repo"]; present {
		t.Error("pinned key \"repo\" must not appear in the schema's properties")
	}

	if _, present := decl.Parameters.Properties["action"]; !present {
		t.Error("non-pinned key \"action\" must still appear in the schema")
	}

	for _, r := range decl.Parameters.Required {
		if r == "repo" {
			t.Error("pinned key \"repo\" must not appear in Required")
		}
	}
}

// TestExecCustomToolPinnedArgsOverrideModel proves a pinned value always
// wins over whatever the model supplied at the same key, and that a
// model-supplied value the model was never asked for (because it's pinned)
// still gets overridden if somehow present.
func TestExecCustomToolPinnedArgsOverrideModel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := config.ToolSpec{
		Name: "post_review",
		Run:  `echo "{{ .args.repo }}"`,
		Args: map[string]string{"repo": "pinned/value"},
	}
	impl := execCustomTool(spec, []string{"repo"})

	result := impl(context.Background(), map[string]any{"repo": "model-supplied/value"}, testEnv(dir))
	if err, hasErr := result["error"]; hasErr {
		t.Fatalf("unexpected error: %v", err)
	}

	stdout, _ := result["stdout"].(string)
	if strings.TrimSpace(stdout) != "pinned/value" {
		t.Errorf("stdout = %q, want the pinned value to win", stdout)
	}
}

// TestExecCustomToolPinnedArgsSatisfyMissingCheck proves a pinned arg the
// model never supplies (because it's not in its schema) still counts as
// present for the tool's own required-argument check.
func TestExecCustomToolPinnedArgsSatisfyMissingCheck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := config.ToolSpec{
		Name: "post_review",
		Run:  `echo "{{ .args.repo }}"`,
		Args: map[string]string{"repo": "pinned/value"},
	}
	impl := execCustomTool(spec, []string{"repo"})

	// The model supplies no args at all — repo is pinned, so it must not be
	// reported missing.
	result := impl(context.Background(), map[string]any{}, testEnv(dir))
	if err, hasErr := result["error"]; hasErr {
		t.Fatalf("pinned arg should satisfy the missing-argument check, got error: %v", err)
	}
}

func TestMergePinnedArgs(t *testing.T) {
	t.Parallel()

	t.Run("nil pinned returns args unchanged", func(t *testing.T) {
		t.Parallel()

		args := map[string]any{"a": "1"}

		got := mergePinnedArgs(args, nil)
		if len(got) != 1 || got["a"] != "1" {
			t.Errorf("got %#v, want args unchanged", got)
		}
	})

	t.Run("pinned wins over model-supplied", func(t *testing.T) {
		t.Parallel()

		args := map[string]any{"a": "model"}
		pinned := map[string]string{"a": "pinned"}

		got := mergePinnedArgs(args, pinned)
		if got["a"] != "pinned" {
			t.Errorf("got[a] = %v, want pinned to win", got["a"])
		}
	})

	t.Run("pinned adds keys the model never supplied", func(t *testing.T) {
		t.Parallel()

		args := map[string]any{"a": "1"}
		pinned := map[string]string{"b": "2"}

		got := mergePinnedArgs(args, pinned)
		if got["a"] != "1" || got["b"] != "2" {
			t.Errorf("got %#v, want both keys present", got)
		}
	})
}

func TestVisibleParams(t *testing.T) {
	t.Parallel()

	params := []string{"a", "b", "c"}

	t.Run("no pinned keys returns params unchanged", func(t *testing.T) {
		t.Parallel()

		got := visibleParams(params, nil)
		if len(got) != 3 {
			t.Errorf("got %v, want all 3 params", got)
		}
	})

	t.Run("pinned keys excluded", func(t *testing.T) {
		t.Parallel()

		got := visibleParams(params, map[string]string{"b": "x"})
		want := []string{"a", "c"}

		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}

		for i := range want {
			if got[i] != want[i] {
				t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

// TestExecuteBudgetedToolRejectsAfterLimit proves the (N+1)th call to a
// budgeted tool is rejected as error data without ever reaching the tool's
// impl, while calls within budget succeed and increment the counter.
func TestExecuteBudgetedToolRejectsAfterLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	var invocations int

	registry := map[string]toolImpl{
		"post_review": func(_ context.Context, _ map[string]any, _ toolEnv) map[string]any {
			invocations++

			return map[string]any{"exit_code": 0}
		},
	}
	maxCalls := map[string]int{"post_review": 1}
	callCounts := map[string]int{}

	call := &genai.FunctionCall{Name: "post_review"}

	first := executeBudgetedTool(context.Background(), call, testEnv(dir), registry, maxCalls, callCounts)
	if _, hasErr := first["error"]; hasErr {
		t.Fatalf("first call within budget should succeed, got %#v", first)
	}

	second := executeBudgetedTool(context.Background(), call, testEnv(dir), registry, maxCalls, callCounts)

	msg, ok := second["error"].(string)
	if !ok || !strings.Contains(msg, "call budget") {
		t.Fatalf("second call should be rejected as budget-exhausted error data, got %#v", second)
	}

	if invocations != 1 {
		t.Errorf("tool impl invoked %d times, want exactly 1 — the rejected call must never reach it", invocations)
	}
}

// TestExecuteBudgetedToolUnlimitedWhenAbsent proves a tool absent from
// maxCalls has no budget at all.
func TestExecuteBudgetedToolUnlimitedWhenAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	registry := map[string]toolImpl{
		"read_file": func(_ context.Context, _ map[string]any, _ toolEnv) map[string]any {
			return map[string]any{"content": "x"}
		},
	}
	callCounts := map[string]int{}
	call := &genai.FunctionCall{Name: "read_file"}

	for i := range 5 {
		got := executeBudgetedTool(context.Background(), call, testEnv(dir), registry, nil, callCounts)
		if _, hasErr := got["error"]; hasErr {
			t.Fatalf("call %d: unbudgeted tool should never be rejected, got %#v", i, got)
		}
	}
}

// TestRunAgentConversationCallBudgetResetsAcrossAttempts drives
// runAgentConversation twice (simulating two attempts.Do calls) against a
// budgeted required tool, proving the second call — a fresh
// runAgentConversation invocation — gets a fresh budget rather than
// inheriting exhaustion from the first.
func TestRunAgentConversationCallBudgetResetsAcrossAttempts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	specs := []config.ToolSpec{{Name: "post_review", Run: "true", Required: true, MaxCalls: 1}}

	decls, registry, _, err := buildAgentTools(context.Background(), nil, specs, "")
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner("", dir)
	if err != nil {
		t.Fatal(err)
	}

	conv := agentConversation{
		prompt: "review it",
		env:    toolEnv{dir: dir, runner: runner},
		tools: agentTools{
			decls:    decls,
			registry: registry,
			required: requiredToolNames(specs),
			maxCalls: maxCallsByName(specs),
		},
		maxTurns: testMaxTurns,
	}

	toolCallResp := &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "post_review"}}},
		},
	}
	doneResp := &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}},
	}

	for attempt := range 2 {
		fake := &fakeLLM{responses: []*model.LLMResponse{toolCallResp, doneResp}}

		res, err := runAgentConversation(context.Background(), fake, conv)
		if err != nil {
			t.Fatalf("attempt %d: runAgentConversation: %v", attempt, err)
		}

		if res.text != "done" {
			t.Errorf("attempt %d: content = %q, want %q", attempt, res.text, "done")
		}
	}
}
