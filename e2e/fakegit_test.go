package e2e

// A real git repository behind the fake GitHub, served over smart HTTP by
// git's own http-backend, for `checkout: git`: the get must clone it the
// way it would clone github.com, token and all, so a fake that answered
// canned packs would prove nothing.

import (
	"encoding/base64"
	"net/http"
	"net/http/cgi" //nolint:gosec // test-only: serves git's http-backend to the clone under test; httpoxy was fixed in Go 1.6.3
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGitRepo is the history a pull request sits on: main has three
// commits, the pull request branched from the second and added two.
type fakeGitRepo struct {
	root string // GIT_PROJECT_ROOT; the bare repository is root/<repo>.git
	work string // the clone the history was written in
	// First, MergeBase and BaseTip are main's commits, oldest first; Head is the pull request's.
	First, MergeBase, BaseTip, Head string
}

func gitHistory(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // args come from the callers below, all literals
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}

	return strings.TrimSpace(string(out))
}

// fakeGitPull is the pull request number the history is published under.
const fakeGitPull = "7"

// newFakeGitRepo builds the history and publishes it as GitHub does: the
// base branch under refs/heads, the pull request's head under
// refs/pull/<number>/head and on no branch, as a fork's would be.
func newFakeGitRepo(t *testing.T) fakeGitRepo {
	t.Helper()

	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	work := t.TempDir()
	commit := func(file, body string) string {
		err := os.WriteFile(filepath.Join(work, file), []byte(body), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		gitHistory(t, work, "add", file)
		gitHistory(t, work, "commit", "-q", "-m", file)

		return gitHistory(t, work, "rev-parse", "HEAD")
	}

	gitHistory(t, work, "init", "-q", "-b", "main")

	repo := fakeGitRepo{root: t.TempDir(), work: work}
	repo.First = commit("README.md", "hello\n")
	repo.MergeBase = commit("base.txt", "base\n")
	gitHistory(t, work, "checkout", "-q", "-b", "feature")
	commit("feature.txt", "one\n")
	repo.Head = commit("feature.txt", "two\n")
	gitHistory(t, work, "checkout", "-q", "main")
	repo.BaseTip = commit("later.txt", "later\n")

	bare := filepath.Join(repo.root, fakeRepo+".git")
	gitHistory(t, work, "init", "-q", "--bare", bare)
	gitHistory(t, work, "push", "-q", bare, "main:refs/heads/main", "feature:refs/pull/"+fakeGitPull+"/head")

	return repo
}

// pushToPull adds a commit on top of the pull request's head, so the head a version names is no longer the tip any ref advertises.
func (r fakeGitRepo) pushToPull(t *testing.T) {
	t.Helper()

	gitHistory(t, r.work, "checkout", "-q", "feature")

	err := os.WriteFile(filepath.Join(r.work, "next.txt"), []byte("next\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	gitHistory(t, r.work, "add", "next.txt")
	gitHistory(t, r.work, "commit", "-q", "-m", "next")
	gitHistory(t, r.work, "push", "-q", filepath.Join(r.root, fakeRepo+".git"), "feature:refs/pull/"+fakeGitPull+"/head")
}

// serveGit answers git's smart HTTP under /<repo>.git/, to the token as
// GitHub takes it over git: Basic, as x-access-token.
func (f *fakeGitHub) serveGit(repo fakeGitRepo) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.git = &repo
}

func (f *fakeGitHub) gitHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeGitHubToken))
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			http.Error(w, "Bad credentials", http.StatusUnauthorized)

			return
		}

		f.mu.Lock()
		repo := f.git
		f.gitRequests++
		refuseSHA := f.refuseSHAWants
		f.mu.Unlock()

		if repo == nil {
			http.NotFound(w, r)

			return
		}

		env := []string{"GIT_PROJECT_ROOT=" + repo.root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}
		if refuseSHA {
			// Protocol v0 with no allow*SHA1InWant: a want naming a commit no ref advertises is refused, as a server that only serves refs does.
			r.Header.Del("Git-Protocol")
		}

		handler := &cgi.Handler{Path: gitPath(), Args: []string{"http-backend"}, Env: env, InheritEnv: []string{"PATH"}}
		handler.ServeHTTP(w, r)
	})
}

func gitPath() string {
	path, err := exec.LookPath("git")
	if err != nil {
		return "git"
	}

	return path
}

// cloneURL is what the fake's pulls answer as base.repo.clone_url.
func (f *fakeGitHub) cloneURL() string { return f.server.URL + "/" + f.repo + ".git" }
