package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// newTestSubAgent builds a preparedSubAgent backed by fake, with only the
// built-in tools, so tests can drive a child conversation without a real LLM.
func newTestSubAgent(t *testing.T, fake model.LLM) preparedSubAgent {
	t.Helper()

	built, _, err := buildAgentTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	return preparedSubAgent{
		ri:    config.ResolvedInvocation{AgentName: "extra", MaxTurns: testMaxTurns},
		llm:   fake,
		tools: built,
	}
}

func TestSubAgentRunReturnsResult(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "the gist"}}}},
	}}

	child := newTestSubAgent(t, fake)
	env := toolEnv{dir: t.TempDir()}

	got := child.run(context.Background(), map[string]any{"request": "summarize this"}, env)

	if got["result"] != "the gist" {
		t.Errorf("result = %#v, want %q", got["result"], "the gist")
	}

	if _, hasErr := got["error"]; hasErr {
		t.Errorf("unexpected error in result: %#v", got)
	}
}

// TestSubAgentRunPrintsResponse: a sub-agent's own final text must reach the
// terminal (labeled as a sub-agent), not just come back as an opaque tool
// result the parent model consumes — previously the child conversation's
// output was never echoed anywhere a human could see it directly. Not
// t.Parallel(): captureStdout (internal/agent/step_test.go) swaps the
// package-global os.Stdout.
func TestSubAgentRunPrintsResponse(t *testing.T) {
	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "the gist"}}}},
	}}

	child := newTestSubAgent(t, fake)
	env := toolEnv{dir: t.TempDir()}

	output := captureStdout(t, func() {
		child.run(context.Background(), map[string]any{"request": "summarize this"}, env)
	})

	if !strings.Contains(output, "agent: extra (sub-agent)") {
		t.Errorf("stdout = %q, want it to contain %q", output, "agent: extra (sub-agent)")
	}

	if !strings.Contains(output, "the gist") {
		t.Errorf("stdout = %q, want it to contain the child's response %q", output, "the gist")
	}
}

// TestSubAgentRunTruncatesResult proves a child's oversized final text is
// capped like every other tool result, so it can't flood the parent's context
// past maxToolOutputBytes — degrading to a plain truncation marker when no
// spillDir is available (the zero-value toolEnv here).
func TestSubAgentRunTruncatesResult(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", maxToolOutputBytes+5_000)
	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: huge}}}},
	}}

	child := newTestSubAgent(t, fake)

	got := child.run(context.Background(), map[string]any{"request": "dump everything"}, toolEnv{dir: t.TempDir()})

	result, ok := got["result"].(string)
	if !ok {
		t.Fatalf("result = %#v, want a string", got["result"])
	}

	if len(result) >= len(huge) {
		t.Errorf("result was not truncated: len %d, child emitted %d", len(result), len(huge))
	}

	if !strings.Contains(result, "truncated") {
		t.Errorf("truncated result should carry the truncation marker, got tail %q", result[max(0, len(result)-40):])
	}
}

// TestSubAgentRunSpillsOversizedResult proves a child's oversized final text
// is spilled to a file (a <persistent_file> pointer, not a dropped-overflow
// marker) when a spillDir is available.
func TestSubAgentRunSpillsOversizedResult(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", maxToolOutputBytes+5_000)
	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: huge}}}},
	}}

	child := newTestSubAgent(t, fake)

	spillDir := t.TempDir()
	got := child.run(context.Background(), map[string]any{"request": "dump everything"}, toolEnv{dir: t.TempDir(), spillDir: spillDir})

	result, ok := got["result"].(string)
	if !ok {
		t.Fatalf("result = %#v, want a string", got["result"])
	}

	if !strings.Contains(result, "<persistent_file>") {
		t.Errorf("result = %q, want a spill pointer message", result)
	}

	entries, err := os.ReadDir(spillDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = (%v entries, %v), want exactly one spill file", spillDir, entries, err)
	}
}

func TestSubAgentRunMissingRequest(t *testing.T) {
	t.Parallel()

	child := newTestSubAgent(t, &fakeLLM{})
	env := toolEnv{dir: t.TempDir()}

	got := child.run(context.Background(), map[string]any{}, env)

	msg, ok := got["error"].(string)
	if !ok || !strings.Contains(msg, "request") {
		t.Errorf("error = %#v, want it to mention the missing request argument", got["error"])
	}
}

