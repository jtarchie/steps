package e2e

// The built-in github-* resource types, end to end through the CLI and a
// running daemon, against the in-process GitHub in fakegithub_test.go.
//
// The feature is that a pull request's tree is fetched by steps itself, with
// no gh and no git anywhere, and still reaches every place a step can run:
// this machine, a worker, a container. So the first tests here cross those
// seams, and the triggers after them are what the types exist for.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

const fakeRepo = "acme/app"

// reviewablePR is one pull request whose tree carries every entry kind the
// artifact digest distinguishes: a plain file, an executable and a symlink.
func reviewablePR() fakePR {
	return fakePR{
		Number: 7, SHA: "abc1234def5678abc1234def5678abc1234def56", Title: "Add a greeting",
		Author: "alice", Base: "main", MergeBase: "base000",
		Diff: "diff --git a/README.md b/README.md\n+hello\n",
		Files: []fakeFile{
			{Path: "README.md", Body: "hello\n"},
			{Path: "bin/run.sh", Body: "#!/bin/sh\necho ran\n", Exec: true},
			{Path: "docs/link", Link: "../README.md"},
		},
	}
}

func githubPipeline(fake *fakeGitHub, readTask string) string {
	return `
resources:
- name: pr
  type: github-prs
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
    author: alice
- name: comment
  type: github-pr-comment
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: review
  plan:
  - get: pr
  - task: read
    inputs: [pr]
    outputs: [review]` + readTask + `
    run: |
      set -e
      test -x pr/bin/run.sh
      test "$(readlink pr/docs/link)" = ../README.md
      grep -q hello pr/docs/link
      printf 'reviewed #%s at %s on %s from %s\n' "$(cat pr/pr.number)" "$(cat pr/pr.sha)" "$(cat pr/pr.mergebase)" "${STEPS_WORKER:-here}" > review/body.md
      grep -c '^diff --git' pr/pr.diff >> review/body.md
  - put: comment
    inputs: [pr, review]
    params:
      body_file: review/body.md
`
}

func assertReviewed(t *testing.T, fake *fakeGitHub, where string) {
	t.Helper()

	posted := fake.postedComments()
	want := "reviewed #7 at " + reviewablePR().SHA + " on base000 from " + where + "\n1\n"

	if len(posted) != 1 || posted[0].Number != 7 || posted[0].Body != want {
		t.Errorf("posted = %+v, want one comment on #7 reading %q", posted, want)
	}
}

// TestEndToEndGitHubPRFetchesTheTreeAndComments is the feature on this
// machine: the check finds the one PR its filter names, the get lays down
// the tree at the version's commit beside the PR's metadata, and the put
// comments on the PR that get fetched.
func TestEndToEndGitHubPRFetchesTheTreeAndComments(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())
	fake.addPR(fakePR{Number: 8, SHA: "ffff000011112222333344445555666677778888", Author: "bob", Base: "main"})

	mustRun(t, "run", writePipeline(t, t.TempDir(), githubPipeline(fake, "")), "--job", "review")

	assertReviewed(t, fake, "here")

	queries := fake.searchQueries()
	if len(queries) == 0 || !strings.Contains(queries[0], "repo:"+fakeRepo+" is:pr is:open") || !strings.Contains(queries[0], "author:alice") {
		t.Errorf("search queries = %q, want the repository, open pull requests, and the author filter", queries)
	}
}

// TestEndToEndGitHubPRTreeReachesAWorker: the tree was fetched here, in this
// process, and a placed task reads it on the worker — the artifact travels
// the way any fetched tree does.
func TestEndToEndGitHubPRTreeReachesAWorker(t *testing.T) {
	requireDockerE2E(t)

	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	path := writePipeline(t, t.TempDir(), githubPipeline(fake, "\n    tags: [vpc]\n    image: "+dockerE2EImage))
	mustRun(t, "run", path, "--job", "review", "--worker", "vpc=local:")

	assertReviewed(t, fake, "vpc")
}

