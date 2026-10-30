package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

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
	// Reactions is GitHub's count of each reaction on the comment, which
	// says whether one is there but not whose it is.
	Reactions map[string]any `json:"reactions"`
}

type commentWatch struct {
	source  config.GitHubCommentsSource
	pattern *regexp.Regexp
	authors []string
	since   string
	// self is the token's own user, resolved only when skip_reacted needs it.
	self string
}

func githubCommentsCheck(ctx context.Context, raw, cursor map[string]any) ([]map[string]any, error) {
	watch, client, err := newCommentWatch(ctx, raw, cursor)
	if err != nil {
		return nil, err
	}

	found, err := watch.snapshot(ctx, client)
	if err != nil {
		return nil, err
	}

	var versions []map[string]any

	labeled := map[string]bool{}

	for _, match := range found {
		wanted, err := watch.wanted(ctx, client, match, labeled)
		if err != nil {
			return nil, err
		}

		if wanted {
			versions = append(versions, match.version)
		}
	}

	sortCommentVersions(versions)

	// The overlap lists comments older than the cursor; when the cursor's own comment is no longer listed (deleted, reacted, edited away), the newest of them would be the next cursor, a step back the trigger reads as a change.
	if len(versions) > 0 && versionField(cursor, "updated") != "" && commentOlder(versions[len(versions)-1], cursor) {
		versions = append(versions, cursor)
	}

	return versions, nil
}

// wanted is the filters that cost a request, run after author and body so only a comment that would otherwise be work pays: labels before skip_reacted, because one read answers for every comment on its issue and a comment it refuses needs no reactions listed.
func (w commentWatch) wanted(ctx context.Context, client *githubClient, match commentMatch, labeled map[string]bool) (bool, error) {
	ok, err := w.onLabeled(ctx, client, match.version, labeled)
	if err != nil || !ok {
		return false, err
	}

	done, err := w.markedDone(ctx, client, match.comment, match.version)

	return !done, err
}

type commentMatch struct {
	comment githubComment
	version map[string]any
}

type commentFeed struct {
	path   string
	review bool
}

// listings is what the watched kinds are read from.
func (w commentWatch) listings() []commentFeed {
	kinds := w.source.WatchedKinds()

	var listings []commentFeed

	if slices.Contains(kinds, config.GitHubCommentConversation) || slices.Contains(kinds, config.GitHubCommentIssue) {
		listings = append(listings, commentFeed{"/repos/" + w.source.Repo + "/issues/comments", false})
	}

	if slices.Contains(kinds, config.GitHubCommentReview) {
		listings = append(listings, commentFeed{"/repos/" + w.source.Repo + "/pulls/comments", true})
	}

	return listings
}

// commentSnapshotRounds bounds how many times a check reads its listings
// looking for two rounds that agree.
const commentSnapshotRounds = 5

// snapshot is every listing's matches as they all stood at one instant.
//
// Conversation and issue comments share one listing; inline review comments
// have their own, and GitHub has no read of both at once. Read one after the
// other, a comment landing in the first after it was read can be older than
// one the second then shows — and the newest version is the next since:, so
// that comment would be behind the cursor for good. Two consecutive rounds
// that agree are a moment both listings held (the end of the first round sits
// inside every listing's unchanged interval). The repeat round is conditional
// requests, which an untouched listing answers without charging the rate
// limit.
func (w commentWatch) snapshot(ctx context.Context, client *githubClient) ([]commentMatch, error) {
	listings := w.listings()

	read := func() ([][]commentMatch, error) {
		round := make([][]commentMatch, 0, len(listings))

		for _, one := range listings {
			found, err := w.scan(ctx, client, one.path, one.review)
			if err != nil {
				return nil, err
			}

			round = append(round, found)
		}

		return round, nil
	}

	previous, err := read()
	if err != nil || len(listings) < 2 {
		return slices.Concat(previous...), err
	}

	for range commentSnapshotRounds {
		next, err := read()
		if err != nil {
			return nil, err
		}

		if slices.EqualFunc(previous, next, sameMatches) {
			return slices.Concat(next...), nil
		}

		previous = next
	}

	return nil, fmt.Errorf("github: comments kept changing across %d reads of %s; the next check tries again", commentSnapshotRounds, w.source.Repo)
}

func sameMatches(a, b []commentMatch) bool {
	return slices.EqualFunc(a, b, func(x, y commentMatch) bool {
		return maps.Equal(x.version, y.version)
	})
}

// commentLookback is how far behind the cursor a check asks: GitHub can list a comment after a newer one, and since: the cursor exactly would pass it for good. What the overlap re-reports, version history already holds.
const commentLookback = 5 * time.Minute

