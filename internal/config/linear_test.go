package config

import (
	"strings"
	"testing"
)

func linearPipeline(resources, plan string) string {
	return `
resources:
- name: ticket
  type: linear-issues
  source: {team: ENG` + resources + `}
- name: board
  type: linear-update
  source: {}
- name: reply
  type: linear-comment
  source: {}
jobs:
- name: build
  plan:` + plan + `
`
}

const linearHappyPlan = `
  - get: ticket
  - task: write
    outputs: [out]
    run: echo hi > out/body.md
  - put: reply
    inputs: [ticket, out]
    params: {body_file: out/body.md}
  - put: board
    inputs: [ticket]
    params: {state: Done}`

func TestLinearTypesAreBuiltIn(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, linearPipeline("", linearHappyPlan)))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	for _, name := range LinearTypes() {
		resourceType, err := cfg.FindResourceType(name)
		if err != nil {
			t.Fatal(err)
		}

		if resourceType.Config.Backend() != BackendLinear || resourceType.Config.Linear != name {
			t.Errorf("%s: backend %s, kind %q", name, resourceType.Config.Backend(), resourceType.Config.Linear)
		}
	}
}

// TestLinearLoadRules: everything wrong with a linear-* resource or put that can be known without a request is a load error naming it.
func TestLinearLoadRules(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ extra, plan, want string }{
		"a valid pipeline loads":    {plan: linearHappyPlan},
		"misspelled key":            {extra: ", stats: [Todo]", plan: linearHappyPlan, want: "stats"},
		"states with state_types":   {extra: ", states: [Todo], state_types: [started]", plan: linearHappyPlan, want: "name one"},
		"unknown state type":        {extra: ", state_types: [doing]", plan: linearHappyPlan, want: `"doing" is not a Linear state type`},
		"blank state":               {extra: `, states: [""]`, plan: linearHappyPlan, want: "source.states: a blank name"},
		"blank label":               {extra: `, labels: [" "]`, plan: linearHappyPlan, want: "source.labels: a blank name"},
		"assignee by email":         {extra: ", assignee: a@b.co", plan: linearHappyPlan, want: "not @me or a user id"},
		"key as a value":            {extra: ", token_env: lin_api_abc=1", plan: linearHappyPlan, want: "NAME of an environment variable"},
		"cleartext endpoint":        {extra: ", endpoint: 'http://linear.example/graphql'", plan: linearHappyPlan, want: "loopback"},
		"loopback http endpoint":    {extra: ", endpoint: 'http://127.0.0.1:9/graphql'", plan: linearHappyPlan},
		"put to the finder":         {plan: "\n  - put: ticket", want: "only finds work"},
		"get of a publisher":        {plan: "\n  - get: reply", want: "only publishes"},
		"comment with no body":      {plan: "\n  - get: ticket\n  - put: reply\n    inputs: [ticket]\n    params: {}", want: "params.body_file is required"},
		"update that does nothing":  {plan: "\n  - get: ticket\n  - put: board\n    inputs: [ticket]\n    params: {}", want: "changes nothing"},
		"label added and removed":   {plan: "\n  - get: ticket\n  - put: board\n    inputs: [ticket]\n    params: {add_labels: [a], remove_labels: [a]}", want: `both name "a"`},
		"blank label to add":        {plan: "\n  - get: ticket\n  - put: board\n    inputs: [ticket]\n    params: {add_labels: [\"\"]}", want: "params.add_labels: a blank label"},
		"from with issue":           {plan: "\n  - get: ticket\n  - put: board\n    inputs: [ticket]\n    params: {state: Done, from: ticket, issue: ENG-1}", want: "name one"},
		"issue not an identifier":   {plan: "\n  - put: board\n    params: {state: Done, issue: 'eng 1'}", want: "not an issue identifier"},
		"a rendered issue loads":    {plan: "\n  - task: write\n    outputs: [out]\n    run: echo ENG-1 > out/id\n  - load_var: issue\n    inputs: [out]\n    file: out/id\n  - put: board\n    params: {state: Done, issue: '((.:issue))'}"},
		"no get names the issue":    {plan: "\n  - put: board\n    params: {state: Done}", want: "no linear-issues or linear-issue get among its inputs"},
		"issue stands in for a get": {plan: "\n  - put: board\n    params: {state: Done, issue: ENG-1}"},
		"from a get not in inputs":  {plan: "\n  - get: ticket\n  - put: board\n    inputs: [ticket]\n    params: {state: Done, from: elsewhere}", want: "not a linear-issues or linear-issue get among this put's inputs"},
		"get takes no params":       {plan: "\n  - get: ticket\n    params: {depth: 1}", want: "linear-issues get takes no params"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, linearPipeline(tc.extra, tc.plan)))

			switch {
			case tc.want == "" && err != nil:
				t.Errorf("LoadConfig: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("LoadConfig = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

func TestLinearTeamIsRequired(t *testing.T) {
	t.Parallel()

	_, err := ParseLinearIssuesSource(map[string]any{"states": []any{"Todo"}})
	if err == nil || !strings.Contains(err.Error(), "source.team is required") {
		t.Fatalf("ParseLinearIssuesSource = %v, want it to require the team", err)
	}
}

func TestLinearEnvOnAResourceIsRefused(t *testing.T) {
	t.Parallel()

	pipeline := strings.Replace(linearPipeline("", linearHappyPlan), "  source: {team: ENG}", "  env: [LINEAR_API_KEY]\n  source: {team: ENG}", 1)

	_, err := LoadConfig(writeConfig(t, pipeline))
	if err == nil || !strings.Contains(err.Error(), "env: has no effect on a linear-issues resource") {
		t.Fatalf("LoadConfig = %v, want env: refused", err)
	}
}

// TestLinearBlankNamesAreReportedInFieldOrder: with two fields wrong, the error names the first every time, not whichever a map yields.
func TestLinearBlankNamesAreReportedInFieldOrder(t *testing.T) {
	t.Parallel()

	for range 20 {
		_, err := ParseLinearIssuesSource(map[string]any{"team": "ENG", "states": []any{""}, "labels": []any{""}})
		if err == nil || !strings.Contains(err.Error(), "source.states") {
			t.Fatalf("ParseLinearIssuesSource = %v, want source.states named first", err)
		}

		_, err = ParseLinearUpdateParams(map[string]any{"add_labels": []any{""}, "remove_labels": []any{""}})
		if err == nil || !strings.Contains(err.Error(), "params.add_labels") {
			t.Fatalf("ParseLinearUpdateParams = %v, want params.add_labels named first", err)
		}
	}
}

func linearIssuePipeline(source, plan string) string {
	return `
resources:
- name: ticket
  type: linear-issue
  source: {` + source + `}
- name: reply
  type: linear-comment
  source: {}
jobs:
- name: build
  plan:
  - task: write
    outputs: [out]
    run: echo hi > out/title; echo hi > out/body.md` + plan + `
`
}

const linearIssueHappyPlan = `
  - put: ticket
    inputs: [out]
    params: {title_file: out/title, body_file: out/body.md}`

// TestLinearIssueLoadRules: what is wrong with a linear-issue source or put and needs no request is a load error naming the field.
func TestLinearIssueLoadRules(t *testing.T) {
	t.Parallel()

	put := func(params string) string {
		return "\n  - put: ticket\n    inputs: [out]\n    params: {title_file: out/title, body_file: out/body.md" + params + "}"
	}

	for name, tc := range map[string]struct{ source, plan, want string }{
		"a valid pipeline loads": {source: "team: ENG", plan: linearIssueHappyPlan},
		"every field loads": {source: "team: ENG, token_env: LINEAR_API_KEY, endpoint: 'https://api.linear.app/graphql'",
			plan: put(", state: Started, labels: [Dev Investment, Chore], assignee: '@me', estimate: 0, project: Platform, parent: ENG-7, reuse: title")},
		"a user id assignee loads":          {source: "team: ENG", plan: put(", assignee: 4f1c-9a")},
		"a rendered parent loads":           {source: "team: ENG", plan: "\n  - load_var: epic\n    inputs: [out]\n    file: out/title" + put(", parent: '((.:epic))'")},
		"no team":                           {source: "", plan: linearIssueHappyPlan, want: "source.team is required"},
		"a filter in the source":            {source: "team: ENG, states: [Todo]", plan: linearIssueHappyPlan, want: `unknown key "states"`},
		"key as a value":                    {source: "team: ENG, token_env: lin_api_abc=1", plan: linearIssueHappyPlan, want: "NAME of an environment variable"},
		"no title file":                     {source: "team: ENG", plan: "\n  - put: ticket\n    inputs: [out]\n    params: {body_file: out/body.md}", want: "params.title_file is required"},
		"no body file":                      {source: "team: ENG", plan: "\n  - put: ticket\n    inputs: [out]\n    params: {title_file: out/title}", want: "params.body_file is required"},
		"blank label":                       {source: "team: ENG", plan: put(`, labels: [""]`), want: "params.labels: a blank label name"},
		"assignee by email":                 {source: "team: ENG", plan: put(", assignee: a@b.co"), want: "params.assignee: \"a@b.co\" is not @me or a user id"},
		"negative estimate":                 {source: "team: ENG", plan: put(", estimate: -1"), want: "params.estimate: -1 is not an estimate"},
		"parent not an identifier":          {source: "team: ENG", plan: put(", parent: 'eng 7'"), want: "params.parent: \"eng 7\" is not an issue identifier"},
		"reuse by something else":           {source: "team: ENG", plan: put(", reuse: body"), want: `params.reuse: "body" is not a way to find an existing issue`},
		"misspelled param":                  {source: "team: ENG", plan: put(", lables: [bug]"), want: `unknown key "lables"`},
		"get with no put before it":         {source: "team: ENG", plan: "\n  - get: ticket", want: "only publishes"},
		"get after the put loads":           {source: "team: ENG", plan: linearIssueHappyPlan + "\n  - get: ticket"},
		"get after the put takes no params": {source: "team: ENG", plan: linearIssueHappyPlan + "\n  - get: ticket\n    params: {depth: 1}", want: "linear-issue get takes no params"},
		"comment names the issue from the get": {source: "team: ENG",
			plan: linearIssueHappyPlan + "\n  - get: ticket\n  - put: reply\n    inputs: [ticket, out]\n    params: {body_file: out/body.md}"},
		"comment with no get": {source: "team: ENG",
			plan: linearIssueHappyPlan + "\n  - put: reply\n    inputs: [out]\n    params: {body_file: out/body.md}", want: "no linear-issues or linear-issue get among its inputs"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, linearIssuePipeline(tc.source, tc.plan)))

			switch {
			case tc.want == "" && err != nil:
				t.Errorf("LoadConfig: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("LoadConfig = %v, want it to say %q", err, tc.want)
			}
		})
	}
}
