package mcp

// A login whose redirect lands on a server somebody else runs — a `steps web` daemon answering `steps mcp login -p <pipeline> --target <url>`. The flow is login.go's, unchanged; what differs is that nothing here listens, because a daemon on a machine with no browser cannot be the loopback the redirect comes back to, and the daemon's own public route is a redirect URI that (unlike an ephemeral port) can be registered once with a server that demands exact ones.

import (
	"context"
	"net/http"

	"github.com/jtarchie/steps/internal/config"
)

// HostedCallback is one pending login's redirect target, mounted by whoever owns the HTTP server.
type HostedCallback struct {
	cb       *loopbackCallback
	announce func(authURL string)
}

// NewHostedCallback builds the redirect target for one login. redirectURL is where the provider sends the browser, which the caller must route to ServeHTTP; announce is told the authorization URL, once per attempt, so it can travel to whoever has the browser.
func NewHostedCallback(redirectURL string, announce func(authURL string)) *HostedCallback {
	return &HostedCallback{
		cb:       &loopbackCallback{redirectURL: redirectURL, result: make(chan callbackResult, 1)},
		announce: announce,
	}
}

// Matches reports whether state belongs to this login's outstanding request. It is the whole of the route's authentication: a provider's redirect is a bare browser navigation carrying no credential of the daemon's, and the state is a nonce this process minted that only the party holding the authorization URL has seen.
func (h *HostedCallback) Matches(state string) bool { return h.cb.matches(state) }

// ServeHTTP receives the redirect, under the rules loopback.go's handle already keeps: an unrecognized state is refused without consuming the flow, and a second delivery is a conflict rather than an answer.
func (h *HostedCallback) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.cb.handle(w, r) }

// LoginHosted is Login for a redirect that arrives through hosted, blocking until it does, ctx ends, or the flow fails. The token lands where Login puts it: on THIS machine, which is the one that will spend it.
func LoginHosted(ctx context.Context, srv config.MCPServer, hosted *HostedCallback) error {
	return login(ctx, srv, hosted.cb, hosted.announce)
}
