package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithSessionID(t *testing.T) {
	t.Parallel()

	t.Run("round-trips a valid id", func(t *testing.T) {
		t.Parallel()

		ctx := WithSessionID(t.Context(), "job-abc123")

		got := sessionIDFromContext(ctx)
		if got != "job-abc123" {
			t.Errorf("got %q, want %q", got, "job-abc123")
		}
	})

	t.Run("empty id leaves the context unchanged", func(t *testing.T) {
		t.Parallel()

		got := sessionIDFromContext(WithSessionID(t.Context(), ""))
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("over-long id is dropped rather than truncated", func(t *testing.T) {
		t.Parallel()

		// OpenRouter rejects a session_id past 256 chars; sending an invalid
		// one is worse than sending none at all.
		got := sessionIDFromContext(WithSessionID(t.Context(), strings.Repeat("x", maxSessionIDLen+1)))
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("id at exactly the cap is kept", func(t *testing.T) {
		t.Parallel()

		id := strings.Repeat("x", maxSessionIDLen)

		got := sessionIDFromContext(WithSessionID(t.Context(), id))
		if got != id {
			t.Errorf("id of exactly %d chars was dropped", maxSessionIDLen)
		}
	})

	t.Run("a context with no session id reads empty", func(t *testing.T) {
		t.Parallel()

		got := sessionIDFromContext(t.Context())
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestIsOpenRouterBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"the resolved openrouter provider base url", "https://openrouter.ai/api/v1/", true},
		{"a subdomain", "https://api.openrouter.ai/v1/", true},
		{"openai", "https://api.openai.com/v1/", false},
		{"a local model server", "http://localhost:1234/v1/", false},
		{"empty", "", false},
		// A lookalike host must not be mistaken for OpenRouter — a suffix
		// match without the dot boundary would accept this.
		{"a lookalike host", "https://notopenrouter.ai/v1/", false},
		{"openrouter.ai as a subdomain of another host", "https://openrouter.ai.evil.test/v1/", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := isOpenRouterBaseURL(test.baseURL)
			if got != test.want {
				t.Errorf("isOpenRouterBaseURL(%q) = %v, want %v", test.baseURL, got, test.want)
			}
		})
	}
}

func TestIsAnthropicModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  bool
	}{
		// config.resolveAgentTarget strips the "openrouter/" prefix, so this
		// is the shape the model name actually has by this point.
		{"anthropic/claude-3.5-sonnet", true},
		{"~anthropic/claude-3.5-sonnet", true},
		{"openai/gpt-4o", false},
		{"google/gemini-2.5-pro", false},
		{"", false},
		{"anthropic", false},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			got := isAnthropicModel(test.model)
			if got != test.want {
				t.Errorf("isAnthropicModel(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}

func TestWithCacheControl(t *testing.T) {
	t.Parallel()

	t.Run("anthropic model gets a top-level ephemeral marker", func(t *testing.T) {
		t.Parallel()

		got := withCacheControl([]byte(`{"model":"anthropic/claude-3.5-sonnet","messages":[]}`))

		var doc map[string]any

		err := json.Unmarshal(got, &doc)
		if err != nil {
			t.Fatal(err)
		}

		marker, ok := doc["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("cache_control missing or not an object: %s", got)
		}

		if marker["type"] != "ephemeral" {
			t.Errorf("cache_control.type = %v, want ephemeral", marker["type"])
		}

		// The rest of the body must survive the rewrite.
		if doc["model"] != "anthropic/claude-3.5-sonnet" {
			t.Errorf("model field was lost: %s", got)
		}
	})

	t.Run("non-anthropic model is left byte-identical", func(t *testing.T) {
		t.Parallel()

		body := []byte(`{"model":"openai/gpt-4o","messages":[]}`)

		got := withCacheControl(body)
		if string(got) != string(body) {
			t.Errorf("body was rewritten for a non-anthropic model: %s", got)
		}
	})

	t.Run("unparsable body passes through untouched", func(t *testing.T) {
		t.Parallel()

		body := []byte(`not json at all`)

		got := withCacheControl(body)
		if string(got) != string(body) {
			t.Errorf("got %q, want the original body back", got)
		}
	})

	t.Run("body with no model field passes through untouched", func(t *testing.T) {
		t.Parallel()

		body := []byte(`{"messages":[]}`)

		got := withCacheControl(body)
		if string(got) != string(body) {
			t.Errorf("got %q, want the original body back", got)
		}
	})
}

// capturedRequest is what the fake OpenRouter records about the request the
// transport actually put on the wire.
type capturedRequest struct {
	sessionID string
	body      map[string]any
	path      string
}

// serveCapturing stands up a fake OpenRouter and returns a client wired
// through openRouterTransport plus a pointer to the last captured request.
func serveCapturing(t *testing.T) (*http.Client, string, *capturedRequest) {
	t.Helper()

	captured := &capturedRequest{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}

		captured.sessionID = r.Header.Get("x-session-id")
		captured.path = r.URL.Path
		captured.body = nil

		_ = json.Unmarshal(body, &captured.body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: &openRouterTransport{base: http.DefaultTransport}}

	return client, server.URL, captured
}

// postJSON issues a POST through client and closes the response body.
func postJSON(ctx context.Context, t *testing.T, client *http.Client, url, body string) {
	t.Helper()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)
}

func TestOpenRouterTransportWireMutations(t *testing.T) {
	t.Parallel()

	t.Run("stamps session id and cache_control on an anthropic chat completion", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithSessionID(t.Context(), "job-xyz")

		postJSON(ctx, t, client, base+"/api/v1/chat/completions",
			`{"model":"anthropic/claude-3.5-sonnet","messages":[]}`)

		if captured.sessionID != "job-xyz" {
			t.Errorf("x-session-id = %q, want %q", captured.sessionID, "job-xyz")
		}

		marker, ok := captured.body["cache_control"].(map[string]any)
		if !ok {
			t.Fatalf("cache_control missing from the wire body: %v", captured.body)
		}

		if marker["type"] != "ephemeral" {
			t.Errorf("cache_control.type = %v, want ephemeral", marker["type"])
		}
	})

	t.Run("sends no session header when the context carries none", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)

		postJSON(t.Context(), t, client, base+"/api/v1/chat/completions",
			`{"model":"anthropic/claude-3.5-sonnet","messages":[]}`)

		if captured.sessionID != "" {
			t.Errorf("x-session-id = %q, want no header", captured.sessionID)
		}
	})

	t.Run("session id is stamped for a non-anthropic model but cache_control is not", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithSessionID(t.Context(), "job-xyz")

		postJSON(ctx, t, client, base+"/api/v1/chat/completions",
			`{"model":"openai/gpt-4o","messages":[]}`)

		if captured.sessionID != "job-xyz" {
			t.Errorf("x-session-id = %q, want %q", captured.sessionID, "job-xyz")
		}

		_, present := captured.body["cache_control"]
		if present {
			t.Errorf("cache_control was sent to a non-anthropic model: %v", captured.body)
		}
	})

	t.Run("leaves a non-chat path alone", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithSessionID(t.Context(), "job-xyz")

		postJSON(ctx, t, client, base+"/api/v1/models",
			`{"model":"anthropic/claude-3.5-sonnet"}`)

		if captured.sessionID != "" {
			t.Errorf("x-session-id was stamped on a non-chat request: %q", captured.sessionID)
		}

		_, present := captured.body["cache_control"]
		if present {
			t.Errorf("cache_control was stamped on a non-chat request: %v", captured.body)
		}
	})
}

