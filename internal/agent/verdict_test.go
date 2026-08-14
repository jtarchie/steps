package agent

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/shell"
)

// verdictConversation builds a conversation whose only tool is the synthesized
// required verdict tool over the given verdicts, so a fakeLLM can drive it.
func verdictConversation(t *testing.T, dir string, verdicts []string) agentConversation {
	t.Helper()

	built, _, err := buildAgentTools(context.Background(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	verdictTool, err := injectVerdictTool(verdicts, false, built)
	if err != nil {
		t.Fatal(err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return agentConversation{
		prompt:      "judge it",
		env:         toolEnv{dir: dir, runner: runner},
		tools:       built,
		maxTurns:    testMaxTurns,
		verdictTool: verdictTool,
	}
}

func verdictCall(choice string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "v", Name: verdictToolName, Args: map[string]any{"choice": choice}}}},
	}}
}

func finalText(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}}
}

func TestVerdictEmitted(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{responses: []*model.LLMResponse{verdictCall("revise"), finalText("done")}}

	res, err := runAgentConversation(context.Background(), fake, verdictConversation(t, t.TempDir(), []string{"approve", "revise"}))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.verdict != "revise" {
		t.Errorf("verdict = %q, want revise", res.verdict)
	}
}

// TestVerdictLastWins proves a model that calls verdict twice ends on its
// final choice.
func TestVerdictLastWins(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{responses: []*model.LLMResponse{verdictCall("approve"), verdictCall("revise"), finalText("done")}}

	res, err := runAgentConversation(context.Background(), fake, verdictConversation(t, t.TempDir(), []string{"approve", "revise"}))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if res.verdict != "revise" {
		t.Errorf("verdict = %q, want the last choice revise", res.verdict)
	}
}

// TestVerdictNeverEmittedFails proves an agent that never calls the required
// verdict tool exhausts max_turns and fails (routes to to.failure), with an
// empty verdict.
func TestVerdictNeverEmittedFails(t *testing.T) {
	t.Parallel()

	// The model keeps replying with plain text, never calling verdict.
	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = finalText("I won't decide")
	}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, verdictConversation(t, t.TempDir(), []string{"approve", "revise"}))
	if err == nil {
		t.Fatal("expected failure when the required verdict tool is never called")
	}

	if res.verdict != "" {
		t.Errorf("verdict = %q, want empty on a never-emitted run", res.verdict)
	}
}

// TestVerdictToolSchemaEnum proves the synthesized tool constrains choice to
// the declared verdicts via a schema enum.
func TestVerdictToolSchemaEnum(t *testing.T) {
	t.Parallel()

	decl, _ := buildVerdictTool([]string{"approve", "revise", "escalate"}, false)

	choice := decl.Parameters.Properties["choice"]
	if choice == nil {
		t.Fatal("verdict tool missing the choice parameter")
	}

	if len(choice.Enum) != 3 {
		t.Errorf("choice enum = %v, want the 3 declared verdicts", choice.Enum)
	}
}

// TestInjectVerdictToolNameCollision proves a pre-existing tool named "verdict"
// is a conflict rather than silently shadowed.
func TestInjectVerdictToolNameCollision(t *testing.T) {
	t.Parallel()

	taken := agentTools{
		decls:    &genai.Tool{},
		registry: map[string]toolImpl{verdictToolName: func(context.Context, map[string]any, toolEnv) map[string]any { return nil }},
		required: map[string]bool{},
	}

	_, err := injectVerdictTool([]string{"approve"}, false, taken)
	if err == nil {
		t.Error("expected a conflict error for a pre-existing verdict tool")
	}
}
