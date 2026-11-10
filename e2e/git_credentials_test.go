package e2e

import (
	"net/http"
	"net/http/cgi" //nolint:gosec // httpoxy needs Go < 1.6.3; this serves git http-backend to the test only
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

const gitTestToken = "tok-3f9a1c7e5b2d" //nolint:gosec // a fixture the test server checks, not a credential

// gitHTTPRemote returns a bare fixture and its smart-HTTP URL, which answers only Basic auth carrying gitTestToken.
func gitHTTPRemote(t *testing.T) (string, string) {
	t.Helper()

	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed; skipping built-in git resource type test")
	}

	root := t.TempDir()
	bare := filepath.Join(root, "x.git")
	gitIn(t, root, "clone", "-q", "--bare", fixtureRepo(t, "served\n"), bare)
	gitIn(t, bare, "config", "http.receivepack", "true")

	backend := &cgi.Handler{
		Path: gitPath,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || password != gitTestToken {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	return bare, server.URL + "/x.git"
}

// poisonGlobalGitConfig mimics an operator's https-to-ssh insteadOf, which a resource handed a credential must not read.
func poisonGlobalGitConfig(t *testing.T, prefix string) {
	t.Helper()

	home := t.TempDir()
	config := "[url \"ssh://git@nowhere.invalid/\"]\n\tinsteadOf = " + prefix + "\n"

	err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
}

// A resource handed a token must authenticate with it alone, or the operator's ssh agent is asked on every poll.
func TestEndToEndBuiltinGitTokenFetchesAndPushes(t *testing.T) {
	bare, url := gitHTTPRemote(t)
	poisonGlobalGitConfig(t, url)
	t.Setenv("GIT_TOKEN", gitTestToken)

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+url+`
    branch: main
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
  - task: change
    inputs: [repo]
    outputs: [repo]
    run: |
      cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
      cd repo && git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m pushed
  - put: repo
    inputs: [repo]
    params:
      repository: repo
      branch: pushed
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs())
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the get produced no checkout: %v", err)
	}

	if strings.TrimSpace(string(got)) != "served" {
		t.Errorf("checked out %q, want the served repository's README", got)
	}

	if subject := gitOut(t, bare, "log", "-1", "--format=%s", "refs/heads/pushed"); subject != "pushed" {
		t.Errorf("refs/heads/pushed on the remote is %q, want the commit the task made", subject)
	}

	if strings.Contains(logs(), gitTestToken) {
		t.Error("the token appeared in the run's output")
	}
}

// A teammate's push after the lease was taken must survive the put.
func TestEndToEndBuiltinGitPushLeaseRefusesAMovedBranch(t *testing.T) {
	bare, url := gitHTTPRemote(t)
	t.Setenv("GIT_TOKEN", gitTestToken)

	stale := gitOut(t, bare, "rev-parse", "refs/heads/main")
	moved := gitOut(t, bare, "commit-tree", "-p", stale, "-m", "a teammate's", stale+"^{tree}")
	gitIn(t, bare, "update-ref", "refs/heads/main", moved)

	// The get sees moved; the lease still names stale, as one taken before the teammate pushed would.

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+url+`
    branch: main
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
  - task: change
    inputs: [repo]
    outputs: [repo]
    run: cd repo && git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m mine
  - put: repo
    inputs: [repo]
    params:
      repository: repo
      force_with_lease: `+stale+`
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("the put succeeded over a branch that moved past its lease")
	}

	if got := logs(); !strings.Contains(got, "stale info") {
		t.Errorf("the put did not fail on the lease:\n%s", got)
	}

	if head := gitOut(t, bare, "rev-parse", "refs/heads/main"); head != moved {
		t.Errorf("main is %s, want the teammate's %s left alone", head, moved)
	}
}

// A developer's clone usually has an ssh origin; with a token, fetch: must not hand that to the ssh agent.
func TestEndToEndBuiltinGitTokenFetchUsesHTTPSForAnSSHRemote(t *testing.T) {
	clone := clonedFixtureRepo(t, "content\n", "ahead\n")
	gitIn(t, clone, "remote", "set-url", "origin", "git@example.invalid:acme/x.git")

	dir := t.TempDir()
	marker := sshShim(t, dir)
	t.Setenv("GIT_TOKEN", gitTestToken)

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+clone+`
    branch: main
    fetch: true
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected the check to fail: example.invalid does not resolve")
	}

	if got := logs(); !strings.Contains(got, "https://example.invalid/acme/x.git") {
		t.Errorf("the failure does not name the https form of the remote:\n%s", got)
	}

	_, statErr := os.Stat(marker)
	if statErr == nil {
		t.Error("ssh ran: the fetch went to the clone's ssh remote instead of https")
	}
}

// The unreachable case above cannot show the token is actually sent.
func TestEndToEndBuiltinGitTokenFetchRefreshesALocalClone(t *testing.T) {
	bare, url := gitHTTPRemote(t)

	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, filepath.Dir(clone), "clone", "-q", bare, clone)
	gitIn(t, clone, "remote", "set-url", "origin", url)
	// A helper in the clone's own config must not answer in place of the token.
	gitIn(t, clone, "config", "credential.helper", "!f() { echo username=x; echo password=wrong; }; f")

	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, filepath.Dir(work), "clone", "-q", bare, work)
	gitIn(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "upstream")
	gitIn(t, work, "push", "-q", "origin", "HEAD:main")

	poisonGlobalGitConfig(t, url)
	t.Setenv("GIT_TOKEN", gitTestToken)

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+clone+`
    branch: main
    fetch: true
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs())
	}

	if got, want := gitOut(t, clone, "rev-parse", "refs/remotes/origin/main"), gitOut(t, bare, "rev-parse", "refs/heads/main"); got != want {
		t.Errorf("refs/remotes/origin/main = %s, want the remote's %s", got, want)
	}
}

