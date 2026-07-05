package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeRT records the request it receives and returns a canned response body.
type fakeRT struct {
	gotHeader http.Header
	gotBody   []byte
	respBody  string
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.gotHeader = req.Header.Clone()
	if req.Body != nil {
		f.gotBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(f.respBody)),
	}, nil
}

func chatReq(t *testing.T, model, session string, slot *captureSlot) *http.Request {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "messages": []any{}})
	ctx := WithSessionID(context.Background(), session)
	if slot != nil {
		ctx = context.WithValue(ctx, captureSlotKey{}, slot)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:1234/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Anthropic-routed model: x-session-id set, cache_control injected, cached
// tokens recovered from the response body via the tee.
func TestOpenRouterTransportAnthropic(t *testing.T) {
	base := &fakeRT{respBody: `{"usage":{"prompt_tokens_details":{"cached_tokens":42}}}`}
	tr := &openRouterTransport{base: base}
	slot := &captureSlot{}

	resp, err := tr.RoundTrip(chatReq(t, "anthropic/claude-haiku-4-5", "run-abc", slot))
	if err != nil {
		t.Fatal(err)
	}
	if got := base.gotHeader.Get("x-session-id"); got != "run-abc" {
		t.Errorf("x-session-id = %q, want run-abc", got)
	}
	var doc map[string]any
	_ = json.Unmarshal(base.gotBody, &doc)
	if _, ok := doc["cache_control"]; !ok {
		t.Errorf("anthropic model must get cache_control; body=%s", base.gotBody)
	}
	// The tee only fills the slot once the response body is consumed.
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	slot.parse()
	if slot.cachedTokens != 42 {
		t.Errorf("cachedTokens = %d, want 42 (tee didn't recover it)", slot.cachedTokens)
	}
}

// Non-Anthropic model: session id still set (sticky routing is universal), but
// no cache_control — that's Anthropic-only.
func TestOpenRouterTransportNonAnthropic(t *testing.T) {
	base := &fakeRT{respBody: `{}`}
	tr := &openRouterTransport{base: base}

	resp, err := tr.RoundTrip(chatReq(t, "qwen/qwen3.6-27b", "run-9", nil))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if got := base.gotHeader.Get("x-session-id"); got != "run-9" {
		t.Errorf("x-session-id = %q, want run-9", got)
	}
	var doc map[string]any
	_ = json.Unmarshal(base.gotBody, &doc)
	if _, ok := doc["cache_control"]; ok {
		t.Errorf("non-anthropic model must NOT get cache_control; body=%s", base.gotBody)
	}
}

// Unrelated traffic (not a chat-completions call) passes through untouched.
func TestOpenRouterTransportPassthrough(t *testing.T) {
	base := &fakeRT{respBody: `{}`}
	tr := &openRouterTransport{base: base}

	req, _ := http.NewRequestWithContext(WithSessionID(context.Background(), "run-1"), http.MethodGet, "http://example.com/v1/models", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if base.gotHeader.Get("x-session-id") != "" {
		t.Errorf("unrelated request must not receive x-session-id")
	}
}

func TestWithSessionIDBounds(t *testing.T) {
	if got := sessionIDFromContext(WithSessionID(context.Background(), "")); got != "" {
		t.Errorf("empty id should be a no-op, got %q", got)
	}
	long := strings.Repeat("x", maxSessionIDLen+1)
	if got := sessionIDFromContext(WithSessionID(context.Background(), long)); got != "" {
		t.Errorf("over-long id should be a no-op")
	}
}

func TestRegistryResolvesOpenRouter(t *testing.T) {
	// The ref splits on the FIRST slash: prefix "openrouter", name keeps the
	// nested "qwen/qwen3.6-27b".
	llm, err := NewRegistry().Resolve("openrouter/qwen/qwen3.6-27b")
	if err != nil {
		t.Fatalf("resolve openrouter: %v", err)
	}
	c, ok := llm.(*cachingLLM)
	if !ok {
		t.Fatalf("want *cachingLLM, got %T", llm)
	}
	if c.Name() != "qwen/qwen3.6-27b" {
		t.Errorf("model name = %q, want qwen/qwen3.6-27b", c.Name())
	}
}
