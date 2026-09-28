package e2e

// An in-process GitHub, for the built-in github-* resource types.
//
// It answers the REST and GraphQL calls those types make, keyed to fixture
// pull requests and comments a test declares, and records what it was sent —
// the comments and reviews a put posted, the search queries a check ran —
// because those are the effects no workspace file can show.
//
// It is deliberately a reading of GitHub's API rather than a mock of the
// client: a check that sends the wrong qualifier or asks for the wrong page
// gets the answer GitHub would give, not a canned one.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitHubToken is what every fake answers to; anything else is a 401.
const fakeGitHubToken = "gh-test-token"

type fakeFile struct {
	Path string
	Body string
	Exec bool
	Link string // non-empty: a symlink to this target, and Body is ignored
}

type fakePR struct {
	Number    int
	SHA       string
	Title     string
	Author    string
	Base      string
	MergeBase string
	Diff      string
	Draft     bool
	Assignees []string
	Reviewers []string
	Labels    []string
	Files     []fakeFile
}

type fakeComment struct {
	ID      int64
	Number  int
	Kind    string // conversation, review or issue
	Author  string
	Body    string
	Updated time.Time
}

type fakeReview struct {
	ID       int64
	User     string
	State    string
	Body     string
	CommitID string
}

type postedComment struct {
	Number int
	Body   string
}

type fakeGitHub struct {
	t      *testing.T
	server *httptest.Server
	repo   string
	login  string

	mu       sync.Mutex
	prs      []fakePR
	comments []fakeComment
	reviews  map[int][]fakeReview
	posted   []postedComment
	queries  []string
	nextID   int64
	requests int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()

	fake := &fakeGitHub{t: t, repo: fakeRepo, login: "octocat", reviews: map[int][]fakeReview{}, nextID: 9000}
	fake.server = httptest.NewServer(fake.routes())
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeGitHub) URL() string { return f.server.URL }

func (f *fakeGitHub) addPR(pr fakePR) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.prs = append(f.prs, pr)
}

func (f *fakeGitHub) setPR(number int, edit func(*fakePR)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.prs {
		if f.prs[i].Number == number {
			edit(&f.prs[i])
		}
	}
}

func (f *fakeGitHub) addComment(comment fakeComment) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.comments = append(f.comments, comment)
}

func (f *fakeGitHub) editComment(id int64, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i := range f.comments {
		if f.comments[i].ID == id {
			f.comments[i].Body = body
			f.comments[i].Updated = at
		}
	}
}

func (f *fakeGitHub) addReview(number int, review fakeReview) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.reviews[number] = append(f.reviews[number], review)
}

func (f *fakeGitHub) postedComments() []postedComment {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.posted)
}

func (f *fakeGitHub) reviewsOn(number int) []fakeReview {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.reviews[number])
}

func (f *fakeGitHub) searchQueries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.queries)
}

// routes is GitHub's API as the github-* types call it. Every route is
// behind the same token check and the same lock, and names the one
// repository the fake serves.
func (f *fakeGitHub) routes() http.Handler {
	mux := http.NewServeMux()
	repo := "/repos/{owner}/{name}"

	handle := func(pattern string, serve func(http.ResponseWriter, *http.Request)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+fakeGitHubToken {
				respondJSON(w, http.StatusUnauthorized, map[string]any{"message": "Bad credentials"})

				return
			}

			if owner := r.PathValue("owner"); owner != "" && owner+"/"+r.PathValue("name") != f.repo {
				respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})

				return
			}

			f.mu.Lock()
			defer f.mu.Unlock()

			f.requests++
			serve(w, r)
		})
	}

	handle("POST /graphql", f.serveSearch)
	handle("GET /user", func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, map[string]any{"login": f.login})
	})
	handle("GET "+repo+"/pulls/{number}", func(w http.ResponseWriter, r *http.Request) { f.servePull(w, r, atoi(r.PathValue("number"))) })
	// pulls/comments/{id} and pulls/{number}/reviews overlap as patterns, so one route serves both, as GitHub's own router must.
	handle("GET "+repo+"/pulls/{number}/{sub}", f.servePullSub)
	handle("GET "+repo+"/pulls/comments", func(w http.ResponseWriter, r *http.Request) { f.serveCommentList(w, r, "review") })
	handle("POST "+repo+"/pulls/{number}/reviews", func(w http.ResponseWriter, r *http.Request) { f.createReview(w, r, atoi(r.PathValue("number"))) })
	handle("DELETE "+repo+"/pulls/{number}/reviews/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.deleteReview(w, atoi(r.PathValue("number")), int64(atoi(r.PathValue("id"))))
	})
	handle("GET "+repo+"/issues/comments", func(w http.ResponseWriter, r *http.Request) { f.serveCommentList(w, r, "conversation", "issue") })
	handle("GET "+repo+"/issues/comments/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.serveComment(w, int64(atoi(r.PathValue("id"))), "conversation", "issue")
	})
	handle("POST "+repo+"/issues/{number}/comments", func(w http.ResponseWriter, r *http.Request) { f.postComment(w, r, atoi(r.PathValue("number"))) })
	handle("GET "+repo+"/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		number := atoi(r.PathValue("number"))
		respondJSON(w, http.StatusOK, map[string]any{"number": number, "title": "an issue", "html_url": f.html("issues", number)})
	})
	handle("GET "+repo+"/compare/{basehead...}", func(w http.ResponseWriter, r *http.Request) { f.serveCompare(w, r, r.PathValue("basehead")) })
	// GitHub answers a tarball with a redirect to codeload; following one is part of the contract. Under the repo prefix, so the one auth check covers it.
	handle("GET "+repo+"/tarball/{sha}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/repos/"+f.repo+"/_codeload/"+url.PathEscape(r.PathValue("sha")), http.StatusFound)
	})
	handle("GET "+repo+"/_codeload/{sha}", func(w http.ResponseWriter, r *http.Request) { f.serveTarball(w, r.PathValue("sha")) })

	return mux
}

