package web

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// GET /auth/:provider is skip-auth, so any anonymous caller can fill this map for ten minutes per entry.
func TestAnonymousSignInStartsAreBounded(t *testing.T) {
	t.Parallel()

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

	first, firstCookies := startGitHub(t, server)

	for range maxPending + 5 {
		startGitHub(t, server)
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
