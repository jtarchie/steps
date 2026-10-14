package e2e

// Signing in from a browser crosses the daemon's middleware, its accounts table and the cookie a browser carries back — so it is driven through cli.Run against a real daemon, with a client that behaves like a browser.

import (
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// Not t.Parallel(): startWeb backgrounds cli.Run in this process and stop signals the whole test binary.
func TestABrowserSignsInAndOut(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "jobs:\n- name: manual\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	page := "/p/" + name

	signingInTakesYouBack(t, served.addr, page)
	cookies := signedInPageOffersSignOut(t, served.addr, page)
	signingOutEndsTheSession(t, served.addr, page, cookies)

	_, location, _ := signIn(t, served.addr, "alex", "a", "//evil.example")
	if location != "/" {
		t.Errorf("a sign-in asked to go on to //evil.example went to %q, want /", location)
	}
}

func signingInTakesYouBack(t *testing.T, addr, page string) {
	t.Helper()

	code, location, _ := browse(t, addr, http.MethodGet, page, "", nil)
	if code != http.StatusSeeOther || location != "/login?next="+url.QueryEscape(page) {
		t.Fatalf("a signed-out browser asking for %s got %d to %q, want sent to sign in", page, code, location)
	}

	code, _, body := browse(t, addr, http.MethodPost, "/login", "username=alex&password=wrong&next="+url.QueryEscape(page), nil)
	if code != http.StatusUnauthorized || !strings.Contains(visibleText(body), "couldn't sign you in") || !strings.Contains(body, `value="alex"`) {
		t.Fatalf("a wrong password = %d; want the form again, saying so, with the name kept", code)
	}
}

func signedInPageOffersSignOut(t *testing.T, addr, page string) []*http.Cookie {
	t.Helper()

	code, location, cookies := signIn(t, addr, "alex", "a", page)
	if code != http.StatusSeeOther || location != page || len(cookies) == 0 {
		t.Fatalf("signing in = %d to %q with %d cookies; want sent on to %s with a session", code, location, len(cookies), page)
	}

	code, _, body := browse(t, addr, http.MethodGet, page, "", cookies)
	if code != http.StatusOK || !strings.Contains(body, `<span class="who">alex</span>`) || !strings.Contains(body, "sign out") {
		t.Fatalf("the signed-in page = %d, without alex's menu or a way to sign out", code)
	}

	return cookies
}

func signingOutEndsTheSession(t *testing.T, addr, page string, cookies []*http.Cookie) {
	t.Helper()

	code, location, _ := browse(t, addr, http.MethodPost, "/logout", "", cookies)
	if code != http.StatusSeeOther || location != "/login" {
		t.Fatalf("signing out = %d to %q, want sent to sign in", code, location)
	}

	// The browser dropped its copy, but a copy taken earlier must be worth nothing too.
	code, _, _ = browse(t, addr, http.MethodGet, page, "", cookies)
	if code != http.StatusSeeOther {
		t.Errorf("the cookie still worked after signing out (%d)", code)
	}
}

func signIn(t *testing.T, addr, user, pass, next string) (int, string, []*http.Cookie) {
	t.Helper()

	form := "username=" + user + "&password=" + pass + "&next=" + url.QueryEscape(next)

	client, req := browserRequest(t, addr, http.MethodPost, "/login", form, nil)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, resp.Header.Get("Location"), resp.Cookies()
}

// browse is one request the way a browser makes it: asking for a page, carrying its cookies, and not following the redirect, so the test sees where it was sent.
func browse(t *testing.T, addr, method, path, form string, cookies []*http.Cookie) (int, string, string) {
	t.Helper()

	client, req := browserRequest(t, addr, method, path, form, cookies)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

func browserRequest(t *testing.T, addr, method, path, form string, cookies []*http.Cookie) (*http.Client, *http.Request) {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var body io.Reader
	if form != "" {
		body = strings.NewReader(form)
	}

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, body)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Accept", "text/html")

	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}

	return client, req
}
