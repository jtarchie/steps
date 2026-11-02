package e2e

// The config file is a SEAM: a YAML key becomes a kong flag value becomes a server option. A test that parses the file alone proves none of that, so this one starts the daemon from a file and reads the effect off the wire.

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// The file carries a scalar (basic-auth-username), a list (allowed-host) and a key the command line overrides (listen: an unbindable port — were the file to win, the daemon would never answer and startWeb would fail).
func TestWebReadsItsFlagsFromAConfigFile(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
	config := filepath.Join(dir, "steps.yml")

	writeConfigFile(t, config, `
web:
  listen: 127.0.0.1:1
  basic-auth-username: ops
  allowed-host: [ci.example.com]
  interval: 200ms
`)

	t.Setenv("STEPS_BASIC_AUTH_PASSWORD", "correct-horse")

	served := startWeb(t, "--db", state, "--config", config)
	defer served.stopIfRunning(t)

	if code, _ := authGet(t, served.addr, "/", "", ""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET / = %d, want 401: the file's basic-auth-username did not reach the server", code)
	}

	if code, _ := authGet(t, served.addr, "/", "ops", "correct-horse"); code != http.StatusOK {
		t.Errorf("authenticated GET / = %d, want 200", code)
	}

	if code := hostGet(t, served.addr, "ci.example.com"); code == http.StatusForbidden {
		t.Errorf("GET with Host ci.example.com = 403: the file's allowed-host list did not reach the server")
	}

	if code := hostGet(t, served.addr, "evil.example.com"); code != http.StatusForbidden {
		t.Errorf("GET with Host evil.example.com = %d, want 403", code)
	}
}

// STEPS_CONFIG names the web daemon's file, and only web's hook may open it: kong fires the hook for an env-set flag on every command, so a local command must never stat a path it has no use for.
func TestConfigFileEnvIsWebsAlone(t *testing.T) {
	t.Setenv("STEPS_CONFIG", filepath.Join(t.TempDir(), "absent.yml"))

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "jobs:\n- name: hi\n  plan:\n  - task: say\n    run: echo hi\n")

	err := cli.Run([]string{"validate", path})
	if err != nil {
		t.Fatalf("steps validate with STEPS_CONFIG set to an absent file: %v", err)
	}

	err = cli.Run([]string{"web", "--listen", "127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "absent.yml") {
		t.Fatalf("steps web with STEPS_CONFIG set to an absent file = %v, want a refusal naming it", err)
	}
}

func writeConfigFile(t *testing.T, path, body string) {
	t.Helper()

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func hostGet(t *testing.T, addr, host string) int {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.Host = host

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET with Host %s: %v", host, err)
	}

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

// A secret source on a daemon nobody signs in to hands every secret to whoever can PUT a pipeline, and every step is given STEPS_URL; so it does not start. Login present, the refusal is gone and the unbindable --listen is what stops it.
func TestSecretSourceNeedsALogin(t *testing.T) {
	t.Parallel()

	config := filepath.Join(t.TempDir(), "steps.yml")
	writeConfigFile(t, config, `
web:
  secret:
    source:
      ci:
        command: cat {{ .path | shellquote }}
        lookup:
        - path: /secrets/{{ .pipeline }}/{{ .name }}
`)

	state := filepath.Join(t.TempDir(), "daemon.db")

	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--db", state, "--config", config})
	if err == nil || !strings.Contains(err.Error(), "secret source") || !strings.Contains(err.Error(), "sign in") {
		t.Fatalf("steps web with a secret source and no login = %v, want a refusal naming both", err)
	}

	err = cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--db", state, "--config", config, "--basic-auth-username", "ops", "--basic-auth-password", "pw"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("steps web with a secret source and a login = %v, want only the bind to fail", err)
	}
}