// TestEndToEndGitHubPRTreeReachesAContainer: the same tree, bind-mounted
// into a task's image, with its exec bit and symlink intact.
func TestEndToEndGitHubPRTreeReachesAContainer(t *testing.T) {
	requireDockerE2E(t)

	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	path := writePipeline(t, t.TempDir(), githubPipeline(fake, "\n    image: "+dockerE2EImage))
	mustRun(t, "run", path, "--job", "review")

	assertReviewed(t, fake, "here")
}

// TestEndToEndGitHubPRCheckoutFalseFetchesNoTree: metadata only, for a
// pipeline that takes its source from a git resource instead.
func TestEndToEndGitHubPRCheckoutFalseFetchesNoTree(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	dir := t.TempDir()
	marker := filepath.Join(dir, "tree.txt")
	path := writePipeline(t, dir, `
resources:
- name: pr
  type: github-prs
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    checkout: false
jobs:
- name: look
  plan:
  - get: pr
  - task: list
    inputs: [pr]
    run: ls pr | tr '\n' ' ' > `+marker+`
`)

	mustRun(t, "run", path, "--job", "look")

	if got := strings.TrimSpace(readFileString(t, marker)); got != "pr.diff pr.json pr.mergebase pr.number pr.sha pr.url" {
		t.Errorf("pr/ held %q, want the metadata files and no tree", got)
	}
}

// TestEndToEndGitHubPendingReviewReplacesOnlyYourOwn: a pending review is a
// draft only its author sees, GitHub allows one per user per PR, so a new
// push replaces the pipeline's own draft — and never touches anybody
// else's.
func TestEndToEndGitHubPendingReviewReplacesOnlyYourOwn(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())
	fake.addReview(7, fakeReview{ID: 1, User: fake.login, State: "PENDING", Body: "stale draft"})
	fake.addReview(7, fakeReview{ID: 2, User: "carol", State: "PENDING", Body: "carol's draft"})
	fake.addReview(7, fakeReview{ID: 3, User: fake.login, State: "COMMENTED", Body: "an old submitted review"})

	path := writePipeline(t, t.TempDir(), `
resources:
- name: pr
  type: github-prs
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    checkout: false
- name: review
  type: github-pr-review
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
jobs:
- name: draft
  plan:
  - get: pr
  - task: write
    outputs: [draft]
    run: echo "fresh draft" > draft/body.md
  - put: review
    inputs: [pr, draft]
    params:
      body_file: draft/body.md
      event: pending
`)

	mustRun(t, "run", path, "--job", "draft")

	reviews := fake.reviewsOn(7)

	ids := make([]int64, 0, len(reviews))
	for _, review := range reviews {
		ids = append(ids, review.ID)
	}

	if slices.Contains(ids, 1) || !slices.Contains(ids, 2) || !slices.Contains(ids, 3) {
		t.Errorf("reviews after the put = %v, want the stale draft (1) gone and carol's draft (2) and the submitted review (3) kept", ids)
	}

	fresh := reviews[len(reviews)-1]
	if fresh.State != "PENDING" || fresh.Body != "fresh draft\n" || fresh.CommitID != reviewablePR().SHA {
		t.Errorf("new review = %+v, want a pending draft on the fetched commit", fresh)
	}
}

// reviewWithComments is a pending review whose task writes its comments
// file with the given JSON.
func reviewWithComments(fake *fakeGitHub, comments string) string {
	return `
resources:
- name: pr
  type: github-prs
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
    checkout: false
- name: review
  type: github-pr-review
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: draft
  plan:
  - get: pr
  - task: write
    outputs: [draft]
    run: |
      echo "Not yet. Details inline." > draft/body.md
      cat > draft/comments.json <<'JSON'
      ` + comments + `
      JSON
  - put: review
    inputs: [pr, draft]
    params:
      body_file: draft/body.md
      comments_file: draft/comments.json
      event: pending
`
}

