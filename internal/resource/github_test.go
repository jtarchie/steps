package resource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// TestGitHubPRsCheckSkipsANodeWithoutBoth: a search hit that is not a pull request comes back as an empty node, and a version with no commit could never be fetched.
func TestGitHubPRsCheckSkipsANodeWithoutBoth(t *testing.T) {
	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"search":{"nodes":[{},{"number":3},{"headRefOid":"c"},{"number":2,"headRefOid":"b"}]}}}`))
	})

	versions, err := githubPRsCheck(context.Background(), map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint})
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 || versions[0]["number"] != "2" || versions[0]["sha"] != "b" {
		t.Errorf("versions = %v, want only #2 at b", versions)
	}
}

// TestGitHubGetRefusesAVersionMissingAField: a get names one thing by every field of its version, and one missing is refused before GitHub is asked anything.
func TestGitHubGetRefusesAVersionMissingAField(t *testing.T) {
	var asked atomic.Int32

	connection := githubServer(t, func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})

	source := map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint}

	for name, tc := range map[string]struct {
		get     func(context.Context, map[string]any, map[string]any, string) error
		version map[string]any
		want    string
	}{
		"pr without sha":         {githubPRsIn, map[string]any{"number": "7"}, "names no pull request number and commit"},
		"pr without number":      {githubPRsIn, map[string]any{"sha": "abc"}, "names no pull request number and commit"},
		"comment without id":     {githubCommentsIn, map[string]any{"kind": "conversation", "number": "7"}, "names no comment"},
		"comment without kind":   {githubCommentsIn, map[string]any{"id": "1", "number": "7"}, "names no comment"},
		"comment without number": {githubCommentsIn, map[string]any{"id": "1", "kind": "conversation"}, "names no comment"},
	} {
		err := tc.get(context.Background(), source, tc.version, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}

	if asked.Load() != 0 {
		t.Errorf("GitHub was asked %d times about versions that name nothing", asked.Load())
	}
}

// TestGitHubPendingReviewReplacesOnlyYourOwnDraft: the token user's draft is found on whichever page it sits, is the only one deleted, and a refused delete stops the review rather than posting a second draft beside it.
func TestGitHubPendingReviewReplacesOnlyYourOwnDraft(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		got := pendingReview(t, http.StatusOK)
		if got.deleted != "500" || got.err != nil || got.version["id"] != "900" || got.version["number"] != "7" || got.posted != 1 {
			t.Errorf("%+v; want draft 500 deleted and the new review on #7", got)
		}
	})

	t.Run("refused", func(t *testing.T) {
		got := pendingReview(t, http.StatusUnprocessableEntity)
		if got.deleted != "500" || got.err == nil || !strings.Contains(got.err.Error(), "DELETE") || got.posted != 0 {
			t.Errorf("%+v; want the refused delete of 500 and nothing posted", got)
		}
	})
}

type pendingReviewResult struct {
	version map[string]any
	err     error
	deleted string
	posted  int
}

// pendingReview posts a pending review on #7, whose reviews are a full page of somebody else's drafts and then the token user's own draft (500) beside an approval, answering the delete with deleteStatus.
func pendingReview(t *testing.T, deleteStatus int) pendingReviewResult {
	t.Helper()
	noBackoff(t)

	others := make([]string, githubPageSize)
	for i := range others {
		others[i] = `{"id":` + strconv.Itoa(i+1) + `,"state":"PENDING","user":{"login":"someone"}}`
	}

	pages := map[string]string{
		"1": "[" + strings.Join(others, ",") + "]",
		"2": `[{"id":500,"state":"PENDING","user":{"login":"octocat"}},{"id":501,"state":"APPROVED","user":{"login":"octocat"}}]`,
	}

	var (
		mu     sync.Mutex
		result pendingReviewResult
	)

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"OctoCat"}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(cmpOr(pages[r.URL.Query().Get("page")], "[]")))
		case r.Method == http.MethodDelete:
			result.deleted += strings.TrimPrefix(r.URL.Path, "/repos/acme/app/pulls/7/reviews/")
			w.WriteHeader(deleteStatus)
		default:
			result.posted++
			_, _ = w.Write([]byte(`{"id":900}`))
		}
	})

	src := t.TempDir()

	err := os.WriteFile(filepath.Join(src, "review.md"), []byte("looks fine"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	version, err := githubReviewOut(context.Background(),
		map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint},
		map[string]any{"body_file": "review.md", "event": "pending"},
		PutInputs{Names: []string{"pr"}, Versions: map[string]map[string]any{"pr": {"number": "7", "sha": "abc"}}},
		src)

	mu.Lock()
	defer mu.Unlock()

	result.version, result.err = version, err

	return result
}

func TestResolveComment(t *testing.T) {
	t.Parallel()

	pr := map[string]any{"number": "7", "sha": "abc"}
	said := map[string]any{"id": "11", "kind": "conversation", "number": "7", "updated": "t"}
	inline := map[string]any{"id": "12", "kind": "review", "number": "7", "updated": "t"}

	for name, tc := range map[string]struct {
		from   string
		inputs PutInputs
		want   commentTarget
		err    string
	}{
		"the one comment":    {inputs: PutInputs{Versions: map[string]map[string]any{"pr": pr, "said": said}}, want: commentTarget{id: "11", kind: "conversation"}},
		"from picks":         {from: "inline", inputs: PutInputs{Versions: map[string]map[string]any{"said": said, "inline": inline}}, want: commentTarget{id: "12", kind: "review"}},
		"none":               {inputs: PutInputs{Names: []string{"pr"}, Versions: map[string]map[string]any{"pr": pr}}, err: "no input names a comment"},
		"two":                {inputs: PutInputs{Versions: map[string]map[string]any{"said": said, "inline": inline}}, err: "several inputs name a comment (inline, said)"},
		"from a pr":          {from: "pr", inputs: PutInputs{Names: []string{"pr", "said"}, Versions: map[string]map[string]any{"pr": pr, "said": said}}, err: `params.from: "pr"`},
		"from never fetched": {from: "said", inputs: PutInputs{Names: []string{"said"}}, err: `params.from: "said"`},
		"an unknown kind":    {inputs: PutInputs{Versions: map[string]map[string]any{"said": {"id": "1", "kind": "commit"}}}, err: `kind "commit"`},
	} {
		got, err := resolveComment(tc.from, tc.inputs)

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

// reactionServer is GitHub's reactions routes for comment 11, as a
// conversation comment: the token user is octocat, the listing is a full
// page of other people's reactions before octocat's own eyes (id 500), and
// add and delete answer with the given statuses.
type reactionServer struct {
	mu      sync.Mutex
	added   []string
	deleted []string
	listed  []string
}

func reactionPut(t *testing.T, params map[string]any, addStatus, deleteStatus int) (*reactionServer, map[string]any, error) {
	t.Helper()
	noBackoff(t)

	others := make([]string, githubPageSize)
	for i := range others {
		others[i] = `{"id":` + strconv.Itoa(i+1) + `,"content":"eyes","user":{"login":"someone"}}`
	}

	pages := map[string]string{
		"1": "[" + strings.Join(others, ",") + "]",
		"2": `[{"id":500,"content":"eyes","user":{"login":"octocat"}}]`,
	}

	recorded := &reactionServer{}

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		recorded.mu.Lock()
		defer recorded.mu.Unlock()

		switch {
		case r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"OctoCat"}`))
		case r.URL.Path != "/repos/acme/app/issues/comments/11/reactions" && r.Method != http.MethodDelete:
			t.Errorf("%s %s, want the conversation comment's reactions", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodGet:
			recorded.listed = append(recorded.listed, r.URL.Query().Get("content")+"@"+r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(cmpOr(pages[r.URL.Query().Get("page")], "[]")))
		case r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			recorded.added = append(recorded.added, string(body))
			w.WriteHeader(addStatus)
			_, _ = w.Write([]byte(`{"id":900,"message":"refused"}`))
		case r.Method == http.MethodDelete:
			recorded.deleted = append(recorded.deleted, r.URL.Path)
			w.WriteHeader(deleteStatus)
		}
	})

	version, err := githubReactionOut(context.Background(),
		map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint},
		params,
		PutInputs{Names: []string{"said"}, Versions: map[string]map[string]any{"said": {"id": "11", "kind": "conversation", "number": "7", "updated": "t"}}},
		t.TempDir())

	recorded.mu.Lock()
	defer recorded.mu.Unlock()

	return recorded, version, err
}

