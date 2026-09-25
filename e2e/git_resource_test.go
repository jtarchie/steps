package e2e

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// fixtureRepo builds a real one-commit git repository in a temp dir and
// returns its path. Skips when git isn't installed, in the spirit of the
// existing docker/btrfs opt-in gates: the built-in git type shells out to the
// real binary, so proving it works means running it.
func fixtureRepo(t *testing.T, content string) string {
	t.Helper()

	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not installed; skipping built-in git resource type test")
	}

	dir := t.TempDir()

	for _, args := range [][]string{
		{"init", "-q", "-b", "main", "."},
		{"config", "user.email", "steps@example.com"},
		{"config", "user.name", "steps tests"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args are literals from the table above, not input
		cmd.Dir = dir

		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("git %v: %v\n%s", args, runErr, out)
		}
	}

	err = os.WriteFile(filepath.Join(dir, "README.md"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", "initial"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args are literals from the table above, not input
		cmd.Dir = dir

		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("git %v: %v\n%s", args, runErr, out)
		}
	}

	return dir
}

// A pipeline that wants a checkout declares a resource and nothing else: no
// resource_types: block, no hand-written check/in shell.
func TestEndToEndBuiltinGitResourceType(t *testing.T) {
	origin := fixtureRepo(t, "hello from the fixture\n")
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+origin+`
    branch: main
jobs:
- name: build
  plan:
  - get: repo
  - task: copy
    inputs: [repo]
    run: cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
`)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the checkout did not produce the file: %v", err)
	}

	if strings.TrimSpace(string(got)) != "hello from the fixture" {
		t.Errorf("checked-out content = %q, want the fixture's README", string(got))
	}
}

// branch: is optional — omitted, the type follows the remote's HEAD.
func TestEndToEndBuiltinGitFollowsHeadWithoutBranch(t *testing.T) {
	origin := fixtureRepo(t, "head content\n")
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+origin+`
jobs:
- name: build
  plan:
  - get: repo
  - task: copy
    inputs: [repo]
    run: cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
`)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the checkout did not produce the file: %v", err)
	}

	if strings.TrimSpace(string(got)) != "head content" {
		t.Errorf("checked-out content = %q, want the fixture's README", string(got))
	}
}

// A branch that doesn't exist fails the check loudly. Left to ls-remote alone
// it prints nothing, which reads as an empty version list — "no versions yet"
// — and the job would quietly do nothing at all.
func TestEndToEndBuiltinGitUnknownBranchFails(t *testing.T) {
	origin := fixtureRepo(t, "content\n")
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+origin+`
    branch: nonexistent
jobs:
- name: build
  plan:
  - get: repo
`)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected a failure for a branch that does not exist")
	}
}

// gitIn runs one git command in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args come from the callers below, all literals
	cmd.Dir = dir

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// clonedFixtureRepo returns a CLONE whose local branch is one commit ahead of
// its remote-tracking ref — the shape every working copy on a developer's
// machine has, and the one a fresh `git init` fixture never reproduces.
func clonedFixtureRepo(t *testing.T, originContent, aheadContent string) string {
	t.Helper()

	origin := fixtureRepo(t, originContent)
	clone := filepath.Join(t.TempDir(), "clone")

	gitIn(t, filepath.Dir(clone), "clone", "-q", origin, clone)
	gitIn(t, clone, "config", "user.email", "steps@example.com")
	gitIn(t, clone, "config", "user.name", "steps tests")

	err := os.WriteFile(filepath.Join(clone, "README.md"), []byte(aheadContent), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitIn(t, clone, "commit", "-qam", "ahead of origin")

	return clone
}

// TestEndToEndBuiltinGitAgainstAClone is a regression test for two bugs that
// `git ls-remote <pattern>` produces against any repo that has remotes.
//
// `main` matches refs/heads/main AND refs/remotes/origin/main, so the check
// printed TWO SHAs into one JSON string — a literal newline inside it, which
// failed the whole job with "invalid character '\n' in string literal". And
// the two SHAs differ (a remote-tracking ref is only as fresh as the last
// fetch), so taking the wrong line silently plans against a stale commit,
// which is the quieter half of the bug.
func TestEndToEndBuiltinGitAgainstAClone(t *testing.T) {
	clone := clonedFixtureRepo(t, "origin content\n", "local content\n")
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+clone+`
    branch: main
jobs:
- name: build
  plan:
  - get: repo
  - task: copy
    inputs: [repo]
    run: cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
`)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the checkout did not produce the file: %v", err)
	}

	if strings.TrimSpace(string(got)) != "local content" {
		t.Errorf("checked-out content = %q, want the clone's own branch head, not origin's stale ref", string(got))
	}
}

