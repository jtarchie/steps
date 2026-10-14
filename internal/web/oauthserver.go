package web

// The daemon as an OAuth 2.1 authorization server, for one purpose: letting an
// agent somewhere else — Claude, Cursor — connect to a pipeline's MCP export
// as the person who signed in and agreed to it. The pieces the MCP
// specification asks for and nothing else: protected-resource metadata per
// export (RFC 9728), authorization-server metadata (RFC 8414), dynamic client
// registration (RFC 7591), the authorization code grant with PKCE S256 only,
// the resource parameter naming the export (RFC 8707), and refresh tokens.
//
// Three things are deliberate, each a hole an earlier version of this idea
// had. Consent is asked every time, on a page that says the client's name is
// unverified and where the browser will be sent, so a site that registered
// itself cannot collect a token from a signed-in person with one navigation.
// Whether a person may use an export is read from the daemon's own record of
// them, at authorization and again at every refresh, never from the token.
// Tokens are opaque and stored as digests, so every one can be revoked.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

const (
	authServerMetadataPath = "/.well-known/oauth-authorization-server"
	resourceMetadataPath   = "/.well-known/oauth-protected-resource/mcp/x/:pipeline/:export"
	registerPath           = "/oauth/register"
	authorizePath          = "/oauth/authorize"
	tokenPath              = "/oauth/token" //nolint:gosec // a route, not a credential
	// exportPath is where an export is served, and so the resource a token is for.
	exportPath = "/mcp/x/:pipeline/:export"

	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
	// consentWait is how long a consent page stays answerable.
	consentWait = 10 * time.Minute
	// maxClients bounds self-registration, which answers anybody.
	maxClients = 1000
)

// WithExternalURL is the address people and agents reach the daemon on, which every OAuth URL it publishes is built from.
func WithExternalURL(base string) Option {
	return func(s *Server) { s.external = strings.TrimRight(base, "/") }
}

func (s *Server) resourceURL(pipeline, export string) string {
	return s.external + "/mcp/x/" + url.PathEscape(pipeline) + "/" + url.PathEscape(export)
}

// exportNamed is an export the named pipeline serves now.
func (s *Server) exportNamed(pipeline, name string) (*Pipeline, config.MCPExport, bool) {
	target := s.Lookup(pipeline)
	if target == nil {
		return nil, config.MCPExport{}, false
	}

	for _, export := range target.Config().MCPExports {
		if export.Name == name {
			return target, export, true
		}
	}

	return nil, config.MCPExport{}, false
}

// exportOfResource reads an RFC 8707 resource back into the export it names, if it is one of this daemon's.
func (s *Server) exportOfResource(resource string) (*Pipeline, config.MCPExport, bool) {
	rest, found := strings.CutPrefix(resource, s.external+"/mcp/x/")
	if !found || s.external == "" {
		return nil, config.MCPExport{}, false
	}

	pipeline, export, found := strings.Cut(rest, "/")
	if !found || strings.Contains(export, "/") {
		return nil, config.MCPExport{}, false
	}

	pipeline, err1 := url.PathUnescape(pipeline)
	export, err2 := url.PathUnescape(export)

	if err1 != nil || err2 != nil {
		return nil, config.MCPExport{}, false
	}

	return s.exportNamed(pipeline, export)
}

// mayUse is whether a person may connect to an export: the daemon must let them see the pipeline, the pipeline must not hide it from them, and the export's own rule must hold. Read from the daemon's record of them every time it is asked.
func (s *Server) mayUse(ctx context.Context, who actor.Actor, target *Pipeline, export config.MCPExport) bool {
	_, allowed := s.mayUseAs(ctx, who, target, export)

	return allowed
}

// mayUseAs is mayUse, and the person the rules saw when it said yes, which a call's own rules are then written against.
func (s *Server) mayUseAs(ctx context.Context, who actor.Actor, target *Pipeline, export config.MCPExport) (Identity, bool) {
	if !s.signsIn(who) {
		return Identity{}, false
	}

	id := s.identityOf(ctx, who)
	if target.narrow(s.roles.highest(id), id) < RoleViewer {
		return Identity{}, false
	}

	program, err := compileRule(export.Access)
	if err != nil {
		return Identity{}, false
	}

	return id, holds(program, id)
}

