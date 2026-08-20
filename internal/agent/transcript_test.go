package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestRunAgentConversationRecordsTranscript walks the same two-turn
// conversation as TestRunAgentConversationMultiTurnToolCalling and asserts
// the transcript captured the full exchange in order: the tool call, its
// result, and the final text — the parts the bounded trajectory drops.
func TestRunAgentConversationRecordsTranscript(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{Text: "let me check"},
					{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": "echo hi"}}},
				},
			}},
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "done"}},
			}},
		},
	}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "do the thing", dir))
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	types := make([]string, 0, len(res.transcript))
	for _, ev := range res.transcript {
		types = append(types, ev.Type)
	}

	want := []string{"text", "call", "result", "text"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("transcript event types = %v, want %v", types, want)
	}

	if res.transcript[0].Text != "let me check" {
		t.Errorf("mid-conversation text = %q, want %q", res.transcript[0].Text, "let me check")
	}

	if res.transcript[1].Name != "run_shell" || res.transcript[1].Args["command"] != "echo hi" {
		t.Errorf("call event = %+v, want run_shell echo hi", res.transcript[1])
	}

	if res.transcript[2].Name != "run_shell" || !strings.Contains(res.transcript[2].Content, "hi") {
		t.Errorf("result event = %+v, want run_shell output containing %q", res.transcript[2], "hi")
	}

	if res.transcript[3].Text != "done" {
		t.Errorf("final text = %q, want %q", res.transcript[3].Text, "done")
	}
}

// TestTranscriptRecorderNilSafe covers the contract toolEnv.transcript
// documents: a toolImpl invoked outside a conversation carries no recorder,
// and every method must be a no-op rather than a panic.
func TestTranscriptRecorderNilSafe(t *testing.T) {
	t.Parallel()

	var rec *transcriptRecorder

	rec.text("x")
	rec.call("y", nil)
	rec.results([]*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: "z"}}})
	rec.subagent("a", "b", nil)
}

// TestTranscriptSubagentNesting asserts a delegation lands in the parent
// recorder as one nested event carrying the child's own events.
func TestTranscriptSubagentNesting(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	rec.call("helper", map[string]any{"request": "check it"})
	rec.subagent("helper", "check it", []transcriptEvent{{Type: "text", Text: "child says hi"}})

	if len(rec.events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.events))
	}

	sub := rec.events[1]
	if sub.Type != "subagent" || sub.Agent != "helper" || sub.Request != "check it" {
		t.Fatalf("subagent event = %+v", sub)
	}

	if len(sub.Events) != 1 || sub.Events[0].Text != "child says hi" {
		t.Fatalf("nested events = %+v, want the child's text event", sub.Events)
	}
}

// TestRenderResultContentTruncates pins the persistence cap: one oversized
// tool result must not dominate a stored transcript, and it must say it was
// cut — using the package's own truncation marker rather than a private one.
func TestRenderResultContentTruncates(t *testing.T) {
	t.Parallel()

	content := renderResultContent(map[string]any{"output": strings.Repeat("x", maxRecordedResultBytes*2)})

	// The cap plus one marker's worth of slack: values are bounded before the
	// map is marshaled, so the JSON around them is the only overshoot.
	limit := maxRecordedResultBytes + 2*len("\n... [truncated 999999 bytes]")
	if len(content) > limit {
		t.Fatalf("rendered content length = %d, want ≤ %d", len(content), limit)
	}

	if !strings.Contains(content, "[truncated") {
		t.Fatalf("expected a truncation marker, got tail %q", content[max(0, len(content)-40):])
	}
}

// TestRenderResultContentBoundsTheLargestRealResult is the reason the cap
// moved ahead of the marshal: a 100KB read_file result (the largest read_file
// itself will return) must not be JSON-escaped in full on every turn just to
// keep 4KB of it. The output bound is the observable proxy — the encoder never
// sees more than the already-bounded map.
//
// Which fields survive is NOT asserted: a total cap has always cut whatever
// ran past it, alphabetically-later keys included, and that predates this
// change.
func TestRenderResultContentBoundsTheLargestRealResult(t *testing.T) {
	t.Parallel()

	content := renderResultContent(map[string]any{
		"content": strings.Repeat("y", maxReadFileBytes),
		"path":    "big.go",
	})

	limit := maxRecordedResultBytes + 2*len("\n... [truncated 999999 bytes]")
	if len(content) > limit {
		t.Fatalf("rendered content length = %d, want ≤ %d", len(content), limit)
	}
}

// TestRecorderResultDoesNotReTruncate pins the seam between the two paths that
// feed the recorder. Both bound their own content before handing it over, and
// a second cap here does not shorten anything usefully — it cuts the marker
// the FIRST cap appended and replaces it with one reporting that marker's own
// length, so a 100KB result claimed 25 bytes had been dropped.
func TestRecorderResultDoesNotReTruncate(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	bounded := renderResultContent(map[string]any{"content": strings.Repeat("x", maxReadFileBytes)})

	rec.result("read_file", bounded)

	if len(rec.events) != 1 {
		t.Fatalf("recorded %d events, want 1", len(rec.events))
	}

	got := rec.events[0].Content
	if got != bounded {
		t.Errorf("result re-bounded content it was handed: %d bytes in, %d out", len(bounded), len(got))
	}

	// One marker, naming the real overflow. Two means the second cap ate the
	// first one's and counted itself.
	if n := strings.Count(got, "[truncated"); n != 1 {
		t.Errorf("content carries %d truncation markers, want 1: tail %q", n, got[max(0, len(got)-60):])
	}
}
