package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
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
// metadata for srv, then obtains the client credentials the flow runs as:
// the pre-registered pair from auth.client_id when the config supplies one,
// otherwise a client dynamically registered with redirectURL as its sole
// redirect URI.
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

	if srv.Auth.ClientID != "" {
		reg, credErr := preregisteredClient(srv)
		if credErr != nil {
			return nil, nil, credErr
		}

		return asm, reg, nil
	}

	if asm.RegistrationEndpoint == "" {
		return nil, nil, fmt.Errorf(
			"mcp server %q: authorization server does not support dynamic client registration; register an application with it and set auth.client_id (plus auth.client_secret_env if it issued a secret)",
			srv.Name)
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

// preregisteredClient builds the same ClientRegistrationResponse shape
// RegisterClient would have returned, from the config's client_id and the
// secret read out of client_secret_env — so every caller downstream
// (buildAuthorizationHandler, persistLoginResult) stays unaware of which
// way the credentials were obtained. A named-but-unset secret variable is
// an error: proceeding would attempt a public-client exchange the server
// will reject, reported as an opaque 401 far from its cause.
func preregisteredClient(srv config.MCPServer) (*oauthex.ClientRegistrationResponse, error) {
	reg := &oauthex.ClientRegistrationResponse{ClientID: srv.Auth.ClientID}

	if srv.Auth.ClientSecretEnv == "" {
		return reg, nil
	}

	secret := os.Getenv(srv.Auth.ClientSecretEnv)
	if secret == "" {
		return nil, fmt.Errorf("mcp server %q: $%s is not set (auth.client_secret_env)", srv.Name, srv.Auth.ClientSecretEnv)
	}

	reg.ClientSecret = secret

	return reg, nil
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
	transport := &sdkmcp.StreamableClientTransport{
		Endpoint:     srv.Endpoint,
		OAuthHandler: handler,
		// challengeRepairClient, not the default: this is the one request
		// whose 401 challenge gets parsed, and some servers spell it in a
		// way the SDK's strict parser rejects (see challenge.go).
		HTTPClient: challengeRepairClient(),
	}

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

	// mu guards want, the state of the authorization request currently in
	// flight. handle runs on the server's goroutine and fetch on the login
	// one, so the two need it even though only fetch ever writes.
	mu   sync.Mutex
	want string
}

type callbackResult struct {
	code, state, iss string
	err              error
}

// expect records the state parameter of the request about to be opened, so
// handle can tell the real redirect from anything else that reaches the
// loopback port. The SDK regenerates state on every attempt, so this is set
// per attempt rather than once per listener.
func (cb *loopbackCallback) expect(state string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.want = state
}

// matches reports whether a callback's state is the one currently expected.
// An empty expectation matches nothing: until fetch has opened a URL there is
// no request outstanding, so anything arriving is not ours.
func (cb *loopbackCallback) matches(state string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.want != "" && state == cb.want
}

// deliver hands a result to whoever is waiting, without ever blocking. The
// channel holds one result and the waiter takes exactly one, so a send that
// would block means a result is already queued — the flow is settled and this
// request is a straggler, not the answer.
func (cb *loopbackCallback) deliver(res callbackResult) bool {
	select {
	case cb.result <- res:
		return true
	default:
		return false
	}
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

// handle receives the browser redirect. Two things it must NOT do, because
// the port is on loopback and therefore reachable by anything else on the
// machine (a stale tab, a prefetch, a port scan): treat an unrecognized
// request as the answer, and block on the result channel.
//
// The state check is what makes it the answer — the SDK compares state again
// after we return, but by then a stray request has already been consumed as
// though it were the redirect, and the real one has nowhere to go. Rejecting
// here keeps the flow waiting for the request that actually matches.
func (cb *loopbackCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if !cb.matches(q.Get("state")) {
		http.Error(w, "Unrecognized authorization callback.", http.StatusBadRequest)

		return
	}

	if errParam := q.Get("error"); errParam != "" {
		cb.deliver(callbackResult{err: fmt.Errorf("authorization server returned error: %s", errParam)})
		_, _ = fmt.Fprintln(w, "Authorization failed. You can close this window.")

		return
	}

	// iss is forwarded rather than dropped: RFC 9207 servers (Keycloak,
	// Curity, most OAuth 2.1 ones) advertise
	// authorization_response_iss_parameter_supported, and the SDK hard-fails
	// the exchange when they do and no iss arrives — so dropping it makes
	// login against those servers impossible, and loses the mix-up-attack
	// defense against every other one.
	delivered := cb.deliver(callbackResult{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss")})
	if !delivered {
		http.Error(w, "This authorization was already completed.", http.StatusConflict)

		return
	}

	_, _ = fmt.Fprintln(w, "Authorization complete. You can close this window and return to steps.")
}

// stateFromAuthURL pulls the state parameter out of the authorization URL the
// SDK built. auth.AuthorizationArgs carries only the URL, and state is the
// only thing that distinguishes this attempt's redirect from any other
// request that reaches the loopback port — so it is read back from where the
// SDK put it. An unparsable URL yields "", which matches nothing, leaving the
// flow to time out rather than accept a callback it cannot verify.
func stateFromAuthURL(authURL string) string {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return ""
	}

	return parsed.Query().Get("state")
}

// drain discards a result left in the buffer by a previous attempt, so a
// retry does not read the old attempt's redirect as its own.
func (cb *loopbackCallback) drain() {
	select {
	case <-cb.result:
	default:
	}
}

func (cb *loopbackCallback) Close() {
	_ = cb.server.Close()
}

// fetch returns an auth.AuthorizationCodeFetcher that opens url via open
// and blocks for this listener's callback or ctx cancellation. The URL is
// always printed, not just when open fails: a browser that silently opens
// the wrong profile, opens nothing visible, or is a headless/SSH session's
// no-op looks identical to success from here, and the flow then just hangs
// with nothing on screen to act on. Printing it unconditionally costs one
// line and makes every one of those recoverable by hand.
func (cb *loopbackCallback) fetch(open func(string) error) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		// Announced before the URL is opened, so handle can already tell this
		// attempt's redirect from a straggler. The SDK generates a fresh
		// state per attempt and may retry Authorize, so a result left over
		// from a previous attempt is drained rather than mistaken for this
		// one's — it would only ever fail the SDK's own state comparison.
		cb.expect(stateFromAuthURL(args.URL))
		cb.drain()

		fmt.Printf("\nAuthorize in your browser:\n\n  %s\n\n", args.URL)

		// Opened on its own goroutine: cmd.Run waits for the opener to exit,
		// and xdg-open with no registered handler (or a broken DISPLAY over
		// SSH) can block indefinitely — which would make ctx.Done below
		// unreachable and Ctrl-C useless. The URL is printed unconditionally
		// above, so nothing is lost by not waiting.
		go func() {
			err := open(args.URL)
			if err != nil {
				fmt.Printf("(could not open a browser automatically: %v — open the URL above)\n", err)
			}
		}()

		select {
		case res := <-cb.result:
			if res.err != nil {
				return nil, res.err
			}

			return &auth.AuthorizationResult{Code: res.code, State: res.state, Iss: res.iss}, nil
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
