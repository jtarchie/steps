package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

func signInServer(t *testing.T) *Server {
	t.Helper()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithSignInProviders(GitHubSignIn("client", "secret", "http://steps.example", fakeGitHub(t))), WithRoles(githubRoles(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server
}

// startGitHubFrom is startGitHub from a browser at addr, which is what a start is counted against.
func startGitHubFrom(t *testing.T, server *Server, addr string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github?next=/p/demo", nil)
	req.Header.Set("Accept", "text/html")
	req.RemoteAddr = addr

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	to, err := url.Parse(rec.Header().Get("Location"))
	if rec.Code != http.StatusSeeOther || err != nil || to.Query().Get("state") == "" {
		t.Fatalf("starting from %s = %d to %q; want sent to github.com with a state", addr, rec.Code, rec.Header().Get("Location"))
	}
}

// GET /auth/:provider is skip-auth, so any anonymous caller can fill this map for ten minutes per entry; spread over enough addresses, a flood still meets the overall cap.
func TestAnonymousSignInStartsAreBounded(t *testing.T) {
	t.Parallel()

	server := signInServer(t)

	first, firstCookies := startGitHub(t, server)

	for i := range maxPending + 5 {
		startGitHubFrom(t, server, "203.0.113."+strconv.Itoa(i%250)+":"+strconv.Itoa(1024+i/250))
	}

	if got := server.pending.len(); got != maxPending {
		t.Errorf("pending sign-ins = %d, want capped at %d", got, maxPending)
	}

	rec := browserRequest(t, server, "/auth/github/callback?code=good-code&state="+url.QueryEscape(first), firstCookies)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("the oldest trip, displaced by the flood, = %d, want refused", rec.Code)
	}

	last, lastCookies := startGitHub(t, server)

	rec = browserRequest(t, server, "/auth/github/callback?code=good-code&state="+url.QueryEscape(last), lastCookies)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("the newest trip = %d, want completed", rec.Code)
	}
}

// TestAFloodFromOneAddressLeavesOtherSignInsAlone: a thousand anonymous starts from one address used to push every in-flight sign-in out of the map; now that address displaces only its own.
func TestAFloodFromOneAddressLeavesOtherSignInsAlone(t *testing.T) {
	t.Parallel()

	server := signInServer(t)

	legit, cookies := startGitHub(t, server)

	for i := range maxPending + 5 {
		startGitHubFrom(t, server, "198.51.100.7:"+strconv.Itoa(1024+i))
	}

	if got := server.pending.len(); got != signInsPerSource+1 {
		t.Errorf("pending sign-ins = %d, want the flood held to %d beside the one legitimate trip", got, signInsPerSource)
	}

	rec := browserRequest(t, server, "/auth/github/callback?code=good-code&state="+url.QueryEscape(legit), cookies)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("the in-flight sign-in after a flood from another address = %d, want completed", rec.Code)
	}
}

func TestSignInSourceIsTheConnectionsAddress(t *testing.T) {
	t.Parallel()

	for remote, want := range map[string]string{
		"198.51.100.7:4242":          "198.51.100.7",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"[2001:db8:1:2:ff::1]:80":    "2001:db8:1:2::/64",
		"[::ffff:198.51.100.7]:1":    "198.51.100.7",
	} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/auth/github", nil)
		req.RemoteAddr = remote
		req.Header.Set("X-Forwarded-For", "192.0.2.99")

		if got := signInSource(req); got != want {
			t.Errorf("signInSource(%s) = %s, want %s", remote, got, want)
		}
	}
}

func TestTTLMapExpiresAndSweepsOnlyWhenFull(t *testing.T) {
	t.Parallel()

	var m ttlMap[int]

	m.put("gone", 1, time.Now().Add(-time.Second))

	if _, ok := m.take("gone"); ok {
		t.Error("an expired entry was returned")
	}

	for i := range maxPending {
		m.put("old"+strconv.Itoa(i), i, time.Now().Add(-time.Second))
	}

	m.put("fresh", 2, time.Now().Add(time.Minute))

	if m.len() != 1 {
		t.Errorf("len = %d after a full map of expired entries took an insert, want only the fresh one", m.len())
	}

	if v, ok := m.take("fresh"); !ok || v != 2 {
		t.Errorf("take = %v %v", v, ok)
	}

	if _, ok := m.take("fresh"); ok {
		t.Error("take is not single-use")
	}
}

// TestAnOwnersCapCountsOnlyWhatItStillHolds: a trip that completed no longer counts against its address, or an office behind one address would lose in-flight sign-ins to its own finished ones.
func TestAnOwnersCapCountsOnlyWhatItStillHolds(t *testing.T) {
	t.Parallel()

	var m ttlMap[int]

	later := time.Now().Add(time.Minute)

	m.putFrom("office", 2, "done", 1, later)

	if _, ok := m.take("done"); !ok {
		t.Fatal("take lost a fresh entry")
	}

	m.putFrom("office", 2, "alex", 2, later)
	m.putFrom("office", 2, "sam", 3, later)

	for _, key := range []string{"alex", "sam"} {
		if _, ok := m.take(key); !ok {
			t.Errorf("%s's trip was displaced by one that had already completed", key)
		}
	}
}
