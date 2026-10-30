package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestGitHubIssuesQueryIsStableAndQuoted(t *testing.T) {
	t.Parallel()

	source := config.GitHubIssuesSource{Repo: "acme/app", Author: "alice", Assignee: "@me", Labels: []string{"self-build", "needs plan"}}

	want := `repo:acme/app is:issue is:open assignee:@me author:alice label:"needs plan" label:"self-build"`

	for range 5 {
		if got := githubIssuesQuery(source); got != want {
			t.Fatalf("query = %q, want %q", got, want)
		}
	}
}

// TestLabeledAtIsTheLatestFilterLabel: only a filter label's events count, matched without regard to case, so a pipeline's own state labels mint no version.
func TestLabeledAtIsTheLatestFilterLabel(t *testing.T) {
	t.Parallel()

	event := func(at, name string) githubLabeledEvent {
		var e githubLabeledEvent
		e.CreatedAt, e.Label.Name = at, name

		return e
	}

	for name, tc := range map[string]struct {
		events []githubLabeledEvent
		filter []string
		want   string
	}{
		"a later state label":      {[]githubLabeledEvent{event("2026-10-01T09:00:00Z", "self-build"), event("2026-10-01T10:00:00Z", "factory:building")}, []string{"self-build"}, "2026-10-01T09:00:00Z"},
		"put back after a removal": {[]githubLabeledEvent{event("2026-10-01T09:00:00Z", "self-build"), event("2026-10-02T09:00:00Z", "self-build")}, []string{"self-build"}, "2026-10-02T09:00:00Z"},
		"another case":             {[]githubLabeledEvent{event("2026-10-01T09:00:00Z", "Self-Build")}, []string{"self-build"}, "2026-10-01T09:00:00Z"},
		"the later of two filters": {[]githubLabeledEvent{event("2026-10-03T09:00:00Z", "b"), event("2026-10-01T09:00:00Z", "a")}, []string{"a", "b"}, "2026-10-03T09:00:00Z"},
		"no event in reach":        {[]githubLabeledEvent{event("2026-10-01T09:00:00Z", "bug")}, []string{"self-build"}, ""},
	} {
		if got := labeledAt(tc.events, tc.filter); got != tc.want {
			t.Errorf("%s: labeledAt = %q, want %q", name, got, tc.want)
		}
	}
}

// TestGitHubIssuesCheckSkipsAPullRequest: a hit that is not an issue comes back from the search as an empty node, and is no version; without labels the version is the number alone.
func TestGitHubIssuesCheckSkipsAPullRequest(t *testing.T) {
	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"search":{"nodes":[{},{"number":3,"timelineItems":{"nodes":[{"createdAt":"2026-10-01T09:00:00Z","label":{"name":"self-build"}}]}}]}}}`))
	})

	source := map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint}

	versions, err := githubIssuesCheck(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 || len(versions[0]) != 1 || versions[0]["number"] != "3" {
		t.Errorf("versions = %v, want only #3, by number alone", versions)
	}

	source["labels"] = []any{"self-build"}

	versions, err = githubIssuesCheck(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 || versions[0]["labeled_at"] != "2026-10-01T09:00:00Z" {
		t.Errorf("versions = %v, want #3 labeled at 09:00", versions)
	}
}

// issueWithComments serves issue #4, bodiless, and 101 comments on it across two pages.
func issueWithComments(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/repos/acme/app/issues/4":
		_, _ = w.Write([]byte(`{"number":4,"title":"Plan it","body":null,"html_url":"https://github.example/acme/app/issues/4"}`))
	case "/repos/acme/app/issues/4/comments":
		count := map[string]int{"1": 100, "2": 1}[r.URL.Query().Get("page")]

		comments := make([]issueComment, count)
		for i := range comments {
			comments[i].Body, comments[i].CreatedAt, comments[i].User.Login = fmt.Sprint("c", i), "2026-10-01T09:00:00Z", "bob"
		}

		err := json.NewEncoder(w).Encode(comments)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// TestGitHubIssuesInReadsEveryComment: comments are paged to the end, and an issue with no body says so in issue.md rather than leaving a blank where a planner looks for the ask.
func TestGitHubIssuesInReadsEveryComment(t *testing.T) {
	connection := githubServer(t, issueWithComments)
	dir := t.TempDir()

	err := githubIssuesIn(context.Background(), map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint}, map[string]any{"number": "4"}, dir)
	if err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	read := func(name string) string {
		body, err := root.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}

		return string(body)
	}

	if md := read("issue.md"); !strings.HasPrefix(md, "# Plan it (#4)\n\nhttps://github.example/acme/app/issues/4\n\n(no description)\n\n## Comment by bob, 2026-10-01T09:00:00Z\n\nc0\n") || strings.Count(md, "## Comment by") != 101 {
		t.Errorf("issue.md = %q, want the placeholder body and all 101 comments", md)
	}

	var comments []any

	err = json.Unmarshal([]byte(read("issue.comments.json")), &comments)
	if err != nil || len(comments) != 101 {
		t.Errorf("issue.comments.json holds %d comments (%v), want 101", len(comments), err)
	}

	for name, want := range map[string]string{"issue.number": "4", "issue.title": "Plan it", "issue.url": "https://github.example/acme/app/issues/4"} {
		if got := read(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}