// TestGitHubReactionAddsThenRemovesOnlyItsOwn: the add goes first, so the
// comment never sits with no mark; the remove finds the token user's own
// reaction on whichever page it is and deletes that one, by id.
func TestGitHubReactionAddsThenRemovesOnlyItsOwn(t *testing.T) {
	recorded, version, err := reactionPut(t, map[string]any{"add": "rocket", "remove": "eyes"}, http.StatusCreated, http.StatusNoContent)
	if err != nil {
		t.Fatal(err)
	}

	for what, tc := range map[string]struct{ got, want []string }{
		"added":   {recorded.added, []string{`{"content":"rocket"}`}},
		"deleted": {recorded.deleted, []string{"/repos/acme/app/issues/comments/11/reactions/500"}},
		"listed":  {recorded.listed, []string{"eyes@1", "eyes@2"}},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s %v, want %v: one rocket, and only octocat's own eyes found across two pages and deleted", what, tc.got, tc.want)
		}
	}

	if version["comment"] != "11" || version["add"] != "rocket" || version["remove"] != "eyes" {
		t.Errorf("version = %v, want the comment and both reactions", version)
	}
}

// TestGitHubReactionIsIdempotent: already there and already gone are the
// world in the state asked for, which a replay or a resume always finds.
func TestGitHubReactionIsIdempotent(t *testing.T) {
	_, _, err := reactionPut(t, map[string]any{"add": "rocket", "remove": "eyes"}, http.StatusOK, http.StatusNotFound)
	if err != nil {
		t.Errorf("200 on add and 404 on delete: %v, want success", err)
	}
}

