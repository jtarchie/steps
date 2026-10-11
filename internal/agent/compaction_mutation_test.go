package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

func TestMaybeCompactExactlyAtTheBudgetIsNotOver(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, strings.Repeat("a", 200)),
		textContent(genai.RoleModel, strings.Repeat("b", 200)),
	}}
	conv := agentConversation{compactAfterTokens: 100}
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary.")}}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, &resumeCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}

	if len(fake.requests) != 0 {
		t.Errorf("made %d LLM calls at exactly compact_after_tokens, want 0: the budget is a ceiling, not a trigger", len(fake.requests))
	}
}

func TestMaybeCompactRecentWindowExactlyAtTheBudgetDoesNotStall(t *testing.T) {
	t.Parallel()

	req := &model.LLMRequest{Contents: []*genai.Content{
		textContent(genai.RoleUser, strings.Repeat("a", 40)),
		textContent(genai.RoleModel, strings.Repeat("b", 400)),
	}}
	conv := agentConversation{compactAfterTokens: 100}
	fake := &fakeLLM{responses: []*model.LLMResponse{textResponse("Summary.")}}
	state := &resumeCheckpoint{}

	err := maybeCompact(context.Background(), fake, req, conv, &reportedSize{}, state)
	if err != nil {
		t.Fatal(err)
	}

	if state.compactions != 1 {
		t.Fatalf("compactions = %d, want 1", state.compactions)
	}

	if state.stalled {
		t.Error("stalled with a recent window of exactly compact_after_tokens, which still fits")
	}
}

func TestFindSplitIndexStopsAtTheTurnThatReachesTheBudget(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		textContent(genai.RoleUser, strings.Repeat("x", 40)),
		textContent(genai.RoleModel, strings.Repeat("x", 40)),
		textContent(genai.RoleUser, strings.Repeat("x", 40)),
	}

	if got := findSplitIndex(contents, 20); got != 1 {
		t.Errorf("findSplitIndex = %d, want 1: the last two turns reach the budget exactly", got)
	}
}

func TestSafeSplitIndexAtTheEdgesIsUnchanged(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		callContent("read_file", nil),
		responseContent("read_file", map[string]any{"ok": true}),
		textContent(genai.RoleModel, "done"),
	}

	if got := safeSplitIndex(contents, 0); got != 0 {
		t.Errorf("safeSplitIndex(0) = %d, want 0: nothing old must not become the leading pair", got)
	}

	if got := safeSplitIndex(contents, len(contents)); got != len(contents) {
		t.Errorf("safeSplitIndex(len) = %d, want %d", got, len(contents))
	}
}

func TestSafeSplitIndexSweepsATrailingDanglingCall(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		callContent("read_file", nil),
		responseContent("read_file", map[string]any{"ok": true}),
		callContent("read_file", nil),
	}

	if got := safeSplitIndex(contents, 2); got != len(contents) {
		t.Errorf("safeSplitIndex = %d, want %d", got, len(contents))
	}
}

func TestBuildSummarizePromptFoldsThePreviousSummaryAndNamesUnknownRoles(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{{Parts: []*genai.Part{{Text: "roleless"}}}}

	first := buildSummarizePrompt(contents, "")
	if strings.Contains(first, "[Previous summary") {
		t.Errorf("a first pass mentioned a previous summary:\n%s", first)
	}

	if !strings.Contains(first, "unknown: roleless\n") {
		t.Errorf("a roleless turn was not rendered as unknown:\n%s", first)
	}

	later := buildSummarizePrompt(contents, "EARLIER FINDINGS")
	if !strings.Contains(later, "[Previous summary for context]\nEARLIER FINDINGS\n[End previous summary]") {
		t.Errorf("a later pass dropped the previous summary:\n%s", later)
	}
}

func TestTruncateForSummaryCutsOnlyPastTheCapAndCountsWhatItCut(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("x", compactionMaxResultBytes)
	if got := truncateForSummary(exact); got != exact {
		t.Errorf("a result of exactly the cap was altered: %q", got[compactionMaxResultBytes:])
	}

	got := truncateForSummary(exact + "0123456789")
	if want := exact + "\n... [truncated 10 bytes]"; got != want {
		t.Errorf("truncated tail = %q, want %q", got[compactionMaxResultBytes:], want[compactionMaxResultBytes:])
	}
}

