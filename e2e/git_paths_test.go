package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commitTouching writes path in repo and commits it, returning the commit.
func commitTouching(t *testing.T, repo, path string) string {
	t.Helper()

	full := filepath.Join(repo, path)

	err := os.MkdirAll(filepath.Dir(full), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(full, []byte(path+" "+gitOut(t, repo, "rev-list", "--count", "--all")+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "touch "+path)

	return gitOut(t, repo, "rev-parse", "HEAD")
}

const pathsPipeline = `
resources:
- name: repo
  type: git
  source:
    uri: %s
    branch: main
%s
jobs:
- name: build
  plan:
  - get: repo
  - task: record
    inputs: [repo]
    run: git -C repo rev-parse HEAD > %s
`

// checkedOut runs the pipeline and returns the commit its get checked out.
func checkedOut(t *testing.T, path, seen string) string {
	t.Helper()

	mustRun(t, "run", path, "--job", "build", "--force")

	got, err := os.ReadFile(seen) //nolint:gosec // a t.TempDir()-scoped file the pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(got))
}

// TestEndToEndBuiltinGitPathsFollowOnlyCommitsThatTouchThem: with paths:, the version is the newest commit that changed one of them, so a commit elsewhere in the repository is no new version; ignore_paths: takes paths back out, as in Concourse's git resource.
func TestEndToEndBuiltinGitPathsFollowOnlyCommitsThatTouchThem(t *testing.T) {
	t.Parallel()

	origin := fixtureRepo(t, "root\n")
	image := commitTouching(t, origin, "images/tools/Dockerfile")
	commitTouching(t, origin, "work/bots.yml")

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	path := writePipeline(t, dir, fmt.Sprintf(pathsPipeline, origin, "    paths: [images/tools]\n    ignore_paths: [images/tools/README.md]", seen))

	if got := checkedOut(t, path, seen); got != image {
		t.Errorf("checked out %s, want %s, the last commit under images/tools", got, image)
	}

	commitTouching(t, origin, "images/tools/README.md")

	if got := checkedOut(t, path, seen); got != image {
		t.Errorf("after a commit only to an ignored path, checked out %s, want still %s", got, image)
	}

	again := commitTouching(t, origin, "images/tools/install.sh")

	if got := checkedOut(t, path, seen); got != again {
		t.Errorf("after a commit under images/tools, checked out %s, want it, %s", got, again)
	}
}

// TestEndToEndBuiltinGitIgnorePathsAlone: ignore_paths: without paths: is every commit but those that touch only ignored paths.
func TestEndToEndBuiltinGitIgnorePathsAlone(t *testing.T) {
	t.Parallel()

	origin := fixtureRepo(t, "root\n")
	code := commitTouching(t, origin, "src/main.go")
	commitTouching(t, origin, "docs/readme.md")

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	path := writePipeline(t, dir, fmt.Sprintf(pathsPipeline, origin, "    ignore_paths: [docs]", seen))

	if got := checkedOut(t, path, seen); got != code {
		t.Errorf("checked out %s, want %s, the last commit outside docs", got, code)
	}
}

// TestEndToEndBuiltinGitPathsReadAFetchedClone: under fetch: true the paths are read from the clone's freshly fetched ref, so a commit pushed upstream that touches them is the version.
func TestEndToEndBuiltinGitPathsReadAFetchedClone(t *testing.T) {
	t.Parallel()

	origin := fixtureRepo(t, "root\n")
	commitTouching(t, origin, "images/a")

	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, filepath.Dir(clone), "clone", "-q", origin, clone)

	upstream := commitTouching(t, origin, "images/b")
	commitTouching(t, origin, "other.txt")

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	path := writePipeline(t, dir, fmt.Sprintf(pathsPipeline, clone, "    fetch: true\n    paths: [images]", seen))

	if got := checkedOut(t, path, seen); got != upstream {
		t.Errorf("checked out %s, want %s, fetched from upstream and the last under images", got, upstream)
	}
}

// TestValidateRefusesPathsWithATagPattern: a tag is a version whatever it touched, so paths: beside tag_pattern: would read as a filter and bind nothing.
func TestValidateRefusesPathsWithATagPattern(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
resources:
- name: repo
  type: git
  source: {uri: https://example.com/r.git, tag_pattern: "v*", paths: [src]}
jobs:
- name: j
  plan:
  - get: repo
`)

	err := stepsRun(t, []string{"validate", "--syntax-only", path})
	if err == nil || !strings.Contains(err.Error(), "paths") || !strings.Contains(err.Error(), "tag_pattern") {
		t.Errorf("validate: err = %v, want paths: beside tag_pattern: refused", err)
	}
}
