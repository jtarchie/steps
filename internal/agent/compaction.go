package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	// compactionRecentRatio is the percentage of conv.compactAfterTokens kept
	// verbatim (the "recent window") when a compaction pass fires; the rest
	// is summarized away. Ported from adk-utils-go's contextguard plugin.
	compactionRecentRatio = 30

	// compactionMaxResultBytes caps how much of a single tool result's text
	// enters the summarization prompt. Sized to survive a spill-file pointer
	// message intact (~2000 bytes: a short header plus a 2000-byte preview,
	// see internal/shell's spillPreviewBytes) with headroom to spare, so a
	// compacted agent can still read_file the file a spilled tool result
	// pointed at — losing the pointer itself would make that unrecoverable.
	compactionMaxResultBytes = 4000
)

// summarizeSystemPrompt is the persona given to the agent's own model when
// it's asked to summarize its own conversation. Adapted from adk-utils-go's
// contextguard plugin, minus its todo-list preservation (steps has no todos
// tool/state) and its dynamic word-limit sentence (that plugin derives a
// limit from a real context-window number via a ModelRegistry; steps has no
// such registry — see maybeCompact's doc comment).
const summarizeSystemPrompt = `You are summarizing a conversation to preserve context for continuing later.

Critical: this summary will be the ONLY context available when the conversation resumes. Assume every message being summarized will be lost. Be thorough.

Required sections:

## Current State

- What was being worked on (the exact request, if apparent)
- Current progress and what has been completed
- What was being addressed right now (incomplete work or an open thread)
- What remains to be done (specific, not vague)

## Key Information

- Facts and specific details mentioned (names, paths, commands, identifiers)
- Findings from tool calls: what files were read, what commands were run, and what they returned
- Decisions made and why; alternatives considered and discarded
- Any blockers, risks, or open questions identified

## Exact Next Steps

Be specific. Don't write "continue with the task" — write exactly what should happen next, with enough detail that someone reading only this summary could pick up without asking questions.

Tone: write as if briefing a colleague taking over mid-conversation.`

// maybeCompact is runAgentConversation's per-turn compaction check (see
// conversation.go). It estimates req.Contents' size in tokens and, once that
// exceeds conv.compactAfterTokens, summarizes everything older than a recent
// window (compactionRecentRatio% of the budget) via the agent's own model,
// replacing the older turns with the summary. summary carries forward the
// running summary from a prior pass (folded into the next one, so multiple
// passes across a long conversation stay coherent); stalled, once true,
// suppresses all further attempts for the rest of this conversation.
//
// stalled exists for one specific case: a single recent turn (e.g. one huge
// tool result) that by itself exceeds the whole budget. Summarizing again
// immediately wouldn't shrink it — replaceSummary keeps the recent window
// verbatim, only the older portion gets truncated (via buildSummarizePrompt's
// compactionMaxResultBytes cap) — so retrying every subsequent turn would
// just pay for LLM calls that cannot fix the actual problem. This does give
// up a compaction opportunity that could in principle appear later (once
// enough further turns accumulate that the oversized turn finally ages out
// of the recent window and gets swept into a future summary), which is a
// known, accepted limitation for a first version: an operator who hits this
// should lower compact_after_tokens or a tool's own output budget, not rely
// on an automatic retry to eventually work around it.
//
// A summarization failure (a transport error, an empty/malformed response)
// is logged and passed through unchanged — matching the codebase's existing
// rule that a tool failure is data the model reacts to, never a reason to
// abort the attempt. maybeCompact only ever reads/mutates req.Contents; it
// never touches turn counting, trajectory, or verdict tracking.
func maybeCompact(ctx context.Context, llm model.LLM, req *model.LLMRequest, conv agentConversation, summary string, stalled bool) (newSummary string, newStalled bool) {
	if stalled {
		return summary, stalled
	}

	if estimateContentTokens(req.Contents) <= conv.compactAfterTokens {
		return summary, stalled
	}

	recentBudget := conv.compactAfterTokens * compactionRecentRatio / 100

	splitIdx := findSplitIndex(req.Contents, recentBudget)
	oldContents := req.Contents[:splitIdx]
	recentContents := req.Contents[splitIdx:]

	if len(oldContents) == 0 {
		// Over budget, but nothing old enough yet to summarize away (a very
		// short conversation whose few turns already fit the recent window).
		// Not stalled: as soon as another turn is appended, req.Contents
		// grows and this is re-evaluated fresh.
		return summary, stalled
	}

	summarized, err := summarizeConversation(ctx, llm, oldContents, summary)
	if err != nil {
		slog.Warn("agent.compaction_failed", "error", err)

		return summary, stalled
	}

	replaceSummary(req, summarized, recentContents)
	injectContinuation(req, conv.prompt)

	slog.Info("agent.compaction", "summarized_turns", len(oldContents), "recent_turns", len(recentContents))

	if estimateContentTokens(recentContents) > conv.compactAfterTokens {
		slog.Warn("agent.compaction_stalled", "reason", "recent window alone exceeds the budget; no further attempts this conversation")

		return summarized, true
	}

	return summarized, false
}

