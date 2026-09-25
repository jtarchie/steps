package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/events"
)

func textContent(role, text string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: text}}}
}

func callContent(name string, args map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: name + "-id", Name: name, Args: args}}},
	}
}

func responseContent(name string, response map[string]any) *genai.Content {
	return &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{Name: name, Response: response}}},
	}
}

func TestFindSplitIndexRespectsRecentBudget(t *testing.T) {
	t.Parallel()

	// 10 turns, each exactly 40 chars of text -> 10 estimated tokens apiece
	// (len/4). A budget of 25 should walk back until accumulated tokens
	// reach/exceed it: turn 9 (10) + turn 8 (10) + turn 7 (10) = 30 >= 25,
	// so the split lands at index 7 (turns 7,8,9 kept as "recent").
	contents := make([]*genai.Content, 10)
	for i := range contents {
		contents[i] = textContent(genai.RoleUser, strings.Repeat("x", 40))
	}

	got := findSplitIndex(contents, 25)
	if got != 7 {
		t.Fatalf("findSplitIndex = %d, want 7", got)
	}

	recentTokens := estimateContentTokens(contents[got:])
	if recentTokens < 25 {
		t.Errorf("recent window tokens = %d, want >= budget (25)", recentTokens)
	}
}

func TestSafeSplitIndexAvoidsToolPairBoundary(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		textContent(genai.RoleModel, "a"),
		textContent(genai.RoleUser, "b"),
		textContent(genai.RoleModel, "c"),
		callContent("repo_shell", map[string]any{"command": "cat foo"}),
		responseContent("repo_shell", map[string]any{"stdout": "package foo"}),
		textContent(genai.RoleModel, "d"),
	}

	// idx=4 would put the call at 3 in "old" and its response at 4 in
	// "recent" -- a dangling function_call. Must walk back past the whole
	// pair to the plain-text turn at 2.
	got := safeSplitIndex(contents, 4)
	if got != 2 {
		t.Fatalf("safeSplitIndex(_, 4) = %d, want 2 (before the tool pair at 3,4)", got)
	}
}

func TestSafeSplitIndexPureToolConversationWalksForward(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		callContent("a", nil),
		responseContent("a", nil),
		callContent("b", nil),
		responseContent("b", nil),
	}

	// idx=1 lands inside the first pair (call@0, response@1). Walking back
	// regresses all the way to 0 (no plain-text turn to land on anywhere),
	// so it must fall forward to the next clean boundary instead: 2.
	got := safeSplitIndex(contents, 1)
	if got != 2 {
		t.Fatalf("safeSplitIndex(_, 1) = %d, want 2 (forward to the next pair boundary)", got)
	}
}

func TestSafeSplitIndexKeepsTrailingPairTogether(t *testing.T) {
	t.Parallel()

	// A trailing tool pair with nothing after it. A candidate idx landing on
	// the response alone (2) must walk forward past the whole pair to 3
	// (len(contents)) -- everything old, nothing recent -- rather than being
	// clamped back to 2, which would split the call at 1 from its response
	// at 2. This is a bug in the adk-utils-go reference this is ported from:
	// its own final clamp (idx >= len(contents) -> len(contents)-1)
	// reintroduces exactly the split safeSplitIndex exists to prevent.
	contents := []*genai.Content{
		textContent(genai.RoleUser, "a"),
		callContent("tool", nil),
		responseContent("tool", nil),
	}

	got := safeSplitIndex(contents, 2)
	if got != len(contents) {
		t.Fatalf("safeSplitIndex(_, 2) = %d, want %d (len(contents) -- the trailing pair must stay together)", got, len(contents))
	}
}