// lookBack is since: for a cursor, commentLookback earlier; a cursor that is no timestamp is asked as it stands.
func lookBack(updated string) string {
	at, err := time.Parse(time.RFC3339, updated)
	if err != nil {
		return updated
	}

	return at.Add(-commentLookback).UTC().Format(time.RFC3339)
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

	watch := commentWatch{source: source, pattern: pattern, authors: slices.Clone(source.Author), since: lookBack(versionField(cursor, "updated"))}
	me := slices.Index(watch.authors, "@me")

	if me >= 0 || source.SkipReacted != "" {
		watch.self, err = client.login(ctx)
		if err != nil {
			return commentWatch{}, nil, err
		}
	}

	if me >= 0 {
		watch.authors[me] = watch.self
	}

	return watch, client, nil
}

// sortCommentVersions orders oldest first by the moment each comment was
// last written, which is also what makes the newest version the cursor the
// next check asks since.
func sortCommentVersions(versions []map[string]any) {
	sort.SliceStable(versions, func(i, j int) bool { return commentOlder(versions[i], versions[j]) })
}

func commentOlder(a, b map[string]any) bool {
	at, bt := versionField(a, "updated"), versionField(b, "updated")
	if at != bt {
		return at < bt
	}

	return versionField(a, "id") < versionField(b, "id")
}

// scan reads one comment listing newest first, back to the cursor.
//
// With no cursor it reads one page and stops: a first check is a look at
// what is recent, not a walk through a repository's whole history, and
// under steps web a cold start builds only the newest of it anyway. With
// one, since: asks GitHub for what changed since commentLookback before it. Every page is a
// conditional request, and a listing nothing touched answers 304, which
// costs no rate limit — so an idle repository is polled for free.
func (w commentWatch) scan(ctx context.Context, client *githubClient, listing string, review bool) ([]commentMatch, error) {
	var matches []commentMatch

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
			version, ok := w.match(comment, review)
			if ok {
				matches = append(matches, commentMatch{comment, version})
			}
		}

		if len(comments) < githubPageSize || w.since == "" {
			return matches, nil
		}
	}

	slog.WarnContext(ctx, "resource.github.comments.truncated", "listing", listing, "since", w.since, "pages", githubPageLimit)

	return matches, nil
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

	if len(w.authors) > 0 && !slices.ContainsFunc(w.authors, func(author string) bool { return strings.EqualFold(comment.User.Login, author) }) {
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

// onLabeled reports whether the issue or pull request a comment sits on carries every filter label. The comment listings do not say, so its issue is read, once a check: GitHub's issue route answers for a pull request too, and seen keeps the answer for the next comment on it.
func (w commentWatch) onLabeled(ctx context.Context, client *githubClient, version map[string]any, seen map[string]bool) (bool, error) {
	if len(w.source.Labels) == 0 {
		return true, nil
	}

	number := versionNumber(version, "number")
	if number == "" {
		return false, fmt.Errorf("github: comment %s names no issue or pull request to read the labels of", versionField(version, "id"))
	}

	if ok, read := seen[number]; read {
		return ok, nil
	}

	var issue struct {
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}

	_, err := client.getJSON(ctx, "/repos/"+w.source.Repo+"/issues/"+number, &issue)
	if err != nil {
		return false, err
	}

	have := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		have = append(have, label.Name)
	}

	ok := !slices.ContainsFunc(w.source.Labels, func(want string) bool {
		return !slices.ContainsFunc(have, func(name string) bool { return strings.EqualFold(name, want) })
	})
	seen[number] = ok

	return ok, nil
}

// markedDone reports whether the token's own user left skip_reacted on a
// comment. The comment's own summary answers "nobody did" for free; only a
// count above zero is worth asking whose they are.
func (w commentWatch) markedDone(ctx context.Context, client *githubClient, comment githubComment, version map[string]any) (bool, error) {
	content := w.source.SkipReacted
	if content == "" {
		return false, nil
	}

	if comment.Reactions != nil {
		count, _ := comment.Reactions[content].(float64)
		if count == 0 {
			return false, nil
		}
	}

	path := commentPath(w.source.Repo, versionNumber(version, "id"), versionField(version, "kind")) + "/reactions"

	_, found, err := findOwnReaction(ctx, client, path, content, w.self)

	return found, err
}

