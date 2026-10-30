package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

// The one query github-issues sends: open issues by search, as github-prs finds pull requests, and with each its label events, so the version can say when a filter label last went on without a request per issue.
const githubIssueSearch = `query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on Issue { number timelineItems(itemTypes: [LABELED_EVENT], last: 100) { nodes { ... on LabeledEvent { createdAt label { name } } } } } }
  }
}`

// The files a github-issues get writes.
const (
	issueJSONFile     = "issue.json"
	issueNumberFile   = "issue.number"
	issueTitleFile    = "issue.title"
	issueURLFile      = "issue.url"
	issueCommentsFile = "issue.comments.json"
	issueMarkdownFile = "issue.md"
)

// githubIssuesQuery is the search string for a source. is:issue is what keeps pull requests out: GitHub's search files both as issues.
func githubIssuesQuery(source config.GitHubIssuesSource) string {
	return githubSearchQuery("repo:"+source.Repo+" is:issue is:open", map[string]string{"author": source.Author, "assignee": source.Assignee}, source.Labels)
}

func githubIssuesCheck(ctx context.Context, raw, _ map[string]any) ([]map[string]any, error) {
	source, err := config.ParseGitHubIssuesSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	nodes, err := client.searchAll(ctx, githubIssueSearch, githubIssuesQuery(source))
	if err != nil {
		return nil, err
	}

	found := map[int]githubSearchNode{}

	for _, node := range nodes {
		if node.Number > 0 {
			found[node.Number] = node
		}
	}

	numbers := make([]int, 0, len(found))
	for number := range found {
		numbers = append(numbers, number)
	}

	// Oldest first, by number, as github-prs reports: a relabel mints {number, new labeled_at}, which history records as newly seen.
	slices.Sort(numbers)

	versions := make([]map[string]any, 0, len(numbers))

	for _, number := range numbers {
		version := map[string]any{"number": strconv.Itoa(number)}
		if len(source.Labels) > 0 {
			version["labeled_at"] = labeledAt(found[number].TimelineItems.Nodes, source.Labels)
		}

		versions = append(versions, version)
	}

	return versions, nil
}

// labeledAt is when a filter label last went on: the latest of their LabeledEvents, matched without regard to case as GitHub matches label names. Other labels are left out so a pipeline's own state labels mint no version.
func labeledAt(events []githubLabeledEvent, filter []string) string {
	latest := "" // ponytail: the search reads the last 100 label events, so an issue labeled more often than that since its filter label went on comes back "", a new version once; page timelineItems backwards with before: if that ever bites.

	for _, event := range events {
		named := slices.ContainsFunc(filter, func(label string) bool { return strings.EqualFold(label, event.Label.Name) })

		// GitHub writes every createdAt as UTC to the second, so the strings sort as the moments do.
		if named && event.CreatedAt > latest {
			latest = event.CreatedAt
		}
	}

	return latest
}

func githubIssuesIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseGitHubIssuesSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	number := versionNumber(version, "number")
	if number == "" {
		return fmt.Errorf("version %v names no issue number", version)
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return err
	}

	// The issue as it is now, whatever the version: an issue has no commit to pin it to, and a comment written since is part of the work.
	var issue struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}

	issueJSON, err := client.getJSON(ctx, "/repos/"+source.Repo+"/issues/"+number, &issue)
	if err != nil {
		return err
	}

	comments, commentsJSON, err := fetchIssueComments(ctx, client, source.Repo, number)
	if err != nil {
		return err
	}

	return writeMetadata(destDir, map[string][]byte{
		issueJSONFile:     issueJSON,
		issueNumberFile:   []byte(number),
		issueTitleFile:    []byte(issue.Title),
		issueURLFile:      []byte(issue.HTMLURL),
		issueCommentsFile: commentsJSON,
		issueMarkdownFile: []byte(issueMarkdown(number, issue.Title, issue.HTMLURL, issue.Body, comments)),
	})
}

type issueComment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

// fetchIssueComments is every comment on an issue, oldest first, decoded and as GitHub sent them.
func fetchIssueComments(ctx context.Context, client *githubClient, repo, number string) ([]issueComment, []byte, error) {
	listing := "/repos/" + repo + "/issues/" + url.PathEscape(number) + "/comments"
	query := url.Values{"per_page": {strconv.Itoa(githubPageSize)}}
	all := []json.RawMessage{}
	complete := false

	for page := 1; page <= githubPageLimit && !complete; page++ {
		query.Set("page", strconv.Itoa(page))

		var listed []json.RawMessage

		_, err := client.getJSON(ctx, listing+"?"+query.Encode(), &listed)
		if err != nil {
			return nil, nil, err
		}

		all = append(all, listed...)
		complete = len(listed) < githubPageSize
	}

	if !complete {
		slog.WarnContext(ctx, "resource.github.issue_comments.truncated", "listing", listing, "pages", githubPageLimit)
	}

	comments := make([]issueComment, len(all))

	for i, raw := range all {
		err := json.Unmarshal(raw, &comments[i])
		if err != nil {
			return nil, nil, fmt.Errorf("github: GET %s: %w", listing, err)
		}
	}

	encoded, err := json.Marshal(all)
	if err != nil {
		return nil, nil, fmt.Errorf("github: comments: %w", err)
	}

	return comments, encoded, nil
}

// issueMarkdown is the issue as a planner reads it: title, page, body, then the discussion, because a decision recorded in a comment after the issue was filed is as binding as the body.
func issueMarkdown(number, title, pageURL, body string, comments []issueComment) string {
	if body == "" {
		body = "(no description)"
	}

	var out strings.Builder

	fmt.Fprintf(&out, "# %s (#%s)\n\n%s\n\n%s\n", title, number, pageURL, body)

	for _, comment := range comments {
		fmt.Fprintf(&out, "\n## Comment by %s, %s\n\n%s\n", comment.User.Login, comment.CreatedAt, comment.Body)
	}

	return out.String()
}
