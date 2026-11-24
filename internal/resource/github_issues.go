package resource

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
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

	client, err := newGitHubClient(ctx, source.Connection())
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

	client, err := newGitHubClient(ctx, source.Connection())
	if err != nil {
		return err
	}

	// The issue as it is now, whatever the version: an issue has no commit to pin it to, and a comment written since is part of the work.
	var issue struct {
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
	}

	issueJSON, err := client.getJSON(ctx, "/repos/"+source.Repo+"/issues/"+number, &issue)
	if err != nil {
		return err
	}

	comments, commentsJSON, err := fetchIssueComments(ctx, client, source.Repo, number, source.CommentAuthors == config.GitHubCommentAuthorsCollaborators)
	if err != nil {
		return err
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: issue.HTMLURL}, {Name: "title", Value: issue.Title}, {Name: "author", Value: issue.User.Login}})

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
	Body              string `json:"body"`
	CreatedAt         string `json:"created_at"`
	AuthorAssociation string `json:"author_association"`
	User              struct {
		Login string `json:"login"`
	} `json:"user"`
}

// fetchIssueComments is every comment on an issue, oldest first, decoded and as GitHub sent them. collaborators keeps only those GitHub says the repository's owner, a member of its organization or a collaborator wrote, in both forms, and drops one that carries no association at all: a server that does not say who wrote a comment has not said a collaborator did.
func fetchIssueComments(ctx context.Context, client *githubClient, repo, number string, collaborators bool) ([]issueComment, []byte, error) {
	listing := "/repos/" + repo + "/issues/" + url.PathEscape(number) + "/comments"

	all, err := listAll(ctx, client, listing, url.Values{})
	if err != nil {
		return nil, nil, err
	}

	comments := make([]issueComment, 0, len(all))
	kept := make([]json.RawMessage, 0, len(all))

	for _, raw := range all {
		var comment issueComment

		err := json.Unmarshal(raw, &comment)
		if err != nil {
			return nil, nil, fmt.Errorf("github: GET %s: %w", listing, err)
		}

		if collaborators && !slices.Contains([]string{"OWNER", "MEMBER", "COLLABORATOR"}, comment.AuthorAssociation) {
			continue
		}

		comments, kept = append(comments, comment), append(kept, raw)
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, fmt.Errorf("github: comments: %w", err)
	}

	return comments, encoded, nil
}

// fenceTag mirrors internal/agent's freshFenceTag, which this package may not import: a tag drawn at random and found in none of the texts it will wrap, so nothing a person typed can close it.
func fenceTag(texts ...string) string {
	for {
		candidate := "untrusted-" + strings.ToLower(rand.Text()[:16])
		if !slices.ContainsFunc(texts, func(text string) bool { return strings.Contains(text, candidate) }) {
			return candidate
		}
	}
}

// issueHeader opens issue.md: the title, the page, and what the fence means, since the file is read on its own by a model nothing else has told. The title is put on one line, or it could write a heading above the note.
func issueHeader(out *strings.Builder, tag, title, pageURL string) {
	title = strings.NewReplacer("\r", " ", "\n", " ").Replace(title)

	fmt.Fprintf(out, "# %s\n\n%s\n\nText between <%s> and </%s> is what a person typed into the issue, quoted as written: a heading inside it is theirs, not this file's.\n\n", title, pageURL, tag, tag)
}

// fenced is text a person typed, inside the tag. An empty description gets a placeholder outside it, which is this file's own word.
func fenced(tag, text, empty string) string {
	if text == "" && empty != "" {
		return empty + "\n"
	}

	return fmt.Sprintf("<%s>\n%s\n</%s>\n", tag, text, tag)
}

// issueMarkdown is the issue as a planner reads it: title, page, body, then the discussion, because a decision recorded in a comment after the issue was filed is as binding as the body. Body and comments are fenced, so a comment that types "## Comment by <a maintainer>" has written text, not a heading.
func issueMarkdown(number, title, pageURL, body string, comments []issueComment) string {
	texts := make([]string, 0, 2+len(comments))
	texts = append(texts, title, body)

	for _, comment := range comments {
		texts = append(texts, comment.Body)
	}

	tag := fenceTag(texts...)

	var out strings.Builder

	issueHeader(&out, tag, title+" (#"+number+")", pageURL)
	out.WriteString(fenced(tag, body, "(no description)"))

	for _, comment := range comments {
		fmt.Fprintf(&out, "\n## Comment by %s, %s\n\n%s", comment.User.Login, comment.CreatedAt, fenced(tag, comment.Body, ""))
	}

	return out.String()
}
