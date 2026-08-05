package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestWithNewRun(t *testing.T) {
	t.Parallel()

	t.Run("carries the job name and a unique token", func(t *testing.T) {
		t.Parallel()

		first := runIDFromContext(WithNewRun(t.Context(), "build"))
		if !strings.HasPrefix(first, "build-") {
			t.Errorf("got %q, want a %q prefix", first, "build-")
		}

		second := runIDFromContext(WithNewRun(t.Context(), "build"))
		if first == second {
			t.Errorf("two runs of the same job produced the same run id %q", first)
		}
	})

	t.Run("a context with no run reads empty", func(t *testing.T) {
		t.Parallel()

		got := runIDFromContext(t.Context())
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("an empty job name still yields a usable run id", func(t *testing.T) {
		t.Parallel()

		got := runIDFromContext(WithNewRun(t.Context(), ""))
		if strings.TrimPrefix(got, "-") == "" {
			t.Errorf("got %q, want a non-empty random token", got)
		}
	})
}

// isHeaderSafeRune mirrors the unreserved set sanitizeLabel is allowed to
// emit, so a composed session id is always a legal HTTP header value.
func isHeaderSafeRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}

func TestSanitizeLabel(t *testing.T) {
	t.Parallel()

	t.Run("drops characters illegal in an HTTP header", func(t *testing.T) {
		t.Parallel()

		// Job and agent names are free-form YAML: spaces and non-ASCII are
		// legal there but not in a header value.
		got := sanitizeLabel("deploy to staging ✨")

		for _, r := range got {
			if !isHeaderSafeRune(r) {
				t.Errorf("sanitized label %q still contains %q", got, r)
			}
		}
	})

	t.Run("bounds length", func(t *testing.T) {
		t.Parallel()

		got := sanitizeLabel(strings.Repeat("a", maxLabelLen*3))
		if len(got) > maxLabelLen {
			t.Errorf("label is %d chars, over the %d bound", len(got), maxLabelLen)
		}
	})
}

func TestComposeSessionID(t *testing.T) {
	t.Parallel()

	t.Run("scopes a run to the agent", func(t *testing.T) {
		t.Parallel()

		got := composeSessionID("build-TOKEN", "reviewer")
		if got != "build-TOKEN-reviewer" {
			t.Errorf("got %q, want %q", got, "build-TOKEN-reviewer")
		}
	})

	t.Run("two agents in one run get different sessions", func(t *testing.T) {
		t.Parallel()

		// Different agents: entries mean different models and personas, so
		// they share no cacheable prefix and must not share a provider pin —
		// a router model would otherwise resolve once and stick for the job.
		first := composeSessionID("build-TOKEN", "writer")
		second := composeSessionID("build-TOKEN", "critic")

		if first == second {
			t.Errorf("two agents shared session %q", first)
		}
	})

	t.Run("one agent re-entered in a run keeps its session", func(t *testing.T) {
		t.Parallel()

		// A to:/verdicts: revise loop, a repeatedly-called sub-agent, and a
		// retrying fix: agent all reuse one prefix; per-invocation scoping
		// would fragment exactly the case caching pays off in.
		first := composeSessionID("build-TOKEN", "writer")
		second := composeSessionID("build-TOKEN", "writer")

		if first != second {
			t.Errorf("same agent got %q then %q", first, second)
		}
	})

	t.Run("two runs of one agent get different sessions", func(t *testing.T) {
		t.Parallel()

		first := composeSessionID("build-TOKEN1", "writer")
		second := composeSessionID("build-TOKEN2", "writer")

		if first == second {
			t.Errorf("two runs shared session %q", first)
		}
	})

	t.Run("no run id disables the session entirely", func(t *testing.T) {
		t.Parallel()

		got := composeSessionID("", "reviewer")
		if got != "" {
			t.Errorf("got %q, want empty for an agent run outside a job", got)
		}
	})

	t.Run("stays within OpenRouter's cap for absurd names", func(t *testing.T) {
		t.Parallel()

		// Over the cap the header is rejected outright, silently disabling
		// caching — no pipeline may be able to name its way there.
		got := composeSessionID(
			sanitizeLabel(strings.Repeat("job", 500))+"-TOKEN",
			strings.Repeat("agent", 500),
		)

		if len(got) > maxSessionIDLen {
			t.Errorf("session id is %d chars, over the %d cap", len(got), maxSessionIDLen)
		}
	})
}

// TestComposeSessionIDIsStableAcrossRetries pins the inversion that came with
// redefining attempts:. The session used to carry a retry-attempt suffix,
// deliberately breaking the provider pin so a restarted conversation would not
// land on the instance that had just failed — cheap, because a restart threw
// the history away and could only ever have reused the short prefix.
//
// attempts: now retries the failing REQUEST and CONTINUES the same
// conversation, so the accumulated prefix is precisely what a retry wants to
// reuse. The session must therefore stay put.
func TestComposeSessionIDIsStableAcrossRetries(t *testing.T) {
	t.Parallel()

	t.Run("a session is fully determined by run and agent", func(t *testing.T) {
		t.Parallel()

		got := composeSessionID("build-TOKEN", "writer")
		if got != "build-TOKEN-writer" {
			t.Errorf("got %q, want %q — nothing but the run and the agent may enter a session", got, "build-TOKEN-writer")
		}
	})

	t.Run("every request of a conversation shares one session", func(t *testing.T) {
		t.Parallel()

		// Whatever a retry does, it must not move the conversation to a new
		// session: that would throw away the warm prompt cache the retry is
		// about to re-send the whole history against.
		first := composeSessionID("build-TOKEN", "writer")

		for range 3 {
			if got := composeSessionID("build-TOKEN", "writer"); got != first {
				t.Fatalf("session changed from %q to %q within one conversation", first, got)
			}
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

	t.Run("other fields keep their original bytes", func(t *testing.T) {
		t.Parallel()

		// The splice must not launder the rest of the payload: a big integer
		// must not go through float64, and the < > in a transition-context
		// block must not be HTML-escaped into </>. Both happen if
		// the body is round-tripped through map[string]any.
		got := withCacheControl([]byte(
			`{"model":"anthropic/claude-3.5-sonnet","seed":12345678901234567,` +
				`"messages":[{"role":"user","content":"<transition_context>x & y</transition_context>"}]}`))

		for _, want := range []string{
			`12345678901234567`,
			`<transition_context>x & y</transition_context>`,
		} {
			if !strings.Contains(string(got), want) {
				t.Errorf("spliced body lost %q:\n%s", want, got)
			}
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
// transport actually put on the wire. The handler writes it on the server's
// goroutine while the test reads it on its own, so the fields are guarded
// rather than left to net/http's internals to order.
type capturedRequest struct {
	mu        sync.Mutex
	sessionID string
	body      map[string]any
	path      string
}

func (c *capturedRequest) record(sessionID, path string, body map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sessionID, c.path, c.body = sessionID, path, body
}

// session returns the last captured x-session-id.
func (c *capturedRequest) session() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.sessionID
}

// field returns the last captured body's top-level value for key.
func (c *capturedRequest) field(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	v, ok := c.body[key]

	return v, ok
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

		var decoded map[string]any

		_ = json.Unmarshal(body, &decoded)

		captured.record(r.Header.Get("x-session-id"), r.URL.Path, decoded)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: &openRouterTransport{base: http.DefaultTransport, agent: "reviewer"}}

	return client, server.URL, captured
}

// clientFor builds a second client against the same fake server, standing in
// for another agents: entry in the same job run.
func clientFor(agentName string) *http.Client {
	return &http.Client{Transport: &openRouterTransport{base: http.DefaultTransport, agent: agentName}}
}

func TestOpenRouterSessionScope(t *testing.T) {
	t.Parallel()

	t.Run("two agents in one run send different sessions", func(t *testing.T) {
		t.Parallel()

		// The case that makes per-agent scoping matter rather than merely be
		// tidy: a router model pins the resolved model per session, so a
		// run-wide session would let whichever agent ran first choose the
		// model for every other agent in the job.
		_, base, captured := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")
		url := base + "/api/v1/chat/completions"
		body := `{"model":"anthropic/claude-3.5-sonnet","messages":[]}`

		postJSON(ctx, t, clientFor("writer"), url, body)
		writerSession := captured.session()

		postJSON(ctx, t, clientFor("critic"), url, body)
		criticSession := captured.session()

		if writerSession == criticSession {
			t.Errorf("writer and critic shared session %q", writerSession)
		}
	})

	t.Run("one agent called twice in a run keeps its session", func(t *testing.T) {
		t.Parallel()

		// The revise-loop / repeated-sub-agent case: same agent, same prefix,
		// so the pin must survive across separate calls.
		_, base, captured := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")
		url := base + "/api/v1/chat/completions"
		body := `{"model":"anthropic/claude-3.5-sonnet","messages":[]}`

		postJSON(ctx, t, clientFor("writer"), url, body)
		first := captured.session()

		postJSON(ctx, t, clientFor("writer"), url, body)
		second := captured.session()

		if first != second {
			t.Errorf("same agent got %q then %q", first, second)
		}
	})

	t.Run("the same agent in two runs sends different sessions", func(t *testing.T) {
		t.Parallel()

		// Concurrent jobs under `steps watch --max-concurrent` must not share
		// a provider pin.
		_, base, captured := serveCapturing(t)
		url := base + "/api/v1/chat/completions"
		body := `{"model":"anthropic/claude-3.5-sonnet","messages":[]}`

		postJSON(WithNewRun(t.Context(), "job"), t, clientFor("writer"), url, body)
		first := captured.session()

		postJSON(WithNewRun(t.Context(), "job"), t, clientFor("writer"), url, body)
		second := captured.session()

		if first == second {
			t.Errorf("two runs shared session %q", first)
		}
	})
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

// assertSessionShape checks a captured x-session-id without pinning the random
// run token: it must name the job the run was opened with and the agent the
// client was built for.
func assertSessionShape(t *testing.T, got, jobName, agentName string) {
	t.Helper()

	if !strings.HasPrefix(got, jobName+"-") {
		t.Errorf("x-session-id = %q, want it to start with %q", got, jobName+"-")
	}

	if !strings.HasSuffix(got, "-"+agentName) {
		t.Errorf("x-session-id = %q, want it to end with %q", got, "-"+agentName)
	}
}

func TestOpenRouterTransportWireMutations(t *testing.T) {
	t.Parallel()

	t.Run("stamps session id and cache_control on an anthropic chat completion", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")

		postJSON(ctx, t, client, base+"/api/v1/chat/completions",
			`{"model":"anthropic/claude-3.5-sonnet","messages":[]}`)

		assertSessionShape(t, captured.session(), "job", "reviewer")

		value, _ := captured.field("cache_control")

		marker, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("cache_control missing from the wire body: %v", value)
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

		if captured.session() != "" {
			t.Errorf("x-session-id = %q, want no header", captured.session())
		}
	})

	t.Run("session id is stamped for a non-anthropic model but cache_control is not", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")

		postJSON(ctx, t, client, base+"/api/v1/chat/completions",
			`{"model":"openai/gpt-4o","messages":[]}`)

		assertSessionShape(t, captured.session(), "job", "reviewer")

		value, present := captured.field("cache_control")
		if present {
			t.Errorf("cache_control was sent to a non-anthropic model: %v", value)
		}
	})

	t.Run("leaves a non-chat path alone", func(t *testing.T) {
		t.Parallel()

		client, base, captured := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")

		postJSON(ctx, t, client, base+"/api/v1/models",
			`{"model":"anthropic/claude-3.5-sonnet"}`)

		if captured.session() != "" {
			t.Errorf("x-session-id was stamped on a non-chat request: %q", captured.session())
		}

		value, present := captured.field("cache_control")
		if present {
			t.Errorf("cache_control was stamped on a non-chat request: %v", value)
		}
	})
}

func TestOpenRouterTransportRequestHandling(t *testing.T) {
	t.Parallel()

	t.Run("does not mutate the caller's request headers", func(t *testing.T) {
		t.Parallel()

		client, base, _ := serveCapturing(t)
		ctx := WithNewRun(t.Context(), "job")

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

func TestAgentHTTPClientTransportStack(t *testing.T) {
	t.Parallel()

	t.Run("openrouter base url gets the session/cache transport outermost", func(t *testing.T) {
		t.Parallel()

		got := agentHTTPClient(config.ResolvedInvocation{
			BaseURL:   "https://openrouter.ai/api/v1/",
			ModelName: "anthropic/claude-3.5-sonnet",
			AgentName: "reviewer",
		})

		transport, ok := got.Transport.(*openRouterTransport)
		if !ok {
			t.Fatalf("transport = %T, want *openRouterTransport", got.Transport)
		}

		if _, ok := transport.base.(*repairTransport); !ok {
			t.Errorf("openRouterTransport.base = %T, want *repairTransport (repair applies to OpenRouter too)", transport.base)
		}
	})

	t.Run("every other provider gets the repair transport only", func(t *testing.T) {
		t.Parallel()

		// Since repair.go, every provider gets the package's client: the
		// argument-repair transport wraps the shared base, and only the
		// OpenRouter session/cache layer is conditional.
		for _, baseURL := range []string{
			"https://api.openai.com/v1/",
			"http://localhost:1234/v1/",
			"https://api.groq.com/openai/v1/",
			"",
		} {
			got := agentHTTPClient(config.ResolvedInvocation{BaseURL: baseURL, AgentName: "reviewer"})

			if _, ok := got.Transport.(*repairTransport); !ok {
				t.Errorf("agentHTTPClient(%q).Transport = %T, want *repairTransport", baseURL, got.Transport)
			}
		}
	})
}
