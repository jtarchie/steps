package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// bareFixture is a bare clone of a one-commit fixture: a remote a put can push to by path.
func bareFixture(t *testing.T) string {
	t.Helper()

	bare := filepath.Join(t.TempDir(), "x.git")
	gitIn(t, t.TempDir(), "clone", "-q", "--bare", fixtureRepo(t, "served\n"), bare)

	return bare
}

// refPipeline gets main, makes two commits (first, second) and records both shas, then puts with the given params.
func refPipeline(t *testing.T, bare, dir, params string) string {
	t.Helper()

	return writePipeline(t, dir, `
resources:
- name: repo
  type: git
  source:
    uri: `+bare+`
    branch: main
jobs:
- name: build
  plan:
  - get: repo
  - task: change
    inputs: [repo]
    outputs: [repo]
    run: |
      cd repo
      git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m first
      git rev-parse HEAD > `+filepath.Join(dir, "first")+`
      git -c user.name=t -c user.email=t@example.com commit -q --allow-empty -m second
  - put: repo
    inputs: [repo]
    params:
      repository: repo
      branch: pushed
`+params)
}

func readSHA(t *testing.T, path string) string {
	t.Helper()

	got, err := os.ReadFile(path) //nolint:gosec // a t.TempDir()-scoped file the pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(got))
}

// params.ref pushes that commit instead of HEAD; the put's version is the sha pushed.
func TestEndToEndBuiltinGitPutRefPushesThatCommit(t *testing.T) {
	bare := bareFixture(t)
	dir := t.TempDir()
	path := refPipeline(t, bare, dir, "      ref: HEAD~1\n")

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	out := logs()

	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	first := readSHA(t, filepath.Join(dir, "first"))

	if got := gitOut(t, bare, "rev-parse", "refs/heads/pushed"); got != first {
		t.Errorf("pushed is %s, want ref's commit %s, not HEAD", got, first)
	}

	if want := "result=map[ref:" + first + "]"; !strings.Contains(out, want) {
		t.Errorf("the put's version is not the sha pushed; want %s in:\n%s", want, out)
	}
}

// A ref that is no commit in the repository fails the put by name, and sends nothing.
func TestEndToEndBuiltinGitPutBadRefFailsNamingIt(t *testing.T) {
	bare := bareFixture(t)
	path := refPipeline(t, bare, t.TempDir(), "      ref: no-such-commit\n")

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("the put succeeded with a ref that is no commit")
	}

	if got := logs(); !strings.Contains(got, "params.ref") {
		t.Errorf("the failure does not name params.ref:\n%s", got)
	}

	if out := gitOut(t, bare, "branch", "--list", "pushed"); out != "" {
		t.Errorf("the remote has %q after a failed put", out)
	}
}

// ref does not weaken the lease: the branch moved past it, so the push is refused.
func TestEndToEndBuiltinGitPutRefKeepsTheLease(t *testing.T) {
	bare := bareFixture(t)
	stale := gitOut(t, bare, "rev-parse", "refs/heads/main")
	moved := gitOut(t, bare, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit-tree", "-p", stale, "-m", "a teammate's", stale+"^{tree}")
	gitIn(t, bare, "update-ref", "refs/heads/pushed", moved)

	path := refPipeline(t, bare, t.TempDir(), "      ref: HEAD~1\n      force_with_lease: "+stale+"\n")

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("the put succeeded over a branch that moved past its lease")
	}

	if got := logs(); !strings.Contains(got, "stale info") {
		t.Errorf("the put did not fail on the lease:\n%s", got)
	}

	if head := gitOut(t, bare, "rev-parse", "refs/heads/pushed"); head != moved {
		t.Errorf("pushed is %s, want the teammate's %s left alone", head, moved)
	}
}

// With the right lease and a force push, ref still lands the commit it names.
func TestEndToEndBuiltinGitPutRefWithLeaseAdvances(t *testing.T) {
	bare := bareFixture(t)
	stale := gitOut(t, bare, "rev-parse", "refs/heads/main")
	gitIn(t, bare, "update-ref", "refs/heads/pushed", stale)

	dir := t.TempDir()
	path := refPipeline(t, bare, dir, "      ref: HEAD~1\n      force_with_lease: "+stale+"\n")

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs())
	}

	if got, first := gitOut(t, bare, "rev-parse", "refs/heads/pushed"), readSHA(t, filepath.Join(dir, "first")); got != first {
		t.Errorf("pushed is %s, want %s", got, first)
	}
}