func randomToken() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)

	return base64.RawURLEncoding.EncodeToString(raw)
}

// ---- metadata ----

func (s *Server) handleAuthServerMetadata(c *echo.Context) error {
	//nolint:wrapcheck // echo writes the response
	return c.JSON(http.StatusOK, map[string]any{
		"issuer":                                s.external,
		"authorization_endpoint":                s.external + authorizePath,
		"token_endpoint":                        s.external + tokenPath,
		"registration_endpoint":                 s.external + registerPath,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (s *Server) handleResourceMetadata(c *echo.Context) error {
	if _, _, found := s.exportNamed(c.Param("pipeline"), c.Param("export")); !found {
		return echo.NewHTTPError(http.StatusNotFound, "no such export")
	}

	//nolint:wrapcheck // echo writes the response
	return c.JSON(http.StatusOK, map[string]any{
		"resource":                 s.resourceURL(c.Param("pipeline"), c.Param("export")),
		"authorization_servers":    []string{s.external},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            c.Param("pipeline") + " / " + c.Param("export"),
	})
}

// ---- registration ----

// redirectAllowed is a redirect address an agent may register: https anywhere, http only to this machine (RFC 8252's loopback for native apps), or an app's own scheme — never one that runs or reads something in the browser.
func redirectAllowed(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Fragment != "" || parsed.Scheme == "" {
		return false
	}

	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return parsed.Host != ""
	case "http":
		host := parsed.Hostname()
		ip := net.ParseIP(host)

		return host == "localhost" || (ip != nil && ip.IsLoopback())
	case "javascript", "data", "file", "vbscript", "blob":
		return false
	default:
		return true
	}
}

func oauthError(c *echo.Context, status int, code, description string) error {
	//nolint:wrapcheck // echo writes the response
	return c.JSON(status, map[string]string{"error": code, "error_description": description})
}

func (s *Server) handleRegister(c *echo.Context) error {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}

	err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)).Decode(&req)
	if err != nil {
		return oauthError(c, http.StatusBadRequest, "invalid_client_metadata", "the registration is not JSON")
	}

	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "register between one and ten redirect_uris")
	}

	for _, uri := range req.RedirectURIs {
		if !redirectAllowed(uri) {
			return oauthError(c, http.StatusBadRequest, "invalid_redirect_uri", "a redirect_uri must be https, http to this machine, or an app's own scheme, with no fragment")
		}
	}

	name := strings.TrimSpace(req.ClientName)
	if len(name) > 100 {
		name = name[:100]
	}

	if name == "" {
		name = "an unnamed client"
	}

	client := store.OAuthClient{ID: randomToken(), Name: name, RedirectURIs: req.RedirectURIs, CreatedAt: time.Now()}

	err = s.sessions.RegisterClient(c.Request().Context(), client, maxClients)
	if errors.Is(err, store.ErrTooManyClients) {
		return oauthError(c, http.StatusServiceUnavailable, "temporarily_unavailable", "this daemon has registered as many clients as it keeps")
	}

	if err != nil {
		return echoError(err)
	}

	//nolint:wrapcheck // echo writes the response
	return c.JSON(http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// ---- authorize and consent ----

// authRequest is an authorization a person has not yet answered, or a code they agreed to and the client has not yet exchanged.
type authRequest struct {
	client      store.OAuthClient
	redirectURI string
	state       string
	challenge   string
	grant       store.Grant
	expires     time.Time
}

// pendingAuth holds consents awaiting an answer and codes awaiting exchange, both single-use, in memory: one daemon is the only thing that will ever be asked for them.
type pendingAuth struct {
	mu       sync.Mutex
	consents map[string]authRequest
	codes    map[string]authRequest
}

func (p *pendingAuth) put(into *map[string]authRequest, key string, req authRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if *into == nil {
		*into = map[string]authRequest{}
	}

	now := time.Now()
	for old, entry := range *into {
		if now.After(entry.expires) {
			delete(*into, old)
		}
	}

	(*into)[key] = req
}

// take reads the map through from under the lock, as put assigns it: the field read is the race, not only the map access.
func (p *pendingAuth) take(from *map[string]authRequest, key string) (authRequest, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	req, found := (*from)[key]
	delete(*from, key)

	return req, found && time.Now().Before(req.expires)
}

// withQuery adds parameters to a redirect address, keeping any query it already had.
func withParams(raw string, params url.Values) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	query := parsed.Query()
	for key, values := range params {
		for _, value := range values {
			query.Add(key, value)
		}
	}

	parsed.RawQuery = query.Encode()

	return parsed.String()
}