func TestGitHubReactionFailsOnAnyOtherAnswer(t *testing.T) {
	for name, tc := range map[string]struct {
		add, remove int
		want        string
	}{
		"add refused":    {http.StatusForbidden, http.StatusNoContent, "POST /repos/acme/app/issues/comments/11/reactions: 403"},
		"delete refused": {http.StatusCreated, http.StatusForbidden, "DELETE /repos/acme/app/issues/comments/11/reactions/500: 403"},
	} {
		t.Run(name, func(t *testing.T) {
			recorded, _, err := reactionPut(t, map[string]any{"add": "rocket", "remove": "eyes"}, tc.add, tc.remove)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}

			if tc.add != http.StatusCreated && len(recorded.deleted) != 0 {
				t.Errorf("deleted %v after the add failed, want the old mark left in place", recorded.deleted)
			}
		})
	}
}

// TestGitHubCommentsSkipReactedAsksOnlyWhenItMatters: a comment whose summary
// counts none of the reaction costs no request; one that counts some is
// asked whose they are, and only the token user's own skip it.
func TestGitHubCommentsSkipReactedAsksOnlyWhenItMatters(t *testing.T) {
	var (
		mu     sync.Mutex
		listed []string
	)

	comment := func(id int, rockets int) string {
		return `{"id":` + strconv.Itoa(id) + `,"body":"/go","updated_at":"2026-09-27T00:00:0` + strconv.Itoa(id) + `Z","user":{"login":"alice"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7","reactions":{"total_count":` + strconv.Itoa(rockets) + `,"rocket":` + strconv.Itoa(rockets) + `,"eyes":0}}`
	}

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/repos/acme/app/issues/comments":
			// 4 carries no summary at all, as an older GitHub Enterprise may answer.
			_, _ = w.Write([]byte("[" + comment(1, 0) + "," + comment(2, 1) + "," + comment(3, 1) + `,{"id":4,"body":"/go","updated_at":"2026-09-27T00:00:04Z","user":{"login":"alice"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7"}]`))
		case "/repos/acme/app/issues/comments/2/reactions", "/repos/acme/app/issues/comments/4/reactions":
			listed = append(listed, r.URL.Path+"?"+r.URL.Query().Get("content"))
			_, _ = w.Write([]byte(`[{"id":1,"content":"rocket","user":{"login":"OctoCat"}}]`))
		case "/repos/acme/app/issues/comments/3/reactions":
			listed = append(listed, r.URL.Path+"?"+r.URL.Query().Get("content"))
			_, _ = w.Write([]byte(`[{"id":2,"content":"rocket","user":{"login":"carol"}}]`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})

	versions, err := githubCommentsCheck(context.Background(), map[string]any{
		"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint,
		"kinds": []any{"conversation"}, "skip_reacted": "rocket",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ids := make([]string, 0, len(versions))
	for _, version := range versions {
		ids = append(ids, versionField(version, "id"))
	}

	if strings.Join(ids, ",") != "1,3" {
		t.Errorf("reported %v, want 1 (no rocket) and 3 (carol's rocket), not 2 and 4 (the token user's own)", ids)
	}

	mu.Lock()
	defer mu.Unlock()

	slices.Sort(listed)

	if strings.Join(listed, " ") != "/repos/acme/app/issues/comments/2/reactions?rocket /repos/acme/app/issues/comments/3/reactions?rocket /repos/acme/app/issues/comments/4/reactions?rocket" {
		t.Errorf("listed %v, want 2, 3 and 4 asked about rockets alone, and 1 not asked", listed)
	}
}

// TestGitHubCommentsSkipReactedRefusesHalfAnAnswer: a lookup that fails
// fails the check, because reporting a done comment as new re-runs work
// with side effects.
func TestGitHubCommentsSkipReactedRefusesHalfAnAnswer(t *testing.T) {
	noBackoff(t)

	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			_, _ = w.Write([]byte(`{"login":"octocat"}`))
		case "/repos/acme/app/issues/comments":
			_, _ = w.Write([]byte(`[{"id":2,"body":"/go","updated_at":"2026-09-27T00:00:02Z","user":{"login":"alice"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7","reactions":{"rocket":1}}]`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	_, err := githubCommentsCheck(context.Background(), map[string]any{
		"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint, "kinds": []any{"conversation"}, "skip_reacted": "rocket",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "/issues/comments/2/reactions") {
		t.Errorf("err = %v, want the failed lookup named", err)
	}
}

// TestGitHubCommentsAuthorMeIsTheTokensUser: @me is resolved once, to the
// login the token answers to, whether or not skip_reacted needed it too.
func TestGitHubCommentsAuthorMeIsTheTokensUser(t *testing.T) {
	connection := githubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			_, _ = w.Write([]byte(`{"login":"octocat"}`))

			return
		}

		_, _ = w.Write([]byte(`[{"id":1,"body":"/go","updated_at":"2026-09-27T00:00:01Z","user":{"login":"alice"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7","reactions":{"rocket":0}},` +
			`{"id":2,"body":"/go","updated_at":"2026-09-27T00:00:02Z","user":{"login":"OctoCat"},"html_url":"https://x/pull/7#c","issue_url":"https://x/issues/7","reactions":{"rocket":0}}]`))
	})

	for _, skip := range []string{"", "rocket"} {
		source := map[string]any{"repo": "acme/app", "token_env": connection.TokenEnv, "endpoint": connection.Endpoint, "author": "@me", "kinds": []any{"conversation"}}
		if skip != "" {
			source["skip_reacted"] = skip
		}

		versions, err := githubCommentsCheck(context.Background(), source, nil)
		if err != nil || len(versions) != 1 || versions[0]["id"] != "2" {
			t.Errorf("skip_reacted %q: %v, %v; want only octocat's comment", skip, versions, err)
		}
	}
}
