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
// the transcript captured the full exchange in order: the opening user
// message, the tool call, its result, and the final text — the parts the
// bounded trajectory drops. newTestConversation sets no system prompt, so no
// "system" event is expected here (see TestRunAgentConversationRecordsSystemPrompt).
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

	want := []string{"user", "text", "call", "result", "text"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("transcript event types = %v, want %v", types, want)
	}

	assertTranscriptText(t, res.transcript, 0, "do the thing")
	assertTranscriptText(t, res.transcript, 1, "let me check")
	assertTranscriptText(t, res.transcript, 4, "done")

	if res.transcript[2].Name != "run_shell" || res.transcript[2].Args["command"] != "echo hi" {
		t.Errorf("call event = %+v, want run_shell echo hi", res.transcript[2])
	}

	if res.transcript[3].Name != "run_shell" || !strings.Contains(res.transcript[3].Content, "hi") {
		t.Errorf("result event = %+v, want run_shell output containing %q", res.transcript[3], "hi")
	}
}

// assertTranscriptText checks one recorded event's Text field, pulled out of
// TestRunAgentConversationRecordsTranscript so that test's per-event checks
// don't each cost it a branch.
func assertTranscriptText(t *testing.T, transcript []transcriptEvent, i int, want string) {
	t.Helper()

	if got := transcript[i].Text; got != want {
		t.Errorf("transcript[%d].Text = %q, want %q", i, got, want)
	}
}

// TestRunAgentConversationRecordsSystemPrompt asserts a non-empty system:
// leads the transcript, ahead of the opening user message.
func TestRunAgentConversationRecordsSystemPrompt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{textResponse("done")},
	}

	conv := newTestConversation(t, "do the thing", dir)
	conv.system = "you are a careful reviewer"

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if len(res.transcript) < 2 || res.transcript[0].Type != "system" || res.transcript[1].Type != "user" {
		t.Fatalf("transcript = %+v, want it to lead with a system then a user event", res.transcript)
	}

	if res.transcript[0].Text != conv.system {
		t.Errorf("system event text = %q, want %q", res.transcript[0].Text, conv.system)
	}
}

// TestRunAgentConversationRecordsSyntheticExchanges asserts the
// upstream:/context_paths: synthetic call/result pairs built into the
// opening request — the same content the model is handed — are recorded
// like any other tool exchange, not silently absorbed into the request.
func TestRunAgentConversationRecordsSyntheticExchanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("done")}}

	conv := newTestConversation(t, "do the thing", dir)
	conv.upstream = []contextBlock{{path: "build", content: "verdict: pass"}}
	conv.contextBlocks = []contextBlock{{path: "notes.md", content: "read me"}}

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	types := make([]string, 0, len(res.transcript))
	for _, ev := range res.transcript {
		types = append(types, ev.Type)
	}

	want := []string{"user", "call", "result", "call", "result", "text"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("transcript event types = %v, want %v", types, want)
	}

	if res.transcript[1].Name != readStepToolName || res.transcript[1].Args["step"] != "build" {
		t.Errorf("upstream call event = %+v, want read_step for build", res.transcript[1])
	}

	if res.transcript[2].Content != "verdict: pass" {
		t.Errorf("upstream result event = %+v, want content %q", res.transcript[2], "verdict: pass")
	}

	if res.transcript[3].Name != "read_file" || res.transcript[3].Args["path"] != "notes.md" {
		t.Errorf("context call event = %+v, want read_file for notes.md", res.transcript[3])
	}

	if res.transcript[4].Content != "read me" {
		t.Errorf("context result event = %+v, want content %q", res.transcript[4], "read me")
	}
}

// TestRunAgentConversationRecordsLaterMessages asserts every messages: entry
// past the first is recorded as its own "user" event when advance sends it,
// in order.
func TestRunAgentConversationRecordsLaterMessages(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{textResponse("first done"), textResponse("second done")},
	}

	conv := newTestConversation(t, "first ask", dir)
	conv.messages = []string{"first ask", "second ask"}

	res, err := runAgentConversation(context.Background(), fake, conv)
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	types := make([]string, 0, len(res.transcript))
	texts := make([]string, 0, len(res.transcript))

	for _, ev := range res.transcript {
		types = append(types, ev.Type)
		texts = append(texts, ev.Text)
	}

	wantTypes := []string{"user", "text", "user", "text"}
	if strings.Join(types, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("transcript event types = %v, want %v", types, wantTypes)
	}

	wantTexts := []string{"first ask", "first done", "second ask", "second done"}
	if strings.Join(texts, "|") != strings.Join(wantTexts, "|") {
		t.Fatalf("transcript event texts = %v, want %v", texts, wantTexts)
	}
}

// TestBuildAgentRequestRecordsSystemAndUserOnceAcrossACascade pins the exact
// seam a failover cascade relies on to avoid double-recording: buildAgentRequest
// is the ONLY call site that records the system prompt and opening message,
// and it does so only on the fresh branch — never when conv.resume carries a
// prior source's checkpoint. Since a cascade (failover.go) reuses the same
// recorder across every source it tries, gating recording on conv.resume is
// what keeps the transcript from repeating the opening once per source.
func TestBuildAgentRequestRecordsSystemAndUserOnceAcrossACascade(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	conv := agentConversation{
		system:   "you are a writer",
		messages: []string{"write it"},
		env:      toolEnv{transcript: rec},
		tools:    agentTools{decls: &genai.Tool{}}, //nolint:exhaustruct // zero value tool declarations are fine for this request-shape test
		recorder: rec,
	}

	req := buildAgentRequest(conv)

	want := []string{"system", "user"}
	if got := eventTypes(rec.events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the fresh branch, transcript = %v, want %v", got, want)
	}

	// A cascade swap sets conv.resume from the prior source's checkpoint and
	// calls buildAgentRequest again on the SAME recorder — simulated here
	// directly, the way failover.go's loop does it.
	conv.resume = &resumeCheckpoint{contents: req.Contents}
	buildAgentRequest(conv)

	if got := eventTypes(rec.events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the resumed branch, transcript = %v, want unchanged %v (no re-recording)", got, want)
	}
}

// eventTypes projects a transcript's event Type field, for assertions that
// only care about the shape of the sequence.
func eventTypes(events []transcriptEvent) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, ev.Type)
	}

	return types
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
