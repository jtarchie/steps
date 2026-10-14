package web

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

const exportingPipeline = `
mcp_servers:
- name: github
  endpoint: https://example.test/mcp
mcp_exports:
- name: support
  access: UserID == "alex"
  tools:
  - mcp: github
    tool: search_issues
    args: { owner: acme }
    timeout: 20s
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`

// exportServer is a daemon serving one pipeline with one export, alex and bob signed in, and the accounts behind them.
func exportServer(t *testing.T, extra ...Option) (*Server, store.Accounts) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writeFile(t, path, exportingPipeline)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "ops")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	accounts, err := sqlite.OpenAccounts(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	opts := append([]Option{WithSessions(accounts), WithLocalUsers([]LocalUser{{Name: "alex", Hash: []byte(mustHash(t, "a"))}}), WithExternalURL("https://steps.example")}, extra...)

	server, err := New([]*Pipeline{NewPipeline("ops", path, cfg, st, events.New(nil))}, nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, who := range []string{"alex", "bob"} {
		err := accounts.CreateSession(t.Context(), sessionFor(who))
		if err != nil {
			t.Fatal(err)
		}
	}

	return server, accounts
}

func serve(t *testing.T, server *Server, method, target, body, as string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, target, strings.NewReader(body))
	req.Header.Set("Accept", "text/html")

	switch {
	case strings.HasPrefix(body, "{"):
		req.Header.Set("Content-Type", "application/json")
	case body != "":
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	if as != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: as, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

func register(t *testing.T, server *Server, redirect string) string {
	t.Helper()

	rec := serve(t, server, http.MethodPost, "/oauth/register", `{"client_name":"Claude","redirect_uris":["`+redirect+`"]}`, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", rec.Code, rec.Body.String())
	}

	var client struct {
		ClientID string `json:"client_id"`
	}

	_ = json.Unmarshal(rec.Body.Bytes(), &client)

	return client.ClientID
}

const verifier = "a-verifier-long-enough-to-be-a-real-pkce-verifier-0123456789"

func challenge() string {
	sum := sha256.Sum256([]byte(verifier))

	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeURL(client, redirect string) string {
	return "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {client}, "redirect_uri": {redirect}, "state": {"xyz"},
		"code_challenge": {challenge()}, "code_challenge_method": {"S256"}, "resource": {"https://steps.example/mcp/x/ops/support"},
	}.Encode()
}

var consentField = regexp.MustCompile(`name="consent" value="([^"]+)"`)

// TestAnAgentConnectsOnlyAfterConsent is the whole trip, and the hole it is shaped around: a signed-in person's browser sent to authorize gets a page, never a code, until they press Allow.
func TestAnAgentConnectsOnlyAfterConsent(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)

	meta := serve(t, server, http.MethodGet, "/.well-known/oauth-protected-resource/mcp/x/ops/support", "", "")
	if !strings.Contains(meta.Body.String(), `"authorization_servers":["https://steps.example"]`) {
		t.Fatalf("resource metadata = %d %s", meta.Code, meta.Body.String())
	}

	redirect := "https://agent.example/cb?keep=1"
	client := register(t, server, redirect)
	consent := consentShown(t, server, client, redirect)

	allowed := serve(t, server, http.MethodPost, "/oauth/authorize", "consent="+consent+"&decision=allow", "alex")

	to, err := url.Parse(allowed.Header().Get("Location"))
	if err != nil || to.Host != "agent.example" || to.Query().Get("keep") != "1" || to.Query().Get("state") != "xyz" || to.Query().Get("code") == "" {
		t.Fatalf("allow sent the browser to %q; want agent.example with its own query kept, the state and a code", allowed.Header().Get("Location"))
	}

	if again := serve(t, server, http.MethodPost, "/oauth/authorize", "consent="+consent+"&decision=allow", "alex"); again.Header().Get("Location") != "" {
		t.Error("a consent answered twice issued a second code")
	}

	pair := exchange(t, server, url.Values{"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
	assertGrant(t, accounts, pair["access_token"], store.Grant{ClientID: client, Actor: "local:alex", Pipeline: "ops", Export: "support"})

	refreshRotates(t, server, client, pair["refresh_token"])
}

// refreshRotates: a refresh hands out a new refresh token and the old one stops working.
func refreshRotates(t *testing.T, server *Server, client, refresh string) {
	t.Helper()

	refreshed := exchange(t, server, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {client}})
	if refreshed["access_token"] == "" || refreshed["refresh_token"] == refresh {
		t.Error("a refresh did not rotate the refresh token")
	}

	reused := serve(t, server, http.MethodPost, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {client}}.Encode(), "")
	if reused.Code != http.StatusBadRequest {
		t.Errorf("an old refresh token = %d, want refused", reused.Code)
	}
}

func assertGrant(t *testing.T, accounts store.Accounts, access string, want store.Grant) {
	t.Helper()

	token, found, err := accounts.FindToken(t.Context(), tokenDigest(access))
	if err != nil || !found || token.Grant != want {
		t.Fatalf("the access token's grant = %+v, %v, %v; want %+v", token, found, err, want)
	}
}

// consentShown asks to authorize as alex and holds the answer to being a page saying what is asked, never a redirect carrying a code.
func consentShown(t *testing.T, server *Server, client, redirect string) string {
	t.Helper()

	page := serve(t, server, http.MethodGet, authorizeURL(client, redirect), "", "alex")
	text := html.UnescapeString(page.Body.String())

	if page.Code != http.StatusOK || page.Header().Get("Location") != "" {
		t.Fatalf("authorize with a session = %d to %q; want the consent page and no redirect", page.Code, page.Header().Get("Location"))
	}

	for _, want := range []string{`“Claude” for ops / support`, "agent.example", "unverified: the name it gave itself", "owner=acme", `value="deny" autofocus`} {
		if !strings.Contains(text, want) {
			t.Errorf("the consent page does not say %q", want)
		}
	}

	return consentField.FindStringSubmatch(page.Body.String())[1]
}

func exchange(t *testing.T, server *Server, form url.Values) map[string]string {
	t.Helper()

	rec := serve(t, server, http.MethodPost, "/oauth/token", form.Encode(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("token %s = %d %s", form.Get("grant_type"), rec.Code, rec.Body.String())
	}

	var pair map[string]any

	_ = json.Unmarshal(rec.Body.Bytes(), &pair)

	out := map[string]string{}
	for key, value := range pair {
		if text, ok := value.(string); ok {
			out[key] = text
		}
	}

	return out
}

// TestAuthorizeRefusesBeforeRedirecting: nothing is sent to an address that is not registered to the client asking, and a person the export does not include is told so instead of asked.
func TestAuthorizeRefusesBeforeRedirecting(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)
	redirect := "https://agent.example/cb"
	client := register(t, server, redirect)

	for name, target := range map[string]string{
		"unknown client": authorizeURL("nobody", redirect),
		"other redirect": authorizeURL(client, "https://evil.example/cb"),
	} {
		rec := serve(t, server, http.MethodGet, target, "", "alex")
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("%s = %d to %q; want a page, never a redirect", name, rec.Code, rec.Header().Get("Location"))
		}
	}

	refused := serve(t, server, http.MethodGet, authorizeURL(client, redirect), "", "bob")
	if refused.Code != http.StatusForbidden || consentField.MatchString(refused.Body.String()) {
		t.Errorf("bob, whom the export does not include = %d; want told so, with nothing to press", refused.Code)
	}

	wrongVerifier := serve(t, server, http.MethodPost, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {"never-issued"}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {"x"}}.Encode(), "")
	if wrongVerifier.Code != http.StatusBadRequest {
		t.Errorf("a code never issued = %d, want refused", wrongVerifier.Code)
	}
}

