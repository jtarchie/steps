package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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

	err := run([]string{path})
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

	err := run([]string{path})
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

	err := run([]string{path})
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

	err := run([]string{path})
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

	err := run([]string{path})
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
