package e2e

// `checkout: git`: a github-prs or github-comments get that lays down a git
// repository instead of a bare tree, cloned by steps from the repository
// GitHub names, deep enough that the merge base with the pull request's base
// branch is in it and nothing more.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitPR is pull request #7 over repo's history.
func gitPR(repo fakeGitRepo) fakePR {
	return fakePR{Number: 7, SHA: repo.Head, Author: "alice", Base: "main", MergeBase: repo.MergeBase, AheadBy: 2, BehindBy: 1, Diff: "diff --git a/feature.txt b/feature.txt\n"}
}

// gitChecks is a task body that holds the repository to what a reviewer
// runs against it, and writes ok to marker when every check passed.
func gitChecks(dir string, repo fakeGitRepo, marker string) string {
	return `
      set -eu
      cd ` + dir + `
      test "$(git rev-parse HEAD)" = ` + repo.Head + `
      test "$(git merge-base HEAD origin/main)" = ` + repo.MergeBase + `
      test "$(git rev-parse origin/main)" = ` + repo.BaseTip + `
      test "$(git diff --name-only origin/main...HEAD)" = feature.txt
      test "$(cat pr.mergebase)" = ` + repo.MergeBase + `
      test -z "$(git status --porcelain)"
      test "$(git rev-parse --is-shallow-repository)" = true
      if git cat-file -e ` + repo.First + `; then echo "fetched past the merge base"; exit 1; fi
      if grep -rq ` + fakeGitHubToken + ` .git; then echo "the token is on disk"; exit 1; fi
      echo ok > ` + marker + `
`
}

func gitPRPipeline(fake *fakeGitHub, repo fakeGitRepo, marker string) string {
	return `
resources:
- name: pr
  type: github-prs
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
    checkout: git
jobs:
- name: look
  plan:
  - get: pr
  - task: check
    inputs: [pr]
    run: |` + gitChecks("pr", repo, marker)
}

func assertMarker(t *testing.T, marker string) {
	t.Helper()

	if got := strings.TrimSpace(readFileString(t, marker)); got != "ok" {
		t.Errorf("marker = %q, want ok", got)
	}
}

// TestEndToEndGitHubPRCheckoutGitIsARepository is the feature: the get is a
// repository at the head commit, with origin/<base> beside it, shallow at the
// merge base, its metadata files ignored, and no token left on disk.
func TestEndToEndGitHubPRCheckoutGitIsARepository(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	repo := newFakeGitRepo(t)
	fake.serveGit(repo)
	fake.addPR(gitPR(repo))

	dir := t.TempDir()
	marker := filepath.Join(dir, "ok")
	mustRun(t, "run", writePipeline(t, dir, gitPRPipeline(fake, repo, marker)), "--job", "look")

	assertMarker(t, marker)
}

// TestEndToEndGitHubPRCheckoutGitDeepensPastAnUndercount: the base branch
// moved between the compare and the fetch, so the depth the compare implied
// stops short of the merge base, and the get fetches until it is there.
func TestEndToEndGitHubPRCheckoutGitDeepensPastAnUndercount(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	repo := newFakeGitRepo(t)
	fake.serveGit(repo)

	pr := gitPR(repo)
	pr.BehindBy = 0
	fake.addPR(pr)

	dir := t.TempDir()
	marker := filepath.Join(dir, "ok")
	mustRun(t, "run", writePipeline(t, dir, withoutShallowChecks(gitPRPipeline(fake, repo, marker), repo)), "--job", "look")

	assertMarker(t, marker)
}

// TestEndToEndGitHubPRCheckoutGitFallsBackToThePullRef: the pull request
// moved on since its version was recorded, and a server that fetches only
// advertised refs refuses the version's commit by id; the get fetches it
// through refs/pull/<n>/head instead, which is also how a fork's commit, on
// no branch of this repository, is reached.
func TestEndToEndGitHubPRCheckoutGitFallsBackToThePullRef(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	repo := newFakeGitRepo(t)
	repo.pushToPull(t)
	fake.serveGit(repo)
	fake.refuseSHAWants = true
	fake.addPR(gitPR(repo))

	dir := t.TempDir()
	marker := filepath.Join(dir, "ok")
	mustRun(t, "run", writePipeline(t, dir, withoutShallowChecks(gitPRPipeline(fake, repo, marker), repo)), "--job", "look")

	assertMarker(t, marker)
}

// withoutShallowChecks drops the checks that the history stops at the merge base, for a get that had to fetch more.
func withoutShallowChecks(pipeline string, repo fakeGitRepo) string {
	return strings.Replace(pipeline, `      test "$(git rev-parse --is-shallow-repository)" = true
      if git cat-file -e `+repo.First+`; then echo "fetched past the merge base"; exit 1; fi
`, "", 1)
}

// TestEndToEndGitHubCommentCheckoutGitIsARepository: a comment brings the
// pull request it sits on as a repository too.
func TestEndToEndGitHubCommentCheckoutGitIsARepository(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	repo := newFakeGitRepo(t)
	fake.serveGit(repo)
	fake.addPR(gitPR(repo))
	fake.addComment(fakeComment{ID: 100, Number: 7, Kind: "conversation", Author: "alice", Body: "/bot rebase", Updated: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)})

	dir := t.TempDir()
	marker := filepath.Join(dir, "ok")
	mustRun(t, "run", writePipeline(t, dir, `
resources:
- name: ask
  type: github-comments
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    body: '^/bot'
    checkout: git
jobs:
- name: act
  plan:
  - get: ask
  - task: check
    inputs: [ask]
    run: |`+gitChecks("ask", repo, marker)), "--job", "act")

	assertMarker(t, marker)
}