// findOwnReaction is the id of login's content reaction among those path
// lists. Each page is a conditional request, so a comment polled again
// unchanged costs no rate limit.
func findOwnReaction(ctx context.Context, client *githubClient, path, content, login string) (int64, bool, error) {
	query := url.Values{"content": {content}, "per_page": {strconv.Itoa(githubPageSize)}}

	for page := 1; page <= githubPageLimit; page++ {
		query.Set("page", strconv.Itoa(page))

		var listed []struct {
			ID   int64 `json:"id"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}

		_, err := client.getJSON(ctx, path+"?"+query.Encode(), &listed)
		if err != nil {
			return 0, false, err
		}

		for _, reaction := range listed {
			if strings.EqualFold(reaction.User.Login, login) {
				return reaction.ID, true, nil
			}
		}

		if len(listed) < githubPageSize {
			return 0, false, nil
		}
	}

	return 0, false, nil
}

// commentPath is a comment's own route: conversation and issue comments are
// both issue comments to GitHub, and an inline one is a pull request's.
func commentPath(repo, id, kind string) string {
	listing := "/repos/" + repo + "/issues/comments/"
	if kind == config.GitHubCommentReview {
		listing = "/repos/" + repo + "/pulls/comments/"
	}

	return listing + url.PathEscape(id)
}

func githubCommentsIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, _, err := config.ParseGitHubCommentsSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	id, kind, number := versionNumber(version, "id"), versionField(version, "kind"), versionNumber(version, "number")
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

	err = fetchCommentedPR(ctx, client, source, number, kind, files, destDir)
	if err != nil {
		return err
	}

	return writeMetadata(destDir, files)
}

// reviewThreadLink is how a review comment names its thread: GitHub keeps
// one flat, every reply pointing at the first comment.
type reviewThreadLink struct {
	ID        int64 `json:"id"`
	InReplyTo int64 `json:"in_reply_to_id"`
}

// keep appends to thread the comments in this link's thread.
func (l reviewThreadLink) keep(thread, comments []json.RawMessage) ([]json.RawMessage, error) {
	top := l.ID
	if l.InReplyTo != 0 {
		top = l.InReplyTo
	}

	for _, raw := range comments {
		var link reviewThreadLink

		err := json.Unmarshal(raw, &link)
		if err != nil {
			return nil, fmt.Errorf("%w", err)
		}

		if link.ID == top || link.InReplyTo == top {
			thread = append(thread, raw)
		}
	}

	return thread, nil
}

// fetchThread is the review thread an inline comment sits in, first comment
// to last, the comment itself included. A command written as a reply says
// "this" about what the thread's first comment said, and the comment alone
// carries only that comment's id. GitHub has no listing of one thread, so
// the pull request's inline comments are read in creation order and kept
// when they are the first comment or point at it.
func fetchThread(ctx context.Context, client *githubClient, repo, number string, commentJSON []byte) ([]byte, error) {
	var self reviewThreadLink

	err := json.Unmarshal(commentJSON, &self)
	if err != nil {
		return nil, fmt.Errorf("github: comment: %w", err)
	}

	listing := "/repos/" + repo + "/pulls/" + url.PathEscape(number) + "/comments"
	query := url.Values{"sort": {"created"}, "direction": {"asc"}, "per_page": {strconv.Itoa(githubPageSize)}}
	thread := []json.RawMessage{}
	complete := false

	for page := 1; page <= githubPageLimit && !complete; page++ {
		query.Set("page", strconv.Itoa(page))

		var comments []json.RawMessage

		_, err := client.getJSON(ctx, listing+"?"+query.Encode(), &comments)
		if err != nil {
			return nil, err
		}

		thread, err = self.keep(thread, comments)
		if err != nil {
			return nil, fmt.Errorf("github: GET %s: %w", listing, err)
		}

		complete = len(comments) < githubPageSize
	}

	if !complete {
		slog.WarnContext(ctx, "resource.github.thread.truncated", "listing", listing, "pages", githubPageLimit)
	}

	encoded, err := json.Marshal(thread)
	if err != nil {
		return nil, fmt.Errorf("github: thread: %w", err)
	}

	return encoded, nil
}

// fetchComment is one comment, as the files a get writes for it.
func fetchComment(ctx context.Context, client *githubClient, repo, id, kind string) (map[string][]byte, error) {
	var comment githubComment

	commentJSON, err := client.getJSON(ctx, commentPath(repo, id, kind), &comment)
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
// command written on it is about what it says today. An inline comment's
// thread is added to files.
func fetchCommentedPR(ctx context.Context, client *githubClient, source config.GitHubCommentsSource, number, kind string, files map[string][]byte, destDir string) error {
	if kind == config.GitHubCommentReview {
		thread, err := fetchThread(ctx, client, source.Repo, number, files["comment.json"])
		if err != nil {
			return err
		}

		files["thread.json"] = thread
	}

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
