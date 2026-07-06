package toolreg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH installs a stub `gh` on PATH that records its argv (one arg per line,
// executions separated by a "--" line) and emits canned stdout routed by
// subcommand: files pr_diff / pr_view / pr_comment / api in the temp dir feed
// `gh pr diff`, `gh pr view`, `gh pr comment`, and `gh api` respectively. An
// `exit` file makes it fail with that code (non-zero exit -> Go error). It
// returns the temp dir (write route files into it) and the argv log path.
func fakeGH(t *testing.T) (dir, argvPath string) {
	t.Helper()
	dir = t.TempDir()
	argvPath = filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\" >> " + q(argvPath) + "; done\n" +
		"printf -- '--\\n' >> " + q(argvPath) + "\n" +
		"case \"$1 $2\" in\n" +
		"  \"pr diff\") cat " + q(filepath.Join(dir, "pr_diff")) + " 2>/dev/null ;;\n" +
		"  \"pr view\") cat " + q(filepath.Join(dir, "pr_view")) + " 2>/dev/null ;;\n" +
		"  \"pr comment\") cat " + q(filepath.Join(dir, "pr_comment")) + " 2>/dev/null ;;\n" +
		"  \"pr edit\"|\"pr review\") : ;;\n" +
		"  *) cat " + q(filepath.Join(dir, "api")) + " 2>/dev/null ;;\n" +
		"esac\n" +
		"if [ -f " + q(filepath.Join(dir, "exit")) + " ]; then echo 'gh: boom' >&2; exit \"$(cat " + q(filepath.Join(dir, "exit")) + ")\"; fi\n" +
		"exit 0\n"
	ghPath := filepath.Join(dir, "gh")
	err := os.WriteFile(ghPath, []byte(script), 0o600)
	if err != nil {
		t.Fatalf("writing fake gh: %v", err)
	}
	err = os.Chmod(ghPath, 0o700)
	if err != nil {
		t.Fatalf("chmod fake gh: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir, argvPath
}

// q shell-quotes a path for embedding in the stub script.
func q(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// argv reads the recorded arguments as a flat list of lines (including the "--"
// separators between executions).
func argv(t *testing.T, argvPath string) []string {
	t.Helper()
	b, err := os.ReadFile(argvPath)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// wantArgs asserts every wanted token appears among the recorded argv lines.
func wantArgs(t *testing.T, argvPath string, want ...string) {
	t.Helper()
	got := argv(t, argvPath)
	set := map[string]bool{}
	for _, g := range got {
		set[g] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("gh argv %v missing %q", got, w)
		}
	}
}

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
	dir, argvPath := fakeGH(t)
	os.WriteFile(filepath.Join(dir, "pr_diff"), []byte("DIFFBODY"), 0o600)
	os.WriteFile(filepath.Join(dir, "pr_view"), []byte(`{"title":"Add X","body":"why"}`), 0o600)

	r := New()
	out, err := r.Call(context.Background(), "gh.pr_diff", map[string]any{"pr": "123", "repo": "o/r"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if out["diff"] != "DIFFBODY" || out["title"] != "Add X" || out["description"] != "why" {
		t.Fatalf("fetch parsed wrong: %#v", out)
	}
	wantArgs(t, argvPath, "diff", "123", "--repo", "o/r", "view", "title,body")
}

func TestGHComment(t *testing.T) {
	dir, argvPath := fakeGH(t)
	os.WriteFile(filepath.Join(dir, "pr_comment"), []byte("https://github.com/o/r/pull/1#issuecomment-9\n"), 0o600)

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
	wantArgs(t, argvPath, "comment", "1", "--body", "nice work", "--repo", "o/r")
}

func TestGHPrMeta(t *testing.T) {
	dir, argvPath := fakeGH(t)
	os.WriteFile(filepath.Join(dir, "pr_view"), []byte(`{
		"number":7,"title":"T","body":"B","isDraft":true,
		"author":{"login":"dependabot","is_bot":true},
		"headRefName":"feature","headRefOid":"abc123","baseRefName":"main",
		"labels":[{"name":"deps"},{"name":"go"}],
		"additions":10,"deletions":2,"changedFiles":3}`), 0o600)

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
	wantArgs(t, argvPath, "view", "7")
}

func TestGHReviewComment(t *testing.T) {
	dir, argvPath := fakeGH(t)
	os.WriteFile(filepath.Join(dir, "api"), []byte(`{"html_url":"https://github.com/o/r/pull/1#discussion_r5"}`), 0o600)

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
	wantArgs(t, argvPath, "api", "repos/o/r/pulls/1/comments",
		"body=bug here", "commit_id=abc123", "path=main.go", "line=42", "side=RIGHT")
}

func TestGHLabel(t *testing.T) {
	_, argvPath := fakeGH(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.label", map[string]any{
		"pr": "1", "repo": "o/r", "add": []any{"reviewed"}, "remove": []any{"needs-review"},
	})
	if err != nil {
		t.Fatalf("label: %v", err)
	}
	wantArgs(t, argvPath, "edit", "1", "--add-label", "reviewed", "--remove-label", "needs-review")
}

func TestGHLabelNeedsOne(t *testing.T) {
	fakeGH(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.label", map[string]any{"pr": "1"})
	if err == nil {
		t.Fatal("gh.label with no add/remove should error")
	}
}

func TestGHStatus(t *testing.T) {
	_, argvPath := fakeGH(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.status", map[string]any{
		"repo": "o/r", "sha": "abc123", "state": "failure", "context": "steps/review", "description": "2 blocking",
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	wantArgs(t, argvPath, "api", "repos/o/r/statuses/abc123",
		"state=failure", "context=steps/review", "description=2 blocking")
}

func TestGHStatusBadState(t *testing.T) {
	fakeGH(t)
	r := New()
	_, err := r.Call(context.Background(), "gh.status", map[string]any{
		"repo": "o/r", "sha": "abc", "state": "green", "context": "c",
	})
	if err == nil {
		t.Fatal("gh.status with invalid state should error")
	}
}

func TestGHNonZeroExitIsError(t *testing.T) {
	dir, _ := fakeGH(t)
	os.WriteFile(filepath.Join(dir, "exit"), []byte("1"), 0o600)

	r := New()
	_, err := r.Call(context.Background(), "gh.comment", map[string]any{"pr": "1", "body": "x"})
	if err == nil {
		t.Fatal("a non-zero gh exit must surface as a Go error")
	}
}