func TestSafeSplitIndexClampsOutOfRange(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		textContent(genai.RoleUser, "a"),
		textContent(genai.RoleModel, "b"),
		textContent(genai.RoleUser, "c"),
	}

	// Regression test for the adk-utils-go reference's own bug: it returns a
	// negative idx unchanged rather than clamping, which panics on
	// contents[:idx]. Confirm this port never does.
	if got := safeSplitIndex(contents, -5); got < 0 {
		t.Errorf("safeSplitIndex(_, -5) = %d, want >= 0", got)
	}

	if got := safeSplitIndex(contents, 100); got > len(contents) {
		t.Errorf("safeSplitIndex(_, 100) = %d, want <= len(contents) (%d)", got, len(contents))
	}

	// Must not panic either way -- reaching these assertions at all is the
	// real test.
	_ = safeSplitIndex(nil, -1)
	_ = safeSplitIndex(nil, 5)
}

func TestBuildSummarizePromptIncludesToolContent(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		callContent("repo_shell", map[string]any{"command": "cat internal/config/config.go"}),
		responseContent("repo_shell", map[string]any{"stdout": "package config\n\ntype Config struct{}"}),
	}

	got := buildSummarizePrompt(contents, "")

	if !strings.Contains(got, "cat internal/config/config.go") {
		t.Error("prompt does not include the tool call's arguments")
	}

	if !strings.Contains(got, "package config") {
		t.Error("prompt does not include the tool result's content")
	}
}

func TestBuildSummarizePromptTruncatesLargeResults(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("y", compactionMaxResultBytes+500)
	contents := []*genai.Content{
		responseContent("repo_shell", map[string]any{"stdout": big}),
	}

	got := buildSummarizePrompt(contents, "")

	if !strings.Contains(got, "truncated") {
		t.Error("prompt does not mark the oversized result as truncated")
	}

	if strings.Count(got, "y") > compactionMaxResultBytes+100 {
		t.Errorf("prompt retained more of the oversized result than the %d-byte budget allows", compactionMaxResultBytes)
	}
}

func TestBuildSummarizePromptPreservesSpillPointer(t *testing.T) {
	t.Parallel()

	// Mirrors internal/shell's spillWriter.resultFromFile format: a short
	// header plus a spillPreviewBytes (2000)-byte preview -- comfortably
	// under compactionMaxResultBytes (4000), so it must survive intact. If
	// it didn't, a compacted agent would lose the path it needs to
	// read_file the spilled output it already gathered.
	pointer := "output too large (150.0 KB); full output saved to /tmp/spill/run_shell-1.txt\n\nfirst 2.0 KB:\n" +
		strings.Repeat("z", 2000)

	contents := []*genai.Content{
		responseContent("repo_shell", map[string]any{"stdout": pointer}),
	}

	got := buildSummarizePrompt(contents, "")

	if !strings.Contains(got, "/tmp/spill/run_shell-1.txt") {
		t.Error("prompt lost the spill file path -- a compacted agent could no longer read_file it")
	}

	if strings.Contains(got, "truncated") {
		t.Error("prompt truncated a spill pointer that should have fit whole")
	}
}

func TestEstimateContentTokens(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		textContent(genai.RoleUser, strings.Repeat("a", 40)),                // 10 tokens
		callContent("tool", map[string]any{"arg": strings.Repeat("b", 20)}), // name(4/4=1) + "arg"(0) + value(20/4=5) ~= 6
	}

	got := estimateContentTokens(contents)
	if got <= 0 {
		t.Fatalf("estimateContentTokens = %d, want > 0", got)
	}

	// Not pinned to an exact number (the heuristic's internals are an
	// implementation detail) -- just confirm it's in the right ballpark and
	// that adding more content strictly increases it.
	more := append(append([]*genai.Content{}, contents...), textContent(genai.RoleModel, strings.Repeat("c", 400)))
	if got2 := estimateContentTokens(more); got2 <= got {
		t.Errorf("adding a large turn did not increase the estimate: %d -> %d", got, got2)
	}
}

