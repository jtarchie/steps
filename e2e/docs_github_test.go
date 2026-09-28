package e2e

// The fake GitHub each github= doc example runs against, and what each one
// must have posted. The reader sees github.com; the test sees
// fakegithub_test.go, reached through the same source.endpoint: seam a
// GitHub Enterprise Server user writes by hand.

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/config"
	"gopkg.in/yaml.v3"
)

type docGitHubFixture struct {
	prs      []fakePR
	comments []fakeComment
	check    func(t *testing.T, fake *fakeGitHub)
}

// docGitHubRepo is the repository every GitHub doc example names, which is the one the fake serves.
const docGitHubRepo = fakeRepo

func docPR() fakePR {
	return fakePR{
		Number: 42, SHA: "4242424242424242424242424242424242424242", Title: "Add the test runner",
		Author: "alice", Base: "main", MergeBase: "1a2b3c4d5e6f1a2b3c4d5e6f1a2b3c4d5e6f1a2b",
		Reviewers: []string{"octocat"}, Assignees: []string{"octocat"}, Labels: []string{"ready-for-review"},
		Diff: "diff --git a/README.md b/README.md\n+tests\ndiff --git a/bin/test.sh b/bin/test.sh\n+go test ./...\n",
		Files: []fakeFile{
			{Path: "README.md", Body: "tests\n"},
			{Path: "bin/test.sh", Body: "#!/bin/sh\ngo test ./...\n", Exec: true},
		},
	}
}

var docGitHubFixtures = map[string]docGitHubFixture{ //nolint:gochecknoglobals // read-only fixture table, like docMCPFixtures
	"review": {
		prs: []fakePR{docPR(), {Number: 41, SHA: "4141414141414141414141414141414141414141", Author: "bob", Base: "main", Draft: true, Reviewers: []string{"octocat"}, Labels: []string{"ready-for-review"}}},
		check: func(t *testing.T, fake *fakeGitHub) {
			t.Helper()

			posted := fake.postedComments()
			if len(posted) != 1 || posted[0].Number != 42 || !strings.HasPrefix(posted[0].Body, "#42 changes 2 file(s)") {
				t.Errorf("posted %+v, want one comment on #42 summarizing its diff — the draft #41 filtered out", posted)
			}
		},
	},
	"assigned": {prs: []fakePR{docPR()}},
	"comment-command": {
		prs: []fakePR{docPR()},
		comments: []fakeComment{
			{ID: 501, Number: 42, Kind: "conversation", Author: "alice", Body: "/steps review please", Updated: time.Date(2026, 9, 27, 9, 0, 0, 0, time.UTC)},
			{ID: 502, Number: 42, Kind: "conversation", Author: "bob", Body: "/steps review please", Updated: time.Date(2026, 9, 27, 9, 1, 0, 0, time.UTC)},
			{ID: 503, Number: 42, Kind: "review", Author: "alice", Body: "thanks", Updated: time.Date(2026, 9, 27, 9, 2, 0, 0, time.UTC)},
		},
		check: func(t *testing.T, fake *fakeGitHub) {
			t.Helper()

			reviews := fake.reviewsOn(42)
			if len(reviews) != 1 || reviews[0].State != "PENDING" || !strings.HasPrefix(reviews[0].Body, "asked by alice on #42") {
				t.Errorf("reviews on #42 = %+v, want one pending draft answering alice", reviews)
			}
		},
	},
	"post-fixed": {
		prs: []fakePR{docPR()},
		check: func(t *testing.T, fake *fakeGitHub) {
			t.Helper()

			posted := fake.postedComments()
			if len(posted) != 1 || posted[0].Number != 42 || posted[0].Body != "all green\n" {
				t.Errorf("posted %+v, want the report on #42", posted)
			}

			reviews := fake.reviewsOn(42)
			if len(reviews) != 1 || reviews[0].State != "APPROVED" {
				t.Errorf("reviews on #42 = %+v, want one approval", reviews)
			}
		},
	},
}

// injectDocGitHubFixture starts the github= fixture a block's fence names
// and points every github-* resource at it, leaving body untouched when the
// fence names none.
func injectDocGitHubFixture(t *testing.T, block docs.Block, body string) (string, *fakeGitHub) {
	t.Helper()

	id := block.GitHubID()
	if id == "" {
		return body, nil
	}

	fixture, ok := docGitHubFixtures[id]
	if !ok {
		t.Fatalf("fence names github=%s but docs_github_test.go has no such fixture", id)
	}

	fake := newFakeGitHub(t)
	for _, pr := range fixture.prs {
		fake.addPR(pr)
	}

	for _, comment := range fixture.comments {
		fake.addComment(comment)
	}

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	pointed := 0

	eachOf(doc, "resources", func(entry any) {
		resource, _ := entry.(map[string]any)
		kind, _ := resource["type"].(string)

		if !strings.HasPrefix(kind, "github-") {
			return
		}

		source, _ := resource["source"].(map[string]any)
		if source["repo"] != docGitHubRepo {
			t.Errorf("resource %v names repo %v; the fixture serves %s", resource["name"], source["repo"], docGitHubRepo)
		}

		source["endpoint"] = fake.URL()
		pointed++
	})

	if pointed == 0 {
		t.Fatalf("the fence names a github fixture, but the block declares no %s resource", strings.Join(config.GitHubTypes(), "/"))
	}

	rewritten, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal block: %v", err)
	}

	return string(rewritten), fake
}
