package e2e

import (
	"strings"
	"testing"
	"time"
)

// planIssue is the issue the github-issues tests build: two comments, so issue.md has a discussion to carry.
func planIssue(fake *fakeGitHub) {
	fake.addIssue(fakeIssue{Number: 4, Title: "Plan the cache", Body: "It should key on content.", Author: "alice", Labels: []string{"self-build"}})
	fake.addComment(fakeComment{ID: 801, Number: 4, Kind: "issue", Author: "bob", Body: "Use the digest.", Updated: time.Date(2026, 10, 1, 9, 30, 0, 0, time.UTC)})
	fake.addComment(fakeComment{ID: 802, Number: 4, Kind: "issue", Author: "alice", Body: "Agreed.", Updated: time.Date(2026, 10, 1, 9, 35, 0, 0, time.UTC)})
}

// issuesPipeline builds each open issue labeled self-build and marks it factory:building, a label the filter does not name. The filter is spelt Self-Build because GitHub compares label names without regard to case.
const issuesPipeline = `
resources:
- name: issue
  type: github-issues
  source:
    repo: ` + fakeRepo + `
    endpoint: ENDPOINT
    labels: [Self-Build]
- name: state
  type: github-label
  source:
    repo: ` + fakeRepo + `
    endpoint: ENDPOINT
jobs:
- name: build
  plan:
  - get: issue
    trigger: true
    version: every
  - task: record
    inputs: [issue]
    run: echo "$(cat issue/issue.number):$(grep -c '^## Comment by' issue/issue.md)" >> PROCESSED
  - put: state
    inputs: [issue]
    params: {add: [factory:building]}
`

// TestWatchGitHubIssuesTriggerOnTheFilterLabel: a labeled open issue builds with its discussion in issue.md, and so does one labeled later; a pull request carrying the label never does; a comment, or the pipeline's own state label, is no new version; taking the label off and putting it back is, which is how a person asks for the work again.
func TestWatchGitHubIssuesTriggerOnTheFilterLabel(t *testing.T) {
	fake := newFakeGitHub(t)
	planIssue(fake)
	fake.addLabeledIssue(5, "bug")
	fake.addIssue(fakeIssue{Number: 6, Title: "Done already", Author: "alice", Labels: []string{"self-build"}, Closed: true})
	pr := reviewablePR()
	pr.Number, pr.Labels = 9, []string{"self-build"}
	fake.addPR(pr)

	fixture := newWatchFixture(t, strings.ReplaceAll(issuesPipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"issue"}
	fixture.watch(t)
	fixture.assertDid(t, "4:2")

	if got := fake.labelsOn(4); strings.Join(got, ",") != "self-build,factory:building" {
		t.Errorf("labels on #4 = %v, want the state label beside the filter's", got)
	}

	fake.relabel(5, "self-build")
	fake.addComment(fakeComment{ID: 803, Number: 4, Kind: "issue", Author: "bob", Body: "Any news?", Updated: time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)})
	fixture.watch(t)
	fixture.assertDid(t, "4:2", "5:0")

	fake.relabel(4, "self-build")
	fixture.watch(t)
	fixture.assertDid(t, "4:2", "5:0", "4:3")

	queries := fake.searchQueries()
	if last := queries[len(queries)-1]; last != `repo:acme/app is:issue is:open label:"Self-Build"` {
		t.Errorf("last search = %q, want open issues alone, by the filter label", last)
	}
}

// TestEndToEndGitHubIssueCommentLandsOnTheIssue is the seam between the finder and a publisher: a github-pr-comment put handed a github-issues get, and nothing else, posts on that issue. The comment it posts is issue.md, so the file is checked byte for byte on the way.
func TestEndToEndGitHubIssueCommentLandsOnTheIssue(t *testing.T) {
	fake := newFakeGitHub(t)
	planIssue(fake)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: issue
  type: github-issues
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    labels: [self-build]
- name: reply
  type: github-pr-comment
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
jobs:
- name: plan
  plan:
  - get: issue
  - task: read
    inputs: [issue]
    outputs: [notes]
    run: |
      cp issue/issue.md notes/body.md
      echo "$(cat issue/issue.number) $(cat issue/issue.title) $(cat issue/issue.url)"
      test -s issue/issue.json && test -s issue/issue.comments.json
    assert:
      stdout: "4 Plan the cache https://github.example/acme/app/issues/4"
  - put: reply
    inputs: [issue, notes]
    params: {body_file: notes/body.md}
`)

	mustRun(t, "run", path, "--job", "plan")

	want := "# Plan the cache (#4)\n\nhttps://github.example/acme/app/issues/4\n\nIt should key on content.\n" +
		"\n## Comment by bob, 2026-10-01T09:30:00Z\n\nUse the digest.\n" +
		"\n## Comment by alice, 2026-10-01T09:35:00Z\n\nAgreed.\n"

	posted := fake.postedComments()
	if len(posted) != 1 || posted[0].Number != 4 || posted[0].Body != want {
		t.Errorf("posted %+v, want issue.md on #4:\n%s", posted, want)
	}
}

// TestEndToEndGitHubIssueOpenedIsFound: an issue a github-issue put opens with the filter label is one github-issues finds.
func TestEndToEndGitHubIssueOpenedIsFound(t *testing.T) {
	fake := newFakeGitHub(t)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: opened
  type: github-issue
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
- name: queue
  type: github-issues
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    labels: [self-build]
jobs:
- name: open
  plan:
  - task: write
    outputs: [child]
    run: |
      echo "Split out the cache" > child/title
      echo "Part of #1" > child/body.md
  - put: opened
    inputs: [child]
    params: {title_file: child/title, body_file: child/body.md, labels: [self-build]}
- name: plan
  plan:
  - get: queue
  - task: read
    inputs: [queue]
    run: head -1 queue/issue.md
    assert:
      stdout: "# Split out the cache (#1)"
`)

	mustRun(t, "run", path, "--job", "open")
	mustRun(t, "run", path, "--job", "plan")
}