// TestPKCEIsRequiredAndChecked: a code is only worth something with the verifier its challenge was made from.
func TestPKCEIsRequiredAndChecked(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)
	redirect := "https://agent.example/cb"
	client := register(t, server, redirect)

	page := serve(t, server, http.MethodGet, authorizeURL(client, redirect), "", "alex")

	found := consentField.FindStringSubmatch(page.Body.String())
	if len(found) < 2 {
		t.Fatalf("no consent on the page (%d, to %q):\n%s", page.Code, page.Header().Get("Location"), page.Body.String())
	}

	consent := found[1]
	allowed := serve(t, server, http.MethodPost, "/oauth/authorize", "consent="+consent+"&decision=allow", "alex")

	to, _ := url.Parse(allowed.Header().Get("Location"))

	rec := serve(t, server, http.MethodPost, "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {"the-wrong-verifier"}}.Encode(), "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "code_verifier") {
		t.Errorf("a code with the wrong verifier = %d %s, want refused", rec.Code, rec.Body.String())
	}

	noChallenge := strings.Replace(authorizeURL(client, redirect), "code_challenge_method=S256", "code_challenge_method=plain", 1)

	plain := serve(t, server, http.MethodGet, noChallenge, "", "alex")
	if !strings.Contains(plain.Header().Get("Location"), "error=invalid_request") {
		t.Errorf("a plain challenge = %d to %q, want refused back to the client", plain.Code, plain.Header().Get("Location"))
	}
}