// The same repo with no branch:, where the pattern HEAD also matches
// refs/remotes/origin/HEAD.
func TestEndToEndBuiltinGitAgainstACloneFollowingHead(t *testing.T) {
	clone := clonedFixtureRepo(t, "origin content\n", "local head content\n")
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+clone+`
jobs:
- name: build
  plan:
  - get: repo
  - task: copy
    inputs: [repo]
    run: cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
`)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the checkout did not produce the file: %v", err)
	}

	if strings.TrimSpace(string(got)) != "local head content" {
		t.Errorf("checked-out content = %q, want the clone's own HEAD", string(got))
	}
}

// gitOut runs one git command in dir and returns its trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args come from the callers below, all literals or fixture paths
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}

	return strings.TrimSpace(string(out))
}

// cloneSnapshot is everything fetch: must leave alone in a developer's clone:
// every ref except the one tracking ref it exists to move, the working tree,
// and FETCH_HEAD.
func cloneSnapshot(t *testing.T, clone string) string {
	t.Helper()

	var refs []string

	for line := range strings.SplitSeq(gitOut(t, clone, "for-each-ref", "--format=%(refname) %(if)%(symref)%(then)-> %(symref)%(else)%(objectname)%(end)"), "\n") {
		if !strings.HasPrefix(line, "refs/remotes/origin/main ") {
			refs = append(refs, line)
		}
	}

	readme, err := os.ReadFile(filepath.Join(clone, "README.md")) //nolint:gosec // a file inside the test's own fixture clone
	if err != nil {
		t.Fatal(err)
	}

	_, statErr := os.Stat(filepath.Join(clone, ".git", "FETCH_HEAD"))

	return strings.Join(refs, "\n") +
		"\nstatus:" + gitOut(t, clone, "status", "--porcelain") +
		"\nreadme:" + string(readme) +
		"\nfetch_head:" + strconv.FormatBool(statErr == nil)
}

// runGitCopy runs a get of the clone plus a copy of its README, in a fresh
// directory (so a fresh state db), and returns what was checked out.
func runGitCopy(t *testing.T, clone, fetch string) string {
	t.Helper()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+clone+`
    branch: main
    fetch: `+fetch+`
jobs:
- name: build
  plan:
  - get: repo
  - task: copy
    inputs: [repo]
    run: cp repo/README.md `+filepath.Join(dir, "fetched.txt")+`
`)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run (fetch: %s): %v", fetch, err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "fetched.txt")) //nolint:gosec // path is a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatalf("the checkout did not produce the file: %v", err)
	}

	return strings.TrimSpace(string(got))
}

