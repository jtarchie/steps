package agent

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

func TestAgentResultRecordWritesOnlyWhatTheRunProduced(t *testing.T) {
	t.Parallel()

	bare := agentResultRecord(conversationResult{text: "done", turns: 2})
	if keys := slices.Sorted(maps.Keys(bare)); !slices.Equal(keys, []string{"response", "turns"}) {
		t.Errorf("a plain run recorded %v; want only response and turns", keys)
	}

	full := agentResultRecord(conversationResult{
		text: "done", turns: 2, model: "backup", verdict: "approve", note: "lgtm",
		trajectory: []recordedToolCall{{name: "run_shell", args: map[string]any{"command": "true"}, ok: true}},
	})

	if full["fallback_model"] != "backup" || full["verdict"] != "approve" || full["note"] != "lgtm" {
		t.Errorf("full record = %v; want fallback_model, verdict and note carried", full)
	}

	trajectory, _ := full["trajectory"].([]map[string]any)
	if len(trajectory) != 1 || trajectory[0]["name"] != "run_shell" {
		t.Errorf("trajectory = %v; want the one call", full["trajectory"])
	}
}

func TestTruncateArgsKeepsAnArgumentOfExactlyTheCap(t *testing.T) {
	t.Parallel()

	exact := strings.Repeat("a", maxRecordedArgBytes)
	if got := truncateArgs(map[string]any{"content": exact})["content"]; got != exact {
		t.Error("an argument of exactly the cap was truncated")
	}
}

// TestAFailedConversationReportsItsProviderRequests: the warning is the one place a failed step says how many requests it really spent.
func TestAFailedConversationReportsItsProviderRequests(t *testing.T) {
	logs := captureLogs(t)
	ri := config.ResolvedInvocation{AgentName: "coder"}

	_, err := runOneConversation(context.Background(), ri, &fakeLLM{responses: []*model.LLMResponse{textResponse("ok")}},
		newTestConversation(t, "go", t.TempDir()), noAgentDeadline)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(logs.String(), "agent.conversation_failed") {
		t.Errorf("a successful conversation logged a failure:\n%s", logs)
	}

	_, err = runOneConversation(context.Background(), ri, &fakeLLM{errs: []error{errors.New("provider down")}},
		newTestConversation(t, "go", t.TempDir()), noAgentDeadline)
	if err == nil {
		t.Fatal("want the provider's error")
	}

	if !strings.Contains(logs.String(), "agent.conversation_failed") || !strings.Contains(logs.String(), "provider_requests=") {
		t.Errorf("a failed conversation did not report its requests:\n%s", logs)
	}
}