func TestReplaceSummaryDiscardsOldContents(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, "old turn 1"),
		textContent(genai.RoleModel, "old turn 2"),
	}}
	recent := []*genai.Content{textContent(genai.RoleUser, "recent turn")}

	replaceSummary(req, "the running summary", recent)

	if len(req.Contents) != 2 {
		t.Fatalf("req.Contents has %d entries, want 2 (summary + 1 recent)", len(req.Contents))
	}

	summaryText := req.Contents[0].Parts[0].Text
	if !strings.Contains(summaryText, "[Previous conversation summary]") {
		t.Error("first content is not marked as a summary")
	}

	if !strings.Contains(summaryText, "the running summary") {
		t.Error("summary content is missing from the rewritten request")
	}

	if req.Contents[1].Parts[0].Text != "recent turn" {
		t.Error("recent content was not preserved verbatim")
	}
}

func TestInjectContinuationIncludesPrompt(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "existing")}}

	injectContinuation(req, "implement story 005")

	last := req.Contents[len(req.Contents)-1]
	if !strings.Contains(last.Parts[0].Text, "implement story 005") {
		t.Error("continuation message does not include the step's prompt")
	}
}

func TestSummarizeConversationFallsBackOnEmptyResponse(t *testing.T) {
	t.Parallel()

	empty := response(40, 0)
	empty.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: ""}}}
	fake := &fakeLLM{responses: []*model.LLMResponse{empty}}
	usage := &stepUsage{}

	oldContents := []*genai.Content{textContent(genai.RoleUser, "something that happened")}

	got, _, err := summarizeConversation(context.Background(), fake, usage, oldContents, "")
	if err != nil {
		t.Fatalf("summarizeConversation: %v", err)
	}

	want := buildFallbackSummary(oldContents, "")
	if got != want {
		t.Errorf("summarizeConversation = %q, want fallback %q", got, want)
	}

	if spent := usage.snapshot().Total; spent != 40 {
		t.Errorf("usage total = %d, want 40 — an empty summary response still cost its tokens", spent)
	}
}

func TestSummarizeConversationWrapsLLMError(t *testing.T) {
	t.Parallel()

	fake := &fakeLLM{errs: []error{errors.New("boom")}}

	_, _, err := summarizeConversation(context.Background(), fake, nil, []*genai.Content{textContent(genai.RoleUser, "x")}, "")
	if err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}

	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want it to wrap the underlying error", err.Error())
	}
}

func TestMaybeCompactNoOpUnderBudget(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "short prompt")}}
	conv := agentConversation{messages: []string{"short prompt"}, compactAfterTokens: 1000}
	fake := &fakeLLM{} // no responses configured -- a call here fails the test via "no more responses"
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if state.summary != "" || state.stalled || state.compactions != 0 {
		t.Errorf("state = %+v, want untouched when under budget", state)
	}

	if len(req.Contents) != 1 {
		t.Errorf("req.Contents was modified despite being under budget: %d entries", len(req.Contents))
	}

	if len(fake.requests) != 0 {
		t.Error("maybeCompact called the LLM despite being under budget")
	}
}

func TestMaybeCompactAlreadyStalledIsANoOp(t *testing.T) {
	t.Parallel()

	// Regardless of req.Contents' actual size, stalled=true must short-circuit
	// before even estimating it -- this is what stops a permanently-oversized
	// recent window from paying for a new LLM call on every remaining turn.
	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, strings.Repeat("x", 100_000))}}
	conv := agentConversation{compactAfterTokens: 10}
	fake := &fakeLLM{}
	state := &resumeCheckpoint{summary: "carried summary", stalled: true}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if state.summary != "carried summary" || !state.stalled {
		t.Errorf("state = (%q, %v), want the summary/stalled passed in left unchanged", state.summary, state.stalled)
	}

	if len(fake.requests) != 0 {
		t.Error("maybeCompact called the LLM despite being already stalled")
	}
}

