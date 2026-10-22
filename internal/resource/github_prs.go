package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

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
