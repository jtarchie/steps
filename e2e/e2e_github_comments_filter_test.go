package e2e

import (
	"strings"
	"testing"
	"time"
)

// factoryPipeline builds alice's /factory fix commands, but only where the issue or pull request carries both filter labels; Factory is spelt in another case on #7 because GitHub compares label names without regard to case.
const factoryPipeline = `
resources:
- name: command
  type: github-comments
  source:
    repo: ` + fakeRepo + `
    endpoint: ENDPOINT
    author: alice
    body: '^/factory fix'
    labels: [factory, approved]
    kinds: [conversation, review, issue]
    checkout: false
jobs:
- name: act
  plan:
  - get: command
    trigger: true
    version: every
  - task: record
    inputs: [command]
    run: |
      number=$(cat command/pr.number 2>/dev/null || cat command/issue.number)
      printf '%s:%s:%s\n' "$(cat command/comment.id)" "$number" "$(cat command/comment.kind)" >> PROCESSED
`

// TestWatchGitHubCommentsLabelsNarrowToLabeledParents: a command builds only on an issue or pull request carrying every filter label, an issue's own labels counting for its comments; one refused for its parent's labels is not built when the label goes on after the cursor has passed it, because a command is an instruction given at a moment.
func TestWatchGitHubCommentsLabelsNarrowToLabeledParents(t *testing.T) {
	fake := newFakeGitHub(t)

	labeled := reviewablePR()
	labeled.Labels = []string{"Factory", "approved"}
	fake.addPR(labeled)

	halfLabeled := reviewablePR()
	halfLabeled.Number, halfLabeled.SHA, halfLabeled.Labels = 8, "8888888888888888888888888888888888888888", []string{"factory"}
	fake.addPR(halfLabeled)

	fake.addLabeledIssue(12, "factory", "approved")
	fake.addLabeledIssue(13, "factory")

	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	command := func(id int64, number int, kind string, offset time.Duration) fakeComment {
		return fakeComment{ID: id, Number: number, Kind: kind, Author: "alice", Body: "/factory fix", Updated: at.Add(offset)}
	}

	fake.addComment(command(200, 7, "conversation", -time.Minute))

	fixture := newWatchFixture(t, strings.ReplaceAll(factoryPipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"command"}
	fixture.coldStart(t)

	fake.addComment(command(201, 7, "conversation", time.Second))
	fake.addComment(command(202, 8, "conversation", 2*time.Second))
	fake.addComment(command(203, 12, "issue", 3*time.Second))
	fake.addComment(command(204, 13, "issue", 4*time.Second))
	fake.addComment(command(205, 7, "review", 5*time.Second))
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review")

	fake.addComment(command(206, 7, "conversation", 10*time.Minute))
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review", "206:7:conversation")

	fake.setPR(8, func(pr *fakePR) { pr.Labels = append(pr.Labels, "approved") })
	fake.addComment(command(207, 8, "conversation", 11*time.Minute))
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review", "206:7:conversation", "207:8:conversation")
}