// handleAuthorize shows the consent page. It answers only to a signed-in person — authenticate sent anybody else to sign in and back here — and issues nothing: a code exists only after they press Allow.
func (s *Server) handleAuthorize(c *echo.Context) error {
	query := c.Request().URL.Query()

	client, found, err := s.sessions.FindClient(c.Request().Context(), query.Get("client_id"))
	if err != nil {
		return echoError(err)
	}

	// Until the client and its redirect check out, nothing may be sent anywhere: an error redirected to an address nobody registered is the open redirect this order prevents.
	if !found || !contains(client.RedirectURIs, query.Get("redirect_uri")) {
		return s.renderConsent(c, http.StatusBadRequest, consentPage{Problem: "This connection request names a client or a return address this daemon does not know, so it was not followed."})
	}

	refuse := func(code string) error {
		return c.Redirect(http.StatusFound, withParams(query.Get("redirect_uri"), url.Values{"error": {code}, "state": {query.Get("state")}}))
	}

	if query.Get("response_type") != "code" {
		return refuse("unsupported_response_type")
	}

	if query.Get("code_challenge") == "" || query.Get("code_challenge_method") != "S256" {
		return refuse("invalid_request")
	}

	target, export, found := s.exportOfResource(query.Get("resource"))
	if !found {
		return refuse("invalid_target")
	}

	who := actor.From(c.Request().Context())
	page := consentPage{Client: client, Host: redirectHost(query.Get("redirect_uri")), Pipeline: target.Slug, Export: export, Who: who.String()}

	if !s.mayUse(c.Request().Context(), who, target, export) {
		page.Refused = true

		return s.renderConsent(c, http.StatusForbidden, page)
	}

	consent := randomToken()
	s.auths.put(&s.auths.consents, consent, authRequest{
		client:      client,
		redirectURI: query.Get("redirect_uri"),
		state:       query.Get("state"),
		challenge:   query.Get("code_challenge"),
		grant:       store.Grant{ClientID: client.ID, Actor: who.String(), Pipeline: target.Slug, Export: export.Name},
		expires:     time.Now().Add(consentWait),
	})

	page.Consent = consent

	return s.renderConsent(c, http.StatusOK, page)
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}

	return false
}

func redirectHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if parsed.Host != "" {
		return parsed.Host
	}

	return parsed.Scheme + ":"
}

// handleConsent is the person's answer. Only a consent this daemon showed this person, unanswered and unexpired, is honoured.
func (s *Server) handleConsent(c *echo.Context) error {
	req, found := s.auths.take(&s.auths.consents, c.FormValue("consent"))
	if !found || req.grant.Actor != actor.From(c.Request().Context()).String() {
		return s.renderConsent(c, http.StatusBadRequest, consentPage{Problem: "That connection request expired or was not shown to you. Start again from the agent."})
	}

	if c.FormValue("decision") != "allow" {
		//nolint:wrapcheck // echo's redirect error is returned verbatim
		return c.Redirect(http.StatusSeeOther, withParams(req.redirectURI, url.Values{"error": {"access_denied"}, "state": {req.state}}))
	}

	code := randomToken()
	req.expires = time.Now().Add(authCodeTTL)
	s.auths.put(&s.auths.codes, code, req)

	if target := s.Lookup(req.grant.Pipeline); target != nil {
		detail, err := json.Marshal(map[string]string{"client": req.client.Name, "to": redirectHost(req.redirectURI)})
		if err == nil {
			s.recordGrantActivity(c.Request().Context(), target, req.grant, "mcp authorize", string(detail))
		}
	}

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return c.Redirect(http.StatusSeeOther, withParams(req.redirectURI, url.Values{"code": {code}, "state": {req.state}}))
}

