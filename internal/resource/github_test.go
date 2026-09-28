package resource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

type tarEntry struct {
	header tar.Header
	body   string
}

func tarball(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}

		err := tw.WriteHeader(&header)
		if err != nil {
			t.Fatal(err)
		}

		_, err = tw.Write([]byte(entry.body))
		if err != nil {
			t.Fatal(err)
		}
	}

	err := tw.Close()
	if err != nil {
		t.Fatal(err)
	}

	err = gz.Close()
	if err != nil {
		t.Fatal(err)
	}

	return &buf
}

func file(name, body string, mode int64) tarEntry {
	return tarEntry{header: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode}, body: body}
}

func link(name, target string) tarEntry {
	return tarEntry{header: tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target}}
}

// TestUnpackTarballKeepsWhatTheDigestSees: the wrapper directory goes, the
// pax header is skipped, the exec bit and the link text survive.
func TestUnpackTarballKeepsWhatTheDigestSees(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := unpackTarball(tarball(t,
		tarEntry{header: tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "abc"}}},
		tarEntry{header: tar.Header{Typeflag: tar.TypeDir, Name: "acme-app-abc1234/", Mode: 0o755}},
		file("acme-app-abc1234/README.md", "hi\n", 0o644),
		file("acme-app-abc1234/bin/run.sh", "#!/bin/sh\n", 0o755),
		link("acme-app-abc1234/docs/readme", "../README.md"),
		link("acme-app-abc1234/outside", "/etc/passwd"),
	), dir)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "bin/run.sh"))
	if err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("bin/run.sh = %v, %v; want it executable", info, err)
	}

	info, err = os.Stat(filepath.Join(dir, "README.md"))
	if err != nil || info.Mode()&0o111 != 0 {
		t.Errorf("README.md = %v, %v; want it not executable", info, err)
	}

	for name, want := range map[string]string{"docs/readme": "../README.md", "outside": "/etc/passwd"} {
		got, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || got != want {
			t.Errorf("%s -> %q (%v), want the link's own text %q", name, got, err, want)
		}
	}

	_, err = os.Stat(filepath.Join(dir, "pax_global_header"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pax_global_header was written as a file: %v", err)
	}
}

// TestUnpackTarballStaysInside: an entry naming a path outside the artifact,
// directly or through a link an earlier entry made, is refused rather than
// written.
func TestUnpackTarballStaysInside(t *testing.T) {
	t.Parallel()

	for name, entries := range map[string][]tarEntry{
		"dot-dot":          {file("acme-app-abc/../../escaped", "x", 0o644)},
		"through a link":   {link("acme-app-abc/out", "../../"), file("acme-app-abc/out/escaped", "x", 0o644)},
		"absolute link in": {link("acme-app-abc/etc", "/tmp"), file("acme-app-abc/etc/escaped", "x", 0o644)},
	} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "a", "b")

		err := os.MkdirAll(dir, 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = unpackTarball(tarball(t, entries...), dir)
		if err == nil {
			t.Errorf("%s: unpacked, want it refused", name)
		}

		for _, escaped := range []string{filepath.Join(parent, "escaped"), filepath.Join(parent, "a", "escaped"), "/tmp/escaped"} {
			_, statErr := os.Lstat(escaped)
			if statErr == nil {
				t.Errorf("%s: wrote %s outside the artifact", name, escaped)
			}
		}
	}
}

func TestUnpackTarballIsBounded(t *testing.T) {
	t.Parallel()

	err := unpackTarballWithin(tarball(t, file("r/a", strings.Repeat("x", 10), 0o644), file("r/b", strings.Repeat("x", 10), 0o644)), t.TempDir(), 15)
	if !errors.Is(err, errTreeTooLarge) {
		t.Errorf("err = %v, want the unpack limit", err)
	}
}

func TestWriteMetadataRefusesACollision(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "pr.json"), []byte("a repository file"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = writeMetadata(dir, map[string][]byte{"pr.json": []byte("{}")})
	if err == nil || !strings.Contains(err.Error(), "already has a pr.json") {
		t.Errorf("err = %v, want the collision named", err)
	}

	body, _ := os.ReadFile(filepath.Join(dir, "pr.json")) //nolint:gosec // the test's own temp directory
	if string(body) != "a repository file" {
		t.Errorf("pr.json = %q, want the repository's file untouched", body)
	}
}

func githubServer(t *testing.T, handler http.HandlerFunc) config.GitHubConnection {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("RESOURCE_TEST_GH_TOKEN", "t0ken")

	return config.GitHubConnection{Repo: "acme/app", TokenEnv: "RESOURCE_TEST_GH_TOKEN", Endpoint: server.URL}
}

func noBackoff(t *testing.T) {
	t.Helper()

	previous := attemptBackoff
	attemptBackoff = 0

	t.Cleanup(func() { attemptBackoff = previous })
}

