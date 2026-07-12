package main

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
	"gopkg.in/yaml.v3"
)

// fakeLLM is an in-process model.LLM stand-in: responses[i] (or errs[i], if
// set) is returned for the i-th call. It records every request it receives
// so tests can assert on conversation-thread contents.
type fakeLLM struct {
	responses []*model.LLMResponse
	errs      []error
	calls     int
	requests  []*model.LLMRequest
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		i := f.calls
		f.calls++
		f.requests = append(f.requests, req)

		if i < len(f.errs) && f.errs[i] != nil {
			yield(nil, f.errs[i])

			return
		}

		if i < len(f.responses) {
			yield(f.responses[i], nil)

			return
		}

		yield(nil, errors.New("fakeLLM: no more responses configured"))
	}
}

func TestRunAgentConversationMultiTurnToolCalling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	fake := &fakeLLM{
		responses: []*model.LLMResponse{
			{Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": "echo hi"}}},
				},
			}},
			{Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "done"}},
			}},
		},
	}

	decls, registry, err := buildAgentTools(nil)
	if err != nil {
		t.Fatal(err)
	}

	content, turns, err := runAgentConversation(context.Background(), fake, "do the thing", dir, agentTools{decls: decls, registry: registry})
	if err != nil {
		t.Fatalf("runAgentConversation: %v", err)
	}

	if content != "done" {
		t.Errorf("content = %q, want %q", content, "done")
	}

	if turns != 2 {
		t.Errorf("turns = %d, want 2", turns)
	}

	if len(fake.requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(fake.requests))
	}

	if !hasFunctionResponseNamed(fake.requests[1].Contents, "run_shell") {
		t.Error("expected the second request to include a FunctionResponse for run_shell")
	}
}

// hasFunctionResponseNamed reports whether any part across contents is a
// FunctionResponse for the named tool.
func hasFunctionResponseNamed(contents []*genai.Content, name string) bool {
	for _, c := range contents {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return true
			}
		}
	}

	return false
}

func TestRunAgentConversationExceedsMaxTurns(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	toolCallResp := &model.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call1", Name: "run_shell", Args: map[string]any{"command": "true"}}}},
		},
	}

	responses := make([]*model.LLMResponse, maxAgentTurns)
	for i := range responses {
		responses[i] = toolCallResp
	}

	fake := &fakeLLM{responses: responses}

	decls, registry, err := buildAgentTools(nil)
	if err != nil {
		t.Fatal(err)
	}

	_, turns, err := runAgentConversation(context.Background(), fake, "loop forever", dir, agentTools{decls: decls, registry: registry})
	if err == nil {
		t.Fatal("expected an error when the model never stops calling tools")
	}

	if turns != maxAgentTurns {
		t.Errorf("turns = %d, want %d", turns, maxAgentTurns)
	}
}

func TestWithRetryRetriesAgentConversationOnFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	decls, registry, err := buildAgentTools(nil)
	if err != nil {
		t.Fatal(err)
	}

	tools := agentTools{decls: decls, registry: registry}

	fake := &fakeLLM{
		errs: []error{errors.New("transient failure")},
		responses: []*model.LLMResponse{
			nil,
			{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}}},
		},
	}

	var finalContent string

	err = withRetry(context.Background(), 3, func(_ int) error {
		content, _, runErr := runAgentConversation(context.Background(), fake, "hi", dir, tools)
		if runErr != nil {
			return runErr
		}

		finalContent = content

		return nil
	})
	if err != nil {
		t.Fatalf("withRetry: %v", err)
	}

	if finalContent != "ok" {
		t.Errorf("finalContent = %q, want %q", finalContent, "ok")
	}

	if fake.calls != 2 {
		t.Errorf("fake.calls = %d, want 2", fake.calls)
	}
}