// fetch: true reports the REMOTE's head, not the clone's memory of it — the
// staleness #135 was about — and moves nothing in the clone but the one
// remote-tracking ref it read.
func TestEndToEndBuiltinGitFetchRefreshesALocalClone(t *testing.T) {
	clone := clonedFixtureRepo(t, "origin v1\n", "local ahead\n")
	origin := gitOut(t, clone, "config", "--get", "remote.origin.url")

	err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("origin v2\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitIn(t, origin, "commit", "-qam", "origin v2")
	gitIn(t, origin, "tag", "v2")

	before := cloneSnapshot(t, clone)
	localMain := gitOut(t, clone, "rev-parse", "refs/heads/main")

	if got := runGitCopy(t, clone, "false"); got != "local ahead" {
		t.Errorf("fetch: false checked out %q, want the clone's own branch head", got)
	}

	if got := runGitCopy(t, clone, "true"); got != "origin v2" {
		t.Errorf("fetch: true checked out %q, want the remote's new head", got)
	}

	if after := cloneSnapshot(t, clone); after != before {
		t.Errorf("fetch: true changed the clone beyond refs/remotes/origin/main\nbefore:\n%s\nafter:\n%s", before, after)
	}

	if got := gitOut(t, clone, "rev-parse", "refs/heads/main"); got != localMain {
		t.Errorf("local main moved from %s to %s", localMain, got)
	}
}

// A fetch that cannot happen fails the check rather than falling back to the
// ref the clone already had — that ref is the stale answer fetch: exists to
// refuse.
func TestEndToEndBuiltinGitFetchFailureFailsTheCheck(t *testing.T) {
	for name, breakRemote := range map[string]func(t *testing.T, clone, origin string){
		"unreachable remote": func(t *testing.T, clone, _ string) {
			t.Helper()
			gitIn(t, clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing"))
		},
		"branch deleted on the remote": func(t *testing.T, _, origin string) {
			t.Helper()
			gitIn(t, origin, "branch", "-D", "feature")
		},
	} {
		t.Run(name, func(t *testing.T) {
			origin := fixtureRepo(t, "content\n")
			gitIn(t, origin, "branch", "feature")

			// A single-branch clone whose feature ref was fetched by hand: its
			// configured refspec never updates or prunes that ref, so only the
			// check's own refspec can notice the branch is gone.
			clone := filepath.Join(t.TempDir(), "clone")
			gitIn(t, filepath.Dir(clone), "clone", "-q", "--single-branch", "--branch", "main", origin, clone)
			gitIn(t, clone, "fetch", "-q", "origin", "+refs/heads/feature:refs/remotes/origin/feature")

			breakRemote(t, clone, origin)

			dir := t.TempDir()
			path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+clone+`
    branch: feature
    fetch: true
jobs:
- name: build
  plan:
  - get: repo
`)

			err := cli.Run([]string{path})
			if err == nil {
				t.Fatal("expected the check to fail, not to report the clone's stale refs/remotes/origin/feature")
			}
		})
	}
}

// A poll has no terminal and no check timeout, so a credential prompt would
// freeze that pipeline's polling for good. The check must fail instead — and
// fail at the prompt, so the remote has to be one that asks for credentials:
// an unreachable one fails before any prompt and proves nothing.
func TestEndToEndBuiltinGitFetchNeverPrompts(t *testing.T) {
	clone := clonedFixtureRepo(t, "content\n", "ahead\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="steps"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	gitIn(t, clone, "remote", "set-url", "origin", server.URL+"/x.git")
	// Answer git's credential lookup from nothing, so a host keychain helper
	// cannot supply (or pop up a dialog for) credentials to the test server.
	gitIn(t, clone, "config", "credential.helper", "")

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+clone+`
    branch: main
    fetch: true
jobs:
- name: build
  plan:
  - get: repo
`)

	logs := captureStderr(t)
	done := make(chan error, 1)

	go func() { done <- cli.Run([]string{path}) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected the check to fail against a remote that demands credentials")
		}

		if got := logs(); !strings.Contains(got, "terminal prompts disabled") {
			t.Errorf("stderr lacks git's \"terminal prompts disabled\" — anything else means git tried to prompt:\n%s", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the check hung instead of failing")
	}
}

// The poll-safe ssh defaults are a fallback, not an override: an operator who
// names their own GIT_SSH_COMMAND (a key, a jump host) through the resource's
// env: must have it used, or fetch: fails auth that plain git gets right.
func TestEndToEndBuiltinGitFetchKeepsTheOperatorsSSHCommand(t *testing.T) {
	clone := clonedFixtureRepo(t, "content\n", "ahead\n")
	gitIn(t, clone, "remote", "set-url", "origin", "ssh://git@example.invalid/x.git")

	dir := t.TempDir()
	marker := filepath.Join(dir, "ssh-used")
	script := filepath.Join(dir, "ssh.sh")

	err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\nexit 1\n"), 0o700) //nolint:gosec // an executable fixture under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("GIT_SSH_COMMAND", script)

	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_SSH_COMMAND]
  source:
    uri: `+clone+`
    branch: main
    fetch: true
jobs:
- name: build
  plan:
  - get: repo
`)

	err = cli.Run([]string{path})
	if err == nil {
		t.Fatal("expected the check to fail: the operator's ssh command exits 1")
	}

	_, statErr := os.Stat(marker)
	if statErr != nil {
		t.Errorf("the operator's GIT_SSH_COMMAND never ran: %v", statErr)
	}
}
