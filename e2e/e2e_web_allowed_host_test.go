package e2e

// --allowed-host is a seam: the flag is parsed in cli and the refusal happens in web's middleware table, so this starts at cli.Run and speaks to a real listener with a forged Host — which is all a DNS-rebinding page is.

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// hostCall sends one request naming host as Host, and as Origin too when origin is set — the shape a rebinding page's own requests take.
func hostCall(t *testing.T, served *webProcess, method, path, host string, origin bool) int {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+served.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = host
	if origin {
		req.Header.Set("Origin", "http://"+host)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s as %s: %v", method, path, host, err)
	}

	_ = resp.Body.Close()

	return resp.StatusCode
}

// Not t.Parallel(): startWeb backgrounds cli.Run in this process and stop signals the whole test binary.
func TestAllowedHostRefusesEveryOtherName(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--allowed-host", "steps.example")
	defer served.stopIfRunning(t)

	// The CLI dials 127.0.0.1, which is always allowed: a local `steps pipeline set` must keep working.
	served.set(t, "demo", path)

	if code := hostCall(t, served, http.MethodGet, "/static/app.css", "evil.test", false); code != http.StatusForbidden {
		t.Errorf("GET /static/app.css as evil.test = %d, want 403", code)
	}

	// The rebinding case the origin check cannot see: Host and Origin agree, and both are the attacker's.
	if code := hostCall(t, served, http.MethodPost, "/p/demo/jobs/build/trigger", "evil.test:8088", true); code != http.StatusForbidden {
		t.Errorf("a rebinding trigger = %d, want 403", code)
	}

	for _, host := range []string{"steps.example", "steps.example:8088", "STEPS.example", "localhost:8088", served.addr} {
		if code := hostCall(t, served, http.MethodGet, "/static/app.css", host, false); code != http.StatusOK {
			t.Errorf("GET /static/app.css as %s = %d, want 200", host, code)
		}
	}

	// A platform probe names the pod's IP, and the answer is "ok" and nothing else.
	if code := hostCall(t, served, http.MethodGet, "/healthz", "10.1.2.3:8088", false); code != http.StatusOK {
		t.Errorf("GET /healthz as an IP = %d, want 200", code)
	}
}

// A daemon bound to loopback has no name a browser can honestly reach it by other than a loopback one, so with no --allowed-host it answers only those — and the --external-url it was told it is published at, which is the name a proxy on this machine forwards. Anything else is a rebinding page. Not t.Parallel(), for startWeb's reason.
func TestALoopbackListenRefusesOtherNamesByDefault(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    run: "true"
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--external-url", "https://steps.example")
	defer served.stopIfRunning(t)

	served.set(t, "demo", path)

	if code := hostCall(t, served, http.MethodPost, "/p/demo/jobs/build/trigger", "evil.test:8088", true); code != http.StatusForbidden {
		t.Errorf("a rebinding trigger against a loopback daemon = %d, want 403", code)
	}

	if code := hostCall(t, served, http.MethodGet, "/p/demo", "evil.test", false); code != http.StatusForbidden {
		t.Errorf("a rebinding read against a loopback daemon = %d, want 403", code)
	}

	for _, host := range []string{"localhost:8088", served.addr, "steps.example"} {
		if code := hostCall(t, served, http.MethodGet, "/static/app.css", host, false); code != http.StatusOK {
			t.Errorf("GET /static/app.css as %s = %d, want 200", host, code)
		}
	}

	// docs/webhooks.md's tunnel forwards the sender's name to a loopback daemon; the delivery's signature is what admits it, so the name must not refuse it first.
	if code := hostCall(t, served, http.MethodPost, "/p/demo/hooks/push", "hooks.example.com", false); code == http.StatusForbidden {
		t.Errorf("a tunnelled delivery = %d, want it to reach the hook route", code)
	}
}
