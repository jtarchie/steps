package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestARequestCarriesManyUpstreamAndContextBlocks(t *testing.T) {
	t.Parallel()

	many := []contextBlock{{path: "a", content: "1"}, {path: "b", content: "2"}, {path: "c", content: "3"}}

	for _, conv := range []agentConversation{
		{messages: []string{"go"}, upstream: many},
		{messages: []string{"go"}, contextBlocks: many},
	} {
		conv.tools = newTestConversation(t, "go", t.TempDir()).tools

		req := buildAgentRequest(conv)
		if got, want := len(req.Contents), 1+len(many)*2; got != want {
			t.Errorf("contents = %d; want the task plus a call and a response per block (%d)", got, want)
		}
	}
}

func TestDebugToolResultPreviewBoundary(t *testing.T) {
	t.Parallel()

	envelope := len(`{"content":""}`)

	fits := map[string]any{"content": strings.Repeat("x", debugToolResultPreviewBytes-envelope)}
	if got := debugToolResultPreview(fits); len(got) != debugToolResultPreviewBytes || strings.Contains(got, "truncated") {
		t.Errorf("a preview of exactly the cap was altered: %q", got)
	}

	over := map[string]any{"content": strings.Repeat("x", debugToolResultPreviewBytes)}
	if got := debugToolResultPreview(over); !strings.HasSuffix(got, fmt.Sprintf("... [truncated %d bytes]", envelope)) {
		t.Errorf("preview should report the %d bytes it dropped: %q", envelope, got)
	}
}
