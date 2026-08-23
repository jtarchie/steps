package agent

import (
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// TestAdvanceKeepsAnExhaustedRequiredToolSatisfied pins the one thing that
// does NOT reset at a message boundary.
//
// max_calls: resets on an attempts: restart and not before, so a tool that is
// both required: and budgeted has nothing left to give on the second message.
// Clearing its satisfaction made finishOrForce force it every turn while
// executeBudgetedTool answered "call budget exhausted", and the step burned
// max_turns failing to do something it had itself forbidden.
func TestAdvanceKeepsAnExhaustedRequiredToolSatisfied(t *testing.T) {
	t.Parallel()

	conv := agentConversation{
		messages: []string{"first", "second"},
		tools:    agentTools{maxCalls: map[string]int{"post_review": 1}},
	}

	state := &resumeCheckpoint{
		satisfied:  map[string]bool{"post_review": true, "verdict": true},
		callCounts: map[string]int{"post_review": 1},
	}

	conv.advance(&model.LLMRequest{Config: &genai.GenerateContentConfig{}}, state)

	if !state.satisfied["post_review"] {
		t.Error("a required tool that has spent its call budget was un-satisfied — the next message can only force what it has already forbidden")
	}

	// Everything else is asked again, which is the whole point of a second
	// message: the verdict must be about the last question.
	if state.satisfied["verdict"] {
		t.Error("the verdict stayed satisfied across a message boundary — the step would route on a decision made about an earlier question")
	}
}