// TestEndToEndGitHubReviewCarriesInlineComments: comments_file puts each
// comment on its line, in the same review as the body, so the author reads
// the problem beside the code rather than looking a path:line up.
func TestEndToEndGitHubReviewCarriesInlineComments(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	path := writePipeline(t, t.TempDir(), reviewWithComments(fake,
		`[{"path": "app/a.rb", "line": 12, "body": "a suspended one still shows"}, {"path": "app/b.rb", "start_line": 3, "line": 5, "side": "LEFT", "body": "this was the guard"}]`))

	mustRun(t, "run", path, "--job", "draft")

	reviews := fake.reviewsOn(7)
	if len(reviews) != 1 {
		t.Fatalf("reviews = %d, want 1", len(reviews))
	}

	want := []fakeReviewComment{
		{Path: "app/a.rb", Line: 12, Body: "a suspended one still shows"},
		{Path: "app/b.rb", StartLine: 3, Line: 5, Side: "LEFT", StartSide: "LEFT", Body: "this was the guard"},
	}
	if !slices.Equal(reviews[0].Comments, want) {
		t.Errorf("comments = %+v, want %+v", reviews[0].Comments, want)
	}
}

// TestEndToEndGitHubReviewRefusesABadCommentBeforeDeletingTheDraft: a
// pending put replaces the last draft by deleting it first, so a comments
// file that cannot be posted must fail before that, or the next review is
// lost and the last one with it.
func TestEndToEndGitHubReviewRefusesABadCommentBeforeDeletingTheDraft(t *testing.T) {
	for name, comments := range map[string]string{
		"misnamed field": `[{"file": "app/a.rb", "line": 12, "body": "x"}]`,
		"no line":        `[{"path": "app/a.rb", "body": "x"}]`,
		"empty body":     `[{"path": "app/a.rb", "line": 12, "body": " "}]`,
		"range reversed": `[{"path": "app/a.rb", "start_line": 9, "line": 4, "body": "x"}]`,
		"not an array":   `{"path": "app/a.rb", "line": 12, "body": "x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			pr := reviewablePR()
			pr.Number = 8

			fake := newFakeGitHub(t)
			fake.addPR(pr)
			fake.addReview(8, fakeReview{ID: 1, User: fake.login, State: "PENDING", Body: "last draft"})

			path := writePipeline(t, t.TempDir(), reviewWithComments(fake, comments))

			err := cli.Run([]string{"run", path, "--job", "draft"})
			if err == nil {
				t.Fatal("run succeeded, want the put refused")
			}

			reviews := fake.reviewsOn(8)
			if len(reviews) != 1 || reviews[0].Body != "last draft" {
				t.Errorf("reviews = %+v, want the last draft untouched", reviews)
			}
		})
	}
}

// commentPipeline triggers on comments by alice that start with /steps, on
// every kind GitHub has.
const commentPipeline = `
resources:
- name: command
  type: github-comments
  source:
    repo: ` + fakeRepo + `
    endpoint: ENDPOINT
    author: alice
    body: '^/steps\s'
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

// TestWatchGitHubCommentsTriggerOnAuthorAndContent is the comment trigger
// under a running daemon: a comment builds when its author AND its body
// match, an edit that still matches is new work, and nothing else is.
func TestWatchGitHubCommentsTriggerOnAuthorAndContent(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	// A poll that finds nothing records nothing, so a cold start needs one
	// version to be seen at all; it is built once and discarded, and every
	// poll after it asks since that version.
	fake.addComment(fakeComment{ID: 99, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps baseline", Updated: at.Add(-time.Minute)})
	fake.addComment(fakeComment{ID: 100, Number: 7, Kind: "conversation", Author: "alice", Body: "lgtm", Updated: at})
	fake.addComment(fakeComment{ID: 101, Number: 7, Kind: "conversation", Author: "bob", Body: "/steps review", Updated: at.Add(time.Second)})

	fixture := newWatchFixture(t, strings.ReplaceAll(commentPipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"command"}
	fixture.coldStart(t)

	fake.addComment(fakeComment{ID: 102, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps review", Updated: at.Add(2 * time.Second)})
	fixture.watch(t)
	fixture.assertDid(t, "102:7:conversation")

	fake.addComment(fakeComment{ID: 103, Number: 7, Kind: "review", Author: "alice", Body: "/steps fix this line", Updated: at.Add(3 * time.Second)})
	fake.addComment(fakeComment{ID: 104, Number: 12, Kind: "issue", Author: "Alice", Body: "/steps build", Updated: at.Add(4 * time.Second)})
	fixture.watch(t)
	fixture.assertDid(t, "102:7:conversation", "103:7:review", "104:12:issue")

	fake.editComment(102, "/steps review again", at.Add(5*time.Second))
	fake.addComment(fakeComment{ID: 105, Number: 7, Kind: "conversation", Author: "bob", Body: "/steps review", Updated: at.Add(6 * time.Second)})
	fake.editComment(100, "still just lgtm", at.Add(7*time.Second))
	fixture.watch(t)
	fixture.assertDid(t, "102:7:conversation", "103:7:review", "104:12:issue", "102:7:conversation")
}

// TestWatchGitHubPRsTriggerWhenAssigned: an assignment brings a pull request
// into the filtered set, which is what makes it a new version.
func TestWatchGitHubPRsTriggerWhenAssigned(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())
	// The baseline a cold start needs: a poll that finds nothing records nothing.
	fake.addPR(fakePR{Number: 3, SHA: "3333333333333333333333333333333333333333", Author: "dave", Base: "main", Assignees: []string{fake.login}})

	fixture := newWatchFixture(t, `
resources:
- name: assigned
  type: github-prs
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
    assignee: "@me"
    checkout: false
jobs:
- name: pick-up
  plan:
  - get: assigned
    trigger: true
    version: every
  - task: record
    inputs: [assigned]
    run: cat assigned/pr.number >> PROCESSED && echo >> PROCESSED
`)
	fixture.resources = []string{"assigned"}
	fixture.coldStart(t)
	fixture.assertDid(t)

	fake.setPR(7, func(pr *fakePR) { pr.Assignees = []string{fake.login} })
	fixture.watch(t)
	fixture.assertDid(t, "7")

	queries := fake.searchQueries()
	if !strings.Contains(queries[len(queries)-1], "assignee:@me") {
		t.Errorf("last search = %q, want the assignee filter", queries[len(queries)-1])
	}
}

// TestValidateGitHubRefusesAnUnsetToken: the token is the one thing a GitHub
// resource cannot run without, so validate names it before a run is paid for.
func TestValidateGitHubRefusesAnUnsetToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")

	fake := newFakeGitHub(t)
	path := writePipeline(t, t.TempDir(), githubPipeline(fake, ""))

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "$GH_TOKEN is not set") {
		t.Fatalf("validate = %v, want it to name the unset $GH_TOKEN", err)
	}
}

// reactionPipeline marks the one comment alice wrote that starts with
// /steps: rocket on, the token user's own eyes off.
func reactionPipeline(fake *fakeGitHub) string {
	return `
resources:
- name: command
  type: github-comments
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
    author: alice
    body: '^/steps\s'
    kinds: [conversation, review, issue]
    checkout: false
- name: mark
  type: github-reaction
  source:
    repo: ` + fakeRepo + `
    endpoint: ` + fake.URL() + `
jobs:
- name: act
  plan:
  - get: command
  - put: mark
    inputs: [command]
    params: {add: rocket, remove: eyes}
`
}

// TestEndToEndGitHubReactionSwapsOnlyItsOwn: a put marks the comment its
// github-comments get fetched, on whichever route that comment's kind lives
// under, takes off only the token user's own reaction, and a second run —
// a replay — finds the world already as asked and stays green.
func TestEndToEndGitHubReactionSwapsOnlyItsOwn(t *testing.T) {
	for _, kind := range []string{"conversation", "review", "issue"} {
		t.Run(kind, func(t *testing.T) {
			fake := newFakeGitHub(t)
			fake.addPR(reviewablePR())
			fake.addComment(fakeComment{ID: 501, Number: 7, Kind: kind, Author: "alice", Body: "/steps go", Updated: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)})
			fake.react(501, "carol", "eyes")
			fake.react(501, fake.login, "eyes")

			path := writePipeline(t, t.TempDir(), reactionPipeline(fake))

			for range 2 {
				mustRun(t, "run", path, "--job", "act")

				got := map[string]bool{}
				for _, reaction := range fake.reactionsOn(501) {
					got[reaction.User+":"+reaction.Content] = true
				}

				want := map[string]bool{"carol:eyes": true, fake.login + ":rocket": true}
				if len(got) != len(want) || !got["carol:eyes"] || !got[fake.login+":rocket"] {
					t.Errorf("reactions on 501 = %v, want %v: rocket added once, the token user's eyes gone, carol's kept", got, want)
				}
			}
		})
	}
}

// TestEndToEndGitHubReactionFailsOnARefusal: anything but already-there or
// already-gone is a failure naming GitHub's answer and the route — here a
// token that may read the repository but not react in it.
func TestEndToEndGitHubReactionFailsOnARefusal(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())
	fake.addComment(fakeComment{ID: 501, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps go", Updated: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)})
	fake.mu.Lock()
	fake.refuseReactions = true
	fake.mu.Unlock()

	err := cli.Run([]string{"run", writePipeline(t, t.TempDir(), reactionPipeline(fake)), "--job", "act"})
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "/issues/comments/501/reactions") {
		t.Fatalf("run = %v, want the put failed naming the 403 and the route", err)
	}
}

// TestWatchGitHubCommentsSkipReactedSurvivesAWipe is what skip_reacted is
// for: the reaction a build leaves on its comment is the record that the
// comment was done, and it lives on GitHub, so deleting the state database
// does not re-run a command already answered. Only the token user's own
// reaction counts — anybody can react with anything.
func TestWatchGitHubCommentsSkipReactedSurvivesAWipe(t *testing.T) {
	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	command := func(id int64, offset time.Duration) fakeComment {
		return fakeComment{ID: id, Number: 7, Kind: "conversation", Author: "alice", Body: "/steps go", Updated: at.Add(offset)}
	}

	fake.addComment(command(500, 0))
	// Bob's comment never passes the author filter, so its reactions are
	// never worth asking about, whatever its summary counts.
	fake.addComment(fakeComment{ID: 499, Number: 7, Kind: "conversation", Author: "bob", Body: "/steps go", Updated: at.Add(-time.Minute)})
	fake.react(499, fake.login, "rocket")

	// A check that reports nothing records nothing, and the run this test is
	// about reports nothing — so the daemon's polls are read off clock, a
	// resource that always has a version, checked in the same poll.
	fixture := newWatchFixture(t, strings.ReplaceAll(`
defaults:
  preflight:
    disabled: true
resource_types:
- name: constant
  config:
    check: echo '[{"v":"1"}]'
    in: "true"
resources:
- name: command
  type: github-comments
  source:
    repo: `+fakeRepo+`
    endpoint: ENDPOINT
    author: alice
    body: '^/steps\s'
    checkout: false
    skip_reacted: rocket
- name: done
  type: github-reaction
  source:
    repo: `+fakeRepo+`
    endpoint: ENDPOINT
- name: clock
  type: constant
  source: {}
jobs:
- name: act
  plan:
  - get: command
    trigger: true
    version: every
  - task: record
    inputs: [command]
    run: cat command/comment.id >> PROCESSED && echo >> PROCESSED
  - put: done
    inputs: [command]
    params: {add: rocket}
- name: tick
  plan:
  - get: clock
    trigger: true
`, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"clock"}
	fixture.coldStart(t)

	fake.addComment(command(501, time.Second))
	fake.addComment(command(502, 2*time.Second))
	fixture.watch(t)
	fixture.assertDid(t, "501", "502")

	for _, id := range []int64{500, 501, 502} {
		if reactions := fake.reactionsOn(id); len(reactions) != 1 || reactions[0].Content != "rocket" {
			t.Errorf("reactions on %d = %+v, want the build's rocket", id, reactions)
		}
	}

	fixture.served.stop(t)
	fixture.served = nil

	for _, suffix := range []string{"", "-wal", "-shm"} {
		err := os.Remove(fixture.db + suffix)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	// A first check again, with no cursor: every command it would report is
	// marked done, so nothing is built — without the marker, 502 would be.
	fixture.watch(t)
	fixture.assertDid(t, "501", "502")

	fake.addComment(command(503, 3*time.Second))
	fake.react(503, "carol", "rocket")
	fixture.watch(t)
	fixture.assertDid(t, "501", "502", "503")

	if slices.Contains(fake.listedReactions(), 499) {
		t.Errorf("reactions listed for %v, want bob's comment, which the author filter drops, never asked about", fake.listedReactions())
	}
}