func TestRegistrationRefusesRedirectsThatRunSomething(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)

	for _, redirect := range []string{"javascript:alert(1)", "http://evil.example/cb", "https://x.example/cb#frag", "data:text/html,x"} {
		rec := serve(t, server, http.MethodPost, "/oauth/register", `{"client_name":"x","redirect_uris":["`+redirect+`"]}`, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("registering %q = %d, want refused", redirect, rec.Code)
		}
	}

	for _, redirect := range []string{"http://127.0.0.1:33418/cb", "http://localhost/cb", "cursor://anysphere/callback"} {
		if rec := serve(t, server, http.MethodPost, "/oauth/register", `{"client_name":"x","redirect_uris":["`+redirect+`"]}`, ""); rec.Code != http.StatusCreated {
			t.Errorf("registering %q = %d, want accepted", redirect, rec.Code)
		}
	}
}

// TestARefreshAsksAgain: access is re-read at every refresh, so a person the export stops including loses their agent's connection at its next refresh, and the whole grant is ended.
func TestARefreshAsksAgain(t *testing.T) {
	t.Parallel()

	server, accounts := exportServer(t)
	redirect := "https://agent.example/cb"
	client := register(t, server, redirect)

	page := serve(t, server, http.MethodGet, authorizeURL(client, redirect), "", "alex")
	consent := consentField.FindStringSubmatch(page.Body.String())[1]
	allowed := serve(t, server, http.MethodPost, "/oauth/authorize", "consent="+consent+"&decision=allow", "alex")
	to, _ := url.Parse(allowed.Header().Get("Location"))

	pair := exchange(t, server, url.Values{"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")}, "client_id": {client}, "redirect_uri": {redirect}, "code_verifier": {verifier}})

	narrowed, err := config.Parse([]byte(strings.Replace(exportingPipeline, `access: UserID == "alex"`, `access: UserID == "somebody-else"`, 1)), "ops", nil)
	if err != nil {
		t.Fatal(err)
	}

	server.Lookup("ops").SetConfig(narrowed)

	rec := serve(t, server, http.MethodPost, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair["refresh_token"]}, "client_id": {client}}.Encode(), "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "ended") {
		t.Fatalf("a refresh after access ended = %d %s, want refused", rec.Code, rec.Body.String())
	}

	_, found, err := accounts.FindToken(t.Context(), tokenDigest(pair["access_token"]))
	if err != nil || found {
		t.Errorf("the grant's access token still works after access ended (%v, %v)", found, err)
	}
}

// sessionFor is a signed-in local account whose cookie is its own name.
func sessionFor(who string) store.Session {
	return store.Session{TokenHash: tokenDigest(who), Actor: "local:" + who, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
}
