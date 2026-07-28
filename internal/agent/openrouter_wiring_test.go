package agent

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

// TestAgentHTTPClientScopesBySessionAgentName guards the field mapping every
// agent LLM depends on: the session is scoped by the agents: entry's name, not
// by the model. AgentName and ModelName sit next to each other on
// ResolvedInvocation, so a swap compiles cleanly and would silently put two
// different agents that share a model back onto one session — the cross-agent
// pinning this scoping exists to prevent.
func TestAgentHTTPClientScopesBySessionAgentName(t *testing.T) {
	t.Parallel()

	client := agentHTTPClient(config.ResolvedInvocation{
		BaseURL:   "https://openrouter.ai/api/v1/",
		ModelName: "anthropic/claude-3.5-sonnet",
		AgentName: "reviewer",
	})
	if client == nil {
		t.Fatal("got nil, want a caching client for an openrouter base url")
	}

	transport, ok := client.Transport.(*openRouterTransport)
	if !ok {
		t.Fatalf("transport = %T, want *openRouterTransport", client.Transport)
	}

	if transport.agent != "reviewer" {
		t.Errorf("transport.agent = %q, want the AgentName %q (not the model)", transport.agent, "reviewer")
	}
}

// TestAgentHTTPClientSkipsNonOpenRouter guards the layering that is NOT
// installed: a non-OpenRouter provider gets the every-provider repair
// transport with no openRouterTransport on top — the session/cache levers
// stay OpenRouter-only even though repair is universal.
func TestAgentHTTPClientSkipsNonOpenRouter(t *testing.T) {
	t.Parallel()

	client := agentHTTPClient(config.ResolvedInvocation{
		BaseURL:   "https://api.openai.com/v1/",
		ModelName: "gpt-4o",
		AgentName: "reviewer",
	})

	if _, ok := client.Transport.(*openRouterTransport); ok {
		t.Errorf("transport = %T, want no openRouterTransport for a non-OpenRouter base url", client.Transport)
	}

	if _, ok := client.Transport.(*repairTransport); !ok {
		t.Errorf("transport = %T, want *repairTransport for a non-OpenRouter base url", client.Transport)
	}
}

// attemptRecordingLLM records the attempt index carried by the context of
// every GenerateContent call, and fails each call so retry.Do keeps going.
type attemptRecordingLLM struct {
	attempts []int
}

func (l *attemptRecordingLLM) Name() string { return "attempt-recorder" }

func (l *attemptRecordingLLM) GenerateContent(
	ctx context.Context,
	_ *model.LLMRequest,
	_ bool,
) iter.Seq2[*model.LLMResponse, error] {
	l.attempts = append(l.attempts, attemptFromContext(ctx))

	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("attempt-recorder: always fails"))
	}
}

// TestRunPreparedThreadsAttemptIntoContext guards the retry-loop half of the
// session scoping: composeSessionID's own tests prove a differing attempt
// yields a differing session, but only this proves the retry loop actually
// supplies one. Both call sites previously discarded the index as `_ int`, so
// a regression to that shape would otherwise leave every test green while
// pinning all retries to the provider instance that just failed.
func TestRunPreparedThreadsAttemptIntoContext(t *testing.T) {
	t.Parallel()

	llm := &attemptRecordingLLM{}

	_, err := runPrepared(t.Context(), preparedAgentStep{
		ri:  config.ResolvedInvocation{Attempts: 3, MaxTurns: testMaxTurns},
		llm: llm,
		conv: agentConversation{
			system:   "system",
			prompt:   "prompt",
			maxTurns: testMaxTurns,
		},
	})
	if err == nil {
		t.Fatal("expected the always-failing LLM to exhaust every attempt")
	}

	want := []int{0, 1, 2}
	if len(llm.attempts) != len(want) {
		t.Fatalf("recorded attempts %v, want %v", llm.attempts, want)
	}

	for i, got := range llm.attempts {
		if got != want[i] {
			t.Errorf("call %d saw attempt %d, want %d", i, got, want[i])
		}
	}
}