func TestMaybeCompactSkipsWhenNothingOldEnough(t *testing.T) {
	t.Parallel()

	// A single turn already over budget on its own, with nothing preceding
	// it old enough to summarize away (findSplitIndex has nowhere to cut).
	// Must stay unstalled -- there is nothing structurally wrong here, it
	// just needs one more turn to accumulate before a split is possible.
	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, strings.Repeat("x", 2000))}}
	conv := agentConversation{compactAfterTokens: 10}
	fake := &fakeLLM{}
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if state.summary != "" || state.stalled {
		t.Errorf("state = (%q, %v), want (\"\", false) -- nothing old enough yet, not a permanent stall", state.summary, state.stalled)
	}

	if len(fake.requests) != 0 {
		t.Error("maybeCompact called the LLM despite having nothing old enough to summarize")
	}
}

// compactableRequest is four turns over a 150-token budget; one pass
// summarizes the first two and keeps the last two verbatim.
func compactableRequest() *model.LLMRequest {
	return &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, "the original request"),
		textContent(genai.RoleModel, strings.Repeat("a", 400)),
		textContent(genai.RoleUser, strings.Repeat("b", 400)),
		textContent(genai.RoleModel, "a short recent reply"),
	}}
}

func TestMaybeCompactFiresAndReplacesContents(t *testing.T) {
	t.Parallel()

	req := compactableRequest()
	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 150}
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary: discussed a and b.")}}
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if state.summary == "" {
		t.Error("maybeCompact left an empty summary after a successful pass")
	}

	if state.stalled {
		t.Error("maybeCompact stalled even though the recent window fits comfortably under budget")
	}

	if state.compactions != 1 {
		t.Errorf("compactions = %d, want 1", state.compactions)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("made %d LLM calls, want exactly 1 (the summarization call)", len(fake.requests))
	}

	if !hasTextContaining(req.Contents, "[Previous conversation summary]") {
		t.Error("req.Contents was not rewritten with the summary")
	}

	if !hasTextContaining(req.Contents, "a short recent reply") {
		t.Error("the most recent turn should have survived compaction verbatim")
	}
}

// TestMaybeCompactMarksTheTranscript: from the marker on, the summary is what
// the model had, so the transcript carries it and says what was kept.
func TestMaybeCompactMarksTheTranscript(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 150}
	conv.env.transcript = rec
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary: discussed a and b.")}}

	err := maybeCompact(context.Background(), fake, compactableRequest(), conv, &reportedSize{}, &resumeCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}

	recorded := rec.recorded()
	if len(recorded) != 1 || recorded[0].Type != "compaction" {
		t.Fatalf("transcript = %+v, want one compaction event", recorded)
	}

	if recorded[0].Text != "Summary: discussed a and b." {
		t.Errorf("compaction text = %q, want the summary the model was handed", recorded[0].Text)
	}

	// The last contents are kept verbatim, so the label must not claim every
	// turn above the marker was summarized.
	if want := "compacted: 2 older messages summarized, last 2 kept verbatim"; recorded[0].Name != want {
		t.Errorf("compaction label = %q, want %q", recorded[0].Name, want)
	}
}

func TestCompactionLabelWithNothingKept(t *testing.T) {
	t.Parallel()

	if got, want := compactionLabel(4, 0), "compacted: 4 messages summarized"; got != want {
		t.Errorf("compactionLabel(4, 0) = %q, want %q", got, want)
	}
}