func (f *fakeGitHub) servePullSub(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.PathValue("number") == "comments":
		f.serveComment(w, int64(atoi(r.PathValue("sub"))), "review")
	case r.PathValue("sub") == "reviews":
		f.listReviews(w, atoi(r.PathValue("number")))
	default:
		respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
	}
}

func (f *fakeGitHub) html(kind string, number int) string {
	return fmt.Sprintf("https://github.example/%s/%s/%d", f.repo, kind, number)
}

func (f *fakeGitHub) pr(number int) (fakePR, bool) {
	for _, pr := range f.prs {
		if pr.Number == number {
			return pr, true
		}
	}

	return fakePR{}, false
}

func (f *fakeGitHub) prBySHA(sha string) (fakePR, bool) {
	for _, pr := range f.prs {
		if pr.SHA == sha {
			return pr, true
		}
	}

	return fakePR{}, false
}

// serveSearch answers the one GraphQL query the github-prs check sends: a
// search over issues, read the way GitHub reads its qualifiers.
func (f *fakeGitHub) serveSearch(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}

	err := json.NewDecoder(r.Body).Decode(&request)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})

		return
	}

	q, _ := request.Variables["q"].(string)
	f.queries = append(f.queries, q)

	nodes := []map[string]any{}

	for _, pr := range f.prs {
		if f.matches(pr, searchQualifiers(q)) {
			nodes = append(nodes, map[string]any{"number": pr.Number, "headRefOid": pr.SHA})
		}
	}

	respondJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"search": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil},
		"nodes":    nodes,
	}}})
}

type qualifier struct{ key, value string }

// searchQualifiers splits a search string into key:value pairs, honoring
// double quotes the way GitHub does for a label with a space in it.
func searchQualifiers(q string) []qualifier {
	var (
		out     []qualifier
		current strings.Builder
		quoted  bool
	)

	flush := func() {
		key, value, _ := strings.Cut(current.String(), ":")
		out = append(out, qualifier{key: key, value: value})
		current.Reset()
	}

	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ' ' && !quoted:
			if current.Len() > 0 {
				flush()
			}
		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		flush()
	}

	return out
}

func (f *fakeGitHub) matches(pr fakePR, qualifiers []qualifier) bool {
	me := func(login string) string {
		if login == "@me" {
			return f.login
		}

		return login
	}

	reads := map[string]func(string) bool{
		"repo":             func(v string) bool { return v == f.repo },
		"is":               func(v string) bool { return v == "pr" || v == "open" },
		"author":           func(v string) bool { return pr.Author == me(v) },
		"assignee":         func(v string) bool { return slices.Contains(pr.Assignees, me(v)) },
		"review-requested": func(v string) bool { return slices.Contains(pr.Reviewers, me(v)) },
		"label":            func(v string) bool { return slices.Contains(pr.Labels, v) },
		"base":             func(v string) bool { return pr.Base == v },
		"draft":            func(v string) bool { return strconv.FormatBool(pr.Draft) == v },
	}

	for _, q := range qualifiers {
		read, known := reads[q.key]
		if !known {
			f.t.Errorf("search sent an unknown qualifier %q", q.key+":"+q.value)

			return false
		}

		if !read(q.value) {
			return false
		}
	}

	return true
}

