package toolreg

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/ghfake"
)

func TestGHPrDiffPassthrough(t *testing.T) {
	// A supplied diff must be echoed WITHOUT invoking gh — the offline/fixture
	// seam. No fake gh is installed, so any exec would fail.
	r := New()
	out, err := r.Call(context.Background(), "gh.pr_diff", map[string]any{
		"diff": "--- a\n+++ b\n", "title": "T", "description": "D",
	})
	if err != nil {
		t.Fatalf("passthrough should not error: %v", err)
	}
	if out["diff"] != "--- a\n+++ b\n" || out["title"] != "T" || out["description"] != "D" {
		t.Fatalf("passthrough echoed wrong values: %#v", out)
	}
}

func TestGHPrDiffFetch(t *testing.T) {
	gh := ghfake.Install(t)
	gh.Respond("pr diff", "DIFFBODY")
	gh.Respond("pr view", `{"title":"Add X","body":"why"}`)

	r := New()
	out, err := r.Call(context.Background(), "gh.pr_diff", map[string]any{"pr": "123", "repo": "o/r"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if out["diff"] != "DIFFBODY" || out["title"] != "Add X" || out["description"] != "why" {
		t.Fatalf("fetch parsed wrong: %#v", out)
	}
	gh.WantCall("diff", "123", "--repo", "o/r")
	gh.WantCall("view", "title,body")
}

func TestGHComment(t *testing.T) {
	gh := ghfake.Install(t)
	gh.Respond("pr comment", "https://github.com/o/r/pull/1#issuecomment-9\n")

	r := New()
	out, err := r.Call(context.Background(), "gh.comment", map[string]any{
		"pr": "1", "repo": "o/r", "body": "nice work",
	})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if out["posted"] != true || out["url"] != "https://github.com/o/r/pull/1#issuecomment-9" {
		t.Fatalf("comment output: %#v", out)
	}
	gh.WantCall("comment", "1", "--body", "nice work", "--repo", "o/r")
}

func TestGHPrMeta(t *testing.T) {
	gh := ghfake.Install(t)
	gh.Respond("pr view", `{
		"number":7,"title":"T","body":"B","isDraft":true,
		"author":{"login":"dependabot","is_bot":true},
		"headRefName":"feature","headRefOid":"abc123","baseRefName":"main",
		"labels":[{"name":"deps"},{"name":"go"}],
		"additions":10,"deletions":2,"changedFiles":3}`)

	r := New()
	out, err := r.Call(context.Background(), "gh.pr_meta", map[string]any{"pr": "7"})
	if err != nil {
		t.Fatalf("pr_meta: %v", err)
	}
	if out["draft"] != true || out["isBot"] != true || out["headSha"] != "abc123" || out["author"] != "dependabot" {
		t.Fatalf("pr_meta signals wrong: %#v", out)
	}
	labels, _ := out["labels"].([]any)
	if len(labels) != 2 || labels[0] != "deps" {
		t.Fatalf("pr_meta labels: %#v", out["labels"])
	}
	gh.WantCall("view", "7")
}

func TestGHReviewComment(t *testing.T) {
	gh := ghfake.Install(t)
	gh.Respond("api", `{"html_url":"https://github.com/o/r/pull/1#discussion_r5"}`)

	r := New()
	out, err := r.Call(context.Background(), "gh.review_comment", map[string]any{
		"repo": "o/r", "pr": "1", "commit_id": "abc123", "path": "main.go", "line": 42, "body": "bug here",
	})
	if err != nil {
		t.Fatalf("review_comment: %v", err)
	}
	if out["url"] != "https://github.com/o/r/pull/1#discussion_r5" {
		t.Fatalf("review_comment url: %#v", out)
	}
	gh.WantCall("api", "repos/o/r/pulls/1/comments",
		"body=bug here", "commit_id=abc123", "path=main.go", "line=42", "side=RIGHT")
}

func TestGHLabel(t *testing.T) {
	gh := ghfake.Install(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.label", map[string]any{
		"pr": "1", "repo": "o/r", "add": []any{"reviewed"}, "remove": []any{"needs-review"},
	})
	if err != nil {
		t.Fatalf("label: %v", err)
	}
	gh.WantCall("edit", "1", "--add-label", "reviewed", "--remove-label", "needs-review")
}

func TestGHLabelNeedsOne(t *testing.T) {
	ghfake.Install(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.label", map[string]any{"pr": "1"})
	if err == nil {
		t.Fatal("gh.label with no add/remove should error")
	}
}

func TestGHStatus(t *testing.T) {
	gh := ghfake.Install(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.status", map[string]any{
		"repo": "o/r", "sha": "abc123", "state": "failure", "context": "steps/review", "description": "2 blocking",
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	gh.WantCall("api", "repos/o/r/statuses/abc123",
		"state=failure", "context=steps/review", "description=2 blocking")
}

func TestGHStatusBadState(t *testing.T) {
	ghfake.Install(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.status", map[string]any{
		"repo": "o/r", "sha": "abc", "state": "green", "context": "c",
	})
	if err == nil {
		t.Fatal("gh.status with invalid state should error")
	}
}

func TestGHNonZeroExitIsError(t *testing.T) {
	gh := ghfake.Install(t)
	gh.Fail(1)

	r := New()
	_, err := r.Call(context.Background(), "gh.comment", map[string]any{"pr": "1", "body": "x"})
	if err == nil {
		t.Fatal("a non-zero gh exit must surface as a Go error")
	}
}