func TestWithRetryFailsAfterExhaustingAttempts(t *testing.T) {
	t.Parallel()

	calls := 0

	err := withRetry(context.Background(), 2, func(_ int) error {
		calls++

		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}

	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// TestRunJobAgentNeverSkipped is the one test that goes through a real HTTP
// round trip (via httptest.Server), since runAgentStep constructs its
// OpenAI-compatible client internally rather than accepting an injectable
// one. It mirrors TestRunJobPutNeverSkipped's intent, using an in-memory
// request counter since HTTP calls aren't file-shaped like that test's
// shell-builtin counters. Not run with t.Parallel(): it uses t.Setenv,
// which panics if called after a parallel test has started.
func TestRunJobAgentNeverSkipped(t *testing.T) {
	var calls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"id": "test-completion",
			"object": "chat.completion",
			"created": 0,
			"model": "test-model",
			"choices": [{"index": 0, "finish_reason": "stop", "logprobs": null, "message": {"role": "assistant", "content": "done"}}]
		}`)
	}))
	defer server.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := fmt.Sprintf(`
agents:
- name: reviewer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - agent: reviewer
    prompt: hello
`, server.URL)

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if calls != 1 {
		t.Errorf("calls after first run = %d, want 1", calls)
	}

	mustRun(t, path)

	if calls != 2 {
		t.Errorf("calls after second run = %d, want 2 (agent steps are never skip-cached)", calls)
	}
}

func TestResolveAgentTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		source          AgentSource
		wantBaseURL     string
		wantModel       string
		wantAPIKeyEnv   string
		wantRequiresKey bool
		wantErr         bool
	}{
		{ //nolint:gosec // wantAPIKeyEnv values are env-var *names*, not credential values
			name:            "openrouter prefix with slashed model id",
			source:          AgentSource{Model: "openrouter/anthropic/claude-3.5-sonnet"},
			wantBaseURL:     "https://openrouter.ai/api/v1/",
			wantModel:       "anthropic/claude-3.5-sonnet",
			wantAPIKeyEnv:   "OPENROUTER_API_KEY",
			wantRequiresKey: true,
		},
		{
			name:            "lmstudio prefix requires no key",
			source:          AgentSource{Model: "lmstudio/qwen2.5-coder"},
			wantBaseURL:     "http://localhost:1234/v1/",
			wantModel:       "qwen2.5-coder",
			wantAPIKeyEnv:   "",
			wantRequiresKey: false,
		},
		{
			name:    "bare model with no endpoint errors",
			source:  AgentSource{Model: "gpt-4o"},
			wantErr: true,
		},
		{
			name:            "explicit endpoint and api_key_env override derived values",
			source:          AgentSource{Model: "openrouter/anthropic/claude-3.5-sonnet", Endpoint: "https://gateway.internal/v1", APIKeyEnv: "CUSTOM_KEY"},
			wantBaseURL:     "https://gateway.internal/v1/",
			wantModel:       "anthropic/claude-3.5-sonnet",
			wantAPIKeyEnv:   "CUSTOM_KEY",
			wantRequiresKey: true,
		},
		{
			name:            "unrecognized prefix requires explicit endpoint",
			source:          AgentSource{Model: "foo/bar", Endpoint: "https://foo.example/v1/"},
			wantBaseURL:     "https://foo.example/v1/",
			wantModel:       "foo/bar",
			wantAPIKeyEnv:   "",
			wantRequiresKey: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			baseURL, modelName, apiKeyEnv, requiresKey, err := resolveAgentTarget(tt.source)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveAgentTarget: %v", err)
			}

			if baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", baseURL, tt.wantBaseURL)
			}

			if modelName != tt.wantModel {
				t.Errorf("modelName = %q, want %q", modelName, tt.wantModel)
			}

			if apiKeyEnv != tt.wantAPIKeyEnv {
				t.Errorf("apiKeyEnv = %q, want %q", apiKeyEnv, tt.wantAPIKeyEnv)
			}

			if requiresKey != tt.wantRequiresKey {
				t.Errorf("requiresKey = %v, want %v", requiresKey, tt.wantRequiresKey)
			}
		})
	}
}

func TestLookupAPIKey(t *testing.T) {
	t.Run("required and set succeeds", func(t *testing.T) {
		t.Setenv("STEPS_TEST_KEY_1", "secret")

		got, err := lookupAPIKey("STEPS_TEST_KEY_1", true)
		if err != nil {
			t.Fatal(err)
		}

		if got != "secret" {
			t.Errorf("got %q, want %q", got, "secret")
		}
	})

	t.Run("required and unset errors", func(t *testing.T) {
		_, err := lookupAPIKey("STEPS_TEST_KEY_DOES_NOT_EXIST", true)
		if err == nil {
			t.Error("expected an error for a required but unset env var")
		}
	})

	t.Run("required with empty envVar name errors", func(t *testing.T) {
		_, err := lookupAPIKey("", true)
		if err == nil {
			t.Error("expected an error for a required key with no envVar name")
		}
	})

	t.Run("not required and unset returns empty, no error", func(t *testing.T) {
		got, err := lookupAPIKey("", false)
		if err != nil {
			t.Fatal(err)
		}

		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})
}

func TestBuildAgentToolsBuiltins(t *testing.T) {
	t.Parallel()

	t.Run("empty specs enables all built-ins", func(t *testing.T) {
		t.Parallel()

		decls, registry, err := buildAgentTools(nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 3 {
			t.Errorf("got %d declarations, want 3", len(decls.FunctionDeclarations))
		}

		for _, name := range []string{"read_file", "list_dir", "run_shell"} {
			if _, ok := registry[name]; !ok {
				t.Errorf("registry missing %q", name)
			}
		}
	})

	t.Run("selecting a subset omits the rest", func(t *testing.T) {
		t.Parallel()

		decls, registry, err := buildAgentTools([]ToolSpec{{Builtin: "read_file"}})
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 1 {
			t.Errorf("got %d declarations, want 1", len(decls.FunctionDeclarations))
		}

		if _, ok := registry["run_shell"]; ok {
			t.Error("run_shell should not be registered when not selected")
		}
	})

	t.Run("unknown builtin errors", func(t *testing.T) {
		t.Parallel()

		_, _, err := buildAgentTools([]ToolSpec{{Builtin: "nope"}})
		if err == nil {
			t.Error("expected an error for an unknown builtin tool")
		}
	})
}

func TestBuildAgentToolsCustom(t *testing.T) {
	t.Parallel()

	t.Run("custom tool infers params from its run template", func(t *testing.T) {
		t.Parallel()

		decls, registry, err := buildAgentTools([]ToolSpec{
			{Name: "post_review", Description: "post a review", Run: `gh pr review {{ .args.action }} -b "{{ .args.body }}"`},
		})
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 1 {
			t.Fatalf("got %d declarations, want 1", len(decls.FunctionDeclarations))
		}

		decl := decls.FunctionDeclarations[0]
		if decl.Name != "post_review" {
			t.Errorf("name = %q, want post_review", decl.Name)
		}

		for _, name := range []string{"action", "body"} {
			if _, ok := decl.Parameters.Properties[name]; !ok {
				t.Errorf("missing inferred param %q", name)
			}
		}

		if len(decl.Parameters.Properties) != 2 {
			t.Errorf("got %d params, want 2", len(decl.Parameters.Properties))
		}

		if _, ok := registry["post_review"]; !ok {
			t.Error("registry missing post_review")
		}
	})

	t.Run("duplicate tool name errors", func(t *testing.T) {
		t.Parallel()

		_, _, err := buildAgentTools([]ToolSpec{{Builtin: "read_file"}, {Name: "read_file", Run: "echo hi"}})
		if err == nil {
			t.Error("expected an error for a duplicate tool name")
		}
	})

	t.Run("custom tool missing name or run errors", func(t *testing.T) {
		t.Parallel()

		_, _, err := buildAgentTools([]ToolSpec{{Description: "no name or run"}})
		if err == nil {
			t.Error("expected an error for a custom tool with no name/run")
		}
	})
}

func TestExecCustomTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	impl := execCustomTool(ToolSpec{Name: "greet", Run: `echo "hello {{ .args.name }}"`})

	t.Run("renders args and shells out", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{"name": "world"}, dir)
		if result["error"] != nil {
			t.Fatalf("unexpected error: %v", result["error"])
		}

		if stdout, _ := result["stdout"].(string); stdout != "hello world\n" {
			t.Errorf("stdout = %q, want %q", stdout, "hello world\n")
		}
	})

	t.Run("missing required arg yields an error map, not a Go error", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{}, dir)
		if result["error"] == nil {
			t.Error("expected an \"error\" key in the result map")
		}
	})
}

func TestTruncateToolOutput(t *testing.T) {
	t.Parallel()

	t.Run("short output is unchanged", func(t *testing.T) {
		t.Parallel()

		if got := truncateToolOutput("hello"); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("oversized output is capped with a marker", func(t *testing.T) {
		t.Parallel()

		big := strings.Repeat("x", maxToolOutputBytes+500)

		got := truncateToolOutput(big)
		if len(got) <= maxToolOutputBytes {
			t.Errorf("truncated length %d should exceed the cap only by the marker", len(got))
		}

		if !strings.HasPrefix(got, strings.Repeat("x", maxToolOutputBytes)) {
			t.Error("expected the first maxToolOutputBytes to be preserved")
		}

		if !strings.Contains(got, "truncated 500 bytes") {
			t.Errorf("expected a truncation marker, got tail %q", got[len(got)-40:])
		}
	})
}

func TestToolSpecUnmarshalYAML(t *testing.T) {
	t.Parallel()

	const doc = `
tools:
- read_file
- name: post_review
  description: post a review
  run: gh pr review {{ .args.action }}
`

	var v struct {
		Tools []ToolSpec `yaml:"tools"`
	}

	err := yaml.Unmarshal([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}

	if len(v.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(v.Tools))
	}

	if v.Tools[0].Builtin != "read_file" {
		t.Errorf("Tools[0].Builtin = %q, want %q", v.Tools[0].Builtin, "read_file")
	}

	if v.Tools[1].Name != "post_review" || v.Tools[1].Run != "gh pr review {{ .args.action }}" {
		t.Errorf("Tools[1] = %+v, want a custom post_review tool", v.Tools[1])
	}
}

func TestToolSpecUnmarshalYAMLInvalid(t *testing.T) {
	t.Parallel()

	const doc = `
tools:
- [not, a, valid, entry]
`

	var v struct {
		Tools []ToolSpec `yaml:"tools"`
	}

	err := yaml.Unmarshal([]byte(doc), &v)
	if err == nil {
		t.Error("expected an error for a sequence-node tool entry")
	}
}

func TestResolveAgentPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	t.Run("relative path within dir resolves", func(t *testing.T) {
		t.Parallel()

		got, err := resolveAgentPath(dir, "sub/file.txt")
		if err != nil {
			t.Fatal(err)
		}

		if want := filepath.Join(dir, "sub/file.txt"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAgentPath(dir, "/etc/passwd")
		if err == nil {
			t.Error("expected an error for an absolute path")
		}
	})

	t.Run("traversal outside dir rejected", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAgentPath(dir, "../../etc/passwd")
		if err == nil {
			t.Error("expected an error for a path escaping dir")
		}
	})
}

func TestExecReadFileAndListDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("read_file returns content", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "a.txt"}, dir)
		if result["content"] != "hello" {
			t.Errorf("content = %v, want %q", result["content"], "hello")
		}
	})

	t.Run("read_file rejects traversal", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "../../etc/passwd"}, dir)
		if result["error"] == nil {
			t.Error("expected an error for a traversal path")
		}
	})

	t.Run("read_file requires path", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{}, dir)
		if result["error"] == nil {
			t.Error("expected an error for a missing path argument")
		}
	})

	t.Run("list_dir defaults to the working directory", func(t *testing.T) {
		t.Parallel()

		result := execListDir(context.Background(), map[string]any{}, dir)

		entries, ok := result["entries"].([]map[string]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("entries = %v", result["entries"])
		}
	})
}

func TestFindAgent(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "openai/gpt-4o"}}}}

	_, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Errorf("FindAgent(reviewer): %v", err)
	}

	_, err = cfg.FindAgent("nope")
	if err == nil {
		t.Error("expected an error for a missing agent")
	}
}