// TestMaybeCompactCountsTheSummaryRequest is #162's bug: the summary is a
// real model call, and its tokens belong in the step's spend. The last-
// response fields must stay the conversation's own, or the summary's
// history-sized total fires a spurious wrap-up nudge.
func TestMaybeCompactCountsTheSummaryRequest(t *testing.T) {
	t.Parallel()

	summary := response(900, 100)
	summary.Content = textResponse("Summary.").Content
	summary.FinishReason = genai.FinishReasonMaxTokens
	summary.ModelVersion = "summarizer-v1"

	usage := &stepUsage{}
	usage.record(&model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: 30},
		FinishReason:  genai.FinishReasonStop,
		ModelVersion:  "turn-v1",
	})

	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 150, usage: usage}
	fake := &fakeLLM{responses: []*model.LLMResponse{summary}}

	err := maybeCompact(context.Background(), fake, compactableRequest(), conv, &reportedSize{}, &resumeCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}

	spent := usage.snapshot()
	if spent.Total != 1030 || spent.Prompt != 900 || spent.Completion != 100 {
		t.Errorf("usage = %+v, want the summary's 1000 tokens counted on top of 30", spent)
	}

	if usage.last != 30 || spent.FinishReason != string(genai.FinishReasonStop) || spent.ModelServed != "turn-v1" {
		t.Errorf("last/finish/served = %d/%q/%q, want the conversation's own last turn untouched", usage.last, spent.FinishReason, spent.ModelServed)
	}

	if !strings.Contains(spent.Raw, "30") || strings.Contains(spent.Raw, "900") {
		t.Errorf("raw = %s, want the conversation's last usage block, not the summary's", spent.Raw)
	}
}

// TestMaybeCompactStopsWhenTheSummaryCrossesTheBudget: a summary always has
// a request behind it, so crossing a ceiling stops the step — and the
// breached summary is not applied, since the model never worked from it.
func TestMaybeCompactStopsWhenTheSummaryCrossesTheBudget(t *testing.T) {
	t.Parallel()

	summary := response(400, 200)
	summary.Content = textResponse("Summary.").Content

	rec := &transcriptRecorder{}
	conv := agentConversation{
		messages: []string{"the original request"}, compactAfterTokens: 150,
		usage: &stepUsage{name: "writer", budget: 500},
	}
	conv.env.transcript = rec
	fake := &fakeLLM{responses: []*model.LLMResponse{summary}}
	req := compactableRequest()
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("err = %v, want a budget breach", err)
	}

	if hasTextContaining(req.Contents, "[Previous conversation summary]") || len(req.Contents) != 4 {
		t.Error("a summary that crossed the budget was applied anyway")
	}

	if state.summary != "" || state.compactions != 0 || len(rec.recorded()) != 0 {
		t.Errorf("state = %+v, transcript = %+v; want nothing recorded for a summary the model never saw", state, rec.recorded())
	}

	if spent := conv.usage.snapshot().Total; spent != 600 {
		t.Errorf("usage total = %d, want 600 — the breaching summary's tokens were still spent", spent)
	}
}

// TestMaybeCompactTransportErrorCountsNothing keeps the failure-is-data
// rule: no usage was reported, so nothing is counted or recorded.
func TestMaybeCompactTransportErrorCountsNothing(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 150, usage: &stepUsage{budget: 1}}
	conv.env.transcript = rec
	fake := &fakeLLM{errs: []error{errors.New("boom")}}
	req := compactableRequest()
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatalf("err = %v, want a failed summary passed through", err)
	}

	if len(req.Contents) != 4 || state.compactions != 0 || state.stalled || len(rec.recorded()) != 0 {
		t.Errorf("contents %d, state %+v, transcript %+v; want nothing changed", len(req.Contents), state, rec.recorded())
	}

	if spent := conv.usage.snapshot().Total; spent != 0 {
		t.Errorf("usage total = %d, want 0", spent)
	}
}

func TestCompactionRecordsATruncatedSummary(t *testing.T) {
	t.Parallel()

	rec := &transcriptRecorder{}
	rec.compaction("label", strings.Repeat("s", maxRecordedResultBytes+100))

	got := rec.recorded()[0].Text
	if len(got) >= maxRecordedResultBytes+100 || !strings.Contains(got, "truncated") {
		t.Errorf("recorded summary is %d bytes, want it capped at %d with a marker", len(got), maxRecordedResultBytes)
	}
}

