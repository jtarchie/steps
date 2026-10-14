package agent

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

// callCountingLLM counts GenerateContent calls and fails every one.
type callCountingLLM struct {
	calls int
}

func (l *callCountingLLM) Name() string { return "call-counter" }

func (l *callCountingLLM) GenerateContent(
	_ context.Context,
	_ *model.LLMRequest,
	_ bool,
) iter.Seq2[*model.LLMResponse, error] {
	l.calls++

	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("call-counter: always fails"))
	}
}

// TestRunPreparedRunsOneConversation pins the removal of whole-conversation
// restart. attempts: 3 used to mean "run this entire conversation three
// times", discarding every accumulated turn between them and re-billing the
// whole history — against a failure the transport had already retried and
// concluded was not transient. It now retries the failing REQUEST (see
// llmclient/requests.go), so the conversation itself runs exactly once no matter what
// attempts: says.
//
// The LLM here is above the transport, so it sees conversations, not requests:
// exactly one call is the whole assertion.
func TestRunPreparedRunsOneConversation(t *testing.T) {
	t.Parallel()

	llm := &callCountingLLM{}

	// An agent with no fallback: entries — the cascade has nowhere to go, so
	// this still pins the same "exactly one conversation" contract runPrepared
	// used to.
	//
	// The agent is a real (empty) one rather than nil: preparation fails
	// outright when an agent cannot be found, so a prepared step never carries
	// a nil one, and a test that fabricates that state is testing a shape
	// production cannot reach.
	_, _, err := runPreparedWithFailover(t.Context(), preparedAgentStep{
		ri:    config.ResolvedInvocation{Attempts: 3, MaxTurns: testMaxTurns},
		agent: &config.Agent{Name: "writer"},
		llm:   llm,
		conv: agentConversation{
			system:   "system",
			messages: []string{"prompt"},
			maxTurns: testMaxTurns,
		},
	})
	if err == nil {
		t.Fatal("expected the always-failing LLM to fail the conversation")
	}

	if llm.calls != 1 {
		t.Errorf("conversation ran %d times, want 1 — attempts: must not restart it", llm.calls)
	}
}