// An unset token would otherwise surface as an anonymous 401 far from its cause.
func TestEndToEndBuiltinGitTokenUnsetFailsByName(t *testing.T) {
	_, url := gitHTTPRemote(t)
	t.Setenv("GIT_TOKEN", "")

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+url+`
    branch: main
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected the check to fail with the token unset")
	}

	// The rendered command is in the error too, so match only what the shell printed.
	if got := logs(); !strings.Contains(got, "names GIT_TOKEN, which is unset or empty") {
		t.Errorf("the failure does not name the unset variable:\n%s", got)
	}
}

// sshShim records ssh's argv and -i file into the returned path, then fails.
func sshShim(t *testing.T, dir string) string {
	t.Helper()

	bin := filepath.Join(dir, "bin")

	err := os.Mkdir(bin, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(dir, "ssh-used")
	script := `#!/bin/sh
{
  printf 'args:'; printf ' %s' "$@"; printf '\n'
  while [ $# -gt 0 ]; do
    if [ "$1" = -i ]; then printf 'key:'; cat "$2"; printf 'keyfile:%s\n' "$2"; fi
    shift
  done
} > ` + marker + `
exit 1
`

	err = os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700) //nolint:gosec // an executable fixture under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	return marker
}

// A key handed to a resource must be the only identity, and must not outlive the check on disk.
func TestEndToEndBuiltinGitPrivateKeyIsTheOnlyIdentity(t *testing.T) {
	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed; skipping built-in git resource type test")
	}

	dir := t.TempDir()
	marker := sshShim(t, dir)

	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----"

	t.Setenv("GIT_KEY", key)

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_KEY]
  source:
    uri: ssh://git@example.invalid/acme/x.git
    branch: main
    private_key_env: GIT_KEY
jobs:
- name: build
  plan:
  - get: repo
`)

	err = cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected the check to fail: the ssh shim exits 1")
	}

	recorded, err := os.ReadFile(marker) //nolint:gosec // a t.TempDir()-scoped file the shim wrote
	if err != nil {
		t.Fatalf("ssh never ran: %v", err)
	}

	got := string(recorded)
	for _, want := range []string{"-F /dev/null", "IdentitiesOnly=yes", "IdentityAgent=none", "key:" + key} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh was not run with %q:\n%s", want, got)
		}
	}

	_, keyfile, _ := strings.Cut(got, "keyfile:")

	_, statErr := os.Stat(strings.TrimSpace(keyfile)) //nolint:gosec // a path the shim wrote under t.TempDir()
	if statErr == nil {
		t.Errorf("the key file %s outlived the check", strings.TrimSpace(keyfile))
	}
}
