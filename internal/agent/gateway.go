package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// cachingGateway is a gateway whose automatic caching is one top-level body field that the gateway itself applies only to providers needing explicit markers — so, unlike OpenRouter's cache_control, it is safe for every model.
type cachingGateway struct {
	name          string
	host          string
	sessionHeader string
	field         string
	value         json.RawMessage
}

//nolint:gochecknoglobals // static, read-only lookup table
var cachingGateways = []cachingGateway{
	{"vercel", "ai-gateway.vercel.sh", "x-session-affinity", "providerOptions", json.RawMessage(`{"gateway":{"caching":"auto"}}`)},
	{"requesty", "requesty.ai", "", "requesty", json.RawMessage(`{"auto_cache":true}`)},
}

func cachingGatewayFor(baseURL string) (cachingGateway, bool) {
	for _, gateway := range cachingGateways {
		if hostMatches(baseURL, gateway.host) {
			return gateway, true
		}
	}

	return cachingGateway{}, false
}

// gatewayTransport is openRouterTransport's analog for a cachingGateway: the same per-(run, agent) session and automatic caching, in that gateway's spelling.
type gatewayTransport struct {
	base    http.RoundTripper
	agent   string
	gateway cachingGateway
}

// CloseIdleConnections forwards to the wrapped transport; see openRouterTransport's.
func (t *gatewayTransport) CloseIdleConnections() {
	closer, ok := t.base.(interface{ CloseIdleConnections() })
	if ok {
		closer.CloseIdleConnections()
	}
}

// RoundTrip implements http.RoundTripper.
func (t *gatewayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || !strings.HasSuffix(req.URL.Path, chatCompletionsPath) {
		return t.base.RoundTrip(req) //nolint:wrapcheck // pass a non-chat request through verbatim
	}

	ctx := req.Context()
	req = req.Clone(ctx)

	sessionID := composeSessionID(runIDFromContext(ctx), t.agent)
	if sessionID != "" && t.gateway.sessionHeader != "" {
		req.Header.Set(t.gateway.sessionHeader, sessionID)
	}

	err := rewriteBody(req, t.gateway.withAutoCaching)
	if err != nil {
		return nil, fmt.Errorf("%s caching: %w", t.gateway.name, err)
	}

	return t.base.RoundTrip(req) //nolint:wrapcheck // surface the base transport's error verbatim, per the RoundTripper contract
}

// withAutoCaching leaves an existing field alone rather than merging into it: steps never sends one, so one present is somebody's deliberate choice.
func (g cachingGateway) withAutoCaching(body []byte) []byte {
	var doc map[string]json.RawMessage

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return body
	}

	if _, present := doc[g.field]; present {
		return body
	}

	doc[g.field] = g.value

	return encodeDoc(doc, body)
}
