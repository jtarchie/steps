package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

// githubPR is the part of a pull request the put reads back, from the list that finds it and from the create or edit that answers.
type githubPR struct {
	Number int64  `json:"number"`
	NodeID string `json:"node_id"`
	Draft  bool   `json:"draft"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// githubPROut opens the pull request for a pushed branch, or updates the one already open for it. A head branch is an identity where an issue's title is not, so a retried put finds its own pull request rather than opening a second.
func githubPROut(ctx context.Context, raw, rawParams map[string]any, _ PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubPRParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	// Read before anything is sent: a file that cannot be read fails the put before a pull request is opened without it.
	text, err := readPRText(srcDir, params)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(ctx, source.Connection())
	if err != nil {
		return nil, err
	}

	pr, err := openOrUpdatePR(ctx, client, source.Repo, params, text)
	if err != nil {
		return nil, err
	}

	if params.Ready && pr.Draft {
		err = markReady(ctx, client, pr)
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{"number": strconv.FormatInt(pr.Number, 10), "sha": pr.Head.SHA}, nil
}

func openOrUpdatePR(ctx context.Context, client *githubClient, repo string, params config.GitHubPRParams, text prText) (githubPR, error) {
	existing, err := findOpenPR(ctx, client, repo, params.Head)
	if err != nil {
		return githubPR{}, err
	}

	if existing == nil {
		return openPR(ctx, client, repo, params, text)
	}

	return updatePR(ctx, client, repo, *existing, params, text)
}

// prText is the title and body files a put was given, each nil when it was not.
type prText struct {
	title, body *string
}

func readPRText(srcDir string, params config.GitHubPRParams) (prText, error) {
	var text prText

	if params.TitleFile != "" {
		title, err := readIssueTitle(srcDir, params.TitleFile)
		if err != nil {
			return text, err
		}

		text.title = &title
	}

	if params.BodyFile != "" {
		body, err := readPostBody(srcDir, params.BodyFile)
		if err != nil {
			return text, err
		}

		text.body = &body
	}

	return text, nil
}

// findOpenPR is the open pull request from head in repo, nil when there is none. GitHub allows one per head and base, so a head with several open ones is refused rather than guessed at.
func findOpenPR(ctx context.Context, client *githubClient, repo, head string) (*githubPR, error) {
	owner, _, _ := strings.Cut(repo, "/")
	path := "/repos/" + repo + "/pulls?state=open&per_page=100&head=" + url.QueryEscape(owner+":"+head)

	var listed []githubPR

	_, err := client.getJSON(ctx, path, &listed)
	if err != nil {
		return nil, err
	}

	var found []githubPR

	for _, pr := range listed {
		if pr.Head.Ref == head {
			found = append(found, pr)
		}
	}

	switch len(found) {
	case 0:
		return nil, nil //nolint:nilnil // no pull request is an answer, not a failure
	case 1:
		return &found[0], nil
	}

	numbers := make([]string, 0, len(found))
	for _, pr := range found {
		numbers = append(numbers, "#"+strconv.FormatInt(pr.Number, 10))
	}

	return nil, fmt.Errorf("params.head: %s has %d open pull requests (%s); which one to update is not for steps to guess", head, len(found), strings.Join(numbers, ", "))
}

func openPR(ctx context.Context, client *githubClient, repo string, params config.GitHubPRParams, text prText) (githubPR, error) {
	switch {
	case text.title == nil:
		return githubPR{}, fmt.Errorf("params.title_file is required: no pull request is open from %s, so the put opens one and needs its title", params.Head)
	case text.body == nil:
		return githubPR{}, fmt.Errorf("params.body_file is required: no pull request is open from %s, so the put opens one and needs its description", params.Head)
	}

	base := params.Base
	if base == "" {
		var repository struct {
			DefaultBranch string `json:"default_branch"`
		}

		_, err := client.getJSON(ctx, "/repos/"+repo, &repository)
		if err != nil {
			return githubPR{}, err
		}

		base = repository.DefaultBranch
	}

	pr, err := sendPR(ctx, client, http.MethodPost, "/repos/"+repo+"/pulls", http.StatusCreated, map[string]any{
		"title": *text.title, "head": params.Head, "base": base, "body": *text.body, "draft": params.Draft,
	})
	if err != nil {
		return pr, err
	}

	// A pull request is an issue to GitHub, which is where its labels and assignees live.
	issue := "/repos/" + repo + "/issues/" + strconv.FormatInt(pr.Number, 10)

	if len(params.Labels) > 0 {
		err = addLabels(ctx, client, issue+"/labels", params.Labels)
		if err != nil {
			return pr, err
		}
	}

	if len(params.Assignees) > 0 {
		err = assign(ctx, client, issue+"/assignees", params.Assignees)
	}

	return pr, err
}

func assign(ctx context.Context, client *githubClient, path string, logins []string) error {
	reply, err := client.send(ctx, http.MethodPost, client.api+path, githubJSON, map[string]any{"assignees": logins})
	if err != nil {
		return err
	}

	if reply.status != http.StatusCreated {
		return githubFailure(http.MethodPost, path, reply)
	}

	return nil
}

// updatePR rewrites an open pull request's title and body from whichever files were given. Its base is never changed: retargeting moves what a review was about, so a mismatch fails the put before anything is edited.
func updatePR(ctx context.Context, client *githubClient, repo string, pr githubPR, params config.GitHubPRParams, text prText) (githubPR, error) {
	if params.Base != "" && params.Base != pr.Base.Ref {
		return pr, fmt.Errorf("params.base: #%d merges into %s, not %s, and a put does not retarget a pull request", pr.Number, pr.Base.Ref, params.Base)
	}

	edit := map[string]any{}

	if text.title != nil {
		edit["title"] = *text.title
	}

	if text.body != nil {
		edit["body"] = *text.body
	}

	if len(edit) == 0 {
		return pr, nil
	}

	return sendPR(ctx, client, http.MethodPatch, "/repos/"+repo+"/pulls/"+strconv.FormatInt(pr.Number, 10), http.StatusOK, edit)
}

func sendPR(ctx context.Context, client *githubClient, method, path string, want int, payload map[string]any) (githubPR, error) {
	reply, err := client.send(ctx, method, client.api+path, githubJSON, payload)
	if err != nil {
		return githubPR{}, err
	}

	if reply.status != want {
		return githubPR{}, githubFailure(method, path, reply)
	}

	var pr githubPR

	err = json.Unmarshal(reply.body, &pr)
	if err != nil {
		return githubPR{}, fmt.Errorf("github: %s %s: %w", method, path, err)
	}

	return pr, nil
}

// markReady is GraphQL only: REST has no way to take a pull request out of draft.
func markReady(ctx context.Context, client *githubClient, pr githubPR) error {
	reply, err := client.send(ctx, http.MethodPost, client.graphql, githubJSON, map[string]any{
		"query":     `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { number isDraft } } }`,
		"variables": map[string]any{"id": pr.NodeID},
	})
	if err != nil {
		return err
	}

	if reply.status != http.StatusOK {
		return githubFailure(http.MethodPost, "/graphql", reply)
	}

	var answer struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	err = json.Unmarshal(reply.body, &answer)
	if err != nil {
		return fmt.Errorf("github: marking #%d ready: %w", pr.Number, err)
	}

	if len(answer.Errors) > 0 {
		return fmt.Errorf("github: marking #%d ready: %w", pr.Number, errors.New(answer.Errors[0].Message))
	}

	return nil
}