// findSplitIndex walks contents backward, accumulating estimated tokens,
// until recentBudget is reached, then hands the candidate boundary to
// safeSplitIndex so the split never lands mid tool-call/tool-response pair.
// Returns 0 (nothing to compact — the whole conversation already fits the
// recent budget) when contents is empty or the walk never reaches the
// budget.
func findSplitIndex(contents []*genai.Content, recentBudget int) int {
	tokens := 0

	for i := len(contents) - 1; i >= 0; i-- {
		if contents[i] == nil {
			continue
		}

		for _, part := range contents[i].Parts {
			tokens += estimatePartTokens(part)
		}

		if tokens >= recentBudget {
			return safeSplitIndex(contents, i)
		}
	}

	return safeSplitIndex(contents, 0)
}

// safeSplitIndex adjusts a candidate split index so it never lands in the
// middle of a tool-call/tool-response pair. It deviates from the
// adk-utils-go reference this is ported from in two places, both found while
// building this port's own tests:
//
//  1. It always clamps to [0, len(contents)] on entry. The reference returns
//     an out-of-range idx unchanged; its own caller can pass a negative index
//     whenever its recentKeep floor exceeds the current content length, and
//     contents[:idx] on a negative idx panics — reachable here for a small
//     compact_after_tokens.
//  2. It allows idx == len(contents) as a final result. The reference clamps
//     that down to len(contents)-1, which can reintroduce the exact
//     dangling-FunctionCall split this function exists to prevent: when
//     walkForwardToPairBoundary legitimately returns len(contents) (the
//     trailing pair(s) all need to move to "old" together), forcing idx back
//     one splits that last pair's FunctionCall from its FunctionResponse
//     again, right after walking forward had correctly kept them together.
//
// It first tries walking backward to a clean boundary (a plain-text turn, or
// the start of a tool pair); if that regresses all the way to 0, it walks
// forward instead, so a conversation made entirely of tool-call/tool-response
// pairs still finds a valid split rather than collapsing everything into
// "recent."
func safeSplitIndex(contents []*genai.Content, idx int) int {
	if idx < 0 {
		idx = 0
	}

	if idx > len(contents) {
		idx = len(contents)
	}

	if idx <= 0 || idx >= len(contents) {
		return idx
	}

	origIdx := idx

	idx = walkBackToPairBoundary(contents, idx)
	if idx <= 0 {
		idx = walkForwardToPairBoundary(contents, origIdx)
	}

	// Unlike the reference, do not clamp idx == len(contents) down to
	// len(contents)-1 here. walkForwardToPairBoundary can legitimately
	// return len(contents) when the trailing pair(s) all need to move to
	// "old" together — that is a safe, valid split (recentContents ends up
	// empty; everything gets swept into the summary cleanly). The
	// reference's own clamp reintroduces exactly the dangling-FunctionCall
	// split this function exists to prevent: forcing idx back to
	// len(contents)-1 there would split the last pair's FunctionCall from
	// its FunctionResponse again, right after walkForwardToPairBoundary had
	// correctly moved past both together.
	if idx < 0 {
		idx = 0
	}

	if idx > len(contents) {
		idx = len(contents)
	}

	return idx
}

// walkBackToPairBoundary walks backward from idx while it sits in the middle
// of a tool-call/tool-response pair (a model turn with a FunctionCall, or the
// user turn carrying its matching FunctionResponse), returning the adjusted
// index — 0 if it exhausted every preceding turn.
func walkBackToPairBoundary(contents []*genai.Content, idx int) int {
	for idx > 0 {
		c := contents[idx]
		if c == nil {
			break
		}

		if c.Role == genai.RoleUser && contentHasFunctionResponse(c) {
			idx--

			continue
		}

		if c.Role == genai.RoleModel && contentHasFunctionCall(c) {
			idx--

			continue
		}

		break
	}

	return idx
}

