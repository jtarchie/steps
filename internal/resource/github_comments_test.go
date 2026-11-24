package resource

import (
	"context"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var pullRoute = regexp.MustCompile(`^/repos/acme/app/pulls/\d+$`)

// withPullHeads serves a pull request's head, which a comments check reads to bind a comment nothing has filed, and hands every other request to next.
func withPullHeads(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pullRoute.MatchString(r.URL.Path) {
			_, _ = w.Write([]byte(`{"head":{"sha":"` + headA + `"}}`))

			return
		}

		next(w, r)
	}
}

// commandedPR is pull request #42 with /steps comments on it (and comment 9 on issue #9), whose head a test moves; a commit in gone answers 404, as one force-pushed away does.
type commandedPR struct {
	mu       sync.Mutex
	head     string
	comments []string
	gone     map[string]bool
	asked    []string
}

func (p *commandedPR) set(edit func(*commandedPR)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	edit(p)
}

// headReads is how many times the pull request itself was read.
func (p *commandedPR) headReads() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	count := 0

	for _, path := range p.asked {
		if pullRoute.MatchString(path) {
			count++
		}
	}

	return count
}

func prComment(id, updated string) string {
	return `{"id":` + id + `,"body":"/steps review","updated_at":"` + updated + `","user":{"login":"alice"},"html_url":"https://x/acme/app/pull/42#c` + id + `","issue_url":"https://x/repos/acme/app/issues/42"}`
}

