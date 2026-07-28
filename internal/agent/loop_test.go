package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// repeatedCallResponse builds one turn's worth of "call run_shell with these
// exact args", for loop-detection tests that drive a stuck (or merely
// repetitive) model.
func repeatedCallResponse(command string) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": command}}}},
		},
	}
}

// TestLoopDetectionNudgeThenFail drives a model that makes the identical
// run_shell call every turn — `true` always returns the same empty result,
// so every interaction's signature is identical. The first detection (turn
// loopDetectionMaxRepeats+1) must append the warning; the second must fail
// the attempt as a task failure without reaching maxTurns.
func TestLoopDetectionNudgeThenFail(t *testing.T) {
	t.Parallel()

	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = repeatedCallResponse("true")
	}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "loop forever", t.TempDir()))
	if err == nil {
		t.Fatal("expected the loop detector to fail the stuck conversation")
	}

	if !strings.Contains(err.Error(), "stuck in a loop") {
		t.Errorf("error = %q, want a loop-detection failure", err)
	}

	// Detection fires once at repeat #6 (warning) and again at #7 (fail):
	// the conversation dies two turns before maxTurns would have ended it.
	if want := loopDetectionMaxRepeats + 2; res.turns != want {
		t.Errorf("turns = %d, want %d (warn at %d, fail at %d)", res.turns, want, loopDetectionMaxRepeats+1, want)
	}

	if !sawLoopNudge(fake.requests) {
		t.Error("no request contained the loop warning; the model was failed without being warned first")
	}
}

// TestLoopDetectionDifferentResultsNoTrigger drives the same CALL every
// turn, but with a command whose output changes each time (a counter file):
// the result is part of the interaction signature precisely so this kind of
// productive repetition is NOT mistaken for a stuck loop. The conversation
// should run to maxTurns and die of ordinary turn exhaustion.
func TestLoopDetectionDifferentResultsNoTrigger(t *testing.T) {
	t.Parallel()

	// Prints and increments dir/count, so output differs on every call.
	counter := `n=$(cat count 2>/dev/null || echo 0); n=$((n+1)); echo "$n" | tee count`

	responses := make([]*model.LLMResponse, testMaxTurns)
	for i := range responses {
		responses[i] = repeatedCallResponse(counter)
	}

	fake := &fakeLLM{responses: responses}

	res, err := runAgentConversation(context.Background(), fake, newTestConversation(t, "count up", t.TempDir()))
	if err == nil {
		t.Fatal("expected turn exhaustion, not success")
	}

	if strings.Contains(err.Error(), "stuck in a loop") {
		t.Errorf("productive repetition (same call, changing result) tripped the loop detector: %q", err)
	}

	if res.turns != testMaxTurns {
		t.Errorf("turns = %d, want %d", res.turns, testMaxTurns)
	}

	if sawLoopNudge(fake.requests) {
		t.Error("a loop warning was appended even though every interaction differed")
	}
}

// sawLoopNudge reports whether any request's conversation thread carries the
// loop.go warning text.
func sawLoopNudge(requests []*model.LLMRequest) bool {
	for _, req := range requests {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if strings.Contains(p.Text, "stuck in a loop") {
					return true
				}
			}
		}
	}

	return false
}

// TestLoopSignatureStability pins the two properties the detector relies
// on: identical interactions hash equal (map key order must not matter),
// and a changed result changes the signature.
func TestLoopSignatureStability(t *testing.T) {
	t.Parallel()

	argsA := map[string]any{"command": "true", "timeout": float64(5)}
	argsB := map[string]any{"timeout": float64(5), "command": "true"} // same keys, other order
	result := map[string]any{"exit_code": 0}

	if loopSignature("run_shell", argsA, result) != loopSignature("run_shell", argsB, result) {
		t.Error("same interaction with different map insertion order produced different signatures")
	}

	changed := map[string]any{"exit_code": 1}
	if loopSignature("run_shell", argsA, result) == loopSignature("run_shell", argsA, changed) {
		t.Error("a changed result kept the same signature")
	}

	if loopSignature("run_shell", argsA, result) == loopSignature("read_file", argsA, result) {
		t.Error("a different tool kept the same signature")
	}
}
