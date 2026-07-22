package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// Login runs the interactive OAuth authorization-code + PKCE flow for an
// auth: {type: oauth} server and persists the resulting token to its
// per-user token path (see TokenPath) — the implementation behind `steps
// mcp login <pipeline> <server>`. open is the browser-launch function
// (injected so this is testable without a real browser and so this package
// stays free of an os/exec dependency — see main.go's openBrowser for the
// production implementation), falling back to printing the URL to stdout
// whenever it returns an error.
//
// Login does its own protected-resource/authorization-server metadata
// discovery and dynamic client registration up front (rather than letting
// auth.AuthorizationCodeHandler do it internally, as it would for an
// ordinary request-triggered flow) specifically so it can capture the
// resulting client_id/client_secret/token_endpoint for persistence — the
// handler never exposes what it discovered/registered internally, and a
// headless run/watch process needs those fields to silently refresh later
// without repeating discovery or registering a new client every time (see
// oauthTokenSource in oauth.go, which is what run/watch actually use — this
// function is never called from there, only from the CLI's login command).
func Login(ctx context.Context, srv config.MCPServer, open func(url string) error) error {
	cb, err := newLoopbackCallback(ctx)
	if err != nil {
		return err
	}
	defer cb.Close()

	asm, reg, err := discoverAndRegister(ctx, srv, cb.redirectURL)
	if err != nil {
		return err
	}

	handler, err := buildAuthorizationHandler(cb, open, reg)
	if err != nil {
		return fmt.Errorf("mcp server %q: build authorization handler: %w", srv.Name, err)
	}

	tok, err := authorize(ctx, srv, handler)
	if err != nil {
		return err
	}

	return persistLoginResult(srv, asm, reg, tok)
}

// discoverAndRegister fetches protected-resource and authorization-server
// metadata for srv, then dynamically registers a client with redirectURL as
// its sole redirect URI.
func discoverAndRegister(ctx context.Context, srv config.MCPServer, redirectURL string) (*oauthex.AuthServerMeta, *oauthex.ClientRegistrationResponse, error) {
	prm, err := discoverProtectedResourceMetadata(ctx, srv.Endpoint)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp server %q: discover protected resource metadata: %w", srv.Name, err)
	}

	if len(prm.AuthorizationServers) == 0 {
		return nil, nil, fmt.Errorf("mcp server %q: protected resource metadata has no authorization servers", srv.Name)
	}

	asm, err := auth.GetAuthServerMetadata(ctx, prm.AuthorizationServers[0], http.DefaultClient)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp server %q: discover authorization server metadata: %w", srv.Name, err)
	}

	if asm == nil {
		asm = fallbackAuthServerMeta(prm.AuthorizationServers[0])
	}

	if asm.RegistrationEndpoint == "" {
		return nil, nil, fmt.Errorf("mcp server %q: authorization server does not support dynamic client registration", srv.Name)
	}

	reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs: []string{redirectURL},
		ClientName:   "steps",
	}, http.DefaultClient)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp server %q: register client: %w", srv.Name, err)
	}

	return asm, reg, nil
}

// buildAuthorizationHandler constructs the auth.AuthorizationCodeHandler
// used for exactly one login attempt, pre-registered with reg so it never
// performs its own (redundant) dynamic client registration.
func buildAuthorizationHandler(cb *loopbackCallback, open func(string) error, reg *oauthex.ClientRegistrationResponse) (*auth.AuthorizationCodeHandler, error) {
	preregistered := &oauthex.ClientCredentials{ClientID: reg.ClientID}
	if reg.ClientSecret != "" {
		preregistered.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: reg.ClientSecret}
	}

	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient:      preregistered,
		RedirectURL:              cb.redirectURL,
		AuthorizationCodeFetcher: cb.fetch(open),
	})
	if err != nil {
		return nil, fmt.Errorf("new authorization code handler: %w", err)
	}

	return handler, nil
}

// authorize triggers handler's flow by attempting a real (initially
// unauthenticated) connection to srv: the transport's first request gets a
// 401, which drives the transport to call handler.Authorize — the actual
// discovery/PKCE/code-exchange work — before retrying. A successful Connect
// therefore proves the resulting token is valid; the session is closed
// immediately after, since this connection only exists to drive the flow.
func authorize(ctx context.Context, srv config.MCPServer, handler *auth.AuthorizationCodeHandler) (*oauth2.Token, error) {
	transport := &sdkmcp.StreamableClientTransport{Endpoint: srv.Endpoint, OAuthHandler: handler}

	client := sdkmcp.NewClient(clientImplementation, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: authorize: %w", srv.Name, err)
	}

	defer func() { _ = session.Close() }()

	ts, err := handler.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
	}

	if ts == nil {
		return nil, fmt.Errorf("mcp server %q: authorization did not produce a token", srv.Name)
	}

	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: read authorized token: %w", srv.Name, err)
	}

	return tok, nil
}

