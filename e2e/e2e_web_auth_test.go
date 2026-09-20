package e2e

// Basic auth is a SEAM across two packages — the daemon installs the middleware and the CLI carries the credentials in the userinfo of its --target URL — so this file starts at cli.Run rather than at a handler, where a test of either half alone proves nothing about the pair.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// hookEnvName is the variable the auth fixture's webhook resource reads its signing key from.
const hookEnvName = "STEPS_TEST_AUTH_HOOK_SECRET"

// Not t.Parallel(): startWeb backgrounds cli.Run in this process and stop signals the whole test binary.
func TestBasicAuthGuardsTheDaemonExceptItsWebhooks(t *testing.T) {
	const (
		user = "ops"
		pass = "correct-horse-battery"
	)

	signingKey := "hook-shared-signing-key"
	t.Setenv(hookEnvName, signingKey)

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: `+hookEnvName+`
jobs:
- name: build
  plan:
  - get: push
    trigger: true
  - task: show
    inputs: [push]
    run: cat push/version.json
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"),
		"--basic-auth-username", user, "--basic-auth-password", pass)
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	setIsRefusedWithoutCredentials(t, served, name, path, user, pass)
	setGoesThroughWithThem(t, served, name, path, user, pass)
	everyPageIsBehindThePrompt(t, served, name, user, pass)
	theDeliveryRouteIsNot(t, served, name, signingKey)
}

func setIsRefusedWithoutCredentials(t *testing.T, served *webProcess, name, path, user, pass string) {
	t.Helper()

	none := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", served.target()})
	if none == nil {
		t.Fatal("a set with no credentials was accepted by a daemon that requires them")
	}

	if !strings.Contains(none.Error(), "credentials") || !strings.Contains(none.Error(), "user:password@") {
		t.Errorf("the refusal does not name the fix: %v", none)
	}

	wrong := cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, user, "not-the-password")})
	if wrong == nil {
		t.Fatal("a set with the wrong password was accepted")
	}

	// Neither refusal may carry the password out of the URL it was read from.
	for _, refusal := range []error{none, wrong} {
		if strings.Contains(refusal.Error(), "not-the-password") || strings.Contains(refusal.Error(), pass) {
			t.Errorf("a refusal printed the credentials from the target URL: %v", refusal)
		}
	}
}

func setGoesThroughWithThem(t *testing.T, served *webProcess, name, path, user, pass string) {
	t.Helper()

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, user, pass)})
	})

	if err != nil {
		t.Fatalf("steps pipeline set with credentials in the target URL: %v", err)
	}

	if !strings.Contains(out, "created") {
		t.Errorf("the set did not report creating the pipeline:\n%s", out)
	}

	if strings.Contains(out, pass) {
		t.Errorf("the set printed the password it was given:\n%s", out)
	}
}

// A browser only prompts when told to, so the challenge is as load-bearing as the status.
func everyPageIsBehindThePrompt(t *testing.T, served *webProcess, name, user, pass string) {
	t.Helper()

	code, header := authGet(t, served.addr, "/", "", "")
	if code != http.StatusUnauthorized {
		t.Errorf("GET / with no credentials = %d, want 401", code)
	}

	if challenge := header.Get("WWW-Authenticate"); !strings.HasPrefix(strings.ToLower(challenge), "basic ") {
		t.Errorf("WWW-Authenticate = %q, want a basic challenge so a browser prompts", challenge)
	}

	if code, _ := authGet(t, served.addr, "/", user, pass); code != http.StatusOK {
		t.Errorf("GET / with credentials = %d, want 200", code)
	}

	if code, _ := authGet(t, served.addr, "/p/"+name, "", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /p/%s with no credentials = %d, want 401", name, code)
	}
}

// A webhook delivery carries the sender's own signature, so it is the one route credentials are not asked for — a sender has none to give.
func theDeliveryRouteIsNot(t *testing.T, served *webProcess, name, signingKey string) {
	t.Helper()

	body := []byte(`{"ref":"refs/heads/main"}`)

	code, _ := deliverUnauthenticated(t, served.addr, name, "push", githubSignature(signingKey, "auth-1", body), body)
	if code != http.StatusOK {
		t.Errorf("an unauthenticated signed delivery = %d, want 200 — the hook route is behind basic auth", code)
	}

	// Its own refusals stay its own: a bad signature is the hook saying no, which a challenge header would prove came from the middleware instead.
	code, challenge := deliverUnauthenticated(t, served.addr, name, "push", githubSignature("wrong", "auth-2", body), body)
	if code != http.StatusUnauthorized || challenge != "" {
		t.Errorf("a badly signed delivery = %d with challenge %q, want 401 from the hook itself", code, challenge)
	}
}

// Half a credential pair is a daemon somebody believes is protected and is not, so it does not start.
func TestBasicAuthIsBothOrNeither(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{"--basic-auth-username", "ops"},
		{"--basic-auth-password", "secret"},
	} {
		err := cli.Run(append([]string{"web", "--listen", "127.0.0.1:1"}, args...))
		if err == nil {
			t.Fatalf("steps web started with only %s", args[0])
		}

		if !strings.Contains(err.Error(), "--basic-auth-username") || !strings.Contains(err.Error(), "--basic-auth-password") {
			t.Errorf("%s alone was refused without naming both flags: %v", args[0], err)
		}

		if strings.Contains(err.Error(), "secret") {
			t.Errorf("the refusal printed the password: %v", err)
		}
	}
}

// authTarget is a --target URL carrying credentials, the way an operator writes one.
func authTarget(addr, user, pass string) string {
	return "http://" + user + ":" + pass + "@" + addr
}

func authGet(t *testing.T, addr, path, user, pass string) (int, http.Header) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	if user != "" || pass != "" {
		req.SetBasicAuth(user, pass)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, resp.Header
}

// The challenge header comes back so a refusal by the middleware can be told from a refusal by the hook, which answers 401 of its own for a bad signature.
func deliverUnauthenticated(t *testing.T, addr, pipeline, resource string, header http.Header, body []byte) (int, string) {
	t.Helper()

	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	url := "http://" + addr + "/p/" + pipeline + "/hooks/" + resource

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header = header.Clone()

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

func githubSignature(secret, id string, body []byte) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)

	return http.Header{
		"X-Github-Event":      {"push"},
		"X-Github-Delivery":   {id},
		"X-Hub-Signature-256": {"sha256=" + hex.EncodeToString(mac.Sum(nil))},
	}
}