func (f *fakeGitHub) servePull(w http.ResponseWriter, r *http.Request, number int) {
	pr, ok := f.pr(number)
	if !ok {
		respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})

		return
	}

	if r.Header.Get("Accept") == "application/vnd.github.diff" {
		_, _ = io.WriteString(w, pr.Diff)

		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"number": pr.Number, "title": pr.Title, "draft": pr.Draft, "html_url": f.html("pull", pr.Number),
		"user": map[string]any{"login": pr.Author},
		"head": map[string]any{"sha": pr.SHA},
		"base": map[string]any{"ref": pr.Base},
	})
}

func (f *fakeGitHub) serveCompare(w http.ResponseWriter, r *http.Request, basehead string) {
	base, head, ok := strings.Cut(basehead, "...")

	pr, found := f.prBySHA(head)
	if !ok || !found || pr.Base != base {
		respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})

		return
	}

	if r.Header.Get("Accept") == "application/vnd.github.diff" {
		_, _ = io.WriteString(w, pr.Diff)

		return
	}

	respondJSON(w, http.StatusOK, map[string]any{"merge_base_commit": map[string]any{"sha": pr.MergeBase}})
}

// serveTarball builds the archive GitHub would: every path under one
// top-level directory named for the repository and commit, and a pax global
// header carrying the commit, which an unpacker has to skip.
func (f *fakeGitHub) serveTarball(w http.ResponseWriter, sha string) {
	pr, ok := f.prBySHA(sha)
	if !ok {
		respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})

		return
	}

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	prefix := strings.ReplaceAll(f.repo, "/", "-") + "-" + sha[:7] + "/"

	must := func(err error) {
		if err != nil {
			f.t.Errorf("building tarball: %v", err)
		}
	}

	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": sha}}))
	must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix, Mode: 0o755}))

	for _, file := range pr.Files {
		if file.Link != "" {
			must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: prefix + file.Path, Linkname: file.Link, Mode: 0o777}))

			continue
		}

		mode := int64(0o644)
		if file.Exec {
			mode = 0o755
		}

		must(tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + file.Path, Mode: mode, Size: int64(len(file.Body))}))
		_, err := io.WriteString(tw, file.Body)
		must(err)
	}

	must(tw.Close())
	must(gz.Close())

	w.Header().Set("Content-Type", "application/x-gzip")
	_, _ = w.Write(buf.Bytes())
}

// serveCommentList answers the repo-wide comment listings: sorted by update
// time when asked, trimmed by since:, paged, and carrying an ETag so a
// conditional request for an unchanged page is a 304.
func (f *fakeGitHub) serveCommentList(w http.ResponseWriter, r *http.Request, kinds ...string) {
	query := r.URL.Query()

	var since time.Time

	if raw := query.Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "since: " + err.Error()})

			return
		}

		since = parsed
	}

	if query.Get("sort") != "updated" {
		f.t.Errorf("a comment listing was asked for without sort=updated: %s", r.URL.RawQuery)
	}

	picked := f.listed(kinds, since, query.Get("direction") == "desc")

	perPage := max(atoi(query.Get("per_page")), 1)
	page := max(atoi(query.Get("page")), 1)
	start := min((page-1)*perPage, len(picked))
	end := min(start+perPage, len(picked))

	body := make([]map[string]any, 0, end-start)
	for _, comment := range picked[start:end] {
		body = append(body, f.commentJSON(comment))
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})

		return
	}

	etag := fmt.Sprintf(`"%x"`, len(encoded)*31+int(hashBytes(encoded)))

	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)

		return
	}

	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// listed is the comments of these kinds written at or after since, in update order.
func (f *fakeGitHub) listed(kinds []string, since time.Time, newestFirst bool) []fakeComment {
	var picked []fakeComment

	for _, comment := range f.comments {
		if slices.Contains(kinds, comment.Kind) && !comment.Updated.Before(since) {
			picked = append(picked, comment)
		}
	}

	sort.SliceStable(picked, func(i, j int) bool {
		if newestFirst {
			return picked[i].Updated.After(picked[j].Updated)
		}

		return picked[i].Updated.Before(picked[j].Updated)
	})

	return picked
}

