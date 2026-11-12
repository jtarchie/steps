package config

import (
	"errors"
	"strings"
)

// GitHubPRType opens a pull request for a pushed branch, or updates the one already open for it.
const GitHubPRType = "github-pr"

// GitHubPRParams is a github-pr put's params:.
type GitHubPRParams struct {
	// Head is the branch the pull request is from, already pushed to source.repo. A fork's owner:branch is refused: the lookup and the create both name it in this repository.
	Head string `yaml:"head"`
	// Base is the branch it merges into. Unset, a new pull request targets the repository's default branch; an open one's base is only checked against it, never changed.
	Base string `yaml:"base,omitempty"`
	// TitleFile is the title, a path inside the put's inputs, one line as for github-issue. Required when the put opens the pull request.
	TitleFile string `yaml:"title_file,omitempty"`
	// BodyFile is the description, a path inside the put's inputs. Required when the put opens the pull request.
	BodyFile string `yaml:"body_file,omitempty"`
	// Draft opens the pull request as a draft. An open one is not made a draft again.
	Draft bool `yaml:"draft,omitempty"`
	// Labels go on a pull request the put opens.
	Labels []string `yaml:"labels,omitempty"`
	// Assignees are the logins a pull request the put opens is assigned to.
	Assignees []string `yaml:"assignees,omitempty"`
	// Ready takes a draft pull request out of draft.
	Ready bool `yaml:"ready,omitempty"`
}

// ParseGitHubPRParams decodes a github-pr put's params:.
func ParseGitHubPRParams(raw map[string]any) (GitHubPRParams, error) {
	var params GitHubPRParams

	err := decodeParams(raw, &params)
	if err != nil {
		return params, err
	}

	switch {
	case params.Head == "":
		return params, errors.New("params.head is required: the branch, already pushed to source.repo, the pull request is from")
	case strings.Contains(params.Head, ":"):
		return params, errors.New("params.head: a fork's owner:branch is not supported; name a branch pushed to source.repo")
	case params.Draft && params.Ready:
		return params, errors.New("params.draft and params.ready contradict: one opens the pull request as a draft, the other takes a draft out of draft")
	}

	return params, validateIssueAudience(GitHubIssueParams{Labels: params.Labels, Assignees: params.Assignees})
}
