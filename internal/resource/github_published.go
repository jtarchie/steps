package resource

// The in: of the publishers whose put leaves something to read back: a get
// after a github-pr or github-issue put fetches the pull request or issue that
// put recorded, as files a later step reads.

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

// prTitleFile is the pull request's title, beside pr.url, for the step after a github-pr put.
const prTitleFile = "pr.title"

// githubPRIn writes the pull request a github-pr put recorded: one GET, since
// the version carries the number and head commit but not the url or title.
func githubPRIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	number := versionNumber(version, "number")
	if number == "" {
		return fmt.Errorf("version %v names no pull request number", version)
	}

	client, err := newGitHubClient(ctx, source.Connection())
	if err != nil {
		return err
	}

	var pr githubPR

	body, err := client.getJSON(ctx, "/repos/"+source.Repo+"/pulls/"+number, &pr)
	if err != nil {
		return err
	}

	// The head the put recorded, which a push since may have moved past.
	sha := versionField(version, "sha")
	if sha == "" {
		sha = pr.Head.SHA
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: pr.HTMLURL}, {Name: "title", Value: pr.Title}})

	return writeMetadata(destDir, map[string][]byte{
		prJSONFile:   body,
		prNumberFile: []byte(number),
		prSHAFile:    []byte(sha),
		prURLFile:    []byte(pr.HTMLURL),
		prTitleFile:  []byte(pr.Title),
	})
}

// githubIssueIn writes the issue a github-issue put opened: one GET, as for a
// pull request.
func githubIssueIn(ctx context.Context, raw, version map[string]any, destDir string) error {
	source, err := config.ParseGitHubPostSource(raw)
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

	var issue struct {
		Title   string `json:"title"`
		HTMLURL string `json:"html_url"`
	}

	body, err := client.getJSON(ctx, "/repos/"+source.Repo+"/issues/"+number, &issue)
	if err != nil {
		return err
	}

	reportFacts(ctx, []events.Fact{{Name: "url", Value: issue.HTMLURL}, {Name: "title", Value: issue.Title}})

	return writeMetadata(destDir, map[string][]byte{
		issueJSONFile:   body,
		issueNumberFile: []byte(number),
		issueTitleFile:  []byte(issue.Title),
		issueURLFile:    []byte(issue.HTMLURL),
	})
}