func hashBytes(b []byte) uint32 {
	var h uint32 = 2166136261
	for _, c := range b {
		h = (h ^ uint32(c)) * 16777619
	}

	return h
}

func (f *fakeGitHub) serveComment(w http.ResponseWriter, id int64, kinds ...string) {
	for _, comment := range f.comments {
		if comment.ID == id && slices.Contains(kinds, comment.Kind) {
			respondJSON(w, http.StatusOK, f.commentJSON(comment))

			return
		}
	}

	respondJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

func (f *fakeGitHub) commentJSON(comment fakeComment) map[string]any {
	out := map[string]any{
		"id":         comment.ID,
		"body":       comment.Body,
		"user":       map[string]any{"login": comment.Author},
		"updated_at": comment.Updated.UTC().Format(time.RFC3339),
	}

	switch comment.Kind {
	case "review":
		out["html_url"] = f.html("pull", comment.Number) + "#discussion_r" + strconv.FormatInt(comment.ID, 10)
		out["pull_request_url"] = fmt.Sprintf("%s/repos/%s/pulls/%d", f.server.URL, f.repo, comment.Number)
		out["path"] = "main.go"
	case "conversation":
		out["html_url"] = f.html("pull", comment.Number) + "#issuecomment-" + strconv.FormatInt(comment.ID, 10)
		out["issue_url"] = fmt.Sprintf("%s/repos/%s/issues/%d", f.server.URL, f.repo, comment.Number)
	default:
		out["html_url"] = f.html("issues", comment.Number) + "#issuecomment-" + strconv.FormatInt(comment.ID, 10)
		out["issue_url"] = fmt.Sprintf("%s/repos/%s/issues/%d", f.server.URL, f.repo, comment.Number)
	}

	return out
}

func (f *fakeGitHub) postComment(w http.ResponseWriter, r *http.Request, number int) {
	var body struct {
		Body string `json:"body"`
	}

	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil || body.Body == "" {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed"})

		return
	}

	f.nextID++
	f.posted = append(f.posted, postedComment{Number: number, Body: body.Body})

	respondJSON(w, http.StatusCreated, map[string]any{"id": f.nextID, "html_url": f.html("pull", number)})
}

func (f *fakeGitHub) listReviews(w http.ResponseWriter, number int) {
	out := make([]map[string]any, 0, len(f.reviews[number]))
	for _, review := range f.reviews[number] {
		out = append(out, map[string]any{"id": review.ID, "state": review.State, "user": map[string]any{"login": review.User}})
	}

	respondJSON(w, http.StatusOK, out)
}

// reviewStates is what GitHub files a review under, by the event it was posted with; no event is a draft.
var reviewStates = map[string]string{"": "PENDING", "COMMENT": "COMMENTED", "APPROVE": "APPROVED", "REQUEST_CHANGES": "CHANGES_REQUESTED"} //nolint:gochecknoglobals // read-only table

func (f *fakeGitHub) createReview(w http.ResponseWriter, r *http.Request, number int) {
	var body struct {
		Body     string `json:"body"`
		Event    string `json:"event"`
		CommitID string `json:"commit_id"`
	}

	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": err.Error()})

		return
	}

	state, known := reviewStates[body.Event]
	if !known {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "unknown event " + body.Event})

		return
	}

	if state == "PENDING" && f.hasOwnDraft(number) {
		respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "User can only have one pending review per pull request"})

		return
	}

	f.nextID++
	f.reviews[number] = append(f.reviews[number], fakeReview{ID: f.nextID, User: f.login, State: state, Body: body.Body, CommitID: body.CommitID})

	respondJSON(w, http.StatusOK, map[string]any{"id": f.nextID, "state": state})
}

func (f *fakeGitHub) hasOwnDraft(number int) bool {
	for _, review := range f.reviews[number] {
		if review.State == "PENDING" && review.User == f.login {
			return true
		}
	}

	return false
}

func (f *fakeGitHub) deleteReview(w http.ResponseWriter, number int, id int64) {
	kept := f.reviews[number][:0]

	for _, review := range f.reviews[number] {
		if review.ID == id {
			if review.State != "PENDING" {
				respondJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "Can not delete a non-pending review"})

				return
			}

			continue
		}

		kept = append(kept, review)
	}

	f.reviews[number] = kept

	respondJSON(w, http.StatusOK, map[string]any{"id": id})
}

func respondJSON(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)

	return n
}
