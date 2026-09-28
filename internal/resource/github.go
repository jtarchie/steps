package resource

// The github-* built-ins' check, in and out (see config/github.go for what
// each type is and why there are four).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

const (
	// githubPageSize is GitHub's largest page.
	githubPageSize = 100
	// githubPageLimit bounds one check. A search answers at most a thousand
	// results anyway; a comment listing past a cursor this far behind means
	// more than a thousand comments arrived between two polls, and the
	// newest thousand are the ones worth reading.
	githubPageLimit = 10
)

// The files a pull request's metadata is written to, beside its tree.
const (
	prJSONFile      = "pr.json"
	prDiffFile      = "pr.diff"
	prNumberFile    = "pr.number"
	prSHAFile       = "pr.sha"
	prMergeBaseFile = "pr.mergebase"
	prURLFile       = "pr.url"
)

func githubCheckVersions(ctx context.Context, rt config.ResourceType, source, version map[string]any) ([]map[string]any, error) {
	var (
		versions []map[string]any
		err      error
	)

	switch rt.Config.GitHub {
	case config.GitHubPRsType:
		versions, err = githubPRsCheck(ctx, source)
	case config.GitHubCommentsType:
		versions, err = githubCommentsCheck(ctx, source, version)
	default:
		// No check: a put-only type has nothing to find, and the load rule
		// refuses a get of one before this is reached.
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

func githubRunIn(ctx context.Context, rt config.ResourceType, source, version map[string]any, destDir string) error {
	var err error

	switch rt.Config.GitHub {
	case config.GitHubPRsType:
		err = githubPRsIn(ctx, source, version, destDir)
	case config.GitHubCommentsType:
		err = githubCommentsIn(ctx, source, version, destDir)
	default:
		err = fmt.Errorf("a %s resource only publishes, so there is nothing to fetch", rt.Config.GitHub)
	}

	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}

func githubRunOut(ctx context.Context, rt config.ResourceType, source, params map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	var (
		version map[string]any
		err     error
	)

	switch rt.Config.GitHub {
	case config.GitHubPRCommentType:
		version, err = githubCommentOut(ctx, source, params, inputs, srcDir)
	case config.GitHubPRReviewType:
		version, err = githubReviewOut(ctx, source, params, inputs, srcDir)
	default:
		err = fmt.Errorf("a %s resource only finds work, so there is nothing to publish to", rt.Config.GitHub)
	}

	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.put", "resource_type", rt.Name, "src_dir", srcDir, "result", version)

	return version, nil
}

// The one query github-prs sends. A search rather than the pulls listing,
// because the listing filters on nothing a pipeline asks about — author,
// assignee, requested reviewer and label are all search qualifiers only —
// and GraphQL rather than REST search because REST answers with issues,
// which carry no head commit, and the version needs one.
const githubPRSearch = `query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { number headRefOid } }
  }
}`

// githubPRsQuery is the search string for a source: every filter a
// qualifier, each value already held by ParseGitHubPRsSource to a shape that
// stays one qualifier.
func githubPRsQuery(source config.GitHubPRsSource) string {
	parts := []string{"repo:" + source.Repo, "is:pr", "is:open"}

	for qualifier, value := range map[string]string{"author": source.Author, "assignee": source.Assignee, "review-requested": source.ReviewRequested, "base": source.Base} {
		if value != "" {
			parts = append(parts, qualifier+":"+value)
		}
	}

	for _, label := range source.Labels {
		parts = append(parts, `label:"`+label+`"`)
	}

	if source.Draft != nil {
		parts = append(parts, "draft:"+strconv.FormatBool(*source.Draft))
	}

	// The map above iterates in no order; a stable query keeps the fixture
	// tests and GitHub's own caching honest.
	sort.Strings(parts[3:])

	return strings.Join(parts, " ")
}

func githubPRsCheck(ctx context.Context, raw map[string]any) ([]map[string]any, error) {
	source, err := config.ParseGitHubPRsSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	found, err := client.collectPRs(ctx, githubPRsQuery(source))
	if err != nil {
		return nil, err
	}

	numbers := make([]int, 0, len(found))
	for number := range found {
		numbers = append(numbers, number)
	}

	// Oldest first, by number: a push mints {number, new sha}, which history
	// records as newly seen, so the order a check reports never has to move.
	slices.Sort(numbers)

	versions := make([]map[string]any, 0, len(numbers))
	for _, number := range numbers {
		versions = append(versions, map[string]any{"number": strconv.Itoa(number), "sha": found[number]})
	}

	return versions, nil
}

// collectPRs pages through one search, number to head commit.
func (c *githubClient) collectPRs(ctx context.Context, query string) (map[int]string, error) {
	found := map[int]string{}

	var after any

	for range githubPageLimit {
		page, err := c.searchPRs(ctx, query, after)
		if err != nil {
			return nil, err
		}

		for _, node := range page.Nodes {
			if node.Number > 0 && node.HeadRefOid != "" {
				found[node.Number] = node.HeadRefOid
			}
		}

		if !page.PageInfo.HasNextPage {
			break
		}

		after = page.PageInfo.EndCursor
	}

	return found, nil
}

type githubSearchPage struct {
	PageInfo struct {
		HasNextPage bool   `json:"hasNextPage"`
		EndCursor   string `json:"endCursor"`
	} `json:"pageInfo"`
	Nodes []struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	} `json:"nodes"`
}

func (c *githubClient) searchPRs(ctx context.Context, query string, after any) (githubSearchPage, error) {
	reply, err := c.send(ctx, http.MethodPost, c.graphql, githubJSON, map[string]any{
		"query":     githubPRSearch,
		"variables": map[string]any{"q": query, "after": after},
	})
	if err != nil {
		return githubSearchPage{}, err
	}

	if reply.status != http.StatusOK {
		return githubSearchPage{}, githubFailure(http.MethodPost, "/graphql", reply)
	}

	var answer struct {
		Data struct {
			Search githubSearchPage `json:"search"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	err = json.Unmarshal(reply.body, &answer)
	if err != nil {
		return githubSearchPage{}, fmt.Errorf("github: search: %w", err)
	}

	// GraphQL answers 200 with errors beside, or instead of, data. Half an
	// answer is refused whole: reading it as the whole answer would quietly
	// drop pull requests from the version list.
	if len(answer.Errors) > 0 {
		messages := make([]string, 0, len(answer.Errors))
		for _, e := range answer.Errors {
			messages = append(messages, e.Message)
		}

		return githubSearchPage{}, fmt.Errorf("github: search %q: %s", query, strings.Join(messages, "; "))
	}

	return answer.Data.Search, nil
}

func githubPRsIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseGitHubPRsSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	number, sha := versionField(version, "number"), versionField(version, "sha")
	if number == "" || sha == "" {
		return fmt.Errorf("version %v names no pull request number and commit", version)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return err
	}

	return fetchPullRequest(ctx, client, source.Repo, number, sha, source.WantsTree(), destDir)
}

// fetchPullRequest lays down one pull request at one commit: the tree first,
// when asked for, then the metadata beside it — written so a repository file
// of the same name is a loud collision rather than a silent overwrite.
func fetchPullRequest(ctx context.Context, client *githubClient, repo, number, sha string, tree bool, destDir string) error {
	if tree {
		err := fetchTree(ctx, client, repo, sha, destDir)
		if err != nil {
			return err
		}
	}

	var pull struct {
		HTMLURL string `json:"html_url"`
		Base    struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}

	pullJSON, err := client.getJSON(ctx, "/repos/"+repo+"/pulls/"+number, &pull)
	if err != nil {
		return err
	}

	// The diff and the merge base both come from comparing the base BRANCH
	// with the version's commit, not from the pull request: the pull
	// request's own diff is of its CURRENT head, and a pinned or replayed
	// version must see the change as it was at that commit.
	compare := "/repos/" + repo + "/compare/" + escapeRef(pull.Base.Ref) + "..." + sha

	var comparison struct {
		MergeBase struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}

	_, err = client.getJSON(ctx, compare, &comparison)
	if err != nil {
		return err
	}

	diff, err := client.get(ctx, compare, githubDiff)
	if err != nil {
		return err
	}

	if diff.status != http.StatusOK {
		return githubFailure(http.MethodGet, compare, diff)
	}

	return writeMetadata(destDir, map[string][]byte{
		prJSONFile:      pullJSON,
		prDiffFile:      diff.body,
		prNumberFile:    []byte(number),
		prSHAFile:       []byte(sha),
		prMergeBaseFile: []byte(comparison.MergeBase.SHA),
		prURLFile:       []byte(pull.HTMLURL),
	})
}

// escapeRef keeps a branch's slashes, which the compare route reads as part
// of the ref, and escapes everything else in each segment.
func escapeRef(ref string) string {
	segments := strings.Split(ref, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}

	return strings.Join(segments, "/")
}

func fetchTree(ctx context.Context, client *githubClient, repo, sha string, destDir string) error {
	archive, cancel, err := client.download(ctx, "/repos/"+repo+"/tarball/"+url.PathEscape(sha))
	if err != nil {
		return err
	}

	defer cancel()
	defer func() { _ = archive.Close() }()

	return unpackTarball(archive, destDir)
}

func writeMetadata(destDir string, files map[string][]byte) error {
	root, err := os.OpenRoot(destDir)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	defer func() { _ = root.Close() }()

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("the tree already has a %s at its root, which is where this resource writes its own; set source.checkout: false and fetch the tree with a git resource instead", name)
		}

		if err != nil {
			return fmt.Errorf("%w", err)
		}

		_, err = file.Write(files[name])
		if err != nil {
			_ = file.Close()

			return fmt.Errorf("%w", err)
		}

		err = file.Close()
		if err != nil {
			return fmt.Errorf("%w", err)
		}
	}

	return nil
}

// versionField reads one field of a version as the string it was minted
// as. A version decoded from the store may carry a json.Number.
func versionField(version map[string]any, key string) string {
	value, ok := version[key]
	if !ok || value == nil {
		return ""
	}

	return fmt.Sprint(value)
}

// githubComment is the part of a comment every kind shares.
type githubComment struct {
	ID      int64  `json:"id"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Updated string `json:"updated_at"`
	User    struct {
		Login string `json:"login"`
	} `json:"user"`
	IssueURL       string `json:"issue_url"`
	PullRequestURL string `json:"pull_request_url"`
}

type commentWatch struct {
	source  config.GitHubCommentsSource
	pattern *regexp.Regexp
	author  string
	since   string
}

func githubCommentsCheck(ctx context.Context, raw, cursor map[string]any) ([]map[string]any, error) {
	watch, client, err := newCommentWatch(ctx, raw, cursor)
	if err != nil {
		return nil, err
	}

	kinds := watch.source.WatchedKinds()
	listings := map[string]bool{}

	// Conversation and issue comments share one listing; inline review
	// comments have their own.
	if slices.Contains(kinds, config.GitHubCommentConversation) || slices.Contains(kinds, config.GitHubCommentIssue) {
		listings["/repos/"+watch.source.Repo+"/issues/comments"] = false
	}

	if slices.Contains(kinds, config.GitHubCommentReview) {
		listings["/repos/"+watch.source.Repo+"/pulls/comments"] = true
	}

	var versions []map[string]any

	for listing, review := range listings {
		found, err := watch.scan(ctx, client, listing, review)
		if err != nil {
			return nil, err
		}

		versions = append(versions, found...)
	}

	sortCommentVersions(versions)

	return versions, nil
}

func newCommentWatch(ctx context.Context, raw, cursor map[string]any) (commentWatch, *githubClient, error) {
	source, pattern, err := config.ParseGitHubCommentsSource(raw)
	if err != nil {
		return commentWatch{}, nil, fmt.Errorf("%w", err)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return commentWatch{}, nil, err
	}

	watch := commentWatch{source: source, pattern: pattern, author: source.Author, since: versionField(cursor, "updated")}

	if watch.author == "@me" {
		watch.author, err = client.login(ctx)
		if err != nil {
			return commentWatch{}, nil, err
		}
	}

	return watch, client, nil
}

// sortCommentVersions orders oldest first by the moment each comment was
// last written, which is also what makes the newest version the cursor the
// next check asks since.
func sortCommentVersions(versions []map[string]any) {
	sort.SliceStable(versions, func(i, j int) bool {
		a, b := versionField(versions[i], "updated"), versionField(versions[j], "updated")
		if a != b {
			return a < b
		}

		return versionField(versions[i], "id") < versionField(versions[j], "id")
	})
}

// scan reads one comment listing newest first, back to the cursor.
//
// With no cursor it reads one page and stops: a first check is a look at
// what is recent, not a walk through a repository's whole history, and
// under steps web a cold start builds only the newest of it anyway. With
// one, since: asks GitHub for exactly what changed after it. Every page is a
// conditional request, and a listing nothing touched answers 304, which
// costs no rate limit — so an idle repository is polled for free.
func (w commentWatch) scan(ctx context.Context, client *githubClient, listing string, review bool) ([]map[string]any, error) {
	var versions []map[string]any

	query := url.Values{"sort": {"updated"}, "direction": {"desc"}, "per_page": {strconv.Itoa(githubPageSize)}}
	if w.since != "" {
		query.Set("since", w.since)
	}

	for page := 1; page <= githubPageLimit; page++ {
		query.Set("page", strconv.Itoa(page))

		var comments []githubComment

		_, err := client.getJSON(ctx, listing+"?"+query.Encode(), &comments)
		if err != nil {
			return nil, err
		}

		for _, comment := range comments {
			if version, ok := w.match(comment, review); ok {
				versions = append(versions, version)
			}
		}

		if len(comments) < githubPageSize || w.since == "" {
			return versions, nil
		}
	}

	slog.WarnContext(ctx, "resource.github.comments.truncated", "listing", listing, "since", w.since, "pages", githubPageLimit)

	return versions, nil
}

// match is one comment against the filter, and the version it becomes.
//
// updated is in the version on purpose, against the rule that a version
// carries identity only: here the text IS the payload, so a comment edited
// from one instruction to another is new work — and since: filters on the
// same field, which is what lets the version double as the cursor.
func (w commentWatch) match(comment githubComment, review bool) (map[string]any, bool) {
	kind := config.GitHubCommentReview
	parent := comment.PullRequestURL

	if !review {
		parent = comment.IssueURL
		kind = config.GitHubCommentIssue

		// An issue comment on a pull request is filed under the issue the
		// pull request is; only its page says which.
		if strings.Contains(comment.HTMLURL, "/pull/") {
			kind = config.GitHubCommentConversation
		}
	}

	if !slices.Contains(w.source.WatchedKinds(), kind) {
		return nil, false
	}

	if w.author != "" && !strings.EqualFold(comment.User.Login, w.author) {
		return nil, false
	}

	if !w.pattern.MatchString(comment.Body) {
		return nil, false
	}

	number := parent[strings.LastIndex(parent, "/")+1:]
	if comment.ID == 0 || number == "" || comment.Updated == "" {
		return nil, false
	}

	return map[string]any{
		"id":      strconv.FormatInt(comment.ID, 10),
		"kind":    kind,
		"number":  number,
		"updated": comment.Updated,
	}, true
}

func githubCommentsIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, _, err := config.ParseGitHubCommentsSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	id, kind, number := versionField(version, "id"), versionField(version, "kind"), versionField(version, "number")
	if id == "" || kind == "" || number == "" {
		return fmt.Errorf("version %v names no comment", version)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return err
	}

	files, err := fetchComment(ctx, client, source.Repo, id, kind)
	if err != nil {
		return err
	}

	if kind == config.GitHubCommentIssue {
		issueJSON, err := client.getJSON(ctx, "/repos/"+source.Repo+"/issues/"+number, nil)
		if err != nil {
			return err
		}

		files["issue.json"] = issueJSON
		files["issue.number"] = []byte(number)

		return writeMetadata(destDir, files)
	}

	err = fetchCommentedPR(ctx, client, source, number, destDir)
	if err != nil {
		return err
	}

	return writeMetadata(destDir, files)
}

// fetchComment is one comment, as the files a get writes for it.
func fetchComment(ctx context.Context, client *githubClient, repo, id, kind string) (map[string][]byte, error) {
	listing := "/repos/" + repo + "/issues/comments/"
	if kind == config.GitHubCommentReview {
		listing = "/repos/" + repo + "/pulls/comments/"
	}

	var comment githubComment

	commentJSON, err := client.getJSON(ctx, listing+url.PathEscape(id), &comment)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		"comment.json":   commentJSON,
		"comment.body":   []byte(comment.Body),
		"comment.id":     []byte(id),
		"comment.kind":   []byte(kind),
		"comment.author": []byte(comment.User.Login),
		"comment.url":    []byte(comment.HTMLURL),
	}, nil
}

// fetchCommentedPR lays down the pull request a comment sits on. The version
// names a comment, not a commit, so the pull request comes as it is now: a
// command written on it is about what it says today.
func fetchCommentedPR(ctx context.Context, client *githubClient, source config.GitHubCommentsSource, number, destDir string) error {
	var pull struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}

	_, err := client.getJSON(ctx, "/repos/"+source.Repo+"/pulls/"+number, &pull)
	if err != nil {
		return err
	}

	return fetchPullRequest(ctx, client, source.Repo, number, pull.Head.SHA, source.WantsTree(), destDir)
}

func githubCommentOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubPostParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	target, err := resolvePullRequest(params, inputs)
	if err != nil {
		return nil, err
	}

	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	path := "/repos/" + source.Repo + "/issues/" + target.number + "/comments"

	return postExpecting(ctx, client, path, map[string]any{"body": body}, http.StatusCreated, target.number)
}

func githubReviewOut(ctx context.Context, raw, rawParams map[string]any, inputs PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubReviewParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	target, err := resolvePullRequest(params.Post(), inputs)
	if err != nil {
		return nil, err
	}

	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	reviews := "/repos/" + source.Repo + "/pulls/" + target.number + "/reviews"
	payload := map[string]any{"body": body}

	// On the commit that was fetched, when the input says which: a review
	// lands on a commit, and the pull request may have moved since.
	if target.sha != "" {
		payload["commit_id"] = target.sha
	}

	if params.Event == "pending" {
		err = discardOwnPendingReview(ctx, client, reviews)
		if err != nil {
			return nil, err
		}
	} else {
		payload["event"] = strings.ToUpper(cmpOr(params.Event, "comment"))
	}

	return postExpecting(ctx, client, reviews, payload, http.StatusOK, target.number)
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// discardOwnPendingReview deletes the token user's own draft review, which
// GitHub allows one of per pull request — so a new push's draft replaces the
// last one rather than failing beside it. Anybody else's draft is theirs.
func discardOwnPendingReview(ctx context.Context, client *githubClient, reviews string) error {
	login, err := client.login(ctx)
	if err != nil {
		return err
	}

	for page := 1; page <= githubPageLimit; page++ {
		var listed []struct {
			ID    int64  `json:"id"`
			State string `json:"state"`
			User  struct {
				Login string `json:"login"`
			} `json:"user"`
		}

		_, err := client.getJSON(ctx, reviews+"?per_page=100&page="+strconv.Itoa(page), &listed)
		if err != nil {
			return err
		}

		for _, review := range listed {
			if review.State != "PENDING" || !strings.EqualFold(review.User.Login, login) {
				continue
			}

			path := reviews + "/" + strconv.FormatInt(review.ID, 10)

			reply, err := client.send(ctx, http.MethodDelete, client.api+path, githubJSON, nil)
			if err != nil {
				return err
			}

			if reply.status != http.StatusOK {
				return githubFailure(http.MethodDelete, path, reply)
			}
		}

		if len(listed) < githubPageSize {
			return nil
		}
	}

	return nil
}

func postExpecting(ctx context.Context, client *githubClient, path string, payload map[string]any, want int, number string) (map[string]any, error) {
	reply, err := client.send(ctx, http.MethodPost, client.api+path, githubJSON, payload)
	if err != nil {
		return nil, err
	}

	if reply.status != want {
		return nil, githubFailure(http.MethodPost, path, reply)
	}

	var created struct {
		ID int64 `json:"id"`
	}

	err = json.Unmarshal(reply.body, &created)
	if err != nil {
		return nil, fmt.Errorf("github: POST %s: %w", path, err)
	}

	return map[string]any{"id": strconv.FormatInt(created.ID, 10), "number": number}, nil
}

type pullRequestTarget struct {
	number string
	sha    string
}

// resolvePullRequest finds which pull request a put is about: params.number
// when it says, else the version of the input params.from names, else the
// one input whose version carries a number — which is every github-prs and
// github-comments get, so a put handed exactly one of them needs no params
// to say which.
func resolvePullRequest(params config.GitHubPostParams, inputs PutInputs) (pullRequestTarget, error) {
	if params.Number != "" {
		return pullRequestTarget{number: params.Number}, nil
	}

	if params.From != "" {
		version, ok := inputs.Versions[params.From]
		if !ok || versionField(version, "number") == "" {
			return pullRequestTarget{}, fmt.Errorf("params.from: %q is not an input of this put whose version names a pull request (inputs: %s)",
				params.From, strings.Join(inputs.Names, ", "))
		}

		return targetOf(version), nil
	}

	var carrying []string

	for name, version := range inputs.Versions {
		if versionField(version, "number") != "" {
			carrying = append(carrying, name)
		}
	}

	slices.Sort(carrying)

	switch len(carrying) {
	case 1:
		return targetOf(inputs.Versions[carrying[0]]), nil
	case 0:
		return pullRequestTarget{}, errors.New("no input names a pull request: give this put its github-prs or github-comments get as an input, or set params.number")
	default:
		return pullRequestTarget{}, fmt.Errorf("several inputs name a pull request (%s): set params.from to the one this posts on", strings.Join(carrying, ", "))
	}
}

// targetOf reads a version as a pull request. A comment's version names the
// pull request it sits on and no commit, which leaves a review on the head.
func targetOf(version map[string]any) pullRequestTarget {
	target := pullRequestTarget{number: versionField(version, "number")}

	if _, isComment := version["kind"]; !isComment {
		target.sha = versionField(version, "sha")
	}

	return target
}

// readPostBody reads the text to post from inside the put's own tree. An
// empty one is refused: posting nothing is how a step that wrote nothing
// would otherwise look like one that succeeded.
func readPostBody(srcDir, name string) (string, error) {
	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	defer func() { _ = root.Close() }()

	body, err := root.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("params.body_file: %w — is the step that writes it one of this put's inputs?", err)
	}

	if strings.TrimSpace(string(body)) == "" {
		return "", fmt.Errorf("params.body_file: %s is empty, so there is nothing to post", name)
	}

	return string(body), nil
}
