package web

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAuthorizer is a manager that also runs logins, with one state it will answer for.
type fakeAuthorizer struct {
	fakeManager

	state    string
	started  []string
	bases    []string
	startErr error
	served   int
}

func (f *fakeAuthorizer) StartLogin(_ *Pipeline, server string, req LoginRequest) (LoginStatus, error) {
	f.started = append(f.started, server)
	f.bases = append(f.bases, req.Base)

	if f.startErr != nil {
		return LoginStatus{}, f.startErr
	}

	return LoginStatus{State: LoginPending}, nil
}

func (f *fakeAuthorizer) LoginStatus(server string) (LoginStatus, bool) {
	if len(f.started) == 0 || f.started[0] != server {
		return LoginStatus{}, false
	}

	return LoginStatus{State: LoginPending, AuthorizeURL: "https://as.example/authorize?state=" + f.state}, true
}

func (f *fakeAuthorizer) LoginCallback(state string) http.Handler {
	if state == "" || state != f.state {
		return nil
	}

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.served++

		_, _ = w.Write([]byte("Authorization complete."))
	})
}

// The tab's two questions, which these tests are not about: answered plainly so this fake still IS an Authorizer, since a fake that silently stopped being one would leave every test below asking a nil daemon.
func (f *fakeAuthorizer) MCPState(*Pipeline, string) MCPState { return MCPState{} }

func (f *fakeAuthorizer) StartProbe(*Pipeline, string) error { return nil }

func loginServer(t *testing.T) (*Server, *fakeAuthorizer) {
	t.Helper()

	server := authServer(t)
	authorizer := &fakeAuthorizer{state: "minted-by-the-daemon"}
	server.SetManager(authorizer)

	return server, authorizer
}

// The redirect is a bare navigation a third party caused, so it arrives with no password — and must still land, authenticated by its state and nothing else.
func TestTheLoginCallbackNeedsItsStateAndNotThePassword(t *testing.T) {
	t.Parallel()

	server, authorizer := loginServer(t)

	code, header, body := authCall(t, server, http.MethodGet, MCPCallbackPath+"?code=c&state=minted-by-the-daemon", "", "", nil)
	if code != http.StatusOK || !strings.Contains(body, "Authorization complete") || authorizer.served != 1 {
		t.Errorf("the redirect with its state and no credentials = %d %q, want the login's own handler", code, body)
	}

	if challenge := header.Get("WWW-Authenticate"); challenge != "" {
		t.Errorf("the callback challenged the browser (%q), which would put a password prompt in the middle of an oauth flow", challenge)
	}

	// 404, not 401 (which prompts) and not 400 (which confirms something is being waited for).
	for _, query := range []string{"", "?state=", "?code=c&state=somebody-elses", "?code=c"} {
		code, header, _ := authCall(t, server, http.MethodGet, MCPCallbackPath+query, "", "", nil)
		if code != http.StatusNotFound || header.Get("WWW-Authenticate") != "" {
			t.Errorf("callback%s = %d (challenge %q), want a bare 404", query, code, header.Get("WWW-Authenticate"))
		}
	}

	if authorizer.served != 1 {
		t.Errorf("a callback with the wrong state reached a login's handler (%d served)", authorizer.served)
	}

	// The right credentials do not substitute for the state.
	if code, _, _ := authCall(t, server, http.MethodGet, MCPCallbackPath+"?code=c&state=guess", authUser, authPass, nil); code != http.StatusNotFound {
		t.Errorf("an authenticated callback with a wrong state = %d, want 404", code)
	}
}

// Starting a login and reading its URL are the operator's, and the URL is the one thing that lets somebody finish the flow as themselves.
func TestStartingAndReadingALoginNeedCredentials(t *testing.T) {
	t.Parallel()

	server, authorizer := loginServer(t)

	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if code, _, _ := authCall(t, server, method, "/api/pipelines/demo/mcp/tracker/login", "", "", nil); code != http.StatusUnauthorized {
			t.Errorf("%s login with no credentials = %d, want 401", method, code)
		}
	}

	if len(authorizer.started) != 0 {
		t.Fatal("an unauthenticated request started a login")
	}
}