func (p *commandedPR) source(t *testing.T) map[string]any {
	t.Helper()

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()

		p.asked = append(p.asked, r.URL.Path)

		sha := r.URL.Path[strings.LastIndexAny(r.URL.Path, "/.")+1:]

		switch {
		case r.URL.Path == "/repos/acme/app/issues/comments":
			_, _ = w.Write([]byte("[" + strings.Join(p.comments, ",") + "]"))
		case r.URL.Path == "/repos/acme/app/issues/comments/1":
			_, _ = w.Write([]byte(prComment("1", "2026-11-01T00:00:00Z")))
		case r.URL.Path == "/repos/acme/app/pulls/42":
			_, _ = w.Write([]byte(`{"html_url":"https://x/acme/app/pull/42","title":"t","user":{"login":"mallory"},"head":{"sha":"` + p.head + `"},"base":{"ref":"main","repo":{"clone_url":"https://x/acme/app.git"}}}`))
		case p.gone[sha]:
			w.WriteHeader(http.StatusNotFound)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/tarball/"):
			_, _ = w.Write(tarball(t, file("app-"+sha[:7]+"/MARKER", sha, 0o644)).Bytes())
		case strings.HasPrefix(r.URL.Path, "/repos/acme/app/compare/main..."):
			if strings.Contains(r.Header.Get("Accept"), "diff") {
				_, _ = w.Write([]byte("diff --git a/MARKER b/MARKER\n"))
			} else {
				_, _ = w.Write([]byte(`{"merge_base_commit":{"sha":"base"},"ahead_by":1,"behind_by":0}`))
			}
		default:
			t.Errorf("unserved: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	return map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint, "author": []any{"alice"}, "body": "^/steps", "kinds": []any{"conversation", "issue"}}
}

func laidDown(t *testing.T, dir string) (marker, sha string) {
	t.Helper()

	read := func(name string) string {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a t.TempDir() path
		if err != nil {
			t.Fatal(err)
		}

		return string(data)
	}

	return read("MARKER"), read("pr.sha")
}

// TestGitHubCommentsGetFetchesTheHeadTheCheckSaw: a command is bound to the pull request's head when a check first reported it, so a push made afterwards is not built under it.
func TestGitHubCommentsGetFetchesTheHeadTheCheckSaw(t *testing.T) {
	pr := &commandedPR{head: headA, comments: []string{prComment("1", "2026-11-01T00:00:00Z")}}
	source := pr.source(t)

	versions, err := githubCommentsCheck(context.Background(), source, nil, nil)
	if err != nil || len(versions) != 1 || versions[0]["sha"] != headA {
		t.Fatalf("check: %v, %v; want the one comment at %s", versions, err, headA)
	}

	pr.set(func(p *commandedPR) { p.head = headB })

	dir := t.TempDir()

	err = githubCommentsIn(context.Background(), source, versions[0], dir)
	if err != nil {
		t.Fatal(err)
	}

	if marker, sha := laidDown(t, dir); marker != headA || sha != headA {
		t.Errorf("tree of %s with pr.sha %s, want both %s: the commit pushed after the command was built under it", marker, sha, headA)
	}
}

// firstSeen is two commands on pull request #42 and one on issue #9, checked once at headA: what that check reported, as version history then holds it.
func firstSeen(t *testing.T) (pr *commandedPR, source map[string]any, filed []map[string]any) {
	t.Helper()

	pr = &commandedPR{head: headA, comments: []string{
		prComment("2", "2026-11-01T00:00:02Z"), prComment("1", "2026-11-01T00:00:00Z"),
		`{"id":9,"body":"/steps go","updated_at":"2026-11-01T00:00:01Z","user":{"login":"alice"},"html_url":"https://x/acme/app/issues/9#c9","issue_url":"https://x/repos/acme/app/issues/9"}`,
	}}
	source = pr.source(t)

	versions, err := githubCommentsCheck(context.Background(), source, nil, nil)
	if err != nil || len(versions) != 3 {
		t.Fatalf("first check: %v, %v; want three versions", versions, err)
	}

	for _, version := range versions {
		filed = append(filed, maps.Clone(version))
	}

	return pr, source, filed
}

func history(filed []map[string]any) func(context.Context) ([]map[string]any, error) {
	return func(context.Context) ([]map[string]any, error) { return filed, nil }
}

// TestGitHubCommentsCheckBindsAPullRequestOnce: every new command on one pull request is bound by one read of it, and a command on an issue has no commit to bind.
func TestGitHubCommentsCheckBindsAPullRequestOnce(t *testing.T) {
	pr, _, filed := firstSeen(t)

	if filed[0]["sha"] != headA || filed[2]["sha"] != headA {
		t.Errorf("versions %v, want both pull request comments at %s", filed, headA)
	}

	if _, bound := filed[1]["sha"]; bound || filed[1]["kind"] != "issue" {
		t.Errorf("version %v, want the issue's comment with no commit", filed[1])
	}

	if reads := pr.headReads(); reads != 1 {
		t.Errorf("the pull request was read %d times, want once for both of its comments", reads)
	}
}

// TestGitHubCommentsCheckKeepsTheFirstHead: the look-back lists a comment again on later checks, and a head that moved in between must not make it a second version, which would be a second build of one command.
func TestGitHubCommentsCheckKeepsTheFirstHead(t *testing.T) {
	pr, source, filed := firstSeen(t)

	pr.set(func(p *commandedPR) { p.head = headB })

	again, err := githubCommentsCheck(context.Background(), source, filed[2], history(filed))
	if err != nil || !slices.EqualFunc(again, filed, maps.Equal[map[string]any, map[string]any]) {
		t.Errorf("after the head moved: %v, %v; want %v as first filed", again, err, filed)
	}

	if reads := pr.headReads(); reads != 1 {
		t.Errorf("the pull request was read %d times, want no read for comments already filed", reads)
	}

	pr.set(func(p *commandedPR) {
		p.comments = append([]string{prComment("3", "2026-11-01T00:00:03Z")}, p.comments...)
	})

	later, err := githubCommentsCheck(context.Background(), source, filed[2], history(filed))
	if err != nil || len(later) != 4 || later[3]["id"] != "3" || later[3]["sha"] != headB {
		t.Errorf("a new comment: %v, %v; want it at %s, the head it was first seen at", later, err, headB)
	}
}

// TestGitHubCommentsCheckWithNoHistoryKnowsItsCursor: a caller that keeps no history still hands the cursor, and the cursor's own comment stays as filed.
func TestGitHubCommentsCheckWithNoHistoryKnowsItsCursor(t *testing.T) {
	pr, source, filed := firstSeen(t)

	pr.set(func(p *commandedPR) { p.head = headB })

	versions, err := githubCommentsCheck(context.Background(), source, filed[2], nil)
	if err != nil || len(versions) != 3 || versions[2]["sha"] != headA || versions[0]["sha"] != headB {
		t.Errorf("%v, %v; want the cursor's comment kept at %s and the other bound to the head found now", versions, err, headA)
	}
}

// TestGitHubCommentsVersionsFiledWithoutACommitStay: a version filed before versions held a sha is reported as filed, so an upgrade starts no build, and its get lays down the head as it is now, as it always did.
func TestGitHubCommentsVersionsFiledWithoutACommitStay(t *testing.T) {
	pr := &commandedPR{head: headA, comments: []string{prComment("2", "2026-11-01T00:00:02Z"), prComment("1", "2026-11-01T00:00:00Z")}}
	source := pr.source(t)
	ctx := context.Background()

	filed := []map[string]any{
		{"id": "1", "kind": "conversation", "number": "42", "updated": "2026-11-01T00:00:00Z"},
		{"id": "2", "kind": "conversation", "number": "42", "updated": "2026-11-01T00:00:02Z"},
	}

	versions, err := githubCommentsCheck(ctx, source, filed[1], history(filed))
	if err != nil || len(versions) != 2 || !maps.Equal(versions[0], filed[0]) || !maps.Equal(versions[1], filed[1]) {
		t.Fatalf("check: %v, %v; want %v exactly as filed", versions, err, filed)
	}

	if reads := pr.headReads(); reads != 0 {
		t.Errorf("the pull request was read %d times by the check, want none", reads)
	}

	pr.set(func(p *commandedPR) { p.head = headB })

	dir := t.TempDir()

	err = githubCommentsIn(ctx, source, filed[0], dir)
	if err != nil {
		t.Fatal(err)
	}

	if marker, sha := laidDown(t, dir); marker != headB || sha != headB {
		t.Errorf("tree of %s with pr.sha %s, want the current head %s", marker, sha, headB)
	}
}

// TestGitHubCommentsGetFailsWhenItsCommitIsGone: a commit force-pushed away fails the get, naming it, rather than laying down the head that replaced it.
func TestGitHubCommentsGetFailsWhenItsCommitIsGone(t *testing.T) {
	pr := &commandedPR{head: headB, gone: map[string]bool{headA: true}}
	source := pr.source(t)

	for _, checkout := range []any{true, false} {
		source["checkout"] = checkout
		dir := t.TempDir()

		err := githubCommentsIn(context.Background(), source, map[string]any{"id": "1", "kind": "conversation", "number": "42", "updated": "2026-11-01T00:00:00Z", "sha": headA}, dir)
		if err == nil || !strings.Contains(err.Error(), "pull request #42 at "+headA) || !strings.Contains(err.Error(), "first seen") || !strings.Contains(err.Error(), "404") {
			t.Errorf("checkout %v: err = %v, want the get refused naming the commit and GitHub's answer", checkout, err)
		}

		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Errorf("checkout %v: the get left %d entries behind a commit that is gone", checkout, len(entries))
		}
	}
}

// TestGitHubCommentsCheckRefusesAPullRequestWithNoHead: a version reported without a commit would be fetched at whatever the head became.
func TestGitHubCommentsCheckRefusesAPullRequestWithNoHead(t *testing.T) {
	pr := &commandedPR{comments: []string{prComment("1", "2026-11-01T00:00:00Z")}}

	versions, err := githubCommentsCheck(context.Background(), pr.source(t), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "pull request #42 names no head commit") {
		t.Errorf("%v, %v; want the check refused", versions, err)
	}
}