// fallbackAuthServerMeta returns the predefined-path fallback endpoints for
// an authorization server that publishes no discoverable metadata, per the
// MCP spec's 2025-03-26 fallback (https://modelcontextprotocol.io/specification/2025-03-26/basic/authorization#fallbacks-for-servers-without-metadata-discovery).
func fallbackAuthServerMeta(issuer string) *oauthex.AuthServerMeta {
	return &oauthex.AuthServerMeta{
		Issuer:                issuer,
		AuthorizationEndpoint: issuer + "/authorize",
		TokenEndpoint:         issuer + "/token",
		RegistrationEndpoint:  issuer + "/register",
	}
}

// discoverProtectedResourceMetadata tries the two well-known URLs the MCP
// spec's non-challenge-driven discovery mandates: path-specific first
// (/.well-known/oauth-protected-resource<path>), then root
// (/.well-known/oauth-protected-resource). Mirrors the SDK's own unexported
// protectedResourceMetadataURLs (auth/authorization_code.go), reimplemented
// here since Login needs to run discovery before constructing the handler
// (see Login's doc comment for why), not just react to a 401's challenge.
func discoverProtectedResourceMetadata(ctx context.Context, endpoint string) (*oauthex.ProtectedResourceMetadata, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint: %w", err)
	}

	pathSpecific := *u
	pathSpecific.Path = "/.well-known/oauth-protected-resource/" + strings.TrimLeft(u.Path, "/")

	prm, err := oauthex.GetProtectedResourceMetadata(ctx, pathSpecific.String(), endpoint, http.DefaultClient)
	if err == nil {
		return prm, nil
	}

	root := *u
	root.Path = "/.well-known/oauth-protected-resource"

	rootResource := *u
	rootResource.Path = ""

	prm, err = oauthex.GetProtectedResourceMetadata(ctx, root.String(), rootResource.String(), http.DefaultClient)
	if err != nil {
		return nil, err //nolint:wrapcheck // both attempts' errors are equally informative; wrapping would just say "and also this"
	}

	return prm, nil
}

// loopbackCallback runs a local HTTP server on an ephemeral port to receive
// the OAuth redirect, per auth.AuthorizationCodeFetcher's contract: the SDK
// itself deliberately does not provide this (see docs/mcp.md) — opening a
// browser and catching the redirect is caller/process-integration logic.
type loopbackCallback struct {
	listener    net.Listener
	server      *http.Server
	redirectURL string
	result      chan callbackResult
}

type callbackResult struct {
	code, state string
	err         error
}

func newLoopbackCallback(ctx context.Context) (*loopbackCallback, error) {
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mcp: listen for oauth callback: %w", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // net.Listen("tcp", ...) always yields a *net.TCPAddr

	cb := &loopbackCallback{
		listener:    listener,
		redirectURL: fmt.Sprintf("http://127.0.0.1:%d/callback", port),
		result:      make(chan callbackResult, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cb.handle)
	cb.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = cb.server.Serve(listener) }()

	return cb, nil
}

func (cb *loopbackCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if errParam := q.Get("error"); errParam != "" {
		cb.result <- callbackResult{err: fmt.Errorf("authorization server returned error: %s", errParam)}
		_, _ = fmt.Fprintln(w, "Authorization failed. You can close this window.")

		return
	}

	cb.result <- callbackResult{code: q.Get("code"), state: q.Get("state")}
	_, _ = fmt.Fprintln(w, "Authorization complete. You can close this window and return to steps.")
}

func (cb *loopbackCallback) Close() {
	_ = cb.server.Close()
}

// fetch returns an auth.AuthorizationCodeFetcher that opens url via open
// (falling back to printing it on stdout if open errors) and blocks for
// this listener's callback or ctx cancellation.
func (cb *loopbackCallback) fetch(open func(string) error) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		err := open(args.URL)
		if err != nil {
			fmt.Printf("Open this URL to authorize: %s\n", args.URL)
		}

		select {
		case res := <-cb.result:
			if res.err != nil {
				return nil, res.err
			}

			return &auth.AuthorizationResult{Code: res.code, State: res.state}, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("authorization: %w", ctx.Err())
		}
	}
}

// persistLoginResult writes the completed login's token and the client
// credentials/endpoint used to obtain it to srv's per-user token file.
func persistLoginResult(srv config.MCPServer, asm *oauthex.AuthServerMeta, reg *oauthex.ClientRegistrationResponse, tok *oauth2.Token) error {
	path, err := TokenPath(srv.Name)
	if err != nil {
		return err
	}

	tf := &TokenFile{
		Endpoint:     srv.Endpoint,
		ClientID:     reg.ClientID,
		ClientSecret: reg.ClientSecret,
		TokenURL:     asm.TokenEndpoint,
		Scopes:       srv.Auth.Scopes,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		TokenType:    tok.TokenType,
		Expiry:       tok.Expiry,
	}

	err = tf.Save(path)
	if err != nil {
		return fmt.Errorf("mcp server %q: save token: %w", srv.Name, err)
	}

	return nil
}
