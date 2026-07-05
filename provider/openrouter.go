// Package provider makes OpenRouter a first-class provider prefix. OpenRouter is
// OpenAI-compatible, so the base client is the same adk-utils-go adapter used
// by openai/ollama/lmstudio — but three OpenRouter-specific quirks need
// per-request handling, so we wrap the client with a scoped http.Transport:
//
//  1. x-session-id header. Read from ctx (WithSessionID, set to the run id in
//     the engine). Enables OpenRouter sticky routing from a session's first
//     request, keeping the prompt cache warm across a run's many calls — the
//     "universal" lever, working for implicit caching (Qwen, Gemini, DeepSeek…)
//     and explicit caching (Anthropic). Without it, sticky routing only kicks
//     in after a cache hit is observed, too late for a short run.
//
//  2. Top-level cache_control: {type: ephemeral}. Injected only when the model
//     routes to Anthropic ("anthropic/" or "~anthropic/" prefix) — enables
//     Anthropic's rolling-tail cache. Latent for non-Anthropic models.
//
//  3. Response-body tee. The adapter drops
//     usage.prompt_tokens_details.cached_tokens; we parse it from the raw body
//     and stitch it back onto genai UsageMetadata.CachedContentTokenCount so
//     the engine's cost/token accounting sees cache hits.
//
// Unlike a global http.DefaultTransport hijack, this transport is installed
// only on the openrouter/ provider's own *http.Client (adk-utils-go v0.21.1
// honors HTTPOptions.Client), so nothing else in the process is affected.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"strings"
	"sync"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	adkmodel "google.golang.org/adk/model"
)

const openRouterBaseURL = "https://openrouter.ai/api/v1"

// maxSessionIDLen is OpenRouter's documented cap for the session id.
const maxSessionIDLen = 256

type sessionIDKey struct{}
type captureSlotKey struct{}

// WithSessionID derives a context carrying an OpenRouter session id. The
// openrouter/ provider's transport reads it and sets the x-session-id header,
// so every call in the session lands on the same provider endpoint and the
// prompt cache stays warm. Empty or over-long ids leave ctx unchanged. The
// engine calls this with the run id.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" || len(id) > maxSessionIDLen {
		return ctx
	}
	return context.WithValue(ctx, sessionIDKey{}, id)
}

func sessionIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(sessionIDKey{}).(string)
	return s
}

// newOpenRouter builds the caching LLM for a bare OpenRouter model name.
func newOpenRouter(name string) (adkmodel.LLM, error) {
	base := os.Getenv("OPENROUTER_BASE_URL")
	if base == "" {
		base = openRouterBaseURL
	}
	cfg := genaiopenai.Config{
		APIKey:    os.Getenv("OPENROUTER_API_KEY"), // adapter falls back to OPENAI_API_KEY if empty
		BaseURL:   base,
		ModelName: name,
		HTTPOptions: genaiopenai.HTTPOptions{
			Client: &http.Client{Transport: &openRouterTransport{base: http.DefaultTransport}},
		},
	}
	return &cachingLLM{underlying: genaiopenai.New(cfg), modelName: name}, nil
}

// captureSlot buffers the response body (per call, threaded via ctx) so the
// transport — which sees raw bytes — can hand cached-token metrics to the LLM
// wrapper, which sees only genai shapes. Mutex-guarded: the TeeReader writes
// concurrently with the wrapper's read loop on the final chunk.
type captureSlot struct {
	mu           sync.Mutex
	buf          bytes.Buffer
	cachedTokens int64
	parsed       bool
}

