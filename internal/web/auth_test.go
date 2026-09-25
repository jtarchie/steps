package web

// What every route does with a credential, and what the one exempt route does without one.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

const (
	authUser = "ops"
	authPass = "correct-horse-battery"
)

// authServer is a daemon with credentials on and one pipeline served, including a webhook route.
func authServer(t *testing.T) *Server {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
jobs:
  - name: build
    plan:
      - task: compile
        run: "true"
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "demo")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := NewPipeline("demo", path, cfg, st, events.New(nil))

	// A hook handler that records nothing: the question here is whether the request REACHES it.
	pipeline.Hooks = func(w http.ResponseWriter, _ *http.Request, resource string) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("delivered " + resource))
	}

	server, err := New([]*Pipeline{pipeline}, stubRunner{}, WithBasicAuth(authUser, authPass))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	server.SetManager(&fakeManager{})

	return server
}

// authCall is one request, with credentials when both halves are non-empty.
func authCall(t *testing.T, server *Server, method, target, user, pass string, headers http.Header) (int, http.Header, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)

	for name, values := range headers {
		req.Header[name] = values
	}

	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Result().Header, rec.Body.String()
}

// Every route a person or a CLI reaches, and each one has been forgotten by somebody: a static asset served past the prompt is a fingerprint, and an SSE stream served past it is the whole transcript.
func TestBasicAuthCoversEveryRouteAndPromptsTheBrowser(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	for _, route := range []struct {
		method string
		target string
	}{
		{http.MethodGet, "/"},
		{http.MethodGet, "/static/app.css"},
		{http.MethodGet, "/static/htmx.min.js"},
		{http.MethodGet, "/docs"},
		{http.MethodGet, "/p/demo"},
		{http.MethodGet, "/p/demo/jobs/build"},
		{http.MethodGet, "/p/demo/jobs/build/detail"},
		{http.MethodGet, "/p/demo/runs"},
		{http.MethodGet, "/p/demo/runs/whatever/events"},
		{http.MethodPost, "/p/demo/jobs/build/trigger"},
		{http.MethodGet, "/api/pipelines"},
		{http.MethodGet, "/api/pipelines/demo"},
		{http.MethodPut, "/api/pipelines/demo"},
		{http.MethodDelete, "/api/pipelines/demo"},
		{http.MethodGet, "/nosuch"},
	} {
		code, header, _ := authCall(t, server, route.method, route.target, "", "", nil)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s with no credentials = %d, want 401", route.method, route.target, code)
		}

		if challenge := header.Get("WWW-Authenticate"); !strings.HasPrefix(strings.ToLower(challenge), "basic ") {
			t.Errorf("%s %s challenge = %q, want a basic challenge so a browser prompts", route.method, route.target, challenge)
		}
	}
}

// The realm is what a browser's prompt names, so an operator with several daemons can tell which one is asking.
func TestTheChallengeNamesTheRealm(t *testing.T) {
	t.Parallel()

	_, header, _ := authCall(t, authServer(t), http.MethodGet, "/", "", "", nil)
	if challenge := header.Get("WWW-Authenticate"); !strings.Contains(challenge, `realm="steps"`) {
		t.Errorf("WWW-Authenticate = %q, want realm=\"steps\"", challenge)
	}
}

func TestTheRightCredentialsAreServed(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	for _, target := range []string{"/static/app.css", "/docs/README.md", "/p/demo", "/p/demo/runs"} {
		code, _, _ := authCall(t, server, http.MethodGet, target, authUser, authPass, nil)
		if code != http.StatusOK {
			t.Errorf("GET %s with the right credentials = %d, want 200", target, code)
		}
	}

	// The root of a daemon serving one pipeline redirects to it, which is past the prompt either way.
	if code, _, _ := authCall(t, server, http.MethodGet, "/", authUser, authPass, nil); code != http.StatusFound {
		t.Errorf("GET / with the right credentials = %d, want the 302 an open daemon answers", code)
	}
}

// Each half wrong on its own, because a check that only compared one of them passes every test that gets both wrong at once.
func TestAWrongCredentialIsRefused(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	for _, pair := range []struct{ user, pass string }{
		{authUser, "wrong"},
		{"wrong", authPass},
		{"wrong", "wrong"},
		{authUser, authPass + "x"},
		{authUser + "x", authPass},
		{authUser, authPass[:len(authPass)-1]},
	} {
		code, _, _ := authCall(t, server, http.MethodGet, "/", pair.user, pair.pass, nil)
		if code != http.StatusUnauthorized {
			t.Errorf("GET / as %q/%q = %d, want 401", pair.user, pair.pass, code)
		}
	}
}

// A sender has no credentials to give, so the delivery route is the one exemption — and it is exempt by ROUTE, which is what keeps a path that merely looks like one from inheriting it.
func TestTheWebhookRouteIsTheOnlyExemption(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	code, header, body := authCall(t, server, http.MethodPost, "/p/demo/hooks/push", "", "", nil)
	if code != http.StatusAccepted || !strings.Contains(body, "delivered push") {
		t.Errorf("an unauthenticated delivery = %d %q, want 202 from the hook handler", code, body)
	}

	if challenge := header.Get("WWW-Authenticate"); challenge != "" {
		t.Errorf("the delivery route answered a challenge %q, so the middleware ran on it", challenge)
	}

	// A GET at the same path is not the delivery route, so it is not exempt.
	if code, _, _ := authCall(t, server, http.MethodGet, "/p/demo/hooks/push", "", "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET on the hooks path = %d, want 401 — only the POST route is exempt", code)
	}
}

// /api refuses a browser whatever its credentials: a page that has the operator's password (a rebinding page cannot get one, but a phished operator can type one) still must not be able to set a pipeline.
func TestAuthDoesNotReplaceTheBrowserRefusal(t *testing.T) {
	t.Parallel()

	server := authServer(t)

	code, _, body := authCall(t, server, http.MethodGet, "/api/pipelines", authUser, authPass,
		http.Header{"Origin": {"http://evil.example"}})
	if code != http.StatusForbidden {
		t.Errorf("an authenticated browser request to /api = %d (%s), want 403", code, body)
	}
}

// The loopback default has nothing to authenticate against, and a server that started asking anyway would break every existing local workflow.
func TestWithoutTheOptionNothingIsAsked(t *testing.T) {
	t.Parallel()

	server, _ := managedServer(t)

	code, header, _ := authCall(t, server, http.MethodGet, "/", "", "", nil)
	if code != http.StatusOK {
		t.Errorf("GET / on an open daemon = %d, want 200", code)
	}

	if challenge := header.Get("WWW-Authenticate"); challenge != "" {
		t.Errorf("an open daemon sent a challenge %q", challenge)
	}
}

// A timing test would be flaky, so the source is the assertion: the risk this guards is somebody replacing the compare with `==` while every behavioural test above still passes.
func TestTheCredentialCompareIsConstantTime(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("auth.go")
	if err != nil {
		t.Fatalf("read auth.go: %v", err)
	}

	body := string(source)

	if !strings.Contains(body, "subtle.ConstantTimeCompare") {
		t.Error("the credential compare does not go through crypto/subtle, so a wrong username or a wrong password can be found a byte at a time")
	}

	if strings.Contains(body, "== a.password") || strings.Contains(body, "== a.username") {
		t.Error("auth.go compares a credential with ==")
	}
}
