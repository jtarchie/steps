package agent

import (
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

func TestCLIClockReadsOncePerTenthAndWarnsInTheLastFifth(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	clock := cliClock{start: start, deadline: start.Add(time.Hour)}

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

	var none cliClock
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

	bridge.clock = cliClock{start: time.Now().Add(-30 * time.Minute), deadline: time.Now().Add(30 * time.Minute)}

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
