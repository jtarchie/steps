package config

import (
	"strings"
	"testing"
)

// TestGitHubIssueLoadRules: a github-issue put's params are checked at load, and a get of one is refused like any publisher's.
func TestGitHubIssueLoadRules(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ plan, want string }{
		"loads":              {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, labels: [bug], assignees: [alice, 'dependabot[bot]']}", want: ""},
		"templated assignee": {plan: "\n  - across:\n    - var: who\n      values: [alice]\n    put: issue\n    params: {title_file: a, body_file: b, assignees: ['{{ .vars.who }}']}", want: ""},
		"get of an issue":    {plan: "\n  - get: issue", want: "only publishes"},
		"no title":           {plan: "\n  - put: issue\n    params: {body_file: b}", want: "params.title_file is required"},
		"no body":            {plan: "\n  - put: issue\n    params: {title_file: a}", want: "params.body_file is required"},
		"blank label":        {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, labels: [' ']}", want: `params.labels: " "`},
		"label not a list":   {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, labels: bug}", want: "into []string"},
		"assignee @me":       {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, assignees: ['@me']}", want: `params.assignees: "@me"`},
		"assignee a phrase":  {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, assignees: ['alice bob']}", want: `params.assignees: "alice bob"`},
		"unknown param":      {plan: "\n  - put: issue\n    params: {title_file: a, body_file: b, title: x}", want: "title"},
	} {
		_, err := LoadConfig(writeConfig(t, `
resources:
- name: issue
  type: github-issue
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
