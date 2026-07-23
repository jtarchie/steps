// openrouter.go wires OpenRouter's two prompt-caching levers into the agent
// LLM client. Neither is automatic through a stock OpenAI-compatible client,
// so a `model: openrouter/...` agent otherwise pays full input price on every
// turn of its tool-calling conversation — the case where the whole prior
// history is re-sent each turn and caching is worth the most.
//
//  1. x-session-id header. Read from the request context (see WithSessionID,
//     set once per job run by internal/pipeline). OpenRouter pins a session to
//     one provider instance so the prompt cache stays warm; without it, sticky
//     routing only engages *after* a cache hit is observed, which is too late
//     for a short pipeline job.
//
//  2. Top-level cache_control: {type: ephemeral} body field, injected only
//     when the request's model routes to Anthropic. This is OpenRouter's
//     documented "automatic" caching form: it caches through the last
//     cacheable block and advances the boundary as the conversation grows,
//     rather than spending one of the four explicit per-message breakpoints.
//     Providers with implicit caching (OpenAI, Gemini, DeepSeek, Groq, ...)
//     need no marker and are left alone; sending them one risks a 400 for no
//     gain.
//
// Scope: the mutating transport is installed on a per-LLM *http.Client, and
// only when the resolved base URL is actually OpenRouter (see
// newOpenRouterHTTPClient). The reference implementation this is ported from
// (jtarchie/topbanana's internal/model/openrouter_cache.go) instead swaps
// http.DefaultTransport process-wide, because the adk-utils-go version it
// targeted exposed only an HTTPOptions.Headers seam. v0.22.0 added
// HTTPOptions.Client, so that global is unnecessary here — which is just as
// well, since this repo's `reassign` linter forbids reassigning
// http.DefaultTransport/http.DefaultClient outright.
//
// The reference's third piece — teeing the response body to recover
// usage.prompt_tokens_details.cached_tokens — is deliberately not ported:
// steps tracks no token usage anywhere, so there is nothing to stitch the
// count onto.

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxSessionIDLen is OpenRouter's documented cap on a session identifier.
const maxSessionIDLen = 256

// openRouterHost is the only host whose traffic gets the caching mutations.
const openRouterHost = "openrouter.ai"

// chatCompletionsPath is the only endpoint the mutations apply to. The
// adapter calls nothing else today; the check keeps a future non-chat call
// (a models listing, say) from being handed a cache_control body field it
// has no meaning for.
const chatCompletionsPath = "/chat/completions"

// openRouterResponseHeaderTimeout mirrors the bound openai-go's own
// defaultHTTPClient applies when the caller supplies no client. Supplying one
// opts out of that default, so it is re-applied here rather than silently
// dropping stuck-connection protection for OpenRouter agents specifically.
const openRouterResponseHeaderTimeout = 10 * time.Minute

// sessionIDKey types the context value holding an OpenRouter session ID.
type sessionIDKey struct{}

// WithSessionID returns a derived context carrying an OpenRouter session
// identifier, which openRouterTransport sends as the x-session-id header on
// every OpenRouter chat completion made under that context.
//
// internal/pipeline sets this once per job run, so every LLM call the job
// makes — each agent step, each turn of its conversation loop, any fix: agent
// or sub-agent nested inside it — pins to the same provider instance. It is
// carried in the context rather than baked into the client so that concurrent
// jobs under `steps watch --max-concurrent` stay separated: the transport is
// shared, the session ID is per-request.
//
// An empty or over-long id leaves ctx unchanged — OpenRouter caps session_id
// at 256 characters, and sending an invalid one is worse than sending none.
// The session ID is transport-level only: it never enters a step's merkle
// content (see internal/merkle), so it cannot invalidate a cached step.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" || len(id) > maxSessionIDLen {
		return ctx
	}

	return context.WithValue(ctx, sessionIDKey{}, id)
}

// sessionIDFromContext reads back what WithSessionID stored, or "" when the
// context carries no session ID.
func sessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionIDKey{}).(string)

	return id
}

// isOpenRouterBaseURL reports whether baseURL points at OpenRouter, and so
// whether the caching mutations apply at all. An unparsable URL, or any other
// provider (including a self-hosted gateway set via source.endpoint), reports
// false and gets a stock client.
func isOpenRouterBaseURL(baseURL string) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}

	host := parsed.Hostname()

	return host == openRouterHost || strings.HasSuffix(host, "."+openRouterHost)
}