// walkForwardToPairBoundary walks forward from idx to the nearest complete
// pair boundary — past a model turn's FunctionCall and its matching user
// FunctionResponse — used when walking backward regressed all the way to 0
// (an all-tool-pairs conversation with no plain-text turn to land on).
func walkForwardToPairBoundary(contents []*genai.Content, idx int) int {
	for idx < len(contents) {
		c := contents[idx]
		if c == nil {
			break
		}

		if c.Role == genai.RoleModel && contentHasFunctionCall(c) {
			idx++

			continue
		}

		if c.Role == genai.RoleUser && contentHasFunctionResponse(c) {
			idx++

			break
		}

		break
	}

	return idx
}

func contentHasFunctionCall(c *genai.Content) bool {
	for _, part := range c.Parts {
		if part != nil && part.FunctionCall != nil {
			return true
		}
	}

	return false
}

func contentHasFunctionResponse(c *genai.Content) bool {
	for _, part := range c.Parts {
		if part != nil && part.FunctionResponse != nil {
			return true
		}
	}

	return false
}

// summarizeConversation asks llm to summarize oldContents (folding in
// previousSummary, if this isn't the first pass) via a fresh, tool-less
// request, reusing generateOnce/collectParts exactly as the main
// conversation loop does for an ordinary turn. An empty response (some
// providers return no text for an unusual request shape) falls back to
// buildFallbackSummary rather than losing the turns being compacted away
// entirely.
func summarizeConversation(ctx context.Context, llm model.LLM, oldContents []*genai.Content, previousSummary string) (string, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{
			{Role: genai.RoleUser, Parts: []*genai.Part{{Text: buildSummarizePrompt(oldContents, previousSummary)}}},
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: summarizeSystemPrompt}}},
		},
	}

	resp, err := generateOnce(ctx, llm, req)
	if err != nil {
		return "", fmt.Errorf("agent: summarize conversation: %w", err)
	}

	_, text := collectParts(resp.Content)
	if text == "" {
		return buildFallbackSummary(oldContents, previousSummary), nil
	}

	return text, nil
}

// buildSummarizePrompt renders oldContents as a transcript for the
// summarizer model. Unlike the adk-utils-go reference this is adapted from —
// which renders a FunctionCall/FunctionResponse as a bare "[called tool: X]"
// / "[tool X returned a result]" label, discarding the actual arguments and
// results — this includes both, truncated to compactionMaxResultBytes per
// result. For a tool-driven coding agent (steps' primary use case: read
// files, run commands, edit code via run_shell/custom tools) the reference's
// label-only rendering would discard everything the agent actually learned,
// producing a summary like "called repo_shell 15 times" with none of what
// those calls returned — worse than not compacting at all, since the agent
// would confidently continue from a summary that preserved none of its
// findings.
func buildSummarizePrompt(contents []*genai.Content, previousSummary string) string {
	var sb strings.Builder

	sb.WriteString("Provide a detailed summary of the following conversation.\n\n")

	if previousSummary != "" {
		sb.WriteString("[Previous summary for context]\n")
		sb.WriteString(previousSummary)
		sb.WriteString("\n[End previous summary]\n\n")
		sb.WriteString("Incorporate the previous summary into your new summary, updating anything that has changed.\n\n")
	}

	sb.WriteString("[Conversation to summarize]\n")

	for _, content := range contents {
		if content == nil {
			continue
		}

		role := content.Role
		if role == "" {
			role = "unknown"
		}

		for _, part := range content.Parts {
			writeSummarizePart(&sb, role, part)
		}
	}

	sb.WriteString("[End of conversation]\n")

	return sb.String()
}

func writeSummarizePart(sb *strings.Builder, role string, part *genai.Part) {
	if part == nil {
		return
	}

	switch {
	case part.Text != "":
		fmt.Fprintf(sb, "%s: %s\n", role, part.Text)
	case part.FunctionCall != nil:
		fmt.Fprintf(sb, "%s: [called %s with args %v]\n", role, part.FunctionCall.Name, part.FunctionCall.Args)
	case part.FunctionResponse != nil:
		result := truncateForSummary(fmt.Sprintf("%v", part.FunctionResponse.Response))
		fmt.Fprintf(sb, "%s: [%s returned: %s]\n", role, part.FunctionResponse.Name, result)
	}
}