func TestGitHubClientRetriesAGet(t *testing.T) {
	noBackoff(t)

	var gets atomic.Int32

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0ken" {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if gets.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})

	client, err := newGitHubClient(connection)
	if err != nil {
		t.Fatal(err)
	}

	login, err := client.login(context.Background())
	if err != nil || login != "octocat" || gets.Load() != 3 {
		t.Errorf("login = %q, %v after %d gets; want octocat on the third", login, err, gets.Load())
	}
}

// TestGitHubClientDoesNotRetryAPost: a comment accepted before a 502 would
// post twice.
func TestGitHubClientDoesNotRetryAPost(t *testing.T) {
	noBackoff(t)

	var posts atomic.Int32

	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})

	client, err := newGitHubClient(connection)
	if err != nil {
		t.Fatal(err)
	}

	reply, err := client.send(context.Background(), http.MethodPost, client.api+"/x", "", map[string]any{"body": "hi"})
	if err != nil || reply.status != http.StatusBadGateway || posts.Load() != 1 {
		t.Errorf("post = %d, %v after %d sends; want one 502 and no retry", reply.status, err, posts.Load())
	}
}

// TestGitHubClientDoesNotSitOutARateLimit: a Retry-After longer than a stage
// should wait is the answer, not a pause.
func TestGitHubClientDoesNotSitOutARateLimit(t *testing.T) {
	var sent atomic.Int32

	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})

	client, err := newGitHubClient(connection)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.getJSON(context.Background(), "/user", nil)
	if err == nil || !strings.Contains(err.Error(), "API rate limit exceeded") || sent.Load() != 1 {
		t.Errorf("err = %v after %d sends; want GitHub's words after one", err, sent.Load())
	}
}

// TestGitHubClientAsksConditionally: an unchanged answer is a 304 served from
// the last body, which is what makes an idle repository free to poll.
func TestGitHubClientAsksConditionally(t *testing.T) {
	var conditional atomic.Int32

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)

			return
		}

		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(`{"login":"octocat"}`))
	})

	client, err := newGitHubClient(connection)
	if err != nil {
		t.Fatal(err)
	}

	for range 2 {
		login, err := client.login(context.Background())
		if err != nil || login != "octocat" {
			t.Fatalf("login = %q, %v", login, err)
		}
	}

	if conditional.Load() != 1 {
		t.Errorf("conditional requests = %d, want the second ask to be one", conditional.Load())
	}
}

func TestGitHubClientNeedsItsToken(t *testing.T) {
	t.Setenv("RESOURCE_TEST_MISSING_TOKEN", "")

	_, err := newGitHubClient(config.GitHubConnection{Repo: "acme/app", TokenEnv: "RESOURCE_TEST_MISSING_TOKEN"})
	if !errors.Is(err, errGitHubToken) || !strings.Contains(err.Error(), "$RESOURCE_TEST_MISSING_TOKEN") {
		t.Errorf("err = %v, want the missing variable named", err)
	}
}

func TestGitHubEnterpriseGraphQLSitsBesideREST(t *testing.T) {
	t.Setenv("RESOURCE_TEST_GH_TOKEN", "t")

	client, err := newGitHubClient(config.GitHubConnection{TokenEnv: "RESOURCE_TEST_GH_TOKEN", Endpoint: "https://ghe.example/api/v3/"})
	if err != nil {
		t.Fatal(err)
	}

	if client.api != "https://ghe.example/api/v3" || client.graphql != "https://ghe.example/api/graphql" {
		t.Errorf("api %q graphql %q", client.api, client.graphql)
	}
}

// TestGitHubSearchRefusesHalfAnAnswer: GraphQL's 200 with errors is not a
// short list of pull requests.
func TestGitHubSearchRefusesHalfAnAnswer(t *testing.T) {
	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"search":{"nodes":[{"number":1,"headRefOid":"a"}]}},"errors":[{"message":"timeout"}]}`))
	})

	_, err := githubPRsCheck(context.Background(), map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("err = %v, want the GraphQL error", err)
	}
}

func TestGitHubPRsQueryIsStableAndQuoted(t *testing.T) {
	t.Parallel()

	draft := false
	source := config.GitHubPRsSource{Repo: "acme/app", Author: "alice", Assignee: "@me", ReviewRequested: "bob", Base: "main", Labels: []string{"needs review"}, Draft: &draft}

	want := `repo:acme/app is:pr is:open assignee:@me author:alice base:main draft:false label:"needs review" review-requested:bob`

	for range 5 {
		if got := githubPRsQuery(source); got != want {
			t.Fatalf("query = %q, want %q", got, want)
		}
	}
}

// commentListing serves one full page of alice's conversation comments on
// #7, then a short page holding one issue comment, recording how many pages
// were asked for and the since: of the last ask.
func commentListing(t *testing.T) (map[string]any, *atomic.Int32, *atomic.Value) {
	t.Helper()

	var (
		pages atomic.Int32
		since atomic.Value
	)

	full := make([]string, githubPageSize)
	for i := range full {
		full[i] = `{"id":` + strconv.Itoa(i+1) + `,"body":"/go","updated_at":"2026-09-27T00:00:00Z","user":{"login":"Alice"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7"}`
	}

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		pages.Add(1)
		since.Store(r.URL.Query().Get("since"))

		if r.URL.Query().Get("page") == "1" {
			_, _ = w.Write([]byte("[" + strings.Join(full, ",") + "]"))

			return
		}

		_, _ = w.Write([]byte(`[{"id":999,"body":"/go","updated_at":"2026-09-27T00:00:01Z","user":{"login":"alice"},"html_url":"https://x/issues/9#c","issue_url":"https://x/issues/9"}]`))
	})

	source := map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint, "author": "alice", "body": "^/go", "kinds": []any{"conversation"}}

	return source, &pages, &since
}