// isAnthropicModel reports whether an OpenRouter model slug routes to
// Anthropic, the only family here that needs an explicit cache_control marker.
// OpenRouter accepts both the plain "anthropic/<model>" form and the
// "~anthropic/<model>" form that forces Anthropic-direct routing (Bedrock and
// Vertex ignore the marker).
//
// The slug is matched rather than the endpoint because by this point
// config.resolveAgentTarget has already split "openrouter/anthropic/claude-x"
// into the OpenRouter base URL plus the model name "anthropic/claude-x" — the
// caller's model choice is the source of truth.
func isAnthropicModel(name string) bool {
	return strings.HasPrefix(name, "anthropic/") || strings.HasPrefix(name, "~anthropic/")
}

// openRouterTransport applies the session-ID header and cache_control body
// field to OpenRouter chat completions. Anything else — a non-chat path — is
// passed through to base untouched.
type openRouterTransport struct {
	base http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t *openRouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}

	if req.URL == nil || !strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
		return base.RoundTrip(req) //nolint:wrapcheck // pass a non-chat request through verbatim; callers unwrap transport errors themselves
	}

	// RoundTrip must not modify the request it is handed, so mutate a clone.
	// The clone keeps the caller's own context — nothing here derives a new
	// one, so cancellation and deadlines propagate unchanged.
	ctx := req.Context()
	req = req.Clone(ctx)

	sessionID := sessionIDFromContext(ctx)
	if sessionID != "" {
		req.Header.Set("x-session-id", sessionID)
	}

	err := injectCacheControl(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter cache_control: %w", err)
	}

	return base.RoundTrip(req) //nolint:wrapcheck // surface the base transport's error verbatim, per the RoundTripper contract
}

// injectCacheControl rewrites req's body to carry a top-level cache_control
// marker when the model routes to Anthropic. The body is re-wrapped either
// way, because reading it to inspect the model consumed the original
// io.ReadCloser. GetBody is reset alongside it so openai-go's retry path can
// replay the request.
func injectCacheControl(req *http.Request) error {
	if req.Body == nil {
		return nil
	}

	body, err := io.ReadAll(req.Body)

	closeErr := req.Body.Close()

	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	if closeErr != nil {
		return fmt.Errorf("close request body: %w", closeErr)
	}

	body = withCacheControl(body)

	req.ContentLength = int64(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	return nil
}

// withCacheControl returns body with a top-level cache_control marker added
// when its model field routes to Anthropic, and body unchanged otherwise.
//
// A body that doesn't parse as a JSON object, or that re-marshals to an
// error, is passed through untouched rather than failing the request: this is
// an opportunistic cost optimization, and a caching tweak should never be the
// reason an agent step dies.
func withCacheControl(body []byte) []byte {
	var doc map[string]any

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return body
	}

	name, _ := doc["model"].(string)
	if !isAnthropicModel(name) {
		return body
	}

	doc["cache_control"] = map[string]string{"type": "ephemeral"}

	rewritten, err := json.Marshal(doc)
	if err != nil {
		return body
	}

	return rewritten
}

// newOpenRouterHTTPClient returns the *http.Client an OpenRouter-backed agent
// should use, or nil when baseURL isn't OpenRouter — in which case the caller
// supplies no client at all and openai-go builds its own as before, keeping
// every non-OpenRouter provider byte-identical to how it behaved before this
// file existed.
func newOpenRouterHTTPClient(baseURL string) *http.Client {
	if !isOpenRouterBaseURL(baseURL) {
		return nil
	}

	// Reproduce openai-go's defaultHTTPClient: clone the shared transport and
	// bound the wait for response headers. Cloning also means the connection
	// pool is this client's own, so nothing here perturbs other HTTP users in
	// the process (internal/mcp, resource types shelling out, ...).
	base := http.DefaultTransport

	transport, ok := base.(*http.Transport)
	if ok {
		transport = transport.Clone()
		transport.ResponseHeaderTimeout = openRouterResponseHeaderTimeout
		base = transport
	}

	return &http.Client{Transport: &openRouterTransport{base: base}}
}
