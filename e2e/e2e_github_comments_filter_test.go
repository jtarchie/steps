package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
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
	t.Parallel()

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

	// Inside the look-back, a label going on lets the next check list the refused command again and report it.
	fake.relabel(13, "approved")
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review", "204:13:issue")

	fake.addComment(command(206, 7, "conversation", 10*time.Minute))
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review", "204:13:issue", "206:7:conversation")

	fake.setPR(8, func(pr *fakePR) { pr.Labels = append(pr.Labels, "approved") })
	fake.addComment(command(207, 8, "conversation", 11*time.Minute))
	fixture.watch(t)
	fixture.assertDid(t, "201:7:conversation", "203:12:issue", "205:7:review", "204:13:issue", "206:7:conversation", "207:8:conversation")
}

// TestWatchGitHubCommentsAuthorListTakesAnyOfThem: author: as a list builds a command from any login on it, in any case, and from nobody else.
func TestWatchGitHubCommentsAuthorListTakesAnyOfThem(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	command := func(id int64, author string, offset time.Duration) fakeComment {
		return fakeComment{ID: id, Number: 7, Kind: "conversation", Author: author, Body: "/steps go", Updated: at.Add(offset)}
	}

	fake.addComment(command(300, "alice", 0))

	pipeline := strings.Replace(commentPipeline, "author: alice", "author: [alice, Carol]", 1)
	if pipeline == commentPipeline {
		t.Fatal("commentPipeline no longer names author: alice")
	}

	fixture := newWatchFixture(t, strings.ReplaceAll(pipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"command"}
	fixture.coldStart(t)

	fake.addComment(command(301, "carol", time.Second))
	fake.addComment(command(302, "dave", 2*time.Second))
	fake.addComment(command(303, "alice", 3*time.Second))
	fixture.watch(t)
	fixture.assertDid(t, "301:7:conversation", "303:7:conversation")
}

// TestWatchGitHubCommentsBuildsTheHeadTheCommandWasSeenAt: a command is bound to the pull request's head when the check first reports it. A push made after that, while the build waits its turn, is not laid down under the command, and the look-back listing the comments again at the new head starts no second build of them.
func TestWatchGitHubCommentsBuildsTheHeadTheCommandWasSeenAt(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)

	seen := reviewablePR()
	fake.addPR(seen)

	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	command := func(id int64, offset time.Duration) fakeComment {
		return fakeComment{ID: id, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps go", Updated: at.Add(offset)}
	}

	fake.addComment(command(501, 0))
	fake.addComment(command(502, time.Second))

	dir := t.TempDir()
	hold, started := filepath.Join(dir, "hold"), filepath.Join(dir, "started")

	fixture := newWatchFixture(t, strings.NewReplacer("ENDPOINT", fake.URL(), "HOLD", hold, "STARTED", started).Replace(`
defaults:
  preflight:
    disabled: true
resources:
- name: command
  type: github-comments
  source:
    repo: `+fakeRepo+`
    endpoint: ENDPOINT
    author: alice
    body: '^/steps\s'
jobs:
- name: act
  plan:
  - task: wait
    inputs: []
    run: |
      touch STARTED
      while test -e HOLD; do sleep 0.05; done
  - get: command
    trigger: true
    version: every
  - task: record
    inputs: [command]
    run: echo "$(cat command/comment.id):$(cat command/pr.sha):$(cat command/README.md)" >> PROCESSED
`))
	fixture.resources = []string{"command"}
	fixture.write(t, hold, "")

	// The first check reports both commands at the head it finds and builds the newest, which waits.
	fixture.served = startWeb(t, "--db", fixture.db, "--interval", pollInterval)
	t.Cleanup(func() { fixture.served.stopIfRunning(t) })
	fixture.served.set(t, fixture.name, fixture.pipeline)
	waitForFile(t, started)

	const pushed = "bbbb234def5678abc1234def5678abc1234def56"

	fake.setPR(7, func(pr *fakePR) {
		pr.SHA, pr.Files = pushed, []fakeFile{{Path: "README.md", Body: "pushed-after-the-command\n"}}
	})
	// The commit the command was seen at is still GitHub's to serve.
	fake.addPR(seen)

	err := os.Remove(hold)
	if err != nil {
		t.Fatal(err)
	}

	fixture.watch(t)
	fixture.assertDid(t, "502:"+seen.SHA+":hello")

	fake.addComment(command(503, 2*time.Second))
	fixture.watch(t)
	fixture.assertDid(t, "502:"+seen.SHA+":hello", "503:"+pushed+":pushed-after-the-command")
}

// TestEndToEndGitHubCommentRunAgainGetsTheSameTree: a second run of the same command, after the pull request moved, checks again and still lays down the commit the command was first seen at.
func TestEndToEndGitHubCommentRunAgainGetsTheSameTree(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)

	seen := reviewablePR()
	fake.addPR(seen)
	// 500 is not the cursor, so only version history says where it was first seen.
	fake.addComment(fakeComment{ID: 500, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps go", Updated: time.Date(2026, 9, 27, 11, 59, 0, 0, time.UTC)})
	fake.addComment(fakeComment{ID: 501, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps go", Updated: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)})

	dir := t.TempDir()
	built := filepath.Join(dir, "built")

	path := writePipeline(t, dir, `
resources:
- name: command
  type: github-comments
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    author: alice
jobs:
- name: act
  plan:
  - get: command
  - task: record
    inputs: [command]
    run: echo "$(cat command/pr.sha):$(cat command/README.md)" >> `+built+`
`)

	mustRun(t, "run", path, "--job", "act")

	fake.setPR(7, func(pr *fakePR) {
		pr.SHA, pr.Files = "bbbb234def5678abc1234def5678abc1234def56", []fakeFile{{Path: "README.md", Body: "pushed-after-the-command\n"}}
	})
	fake.addPR(seen)

	mustRun(t, "run", path, "--job", "act", "--force")

	data, err := os.ReadFile(built) //nolint:gosec // a t.TempDir() path
	if err != nil {
		t.Fatal(err)
	}

	if want := strings.Repeat(seen.SHA+":hello\n", 2); string(data) != want {
		t.Errorf("built %q, want %q: both runs at the commit the command was first seen at", data, want)
	}

	st, err := sqlite.OpenStore(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	filed, err := st.ResourceVersionsJSON(t.Context(), "command")
	if err != nil || len(filed) != 2 {
		t.Errorf("filed %v, %v; want the two commands once each, neither again at the pushed head", filed, err)
	}
}
