package e2e

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A pipeline access: hides is told to its reader as a name nobody set, so every answer about one must be the answer about a name nobody set: byte for byte, to a signed-in viewer and to somebody who never signed in.
func TestAHiddenPipelineAnswersExactlyAsAMissingOne(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	hidden := filepath.Join(dir, "hidden.yml")
	writePipelineFile(t, hidden, `
access:
  viewer: UserID != "eve"
mcp_exports:
- name: deploys
  access: "true"
  tools:
  - name: hi
    description: say hi
    run: echo hi
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`)

	open := filepath.Join(dir, "open.yml")
	writePipelineFile(t, open, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--role", `admin=UserID == "alex"`,
		"--local-user", "alex:"+hashed(t, "alex"), "--local-user", "eve:"+hashed(t, "eve"))
	defer served.stopIfRunning(t)

	for name, path := range map[string]string{"zebrahidden": hidden, "zebraopen": open} {
		err := stepsRun(t, []string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", authTarget(served.addr, "alex", "alex")})
		if err != nil {
			t.Fatalf("set %s: %v", name, err)
		}
	}

	same := func(who, method, hiddenPath, missingPath string) {
		t.Helper()

		hiddenCode, hiddenBody := rawRequest(t, served.addr, who, method, hiddenPath)
		missingCode, missingBody := rawRequest(t, served.addr, who, method, missingPath)

		if hiddenCode != missingCode || strings.ReplaceAll(hiddenBody, "zebrahidden", "zebranope") != missingBody {
			t.Errorf("%s %s %s = %d, but %s = %d; the bodies differ: %v", who, method, hiddenPath, hiddenCode, missingPath, missingCode, hiddenBody != missingBody)
		}
	}

	same("eve", http.MethodGet, "/p/zebrahidden", "/p/zebranope")
	same("eve", http.MethodPost, "/api/pipelines/zebrahidden/pause", "/api/pipelines/zebranope/pause")
	same("eve", http.MethodPost, "/p/zebrahidden/pause", "/p/zebranope/pause")
	same("", http.MethodPost, "/p/zebrahidden/hooks/x", "/p/zebranope/hooks/x")
	same("", http.MethodPost, "/p/zebraopen/hooks/x", "/p/zebranope/hooks/x")
	same("", http.MethodGet, "/.well-known/oauth-protected-resource/mcp/x/zebrahidden/deploys", "/.well-known/oauth-protected-resource/mcp/x/zebranope/deploys")

	for _, path := range []string{"/", "/p/zebranope/hooks/x", "/mcp/callback"} {
		for _, who := range []string{"", "nobody"} {
			if _, body := rawRequest(t, served.addr, who, http.MethodGet, path); strings.Contains(body, "zebrahidden") || strings.Contains(body, "zebraopen") || strings.Contains(body, `href="/activity"`) {
				t.Errorf("%q GET %s names a pipeline, or offers the admin's activity log, to somebody who is not signed in:\n%s", who, path, body)
			}
		}
	}
}

// rawRequest sends one request as who ("" for nobody signed in; a user's password is their name) and reads everything back, redirects not followed.
func rawRequest(t *testing.T, addr, who, method, path string) (int, string) {
	t.Helper()

	client := &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Accept", "text/html")

	if who != "" {
		req.SetBasicAuth(who, who)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return resp.StatusCode, string(body)
}