// TestGitHubCommentsFirstCheckReadsOnePage: with no cursor a check is a look
// at what is recent, not a walk through the repository's history.
func TestGitHubCommentsFirstCheckReadsOnePage(t *testing.T) {
	source, pages, _ := commentListing(t)

	versions, err := githubCommentsCheck(context.Background(), source, nil)
	if err != nil || pages.Load() != 1 || len(versions) != githubPageSize {
		t.Fatalf("%d versions, %v, %d pages; want one full page and no more", len(versions), err, pages.Load())
	}
}

// TestGitHubCommentsCheckReadsBackToTheCursor: a cursor is since:, paged
// until GitHub runs out, and only the watched kinds become versions.
func TestGitHubCommentsCheckReadsBackToTheCursor(t *testing.T) {
	source, pages, since := commentListing(t)

	versions, err := githubCommentsCheck(context.Background(), source, map[string]any{"updated": "2026-09-26T00:00:00Z"})
	if err != nil || pages.Load() != 2 || since.Load() != "2026-09-26T00:00:00Z" {
		t.Fatalf("%v, %d pages, since %v; want two pages asked since the cursor", err, pages.Load(), since.Load())
	}

	// 999 is on an issue, and only conversation comments are watched.
	for _, version := range versions {
		if version["id"] == "999" || version["kind"] != config.GitHubCommentConversation || version["number"] != "7" {
			t.Errorf("version %v, want only conversation comments on #7", version)
		}
	}
}

func TestResolvePullRequest(t *testing.T) {
	t.Parallel()

	pr := map[string]any{"number": "7", "sha": "abc"}
	comment := map[string]any{"id": "1", "kind": "conversation", "number": "8", "updated": "t"}

	for name, tc := range map[string]struct {
		params config.GitHubPostParams
		inputs PutInputs
		want   pullRequestTarget
		err    string
	}{
		"the one input":             {inputs: PutInputs{Names: []string{"pr", "notes"}, Versions: map[string]map[string]any{"pr": pr}}, want: pullRequestTarget{number: "7", sha: "abc"}},
		"a comment's has no commit": {inputs: PutInputs{Versions: map[string]map[string]any{"said": comment}}, want: pullRequestTarget{number: "8"}},
		"from picks":                {params: config.GitHubPostParams{From: "said"}, inputs: PutInputs{Versions: map[string]map[string]any{"pr": pr, "said": comment}}, want: pullRequestTarget{number: "8"}},
		"number wins":               {params: config.GitHubPostParams{Number: "12"}, inputs: PutInputs{Versions: map[string]map[string]any{"pr": pr}}, want: pullRequestTarget{number: "12"}},
		"none":                      {inputs: PutInputs{Names: []string{"notes"}}, err: "no input names a pull request"},
		"two":                       {inputs: PutInputs{Versions: map[string]map[string]any{"pr": pr, "said": comment}}, err: "several inputs name a pull request (pr, said)"},
		"from names nothing":        {params: config.GitHubPostParams{From: "gone"}, inputs: PutInputs{Names: []string{"pr"}, Versions: map[string]map[string]any{"pr": pr}}, err: `params.from: "gone"`},
	} {
		got, err := resolvePullRequest(tc.params, tc.inputs)

		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err = %v, want %q", name, err, tc.err)
			}

			continue
		}

		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

func TestReadPostBodyRefusesEmptyAndOutside(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "blank.md"), []byte(" \n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ path, want string }{
		"empty":   {"blank.md", "is empty"},
		"missing": {"none.md", "params.body_file"},
		"outside": {"../../etc/passwd", "params.body_file"},
	} {
		_, err := readPostBody(dir, tc.path)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