// truncateForSummary caps s at compactionMaxResultBytes, matching
// truncateToolOutput's own marker format (internal/agent/tools.go) so a
// truncated result reads the same way here as it would have in the live
// conversation.
func truncateForSummary(s string) string {
	if len(s) <= compactionMaxResultBytes {
		return s
	}

	return s[:compactionMaxResultBytes] + fmt.Sprintf("\n... [truncated %d bytes]", len(s)-compactionMaxResultBytes)
}

// buildFallbackSummary produces a best-effort summary without calling the
// model — used when summarizeConversation's LLM call returns empty text —
// by concatenating each turn's text (capped at 200 chars apiece). Ported
// from the adk-utils-go reference verbatim.
func buildFallbackSummary(contents []*genai.Content, previousSummary string) string {
	var sb strings.Builder

	if previousSummary != "" {
		sb.WriteString(previousSummary)
		sb.WriteString("\n\n---\n\n")
	}

	for _, content := range contents {
		if content == nil {
			continue
		}

		role := content.Role
		if role == "" {
			role = "unknown"
		}

		for _, part := range content.Parts {
			if part == nil || part.Text == "" {
				continue
			}

			text := part.Text
			if len(text) > 200 {
				text = text[:200] + "..."
			}

			fmt.Fprintf(&sb, "%s: %s\n", role, text)
		}
	}

	return sb.String()
}

// replaceSummary rewrites req.Contents to [summary, recentContents...],
// discarding everything older than the split point maybeCompact computed.
func replaceSummary(req *model.LLMRequest, summary string, recentContents []*genai.Content) {
	summaryContent := &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{Text: fmt.Sprintf("[Previous conversation summary]\n%s\n[End of summary — conversation continues below]", summary)},
		},
	}

	req.Contents = append([]*genai.Content{summaryContent}, recentContents...)
}

// injectContinuation appends a message telling the model the conversation
// was just compacted and it should carry on rather than treat the summary as
// a new, unrelated request. prompt, when non-empty (the step's own prompt:,
// via conv.prompt), is echoed back for grounding.
func injectContinuation(req *model.LLMRequest, prompt string) {
	msg := "[System: the conversation was compacted because it exceeded the context budget. " +
		"The summary above contains all prior context. Continue working without asking to repeat anything.]"

	if prompt != "" {
		msg = fmt.Sprintf(
			"[System: the conversation was compacted because it exceeded the context budget. "+
				"The summary above contains all prior context. The original request was: `%s`. "+
				"Continue working on it without asking to repeat anything.]", prompt)
	}

	req.Contents = append(req.Contents, &genai.Content{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: msg}},
	})
}

// estimatePartTokens returns a rough token count for a single Part, using
// the same ~4-chars-per-token heuristic the rest of this feature is built
// on — see maybeCompact's doc comment and docs/agents.md's compaction
// section for why this is a local estimate, never real provider usage data.
// Covers only the Part kinds steps' own conversation loop ever produces
// (Text, FunctionCall, FunctionResponse); the adk-utils-go reference also
// accounts for InlineData/ToolCall/ToolResponse/PartMetadata, none of which
// steps sets anywhere.
func estimatePartTokens(part *genai.Part) int {
	if part == nil {
		return 0
	}

	total := 0

	if part.Text != "" {
		total += len(part.Text) / 4
	}

	if part.FunctionCall != nil {
		total += len(part.FunctionCall.Name) / 4

		for k, v := range part.FunctionCall.Args {
			total += len(k) / 4
			total += len(fmt.Sprintf("%v", v)) / 4
		}
	}

	if part.FunctionResponse != nil {
		total += len(part.FunctionResponse.Name) / 4
		total += len(fmt.Sprintf("%v", part.FunctionResponse.Response)) / 4
	}

	return total
}

// estimateContentTokens sums estimatePartTokens across contents. This is the
// trigger check maybeCompact runs every turn; it deliberately counts
// req.Contents only, not the system prompt or tool schemas also sent with
// every request (see defaultCompactAfterTokens' doc comment in
// internal/config/config.go for why the default budget itself is set well
// under a typical context window to compensate).
func estimateContentTokens(contents []*genai.Content) int {
	total := 0

	for _, content := range contents {
		if content == nil {
			continue
		}

		for _, part := range content.Parts {
			total += estimatePartTokens(part)
		}
	}

	return total
}
