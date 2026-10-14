package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/markbates/goth"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// fakeGitHub answers what goth and the team lookup ask github.com for, as alex in acme's eng team.
func fakeGitHub(t *testing.T) *http.Client {
	t.Helper()

	reply := func(body any) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("code") != "good-code" {
			http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)

			return
		}

		reply(map[string]string{"access_token": "gho_token", "token_type": "bearer", "scope": "read:org"})(w, r)
	})
	mux.HandleFunc("GET /user", reply(map[string]any{"login": "alex", "id": 7, "email": "alex@acme.dev", "name": "Alex"}))
	mux.HandleFunc("GET /user/emails", reply([]map[string]any{{"email": "alex@acme.dev", "primary": true, "verified": true}}))
	mux.HandleFunc("GET /user/orgs", reply([]map[string]string{{"login": "acme"}}))
	mux.HandleFunc("GET /user/teams", reply([]map[string]any{{"slug": "eng", "organization": map[string]string{"login": "acme"}}}))

	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	target, _ := url.Parse(fake.URL)

	return &http.Client{Transport: rewriteHost{to: target}}
}

// rewriteHost sends every request to the fake, whatever host it named: github.com, api.github.com.
type rewriteHost struct{ to *url.URL }

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host, clone.Host = r.to.Scheme, r.to.Host, r.to.Host

	return http.DefaultTransport.RoundTrip(clone) //nolint:wrapcheck // a test transport
}

// TestSigningInWithGitHub crosses the whole trip: the button, the redirect to GitHub, the code back, the person and their teams recorded, and a session under their name.
func TestSigningInWithGitHub(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithSignInProviders(GitHubSignIn("client", "secret", "http://steps.example", fakeGitHub(t))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	page := browserGet(t, server, "/login?next=/p/demo", nil)
	if !strings.Contains(page, `href="/auth/github?next=%2fp%2fdemo">Continue with GitHub</a>`) || strings.Contains(page, `name="password"`) {
		t.Fatalf("the sign-in page should offer GitHub and, with no local accounts, no password form:\n%s", page)
	}

	state, started := startGitHub(t, server)
	callback := "/auth/github/callback?code=good-code&state=" + url.QueryEscape(state)

	signsInAsAlex(t, server, callback, started)

	user, found, err := accounts.FindUser(t.Context(), "github:alex")
	if err != nil || !found || user.Email != "alex@acme.dev" || strings.Join(user.Groups, ",") != "acme/eng" || strings.Join(user.Organizations, ",") != "acme" {
		t.Errorf("FindUser = %+v, %v, %v; want alex with acme and acme/eng", user, found, err)
	}

	refusesForgedReturns(t, server, callback, started)
}

func signsInAsAlex(t *testing.T, server *Server, callback string, started []*http.Cookie) {
	t.Helper()

	rec := browserRequest(t, server, callback, started)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/p/demo" || len(rec.Result().Cookies()) == 0 {
		t.Fatalf("the callback = %d to %q; want a session and sent on to /p/demo", rec.Code, rec.Header().Get("Location"))
	}

	signedIn := browserGet(t, server, "/p/demo", rec.Result().Cookies())
	if !strings.Contains(signedIn, `<span class="who">alex</span>`) || !strings.Contains(signedIn, "alex (github)") {
		t.Error("the signed-in page does not name alex from GitHub")
	}
}

// refusesForgedReturns: a state used twice, one never handed out, one another browser started, and a code GitHub refuses each sign nobody in.
func refusesForgedReturns(t *testing.T, server *Server, used string, started []*http.Cookie) {
	t.Helper()

	if again := browserRequest(t, server, used, started); again.Code != http.StatusBadRequest {
		t.Errorf("the same state answered twice = %d, want refused", again.Code)
	}

	state, started := startGitHub(t, server)

	refused := browserRequest(t, server, "/auth/github/callback?code=good-code&state="+url.QueryEscape(state)+"x", started)
	if refused.Code != http.StatusBadRequest {
		t.Errorf("a state this daemon never handed out = %d, want refused", refused.Code)
	}

	theirs, _ := startGitHub(t, server)

	if sent := browserRequest(t, server, "/auth/github/callback?code=good-code&state="+url.QueryEscape(theirs), nil); sent.Code != http.StatusBadRequest {
		t.Errorf("a callback another browser started = %d, want refused rather than signing this one in as them", sent.Code)
	}

	state, started = startGitHub(t, server)
	badCode := browserRequest(t, server, "/auth/github/callback?code=stolen&state="+url.QueryEscape(state), started)
	if badCode.Code != http.StatusUnauthorized || len(badCode.Result().Cookies()) != 0 {
		t.Errorf("a code GitHub refused = %d with %d cookies, want no session", badCode.Code, len(badCode.Result().Cookies()))
	}
}

// startGitHub presses the button and returns the state the daemon sent to GitHub, and the cookie that ties it to this browser.
func startGitHub(t *testing.T, server *Server) (string, []*http.Cookie) {
	t.Helper()

	rec := browserRequest(t, server, "/auth/github?next=/p/demo", nil)

	to, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || err != nil || to.Host != "github.com" || to.Query().Get("state") == "" {
		t.Fatalf("starting = %d to %q; want sent to github.com with a state", rec.Code, rec.Header().Get("Location"))
	}

	return to.Query().Get("state"), rec.Result().Cookies()
}

func browserRequest(t *testing.T, server *Server, target string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	req.Header.Set("Accept", "text/html")

	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec
}

func browserGet(t *testing.T, server *Server, target string, cookies []*http.Cookie) string {
	t.Helper()

	return browserRequest(t, server, target, cookies).Body.String()
}

// TestAGoogleAccountIsItsEmailAndDomain: Google says nothing about groups, so a Workspace domain is the one thing a rule can match beyond the address, and a personal account has none.
func TestAGoogleAccountIsItsEmailAndDomain(t *testing.T) {
	t.Parallel()

	work, err := describeGoogle(t.Context(), nil, goth.User{Email: "sam@acme.dev", RawData: map[string]any{"hd": "acme.dev", "verified_email": true}})
	if err != nil || work.Actor != "google:sam@acme.dev" || len(work.Organizations) != 1 || work.Organizations[0] != "acme.dev" || len(work.Groups) != 0 {
		t.Errorf("a Workspace account = %+v, %v; want its email as the actor and its domain as the organization", work, err)
	}

	personal, err := describeGoogle(t.Context(), nil, goth.User{Email: "sam@gmail.com", RawData: map[string]any{"verified_email": true}})
	if err != nil || len(personal.Organizations) != 0 {
		t.Errorf("a personal account = %+v, %v; want no organization", personal, err)
	}

	_, err = describeGoogle(t.Context(), nil, goth.User{Email: "ceo@acme.dev", RawData: map[string]any{"verified_email": false}})
	if err == nil {
		t.Error("an address Google has not verified was accepted as the person's name")
	}

	_, err = describeGoogle(t.Context(), nil, goth.User{})
	if err == nil {
		t.Error("an account with no email was accepted, and would have signed in as nobody")
	}
}
