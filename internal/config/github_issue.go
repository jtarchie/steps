package config

import (
	"errors"
	"fmt"
	"strings"
)

// GitHubIssueType opens one issue per put. A pipeline that opens several fans the put out with across:.
const GitHubIssueType = "github-issue"

// GitHubIssueParams is a github-issue put's params:.
type GitHubIssueParams struct {
	// TitleFile is the issue's title, a path inside the put's inputs. One line: a trailing newline is dropped, and a second line is refused rather than cut.
	TitleFile string `yaml:"title_file"`
	// BodyFile is the issue's text, a path inside the put's inputs.
	BodyFile string `yaml:"body_file"`
	// Labels go on the issue as it is opened. GitHub creates one that does not exist yet.
	Labels []string `yaml:"labels,omitempty"`
	// Assignees are the logins the issue is assigned to.
	Assignees []string `yaml:"assignees,omitempty"`
}

// ParseGitHubIssueParams decodes a github-issue put's params:.
func ParseGitHubIssueParams(raw map[string]any) (GitHubIssueParams, error) {
	var params GitHubIssueParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	switch {
	case params.TitleFile == "":
		return params, errors.New("params.title_file is required: the file, inside the put's inputs, whose one line is the issue's title")
	case params.BodyFile == "":
		return params, errors.New("params.body_file is required: the file, inside the put's inputs, whose text is the issue's body")
	}

	return params, validateIssueAudience(params)
}

func validateIssueAudience(params GitHubIssueParams) error {
	for _, label := range params.Labels {
		if strings.TrimSpace(label) == "" || strings.Contains(label, "\n") {
			return fmt.Errorf("params.labels: %q is not a label name", label)
		}
	}

	for _, login := range params.Assignees {
		// An across: cell renders its params after load; the put checks the login it rendered to.
		if strings.Contains(login, "{{") {
			continue
		}

		if login == "@me" || !githubLoginPattern.MatchString(login) {
			return fmt.Errorf("params.assignees: %q is not a GitHub login; GitHub assigns by login, and @me is not one here", login)
		}
	}

	return nil
}