func TestOpenRouterTransportRequestHandling(t *testing.T) {
	t.Parallel()

	t.Run("does not mutate the caller's request headers", func(t *testing.T) {
		t.Parallel()

		client, base, _ := serveCapturing(t)
		ctx := WithSessionID(t.Context(), "job-xyz")

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			base+"/api/v1/chat/completions",
			strings.NewReader(`{"model":"anthropic/claude-3.5-sonnet"}`))
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()

		// RoundTrip must not modify the request it is handed; the transport
		// mutates a clone, so the caller's own header map stays clean.
		if req.Header.Get("x-session-id") != "" {
			t.Error("transport mutated the caller's request headers")
		}
	})

	t.Run("rewritten body is replayable via GetBody", func(t *testing.T) {
		t.Parallel()

		// openai-go retries a failed request by replaying GetBody. If the
		// rewrite left GetBody pointing at the consumed original, a retry
		// would send an empty body.
		var replayed []byte

		transport := &openRouterTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := req.GetBody()
			if err != nil {
				return nil, fmt.Errorf("GetBody: %w", err)
			}

			defer func() { _ = body.Close() }()

			replayed, err = io.ReadAll(body)
			if err != nil {
				return nil, fmt.Errorf("read replayed body: %w", err)
			}

			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("{}")),
				Header:     make(http.Header),
			}, nil
		})}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			"https://openrouter.ai/api/v1/chat/completions",
			strings.NewReader(`{"model":"anthropic/claude-3.5-sonnet"}`))
		if err != nil {
			t.Fatal(err)
		}

		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}

		defer func() { _ = resp.Body.Close() }()

		if !strings.Contains(string(replayed), "cache_control") {
			t.Errorf("GetBody replayed %q, want the rewritten body", replayed)
		}
	})
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestNewOpenRouterHTTPClient(t *testing.T) {
	t.Parallel()

	t.Run("openrouter base url gets a caching client", func(t *testing.T) {
		t.Parallel()

		got := newOpenRouterHTTPClient("https://openrouter.ai/api/v1/")
		if got == nil {
			t.Fatal("got nil, want a client")
		}

		_, ok := got.Transport.(*openRouterTransport)
		if !ok {
			t.Errorf("transport = %T, want *openRouterTransport", got.Transport)
		}
	})

	t.Run("every other provider gets no client at all", func(t *testing.T) {
		t.Parallel()

		// nil is what keeps non-OpenRouter agents byte-identical to their
		// pre-caching behavior: newAgentLLM leaves HTTPOptions zero and
		// openai-go builds its own client exactly as before.
		for _, baseURL := range []string{
			"https://api.openai.com/v1/",
			"http://localhost:1234/v1/",
			"https://api.groq.com/openai/v1/",
			"",
		} {
			got := newOpenRouterHTTPClient(baseURL)
			if got != nil {
				t.Errorf("newOpenRouterHTTPClient(%q) = %v, want nil", baseURL, got)
			}
		}
	})
}