// TestSubAgentRunChildFailureBecomesData proves a child failure (here a
// transport error) comes back to the parent as {"error": ...} data, never a
// Go error that would abort the parent conversation.
func TestSubAgentRunChildFailureBecomesData(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{errs: []error{errors.New("boom")}}

	child := newTestSubAgent(t, fake)
	env := toolEnv{dir: t.TempDir()}

	got := child.run(context.Background(), map[string]any{"request": "do it"}, env)

	if _, hasResult := got["result"]; hasResult {
		t.Errorf("expected an error result, got %#v", got)
	}

	if _, hasErr := got["error"]; !hasErr {
		t.Errorf("child failure should surface as error data, got %#v", got)
	}
}

func TestBuildSubAgentToolNilConfig(t *testing.T) {
	t.Parallel()

	_, _, _, err := buildSubAgentTool(context.Background(), nil, config.ToolSpec{Agent: "extra"})
	if err == nil {
		t.Fatal("expected an error when cfg is nil")
	}
}

// TestBuildAgentToolsWithSubAgent proves the parent's registry ends up with an
// executable sub-agent tool named for the child, built via a real (local, no
// key) resolution — construction only, no conversation.
func TestBuildAgentToolsWithSubAgent(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: []config.Agent{
			{Name: "reviewer", Source: config.AgentSource{Model: "lmstudio/qwen"}},
			{Name: "extra", Source: config.AgentSource{Model: "lmstudio/qwen"}},
		},
	}

	specs := []config.ToolSpec{{Builtin: "read_file"}, {Agent: "extra", Description: "delegate a subtask"}}

	built, _, err := buildAgentTools(context.Background(), cfg, specs, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	if _, ok := built.registry["extra"]; !ok {
		t.Fatalf("built.registry missing sub-agent tool %q: %v", "extra", built.registry)
	}

	var declared bool

	for _, d := range built.decls.FunctionDeclarations {
		if d.Name == "extra" {
			declared = true

			if d.Parameters == nil || d.Parameters.Properties["request"] == nil {
				t.Errorf("sub-agent decl missing the request parameter: %#v", d.Parameters)
			}
		}
	}

	if !declared {
		t.Error("sub-agent tool was not declared to the model")
	}
}

// TestSubAgentUsesChildImageRunner is a smoke check that run builds a runner
// from the child's own image (empty here → host), not the parent's — it must
// not panic and must produce a result via the fake.
func TestSubAgentUsesChildImageRunner(t *testing.T) {
	t.Parallel()

	// Sanity: NewRunner with an empty image is the host runner.
	_, err := shell.NewRunner(shell.RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	fake := &fakeLLM{responses: []*model.LLMResponse{
		{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}}},
	}}
	child := newTestSubAgent(t, fake)

	got := child.run(context.Background(), map[string]any{"request": "x"}, toolEnv{dir: t.TempDir()})
	if got["result"] != "ok" {
		t.Errorf("result = %#v, want ok", got["result"])
	}
}

// TestSubAgentRunChargesBackTheParentsBudget is the regression for a delegation
// silently escaping its parent's ceiling: preparedSubAgent.run builds its own
// *stepUsage with `parent: env.usage` (see the field's own doc comment,
// "charged back up the chain when it finishes"), but that charge-back only
// happens inside stepUsage.finish() — and runConversationLoop stopped calling
// finish() on a caller's behalf once the mid-run fallback: cascade needed to
// call it itself across more than one attempt. Without run's own
// `defer conv.usage.finish()`, a sub-agent's spend never reached its parent's
// `.delegated`, so a capped agent could delegate its way past its own budget
// without ever tripping it — exactly what usage.go's TestDelegatedBudgetDrawsOnTheParent
// pins at the stepUsage level, but nothing exercised through run's actual call
// path until now.
func TestSubAgentRunChargesBackTheParentsBudget(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{responses: []*model.LLMResponse{
		{
			Content:       &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "the gist"}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 500},
		},
	}}

	child := newTestSubAgent(t, fake)
	parent := &stepUsage{budget: 1000, delegateFraction: 1.0}

	child.run(context.Background(), map[string]any{"request": "summarize this"}, toolEnv{dir: t.TempDir(), usage: parent})

	if got := parent.remaining(); got != 500 {
		t.Errorf("parent.remaining() after the sub-agent spent 500/1000 = %d, want 500 — the spend must charge back to the parent when the child's conversation finishes", got)
	}
}
