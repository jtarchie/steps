package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/redact"
)

// TestRunBudgetedToolMasksSecretsBeforeLogAndSpan: a registered secret a tool returns is masked before the debug log line, the span's status and the model's part are built from the result — read_file's content and a failing tool's error alike.
func TestRunBudgetedToolMasksSecretsBeforeLogAndSpan(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	const secret = "s3cr3t-token-value"

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "creds.txt"), []byte("token="+secret+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	secrets := &redact.Secrets{}
	secrets.Add(secret)
	ctx := redact.WithSecrets(context.Background(), secrets)

	registry := map[string]toolImpl{
		"read_file": execReadFile,
		"fails": func(context.Context, map[string]any, toolEnv) map[string]any {
			return errorResult("fails: rejected token " + secret)
		},
		"boom": func(context.Context, map[string]any, toolEnv) map[string]any {
			panic("boom: token " + secret)
		},
	}

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	calls := []*genai.FunctionCall{
		{ID: "1", Name: "read_file", Args: map[string]any{"path": "creds.txt"}},
		{ID: "2", Name: "fails", Args: map[string]any{}},
	}

	for _, call := range calls {
		encoded := mustJSON(t, executeBudgetedTool(ctx, call, testEnv(dir), registry, nil, map[string]int{}))
		if strings.Contains(encoded, secret) {
			t.Errorf("%s: the result the span and transcript are handed holds the secret: %s", call.Name, encoded)
		}
	}

	if !strings.Contains(buf.String(), "agent.tool_result") {
		t.Fatalf("expected tool_result lines at debug, got: %s", buf.String())
	}

	// Two read-only calls run as a group, where a panic is recovered into the call's result.
	grouped := []*genai.FunctionCall{calls[0], {ID: "3", Name: "boom", Args: map[string]any{}}}
	parts := toolResponseParts(ctx, grouped, testEnv(dir), registry, map[string]bool{"read_file": true, "boom": true}, nil, map[string]int{})

	encoded := mustJSON(t, parts)
	if strings.Contains(encoded, secret) || !strings.Contains(encoded, "token=") || !strings.Contains(encoded, "boom: panic") {
		t.Errorf("the model's parts should hold the masked file and panic, got: %s", encoded)
	}

	if strings.Contains(buf.String(), secret) {
		t.Errorf("the debug and panic log lines hold the secret: %s", buf.String())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	encoded, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(encoded)
}

// TestCLIBridgeMasksSecretsInToolResults: the CLI bridge runs a tool outside runBudgetedTool, so its result is masked on that path too before the span, the fork transcript and the child CLI see it.
func TestCLIBridgeMasksSecretsInToolResults(t *testing.T) {
	t.Parallel()

	const secret = "s3cr3t-token-value"

	secrets := &redact.Secrets{}
	secrets.Add(secret)

	leaky := func(context.Context, map[string]any, toolEnv) map[string]any {
		return map[string]any{"content": "token=" + secret}
	}

	bridge, err := newCLIBridge(
		redact.WithSecrets(t.Context(), secrets),
		bridgeConversation([]*genai.FunctionDeclaration{{Name: "leaky"}}, map[string]toolImpl{"leaky": leaky}, nil),
		nil,
	)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	result, err := dialBridge(t, bridge).CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "leaky"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	encoded := mustJSON(t, result.Content)
	if strings.Contains(encoded, secret) || !strings.Contains(encoded, "token=") {
		t.Errorf("the child CLI was handed %s, want the secret masked", encoded)
	}
}
