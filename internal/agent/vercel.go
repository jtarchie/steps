package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const vercelHost = "ai-gateway.vercel.sh"

// vercelAutoCaching is safe for every model, unlike OpenRouter's cache_control: the gateway only acts on it for providers that need explicit markers (Anthropic, MiniMax, Alibaba).
var vercelAutoCaching = json.RawMessage(`{"gateway":{"caching":"auto"}}`) //nolint:gochecknoglobals // immutable literal; never mutated after init

func isVercelBaseURL(baseURL string) bool {
	return hostMatches(baseURL, vercelHost)
}

// vercelTransport is openRouterTransport's analog: the same per-(run, agent) session and automatic caching, spelled x-session-affinity and providerOptions.gateway.caching.
type vercelTransport struct {
	base  http.RoundTripper
	agent string
}

// CloseIdleConnections forwards to the wrapped transport; see openRouterTransport's.
func (t *vercelTransport) CloseIdleConnections() {
	closer, ok := t.base.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (t *vercelTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || !strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
		return t.base.RoundTrip(req) //nolint:wrapcheck // pass a non-chat request through verbatim
	}

	ctx := req.Context()
	req = req.Clone(ctx)

	sessionID := composeSessionID(runIDFromContext(ctx), t.agent)
	if sessionID != "" {
		req.Header.Set("x-session-affinity", sessionID)
	}

	err := rewriteBody(req, withAutoCaching)
	if err != nil {
		return nil, fmt.Errorf("vercel caching: %w", err)
	}

	return t.base.RoundTrip(req) //nolint:wrapcheck // surface the base transport's error verbatim, per the RoundTripper contract
}

// withAutoCaching leaves an existing providerOptions alone rather than merging into it: steps never sends one, so one present is somebody's deliberate choice.
func withAutoCaching(body []byte) []byte {
	var doc map[string]json.RawMessage

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return body
	}

	if _, present := doc["providerOptions"]; present {
		return body
	}

	doc["providerOptions"] = vercelAutoCaching

	return encodeDoc(doc, body)
}