func TestMaybeCompactStallsWhenRecentAloneExceedsBudget(t *testing.T) {
	t.Parallel()

	// The trailing turn is plain text (not a tool call), so
	// walkBackToPairBoundary's conservative "landed on a call, walk back
	// further" correction never engages -- the split lands exactly on it,
	// leaving it alone in the recent window, alone larger than the whole
	// budget. This is the one case maybeCompact gives up on for the rest of
	// the conversation: summarizing again wouldn't shrink an already-recent,
	// already-verbatim turn.
	req := &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, "the original request"),
		textContent(genai.RoleModel, strings.Repeat("a", 400)),
		textContent(genai.RoleUser, strings.Repeat("b", 400)),
		textContent(genai.RoleModel, strings.Repeat("c", 4000)), // huge trailing turn
	}}
	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 150}
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary: discussed a and b.")}}
	state := &resumeCheckpoint{}

	// A stall is actionable, so it is a step note (which falls back to the
	// context's output when no bus is listening) — once, not every turn.
	var out bytes.Buffer

	ctx := events.WithOutput(context.Background(), events.Output{Stdout: &out})

	err := maybeCompact(ctx, fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if !state.stalled {
		t.Fatal("maybeCompact did not stall despite the recent window alone exceeding the budget")
	}

	if state.summary == "" {
		t.Error("maybeCompact still discarded the summary from the pass that did happen")
	}

	// A second call, now stalled=true, must not make another LLM call.
	err = maybeCompact(ctx, fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if !state.stalled {
		t.Error("stalled did not stay true across a repeated call")
	}

	if len(fake.requests) != 1 {
		t.Errorf("made %d LLM calls across two maybeCompact calls, want exactly 1", len(fake.requests))
	}

	if n := strings.Count(out.String(), "warning: compaction stalled"); n != 1 || !strings.Contains(out.String(), "compact_after_tokens") {
		t.Errorf("notes = %q, want exactly one stall warning naming compact_after_tokens", out.String())
	}
}

func TestReportedSizeAddsTheEstimatedTailToTheReport(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		textContent(genai.RoleUser, strings.Repeat("a", 4000)),
		textContent(genai.RoleModel, "ok"),
		textContent(genai.RoleUser, strings.Repeat("b", 400)),
	}

	var size reportedSize

	if got, want := size.of(contents), estimateContentTokens(contents); got != want {
		t.Errorf("unreported size = %d, want the estimate %d", got, want)
	}

	size.observe(&model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount: 2900, CandidatesTokenCount: 100, ThoughtsTokenCount: 5000, TotalTokenCount: 8000,
	}}, 2)

	if got := size.of(contents); got != 3000+100 {
		t.Errorf("size = %d, want 3100 (prompt+candidates, not thoughts, plus the estimated tail)", got)
	}

	size.observe(&model.LLMResponse{}, 3)

	if got := size.of(contents); got != 3100 {
		t.Errorf("a response without usage moved the size to %d, want the previous report kept at 3100", got)
	}

	if got, want := size.of(contents[:1]), estimateContentTokens(contents[:1]); got != want {
		t.Errorf("size of a history shorter than the report = %d, want the estimate %d", got, want)
	}
}

func TestMaybeCompactUsesTheReportAndClearsIt(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, "the original request"),
		textContent(genai.RoleModel, strings.Repeat("a", 1200)),
		textContent(genai.RoleUser, "go on"),
	}}
	conv := agentConversation{messages: []string{"the original request"}, compactAfterTokens: 1000}
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary.")}}
	size := reportedSize{tokens: 5000, upTo: 2}

	err := maybeCompact(context.Background(), fake, req, conv, &size, &resumeCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("made %d LLM calls, want 1: the reported 5000 is over a budget the estimate is not", len(fake.requests))
	}

	if size != (reportedSize{}) {
		t.Errorf("size after compaction = %+v, want cleared: it described the history just replaced", size)
	}
}
