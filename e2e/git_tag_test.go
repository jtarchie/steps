package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// tagFixture is a repository with v1 (lightweight), v2 (annotated), v10 and a tag that does not match v*, on three commits.
func tagFixture(t *testing.T) string {
	t.Helper()

	origin := fixtureRepo(t, "tagged\n")
	commit := []string{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "more"}

	gitIn(t, origin, "tag", "v1")
	gitIn(t, origin, commit...)
	gitIn(t, origin, "-c", "user.name=t", "-c", "user.email=t@example.com", "tag", "-a", "v2", "-m", "annotated")
	gitIn(t, origin, commit...)
	gitIn(t, origin, "tag", "v10")
	gitIn(t, origin, "tag", "nope")

	return origin
}

const tagPipeline = `
resources:
- name: repo
  type: git
  source:
    uri: %s
    tag_pattern: "v*"
jobs:
- name: build
  plan:
  - get: repo
    version: every
  - task: record
    inputs: [repo]
    run: echo "$(git -C repo describe --tags --exact-match) $(git -C repo rev-parse HEAD)" >> %s
`

// tag_pattern tracks tags: one version per match, oldest first by version order, each checked out where describe names the tag, a tag object peeled to its commit.
func TestEndToEndBuiltinGitTagPatternTracksTags(t *testing.T) {
	origin := tagFixture(t)
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")
	path := writePipeline(t, dir, fmt.Sprintf(tagPipeline, origin, seen))

	logs := captureStderr(t)

	// One run builds one version, oldest first; `run it again to build the next`.
	for range 3 {
		err := cli.Run([]string{path})
		if err != nil {
			t.Fatalf("run: %v\n%s", err, logs())
		}
	}

	got, err := os.ReadFile(seen) //nolint:gosec // a t.TempDir()-scoped file the pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"v1 " + gitOut(t, origin, "rev-parse", "v1^{commit}"),
		"v2 " + gitOut(t, origin, "rev-parse", "v2^{commit}"),
		"v10 " + gitOut(t, origin, "rev-parse", "v10^{commit}"),
	}, "\n") + "\n"

	if string(got) != want {
		t.Errorf("checked out:\n%s\nwant:\n%s", got, want)
	}
}

// No tag yet is an empty list: nothing to build, and no git error.
func TestEndToEndBuiltinGitTagPatternWithNoTagsIsEmpty(t *testing.T) {
	origin := fixtureRepo(t, "untagged\n")
	dir := t.TempDir()
	path := writePipeline(t, dir, fmt.Sprintf(tagPipeline, origin, filepath.Join(dir, "seen")))

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	out := logs()

	if strings.Contains(out, "git:") || strings.Contains(out, "exit status") {
		t.Errorf("a repository without tags is an error, want an empty version list:\n%s", out)
	}

	if err != nil {
		t.Errorf("a repository without tags failed the run, want nothing to build: %v\n%s", err, out)
	}

	_, err = os.Stat(filepath.Join(dir, "seen"))
	if err == nil {
		t.Error("a build ran with no tag to build")
	}
}

// A token reaches the tag check and the get as it does a branch's, and the operator's rewrite of the uri never does.
func TestEndToEndBuiltinGitTagPatternUsesTheToken(t *testing.T) {
	bare, url := gitHTTPRemote(t)
	poisonGlobalGitConfig(t, url)
	t.Setenv("GIT_TOKEN", gitTestToken)
	gitIn(t, bare, "tag", "v1")

	dir := t.TempDir()
	path := writePipeline(t, dir, `
resources:
- name: repo
  type: git
  env: [GIT_TOKEN]
  source:
    uri: `+url+`
    tag_pattern: "v*"
    token_env: GIT_TOKEN
jobs:
- name: build
  plan:
  - get: repo
  - task: record
    inputs: [repo]
    run: git -C repo describe --tags --exact-match > `+filepath.Join(dir, "seen")+`
`)

	logs := captureStderr(t)

	err := cli.Run([]string{path})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, logs())
	}

	if got := readSHA(t, filepath.Join(dir, "seen")); got != "v1" {
		t.Errorf("checked out %q, want the tag v1", got)
	}
}