func TestBuildFallbackSummaryRendersEveryTextTurn(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{
		nil,
		{Parts: []*genai.Part{{Text: "roleless"}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{nil, {Text: ""}, {FunctionCall: &genai.FunctionCall{Name: "x"}}, {Text: strings.Repeat("e", 200)}}},
		textContent(genai.RoleUser, strings.Repeat("l", 201)),
	}

	got := buildFallbackSummary(contents, "PREVIOUS")
	want := "PREVIOUS\n\n---\n\n" +
		"unknown: roleless\n" +
		"model: " + strings.Repeat("e", 200) + "\n" +
		"user: " + strings.Repeat("l", 200) + "...\n"

	if got != want {
		t.Errorf("fallback summary =\n%q\nwant\n%q", got, want)
	}

	if got := buildFallbackSummary(contents[3:], ""); strings.Contains(got, "---") {
		t.Errorf("a first pass carried a previous-summary separator: %q", got)
	}
}

func TestEstimatePartTokensCountsToolCallsAndResults(t *testing.T) {
	t.Parallel()

	call := &genai.Part{FunctionCall: &genai.FunctionCall{
		Name: "abcdefgh",                           // 8 -> 2
		Args: map[string]any{"keyy": "valuevalue"}, // 4 -> 1, 10 -> 2
	}}
	if got := estimatePartTokens(call); got != 5 {
		t.Errorf("call estimate = %d, want 5", got)
	}

	result := &genai.Part{FunctionResponse: &genai.FunctionResponse{
		Name:     "readfile",               // 8 -> 2
		Response: map[string]any{"o": "x"}, // "map[o:x]" 8 -> 2
	}}
	if got := estimatePartTokens(result); got != 4 {
		t.Errorf("result estimate = %d, want 4", got)
	}
}

func TestReportedSizeIgnoresAResponseWithNoPromptCount(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{textContent(genai.RoleUser, strings.Repeat("a", 40))}
	size := reportedSize{tokens: 900, upTo: 1}

	size.observe(&model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{CandidatesTokenCount: 50}}, 1)

	if got := size.of(contents); got != 900 {
		t.Errorf("size = %d, want the previous report 900: a response with no prompt count reported nothing", got)
	}
}

func TestReportedSizeCoveringTheWholeHistoryIsTheReport(t *testing.T) {
	t.Parallel()

	contents := []*genai.Content{textContent(genai.RoleUser, strings.Repeat("a", 40))}
	size := reportedSize{tokens: 900, upTo: len(contents)}

	if got := size.of(contents); got != 900 {
		t.Errorf("size = %d, want 900: the report covers every message", got)
	}
}

// Not t.Parallel(): swaps the default logger.
func TestLogCompactionBudgetSaysWhatIsInForce(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cases := []struct {
		ri       config.ResolvedInvocation
		want     []string
		unwanted []string
	}{
		{
			ri:       config.ResolvedInvocation{AgentName: "a"},
			want:     []string{"level=DEBUG", "compaction=disabled"},
			unwanted: []string{"compact_after_tokens"},
		},
		{
			ri:       config.ResolvedInvocation{AgentName: "a", CompactAfterTokens: 1000, ContextWindow: 200000},
			want:     []string{"level=INFO", "compact_after_tokens=1000", "context_window=200000"},
			unwanted: []string{"assumed_window"},
		},
		{
			ri:   config.ResolvedInvocation{AgentName: "a", CompactAfterTokens: 1000},
			want: []string{"context_window=unknown", "assumed_window="},
		},
	}

	for _, tc := range cases {
		buf.Reset()
		logCompactionBudget(t.Context(), tc.ri)

		line := buf.String()
		for _, w := range tc.want {
			if !strings.Contains(line, w) {
				t.Errorf("%+v: line lacks %q: %s", tc.ri, w, line)
			}
		}

		for _, u := range tc.unwanted {
			if strings.Contains(line, u) {
				t.Errorf("%+v: line has %q: %s", tc.ri, u, line)
			}
		}
	}
}
