package config

import (
	"strings"
	"testing"
)

// TestGitHubPRLoadRules: a github-pr put's params are checked at load, and a get of one is refused like any publisher's.
func TestGitHubPRLoadRules(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ plan, want string }{
		"opens":                {plan: "\n  - put: pr\n    params: {head: feat, title_file: a, body_file: b, base: main, draft: true, labels: [bug], assignees: [alice]}", want: ""},
		"updates body only":    {plan: "\n  - put: pr\n    params: {head: feat, body_file: b}", want: ""},
		"marks ready only":     {plan: "\n  - put: pr\n    params: {head: feat, ready: true}", want: ""},
		"templated head":       {plan: "\n  - across:\n    - var: b\n      values: [a]\n    put: pr\n    params: {head: 'ticket/{{ .vars.b }}', title_file: a, body_file: b}", want: ""},
		"get of a pr":          {plan: "\n  - get: pr", want: "only publishes"},
		"no head":              {plan: "\n  - put: pr\n    params: {title_file: a, body_file: b}", want: "params.head is required"},
		"a fork's head":        {plan: "\n  - put: pr\n    params: {head: 'octocat:feat'}", want: "params.head"},
		"draft and ready":      {plan: "\n  - put: pr\n    params: {head: feat, draft: true, ready: true}", want: "contradict"},
		"draft not a bool":     {plan: "\n  - put: pr\n    params: {head: feat, draft: 'yes please'}", want: "into bool"},
		"ready not a bool":     {plan: "\n  - put: pr\n    params: {head: feat, ready: [1]}", want: "into bool"},
		"assignee @me":         {plan: "\n  - put: pr\n    params: {head: feat, assignees: ['@me']}", want: `params.assignees: "@me"`},
		"blank label":          {plan: "\n  - put: pr\n    params: {head: feat, labels: [' ']}", want: `params.labels: " "`},
		"unknown param":        {plan: "\n  - put: pr\n    params: {head: feat, title: x}", want: "title"},
		"reviewers not a name": {plan: "\n  - put: pr\n    params: {head: feat, reviewers: [alice]}", want: "reviewers"},
	} {
		_, err := LoadConfig(writeConfig(t, `
resources:
- name: pr
  type: github-pr
  source: {repo: acme/app}
jobs:
- name: build
  plan:`+tc.plan+`
`))

		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want it to load", name, err)
			}

			continue
		}

		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}
