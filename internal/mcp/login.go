package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
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

	handler, err := buildAuthorizationHandler(cb, open, reg, authRequest{
		scopes:        srv.Auth.Scopes,
		issuer:        asm.Issuer,
		issAdvertised: asm.AuthorizationResponseIssParameterSupported,
	})
	if err != nil {
		return fmt.Errorf("mcp server %q: build authorization handler: %w", srv.Name, err)
	}

	tok, err := authorize(ctx, srv, handler)
	if err != nil {
		return err
	}

	// Persisted before the verdict below, deliberately. The token IS valid,
	// and a login that throws away a working credential because it will not
	// last is worse than one that keeps it and says so — `steps run` right
	// now works either way.
	err = persistLoginResult(srv, asm, reg, tok)
	if err != nil {
		return err
	}

	return checkUnattended(srv, tok)
}

// checkUnattended refuses to call a login successful when what it obtained
// cannot outlive the session that obtained it.
//
// This is the check whose absence caused the incident it was written for: a
// login reported ✓, saved an access token with no refresh token beside it,
// and the pipeline ran fine for fifty minutes. The next morning's first
// trigger failed with "token expired and refresh token is not set" — a
// message about a state that had been true, and knowable, since the moment
// login printed its checkmark. Nothing between those two points could have
// reported it, because nothing between them looks at the token until it is
// needed.
//
// A token with no expiry is not a problem: it does not run out, so there is
// nothing to renew. Only expires-and-cannot-be-renewed earns an error.
func checkUnattended(srv config.MCPServer, tok *oauth2.Token) error {
	if tok.RefreshToken != "" || tok.Expiry.IsZero() {
		return nil
	}

	return fmt.Errorf(
		"mcp server %q: authorized, and the token was saved — but the authorization server issued no refresh token, "+
			"so this credential stops working at %s and `steps run`/`steps watch` will fail from then on with no way to renew it. "+
			"If the server supports dynamic client registration it was asked for the refresh_token grant and declined; "+
			"otherwise register an application that grants refresh tokens and set auth.client_id",
		srv.Name, tok.Expiry.Format(time.RFC3339))
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

	// GrantTypes is not decoration. RFC 7591 §2 defaults an omitted
	// grant_types to ["authorization_code"] ALONE, so a client registered
	// without it has not asked for the refresh grant — and an authorization
	// server that honours the registration then issues an access token with
	// no refresh token beside it, exactly as asked. The result looks like a
	// successful login and dies at the first expiry, unattended, with
	// "token expired and refresh token is not set" and nothing on disk to
	// fix it. Registering the grant we intend to use is what makes silent
	// refresh (oauth.go's persistingTokenSource) possible at all.
	reg, err := oauthex.RegisterClient(ctx, asm.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		RedirectURIs:  []string{redirectURL},
		ClientName:    "steps",
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scope:         strings.Join(srv.Auth.Scopes, " "),
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
// performs its own (redundant) dynamic client registration. req carries what
// this package decides rather than the SDK: the scopes to request, and how
// to treat the iss that comes back (see fetch).
func buildAuthorizationHandler(cb *loopbackCallback, open func(string) error, reg *oauthex.ClientRegistrationResponse, req authRequest) (*auth.AuthorizationCodeHandler, error) {
	preregistered := &oauthex.ClientCredentials{
		ClientID: reg.ClientID,
		// Issuer is what binds these credentials to the authorization server
		// they were obtained from, and leaving it unset is what let the two
		// discoveries below drift apart in silence.
		//
		// discoverAndRegister resolves an authorization server from srv's
		// well-known paths; the SDK then resolves one AGAIN inside Authorize,
		// preferring the resource_metadata URL named by the 401 challenge
		// (auth/authorization_code.go's protectedResourceMetadataURLs). A
		// server that answers the challenge with one authorization server
		// while publishing another therefore gets a client registered — and a
		// client secret sent — at the first, and the code exchanged at the
		// second, with the token file recording the first's token endpoint.
		// Every later refresh then goes to a server that never issued the
		// token, fails, and is reported as needing a login that will
		// "succeed" and leave the same state again.
		//
		// The SDK already has the check that catches this and only runs it
		// when Issuer is non-empty. Setting it costs one field and converts
		// the whole divergence from silent to a named error.
		Issuer: req.issuer,
	}
	if reg.ClientSecret != "" {
		preregistered.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: reg.ClientSecret}
	}

	handler, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		PreregisteredClient: preregistered,
		RedirectURL:         cb.redirectURL,
		// SEP-2207: adds offline_access to the requested scopes on an
		// authorization server that advertises it. The paired half — the
		// refresh_token grant type — is declared at registration above,
		// which the SDK's own doc points out it will not do for you.
		RequestRefreshToken:      true,
		AuthorizationCodeFetcher: cb.fetch(open, req),
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

// authRequest is what fetch decides that the SDK's own arguments do not
// carry: which scopes this pipeline wants, and what the authorization
// server's metadata said about RFC 9207's iss parameter.
type authRequest struct {
	scopes []string
	// issuer is the authorization server's own identifier, as discovered.
	issuer string
	// issAdvertised is its authorization_response_iss_parameter_supported.
	issAdvertised bool
}

// resolveIss decides which iss to hand the SDK, and is the reason login
// works against a server that sends one without advertising that it does.
//
// The SDK validates iss against the metadata flag in both directions and
// hard-fails on either mismatch: advertised-but-absent, and — the case that
// bites here — present-but-not-advertised. Metabase's authorization server
// is the second: it returns iss on every redirect and its metadata never
// mentions the parameter, so forwarding it verbatim makes login impossible
// against a server that is doing MORE than it promised, not less.
//
// Dropping iss wholesale would fix that and throw away RFC 9207's mix-up
// defense for every server, which is the whole reason it is forwarded. So
// the check happens here instead, against the issuer discovery already
// resolved: an iss that matches is verified and then withheld from a SDK
// that would only reject it; an iss that does NOT match is exactly the
// attack the parameter exists to catch, and fails the login by name.
func (a authRequest) resolveIss(got string) (string, error) {
	// Checked before the advertised split, not inside one branch of it. The
	// SDK has the same flag and an expected issuer, but not necessarily the
	// SAME expected issuer — it rediscovers, and may land on a different
	// authorization server (see buildAuthorizationHandler). Deferring to it
	// on the advertised path meant the one case where the two disagree was
	// the one case nothing compared against the issuer this flow actually
	// registered with.
	if got != "" && !issuersEqual(got, a.issuer) {
		return "", fmt.Errorf(
			"authorization response came from issuer %q, but this flow was started against %q — refusing to exchange the code",
			got, a.issuer)
	}

	if a.issAdvertised {
		// Forwarded so the SDK checks it too, against whatever it resolved. If
		// that differs from what this flow registered against, its comparison
		// is the one that fails, by name, instead of the code being exchanged
		// somewhere unexpected.
		return got, nil
	}

	// Verified above and withheld here: the SDK rejects an iss it did not see
	// advertised, which is the Metabase case in the doc comment.
	return "", nil
}

// issuersEqual compares two OAuth 2.0 issuer identifiers the way the SDK does
// (internal/authutil.IssuersEqual): ignoring a trailing slash, which
// RFC 8414 treats as the same server.
//
// Reimplemented rather than imported because the SDK keeps it in internal/.
// An exact == here fails a login outright, accusing the server of a mix-up
// attack, whenever protected-resource metadata lists "https://as.example/"
// and the redirect carries iss=https://as.example.
func issuersEqual(a, b string) bool {
	return strings.TrimSuffix(a, "/") == strings.TrimSuffix(b, "/")
}

// withScopes replaces the scope parameter of the authorization URL the SDK
// built with the pipeline's auth.scopes:, and is what makes that setting
// mean anything. Before this it was persisted into the token file and read
// by nothing: the scopes actually REQUESTED were whichever ones the
// protected-resource metadata advertised, so a pipeline asking for
// `[agent:search]` against a server offering seventeen scopes was granted
// all seventeen, including the ones that write. Least privilege that is
// written down and not sent is worse than none, because it reads as done.
//
// The authorization URL is the seam because it is the only place a scope is
// expressed: auth.AuthorizationCodeHandlerConfig exposes no scope knob (the
// SDK picks them from the 401 challenge, else from the metadata), and the
// code exchange that follows sends no scope at all — RFC 6749 §4.1.3 has no
// such parameter, the grant is already fixed by what the user approved here.
// So rewriting it here is complete rather than partial: there is no second
// place for the old value to leak back in.
//
// An empty auth.scopes: changes nothing, leaving the SDK's choice — asking
// for everything on offer is the right default for a server whose scopes the
// pipeline has no opinion about. offline_access is preserved when the SDK
// added it (RequestRefreshToken, SEP-2207), since dropping it would trade
// this fix for the one above it.
func withScopes(authURL string, scopes []string) string {
	if len(scopes) == 0 {
		return authURL
	}

	parsed, err := url.Parse(authURL)
	if err != nil {
		// Unparsable means the SDK built something this cannot reason about;
		// handing it back unchanged fails no worse than not having tried.
		return authURL
	}

	query := parsed.Query()

	requested := append([]string(nil), scopes...)
	if slices.Contains(strings.Fields(query.Get("scope")), "offline_access") &&
		!slices.Contains(requested, "offline_access") {
		requested = append(requested, "offline_access")
	}

	query.Set("scope", strings.Join(requested, " "))
	parsed.RawQuery = query.Encode()

	return parsed.String()
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
func (cb *loopbackCallback) fetch(open func(string) error, req authRequest) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		authURL := withScopes(args.URL, req.scopes)

		// Announced before the URL is opened, so handle can already tell this
		// attempt's redirect from a straggler. The SDK generates a fresh
		// state per attempt and may retry Authorize, so a result left over
		// from a previous attempt is drained rather than mistaken for this
		// one's — it would only ever fail the SDK's own state comparison.
		cb.expect(stateFromAuthURL(authURL))
		cb.drain()

		fmt.Printf("\nAuthorize in your browser:\n\n  %s\n\n", authURL)

		// Opened on its own goroutine: cmd.Run waits for the opener to exit,
		// and xdg-open with no registered handler (or a broken DISPLAY over
		// SSH) can block indefinitely — which would make ctx.Done below
		// unreachable and Ctrl-C useless. The URL is printed unconditionally
		// above, so nothing is lost by not waiting.
		go func() {
			err := open(authURL)
			if err != nil {
				fmt.Printf("(could not open a browser automatically: %v — open the URL above)\n", err)
			}
		}()

		select {
		case res := <-cb.result:
			if res.err != nil {
				return nil, res.err
			}

			iss, err := req.resolveIss(res.iss)
			if err != nil {
				return nil, err
			}

			return &auth.AuthorizationResult{Code: res.code, State: res.state, Iss: iss}, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("authorization: %w", ctx.Err())
		}
	}
}

// persistLoginResult writes the completed login's token and the client
// credentials/endpoint used to obtain it to srv's per-user token file.
//
// One thing it will not do is replace a renewable credential with a
// disposable one. Login persists before it judges (see its comment), which is
// right when the alternative is nothing on disk — but a re-login is not that
// case. Widening scopes against a server that this time issues no refresh
// token would otherwise overwrite a working, renewable token with one that
// dies at expiry, report the failure, and leave no way back to what was there
// a moment ago.
func persistLoginResult(srv config.MCPServer, asm *oauthex.AuthServerMeta, reg *oauthex.ClientRegistrationResponse, tok *oauth2.Token) error {
	path, err := TokenPath(srv.Name)
	if err != nil {
		return err
	}

	err = checkNotADowngrade(srv, path, tok)
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

// checkNotADowngrade refuses a write that would trade a renewable credential
// for one that cannot outlive the session, keeping what is already on disk.
//
// Only that one direction is refused. Anything else — no file yet, an
// unreadable one, a file for a different endpoint, or a new token that also
// carries a refresh token — is a normal login and proceeds, because the
// alternative to writing is having nothing.
func checkNotADowngrade(srv config.MCPServer, path string, tok *oauth2.Token) error {
	if tok.RefreshToken != "" {
		return nil
	}

	existing, err := LoadTokenFile(path)
	if err != nil {
		// No file, or one this version cannot read. Either way there is
		// nothing here worth preserving over a token that works right now.
		return nil //nolint:nilerr // absence of a previous credential is not an error, it is the common case
	}

	if existing.RefreshToken == "" || existing.Endpoint != srv.Endpoint {
		return nil
	}

	return fmt.Errorf(
		"mcp server %q: this login obtained a token the authorization server issued no refresh token for, "+
			"but the existing one at %s can still be renewed — keeping it rather than replacing a credential that "+
			"survives unattended with one that stops working at %s. "+
			"Re-run with the same scopes as before, or delete that file to accept the downgrade",
		srv.Name, path, tok.Expiry.Format(time.RFC3339))
}
