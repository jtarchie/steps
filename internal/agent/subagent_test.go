package agent

import (
	"context"
	"errors"
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

	decls, registry, _, err := buildAgentTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	return preparedSubAgent{
		ri:       config.ResolvedInvocation{AgentName: "extra", MaxTurns: testMaxTurns},
		llm:      fake,
		decls:    decls,
		registry: registry,
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

// TestSubAgentRunTruncatesResult proves a child's oversized final text is
// capped like every other tool result, so it can't flood the parent's context
// past maxToolOutputBytes.
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

	decls, registry, _, err := buildAgentTools(context.Background(), cfg, specs, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	if _, ok := registry["extra"]; !ok {
		t.Fatalf("registry missing sub-agent tool %q: %v", "extra", registry)
	}

	var declared bool

	for _, d := range decls.FunctionDeclarations {
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
	_, err := shell.NewRunner("", t.TempDir())
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