func (s *captureSlot) write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// parse extracts usage.prompt_tokens_details.cached_tokens once. Handles a
// whole-body JSON document (non-streaming) or an SSE stream whose trailing
// `data: {...}` line carries usage. Idempotent and sealed after first call.
func (s *captureSlot) parse() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.parsed {
		return
	}
	defer func() { s.parsed = true }()

	body := s.buf.Bytes()
	if len(body) == 0 {
		return
	}
	if cached, ok := extractCachedTokens(body); ok {
		s.cachedTokens = cached
		return
	}
	// SSE fallback: walk newline-separated lines from the tail.
	for end := len(body); end > 0; {
		start := bytes.LastIndexByte(body[:end], '\n') + 1
		line := bytes.TrimSpace(body[start:end])
		end = start - 1
		if !bytes.HasPrefix(line, []byte("data:")) {
			if end <= 0 {
				return
			}
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if cached, ok := extractCachedTokens(payload); ok {
			s.cachedTokens = cached
			return
		}
		if end <= 0 {
			return
		}
	}
}

func extractCachedTokens(body []byte) (int64, bool) {
	var doc struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return 0, false
	}
	return doc.Usage.PromptTokensDetails.CachedTokens, true
}

// openRouterTransport injects x-session-id + cache_control and tees the
// response body for chat-completions calls; everything else passes through.
type openRouterTransport struct {
	base http.RoundTripper
}

func (t *openRouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isOpenRouterChatCompletion(req) {
		return t.base.RoundTrip(req)
	}
	if sid := sessionIDFromContext(req.Context()); sid != "" {
		req.Header.Set("x-session-id", sid)
	}
	if err := maybeInjectCacheControl(req); err != nil {
		return nil, fmt.Errorf("inject cache_control: %w", err)
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if slot, ok := req.Context().Value(captureSlotKey{}).(*captureSlot); ok && slot != nil {
		resp.Body = teeBody(resp.Body, slot)
	}
	return resp, nil
}

func isOpenRouterChatCompletion(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return false
	}
	host := req.URL.Hostname()
	return strings.HasSuffix(host, "openrouter.ai") || isLoopbackHost(host)
}

// isLoopbackHost widens scope to loopback so an httptest.Server sees the same
// mutations as a real OpenRouter call. Production traffic never targets
// loopback, so this is defensive, not a test escape hatch.
func isLoopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// maybeInjectCacheControl stamps cache_control:{type:ephemeral} at the request
// body's top level for Anthropic-routed models. The body is always re-wrapped
// because reading it consumed the original ReadCloser.
func maybeInjectCacheControl(req *http.Request) error {
	if req.Body == nil {
		return nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		if name, _ := doc["model"].(string); isAnthropicModel(name) {
			doc["cache_control"] = map[string]string{"type": "ephemeral"}
			if rewritten, merr := json.Marshal(doc); merr == nil {
				body = rewritten
			}
		}
	}
	req.ContentLength = int64(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	return nil
}

func isAnthropicModel(name string) bool {
	return strings.HasPrefix(name, "anthropic/") || strings.HasPrefix(name, "~anthropic/")
}

func teeBody(body io.ReadCloser, slot *captureSlot) io.ReadCloser {
	return &teeReadCloser{r: io.TeeReader(body, writerFunc(slot.write)), c: body}
}

type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) }
func (t *teeReadCloser) Close() error               { return t.c.Close() }

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// cachingLLM gives each call a fresh captureSlot in ctx and stitches any
// non-zero cached_tokens back onto the genai UsageMetadata the adapter leaves
// at zero. x-session-id and cache_control happen in the transport.
type cachingLLM struct {
	underlying adkmodel.LLM
	modelName  string
}

func (m *cachingLLM) Name() string { return m.modelName }

func (m *cachingLLM) GenerateContent(
	ctx context.Context,
	req *adkmodel.LLMRequest,
	stream bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	slot := &captureSlot{}
	ctx = context.WithValue(ctx, captureSlotKey{}, slot)
	base := m.underlying.GenerateContent(ctx, req, stream)

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for resp, err := range base {
			if resp != nil && resp.UsageMetadata != nil {
				slot.parse()
				if slot.cachedTokens > 0 && slot.cachedTokens < int64(1<<31) {
					resp.UsageMetadata.CachedContentTokenCount = int32(slot.cachedTokens)
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}
