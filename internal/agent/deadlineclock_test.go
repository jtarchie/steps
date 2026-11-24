package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func TestCLIClockReadsOncePerTenthAndWarnsInTheLastFifth(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	clock := deadlineClock{start: start, deadline: start.Add(time.Hour)}

	steps := []struct {
		at   time.Duration
		want string
	}{
		{at: time.Minute, want: ""},
		{at: 8 * time.Minute, want: "[clock: 8m0s elapsed, 52m0s left before this step's deadline]"},
		{at: 9 * time.Minute, want: ""},
		{at: 47 * time.Minute, want: "[clock: 47m0s elapsed, 13m0s left before this step's deadline]"},
		{at: 49 * time.Minute, want: "[clock: 49m0s elapsed, 11m0s left before this step's deadline] " + timeoutWarningText},
		{at: 50 * time.Minute, want: ""},
		{at: 55 * time.Minute, want: "[clock: 55m0s elapsed, 5m0s left before this step's deadline]"},
	}

	for _, step := range steps {
		if got := clock.reading(start.Add(step.at)); got != step.want {
			t.Errorf("reading at %s = %q, want %q", step.at, got, step.want)
		}
	}

	var none deadlineClock
	if got := none.reading(start); got != "" {
		t.Errorf("a step with no deadline got a reading: %q", got)
	}
}

func TestCLIBridgeAppendsTheClockToAToolResult(t *testing.T) {
	t.Parallel()

	decl, impl := buildVerdictTool([]string{"approve", "reject"}, false, stepExpectation{})

	bridge, err := newCLIBridge(
		t.Context(),
		bridgeConversation([]*genai.FunctionDeclaration{decl}, map[string]toolImpl{verdictToolName: impl}, map[string]bool{verdictToolName: true}),
		nil,
	)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	bridge.clock = deadlineClock{start: time.Now().Add(-30 * time.Minute), deadline: time.Now().Add(30 * time.Minute)}

	result, err := dialBridge(t, bridge).CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      verdictToolName,
		Arguments: map[string]any{"choice": "approve", "note": "fine"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if len(result.Content) != 2 {
		t.Fatalf("content blocks = %d, want the result and the clock", len(result.Content))
	}

	text, ok := result.Content[1].(*sdkmcp.TextContent)
	if !ok || !strings.HasPrefix(text.Text, "[clock: 30m") {
		t.Errorf("second block = %+v, want a clock reading", result.Content[1])
	}

	if result.IsError {
		t.Error("a clock reading turned a good result into an error")
	}
}

// Real time, as the wrap-up tests: request 2 is checked a tenth of a 2s deadline before any reading is due, request 3 ~500ms in with ~1.5s left against a 1s bar, request 4 under two of the 1.2s request it just made.
func TestAHostedConversationIsToldTheTime(t *testing.T) {
	t.Parallel()

	call := func(id string) *model.LLMResponse {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: id, Name: "run_shell", Args: map[string]any{"command": "true"}}},
		}}}
	}

	fake := &fakeLLM{
		responses: []*model.LLMResponse{call("c1"), call("c2"), call("c3"), call("c4"), textResponse("done")},
		delays:    []time.Duration{0, 500 * time.Millisecond, 1200 * time.Millisecond},
	}

	conv := newTestConversation(t, "do the thing", t.TempDir())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := runAgentConversation(ctx, fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if len(fake.requests) != 5 {
		t.Fatalf("got %d requests, want 5", len(fake.requests))
	}

	if got := readingCount(fake.requests[1]); got != 0 {
		t.Errorf("%d readings before a tenth of the deadline had passed, want none", got)
	}

	if readingCount(fake.requests[2]) == 0 || warningCount(fake.requests[2]) != 0 {
		t.Errorf("third request: %d readings, %d warnings, want a reading and no warning", readingCount(fake.requests[2]), warningCount(fake.requests[2]))
	}

	last := fake.requests[4]
	if got := warningCount(last); got != 1 {
		t.Errorf("%d warnings by the last request, want one", got)
	}

	if got := readingCount(last); got < 2 || got > 4 {
		t.Errorf("%d readings by the last request, want the early one, the warning's, and at most one per tenth between", got)
	}
}

func readingCount(req *model.LLMRequest) (n int) {
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			if strings.HasPrefix(p.Text, "[clock: ") {
				n++
			}
		}
	}

	return n
}