func TestLoginRoutes(t *testing.T) {
	t.Parallel()

	server, authorizer := loginServer(t)

	code, body := postLogin(t, server, "/api/pipelines/demo/mcp/tracker/login", `{"base":"https://steps.example.com"}`)
	if code != http.StatusAccepted || !strings.Contains(body, `"state":"pending"`) {
		t.Fatalf("start = %d %s, want 202 pending", code, body)
	}

	if len(authorizer.bases) != 1 || authorizer.bases[0] != "https://steps.example.com" {
		t.Errorf("the base the CLI sent did not reach the authorizer: %v", authorizer.bases)
	}

	code, _, body = authCall(t, server, http.MethodGet, "/api/pipelines/demo/mcp/tracker/login", authUser, authPass, nil)
	if code != http.StatusOK || !strings.Contains(body, "authorize_url") {
		t.Errorf("status = %d %s, want the authorization URL", code, body)
	}
}

func TestLoginRouteRefusals(t *testing.T) {
	t.Parallel()

	server, authorizer := loginServer(t)

	if code, _, _ := authCall(t, server, http.MethodGet, "/api/pipelines/demo/mcp/other/login", authUser, authPass, nil); code != http.StatusNotFound {
		t.Errorf("status of a login nobody started = %d, want 404", code)
	}

	if code, _ := postLogin(t, server, "/api/pipelines/nosuch/mcp/tracker/login", `{}`); code != http.StatusNotFound {
		t.Errorf("start against an unserved pipeline = %d, want 404", code)
	}

	// A refusal is the sentence the terminal prints, so it travels as one.
	authorizer.startErr = errors.New(`mcp server "tracker" is not auth: {type: oauth}; nothing to log in to`)

	code, body := postLogin(t, server, "/api/pipelines/demo/mcp/tracker/login", `{}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "nothing to log in to") {
		t.Errorf("a refused start = %d %s, want 400 carrying the refusal", code, body)
	}
}

func postLogin(t *testing.T, server *Server, target, body string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(authUser, authPass)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// A manager that cannot run logins says so, rather than 404ing as though the pipeline or the route were missing.
func TestADaemonThatCannotRunLoginsSaysSo(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	code, _, body := authCall(t, server, http.MethodGet, "/api/pipelines/demo/mcp/tracker/login", authUser, authPass, nil)
	if code != http.StatusNotImplemented || !strings.Contains(body, "cannot run an mcp login") {
		t.Errorf("status on a plain manager = %d %s, want 501", code, body)
	}

	if code, _, _ := authCall(t, server, http.MethodGet, MCPCallbackPath+"?state=x", "", "", nil); code != http.StatusNotFound {
		t.Errorf("callback on a plain manager = %d, want 404", code)
	}
}

// The state rides in a URL, and it is a credential for as long as its login is pending — so the day somebody adds an access log to this server, this is the test that says what it must not print.
//
// Not t.Parallel(): swaps the process-wide slog default.
func TestTheCallbackStateIsNeverLogged(t *testing.T) {
	var logged bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))

	defer slog.SetDefault(previous)

	server, _ := loginServer(t)

	authCall(t, server, http.MethodGet, MCPCallbackPath+"?code=secret-code&state=minted-by-the-daemon", "", "", nil)
	authCall(t, server, http.MethodGet, MCPCallbackPath+"?code=secret-code&state=a-wrong-guess", "", "", nil)

	for _, secret := range []string{"minted-by-the-daemon", "secret-code", "a-wrong-guess"} {
		if strings.Contains(logged.String(), secret) {
			t.Errorf("the server logged %q from a callback URL:\n%s", secret, logged.String())
		}
	}
}