func (s *Server) recordGrantActivity(ctx context.Context, target *Pipeline, grant store.Grant, action, detail string) {
	err := target.Store.RecordActivity(context.WithoutCancel(ctx), store.ActivityEntry{Actor: grant.Actor, Action: action, Target: grant.Export, Detail: detail}, s.activityCap())
	if err != nil {
		slog.ErrorContext(ctx, "web.activity_unrecorded", "pipeline", target.Slug, "action", action, "error", err)
	}
}

type consentPage struct {
	Client   store.OAuthClient
	Host     string
	Pipeline string
	Export   config.MCPExport
	Who      string
	Consent  string
	Refused  bool
	Problem  string
}

func (s *Server) renderConsent(c *echo.Context, status int, page consentPage) error {
	nav := s.unscopedNav(c)
	nav.Anchored = false

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(status, "consent", map[string]any{"Nav": nav, "Title": "authorize", "Ask": page})
}

// ---- tokens ----

func (s *Server) handleToken(c *echo.Context) error {
	switch c.FormValue("grant_type") {
	case "authorization_code":
		return s.exchangeCode(c)
	case "refresh_token":
		return s.refresh(c)
	default:
		return oauthError(c, http.StatusBadRequest, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (s *Server) exchangeCode(c *echo.Context) error {
	req, found := s.auths.take(&s.auths.codes, c.FormValue("code"))
	if !found || req.client.ID != c.FormValue("client_id") || req.redirectURI != c.FormValue("redirect_uri") {
		return oauthError(c, http.StatusBadRequest, "invalid_grant", "unknown, used, expired or mismatched code")
	}

	sum := sha256.Sum256([]byte(c.FormValue("code_verifier")))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(req.challenge)) != 1 {
		return oauthError(c, http.StatusBadRequest, "invalid_grant", "code_verifier does not match the challenge")
	}

	return s.issuePair(c, req.grant)
}

// refresh mints the next pair and ends the refresh token it was given. It asks again whether the person may use the export: leaving the team that let them in ends their agent's access at its next refresh, within the hour.
func (s *Server) refresh(c *echo.Context) error {
	ctx := c.Request().Context()

	token, found, err := s.sessions.FindToken(ctx, tokenDigest(c.FormValue("refresh_token")))
	if err != nil {
		return echoError(err)
	}

	if !found || token.Kind != "refresh" || token.Grant.ClientID != c.FormValue("client_id") {
		return oauthError(c, http.StatusBadRequest, "invalid_grant", "unknown, used or expired refresh token")
	}

	revoked, err := s.sessions.RevokeToken(ctx, token.TokenHash)
	if err != nil {
		return echoError(err)
	}

	if !revoked {
		return oauthError(c, http.StatusBadRequest, "invalid_grant", "unknown, used or expired refresh token")
	}

	target, export, exists := s.exportNamed(token.Grant.Pipeline, token.Grant.Export)
	if !exists || !s.mayUse(ctx, actor.Parse(token.Grant.Actor), target, export) {
		_ = s.sessions.RevokeGrant(ctx, token.Grant)

		return oauthError(c, http.StatusBadRequest, "invalid_grant", "access to this export has ended")
	}

	return s.issuePair(c, token.Grant)
}

func (s *Server) issuePair(c *echo.Context, grant store.Grant) error {
	ctx := c.Request().Context()
	access, refresh, now := randomToken(), randomToken(), time.Now()

	for _, token := range []store.OAuthToken{
		{TokenHash: tokenDigest(access), Kind: "access", Grant: grant, CreatedAt: now, ExpiresAt: now.Add(accessTokenTTL)},
		{TokenHash: tokenDigest(refresh), Kind: "refresh", Grant: grant, CreatedAt: now, ExpiresAt: now.Add(refreshTokenTTL)},
	} {
		err := s.sessions.IssueToken(ctx, token)
		if err != nil {
			return echoError(err)
		}
	}

	c.Response().Header().Set("Cache-Control", "no-store")

	//nolint:wrapcheck // echo writes the response
	return c.JSON(http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": refresh,
	})
}
