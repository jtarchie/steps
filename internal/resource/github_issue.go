package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
)

// githubIssueOut opens one issue. Nothing dedupes: a retried put opens a second one, as a retried github-pr-comment posts twice, so a pipeline that must not guards it before the put.
func githubIssueOut(ctx context.Context, raw, rawParams map[string]any, _ PutInputs, srcDir string) (map[string]any, error) {
	source, err := config.ParseGitHubPostSource(raw)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	params, err := config.ParseGitHubIssueParams(rawParams)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	payload, err := issuePayload(srcDir, params)
	if err != nil {
		return nil, err
	}

	client, err := newGitHubClient(source.Connection())
	if err != nil {
		return nil, err
	}

	path := "/repos/" + source.Repo + "/issues"

	reply, err := client.send(ctx, http.MethodPost, client.api+path, githubJSON, payload)
	if err != nil {
		return nil, err
	}

	if reply.status != http.StatusCreated {
		return nil, githubFailure(http.MethodPost, path, reply)
	}

	var created struct {
		ID     int64 `json:"id"`
		Number int64 `json:"number"`
	}

	err = json.Unmarshal(reply.body, &created)
	if err != nil {
		return nil, fmt.Errorf("github: POST %s: %w", path, err)
	}

	return map[string]any{"id": strconv.FormatInt(created.ID, 10), "number": strconv.FormatInt(created.Number, 10)}, nil
}

func issuePayload(srcDir string, params config.GitHubIssueParams) (map[string]any, error) {
	title, err := readIssueTitle(srcDir, params.TitleFile)
	if err != nil {
		return nil, err
	}

	body, err := readPostBody(srcDir, params.BodyFile)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{"title": title, "body": body}

	if len(params.Labels) > 0 {
		payload["labels"] = params.Labels
	}

	if len(params.Assignees) > 0 {
		payload["assignees"] = params.Assignees
	}

	return payload, nil
}

// readIssueTitle reads a title from inside the put's own tree. A second line is refused rather than cut: a model that wrote the body into the title file should fail by name, not open an issue titled with its first sentence.
func readIssueTitle(srcDir, name string) (string, error) {
	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	defer func() { _ = root.Close() }()

	raw, err := root.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("params.title_file: %w — is the step that writes it one of this put's inputs?", err)
	}

	title := strings.TrimRight(string(raw), "\r\n")

	switch {
	case strings.TrimSpace(title) == "":
		return "", fmt.Errorf("params.title_file: %s is empty, so the issue would have no title", name)
	case strings.ContainsAny(title, "\r\n"):
		return "", fmt.Errorf("params.title_file: %s is %d lines; a title is one line, so put the rest in params.body_file", name, strings.Count(title, "\n")+1)
	}

	return title, nil
}
