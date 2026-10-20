package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func hostServer(t *testing.T, hosts ...string) *Server {
	t.Helper()

	server, err := New(nil, stubRunner{}, WithAllowedHosts(hosts))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server
}

func hostStatus(t *testing.T, server *Server, method, target, host string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	req.Host = host

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code
}

func TestAllowedHostsMatchTheNameNotItsSpelling(t *testing.T) {
	t.Parallel()

	server := hostServer(t, "Steps.Example", "steps.internal:8088")

	for host, want := range map[string]int{
		"steps.example":       http.StatusOK,
		"steps.example.":      http.StatusOK,
		"STEPS.EXAMPLE:443":   http.StatusOK,
		"steps.internal":      http.StatusOK,
		"localhost:8088":      http.StatusOK,
		"127.0.0.2:8088":      http.StatusOK,
		"[::1]:8088":          http.StatusOK,
		"evil.test":           http.StatusForbidden,
		"steps.example.evil":  http.StatusForbidden,
		"localhost.evil.test": http.StatusForbidden,
		"10.0.0.5:8088":       http.StatusForbidden,
		"":                    http.StatusForbidden,
	} {
		if got := hostStatus(t, server, http.MethodGet, "/static/app.css", host); got != want {
			t.Errorf("Host %q = %d, want %d", host, got, want)
		}
	}
}

// Uniform: a provider posts to the name the operator gave it, so a delivery naming another one is refused like any request; only the health probe, which learns "ok", is not.
func TestAllowedHostsCoverHooksButNotTheHealthProbe(t *testing.T) {
	t.Parallel()

	server := hostServer(t, "steps.example")

	if got := hostStatus(t, server, http.MethodPost, "/p/demo/hooks/push", "evil.test"); got != http.StatusForbidden {
		t.Errorf("a delivery naming another host = %d, want 403", got)
	}

	if got := hostStatus(t, server, http.MethodGet, HealthPath, "10.0.0.5:8088"); got != http.StatusOK {
		t.Errorf("the health probe by IP = %d, want 200", got)
	}
}

func TestNoAllowedHostsAnswersAnyName(t *testing.T) {
	t.Parallel()

	server := hostServer(t)

	if got := hostStatus(t, server, http.MethodGet, "/static/app.css", "evil.test"); got != http.StatusOK {
		t.Errorf("Host evil.test with no allow-list = %d, want 200", got)
	}
}

// A daemon bound to loopback refuses every other name with no list at all, since a browser can reach it by no other — except the hook route: a tunnel forwards the sender's own name, and a delivery is authenticated by its signature, which a rebinding page has no more of than any other site.
func TestLoopbackHostsRefuseOtherNamesButNotADelivery(t *testing.T) {
	t.Parallel()

	server, err := New(nil, stubRunner{}, WithLoopbackHosts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for host, want := range map[string]int{
		"localhost:8088": http.StatusOK,
		"127.0.0.1:8088": http.StatusOK,
		"[::1]:8088":     http.StatusOK,
		"evil.test":      http.StatusForbidden,
		"evil.test:8088": http.StatusForbidden,
		"10.0.0.5:8088":  http.StatusForbidden,
		"steps.example.": http.StatusForbidden,
	} {
		if got := hostStatus(t, server, http.MethodGet, "/static/app.css", host); got != want {
			t.Errorf("Host %q = %d, want %d", host, got, want)
		}
	}

	if got := hostStatus(t, server, http.MethodPost, "/p/demo/hooks/push", "hooks.example.com"); got == http.StatusForbidden {
		t.Errorf("a tunnelled delivery to a loopback daemon = %d, want it to reach the hook route", got)
	}

	// An explicit list is the operator naming every host, hooks' included, so it stays uniform.
	listed, err := New(nil, stubRunner{}, WithLoopbackHosts(), WithAllowedHosts([]string{"steps.example"}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := hostStatus(t, listed, http.MethodPost, "/p/demo/hooks/push", "hooks.example.com"); got != http.StatusForbidden {
		t.Errorf("a delivery naming an unlisted host under --allowed-host = %d, want 403", got)
	}
}
