package config

import (
	"strings"
	"testing"
)

// githubPipeline is a pipeline with one github-prs resource, one of each
// publishing type, and a job that gets and puts; extra is spliced into the
// pull request resource's source:, and plan replaces the job's plan when set.
func githubPipeline(extra, plan string) string {
	if plan == "" {
		plan = `
  - get: pr
  - task: write
    outputs: [out]
    run: echo hi > out/body.md
  - put: comment
    inputs: [pr, out]
    params: {body_file: out/body.md}`
	}

	return `
resources:
- name: pr
  type: github-prs
  source:
    repo: acme/app` + extra + `
- name: comment
  type: github-pr-comment
  source: {repo: acme/app}
- name: review
  type: github-pr-review
  source: {repo: acme/app}
- name: said
  type: github-comments
  source: {repo: acme/app}
jobs:
- name: build
  plan:` + plan + `
`
}

func TestGitHubTypesAreBuiltIn(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, githubPipeline("", "")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	for _, name := range GitHubTypes() {
		resourceType, err := cfg.FindResourceType(name)
		if err != nil {
			t.Fatal(err)
		}

		if resourceType.Config.Backend() != BackendGitHub || resourceType.Config.GitHub != name {
			t.Errorf("%s: backend %s, kind %q", name, resourceType.Config.Backend(), resourceType.Config.GitHub)
		}
	}
}

// TestGitHubLoadRules: everything wrong with a github-* resource that can be
// known without a request is a load error naming it.
func TestGitHubLoadRules(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ extra, plan, want string }{
		"misspelled key":          {extra: "\n    auhtor: alice", want: "auhtor"},
		"a valid filter loads":    {extra: "\n    base: main", plan: "\n  - get: said", want: ""},
		"login with a space":      {extra: "\n    author: 'alice repo:other/repo'", want: `source.author: "alice repo:other/repo"`},
		"reviewer with a space":   {extra: "\n    review_requested: 'a b'", want: "source.review_requested"},
		"label with a quote":      {extra: "\n    labels: ['say \"hi\"']", want: "source.labels"},
		"base with a space":       {extra: "\n    base: 'main is:closed'", want: "source.base"},
		"token as a value":        {extra: "\n    token_env: ghp_abc=123", want: "NAME of an environment variable"},
		"endpoint not a url":      {extra: "\n    endpoint: api.github.com", want: "source.endpoint"},
		"get with params":         {plan: "\n  - get: pr\n    params: {depth: 1}", want: "takes no params"},
		"put to a find":           {plan: "\n  - put: pr", want: "only finds work"},
		"put to comments":         {plan: "\n  - put: said", want: "only finds work"},
		"get of a post":           {plan: "\n  - get: comment", want: "only publishes"},
		"put without a body":      {plan: "\n  - put: comment\n    params: {}", want: "params.body_file is required"},
		"put with both targets":   {plan: "\n  - put: comment\n    params: {body_file: a, from: pr, number: '1'}", want: "name one"},
		"put with unknown params": {plan: "\n  - put: comment\n    params: {body_file: a, bodyfile: b}", want: "bodyfile"},
		"review with a bad event": {plan: "\n  - put: review\n    params: {body_file: a, event: merge}", want: `params.event: "merge"`},
	} {
		_, err := LoadConfig(writeConfig(t, githubPipeline(tc.extra, tc.plan)))

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

func TestGitHubCommentsLoadRules(t *testing.T) {
	t.Parallel()

	pipeline := func(source string) string {
		return `
resources:
- name: said
  type: github-comments
  source:
    repo: acme/app
` + source + `
jobs:
- name: build
  plan:
  - get: said
`
	}

	for name, tc := range map[string]struct{ source, want string }{
		"bad pattern":               {"    body: '(unclosed'", "source.body"},
		"unknown kind":              {"    kinds: [commit]", `source.kinds: "commit"`},
		"bad author":                {"    author: 'a b'", "source.author"},
		"enterprise endpoint loads": {"    endpoint: https://x.example/api/v3", ""},
	} {
		_, err := LoadConfig(writeConfig(t, pipeline(tc.source)))

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

func TestGitHubResourceRefusals(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ yaml, want string }{
		"repo not owner/name": {`
resources:
- name: pr
  type: github-prs
  source: {repo: app}
jobs:
- name: build
  plan: [{get: pr}]
`, "source.repo must be owner/name"},
		"env on the resource": {`
resources:
- name: pr
  type: github-prs
  env: [OTHER_TOKEN]
  source: {repo: acme/app}
jobs:
- name: build
  plan: [{get: pr}]
`, "env: has no effect"},
		"a type taking the name": {`
resource_types:
- name: github-prs
  config: {check: "echo []"}
resources:
- name: pr
  type: github-prs
  source: {repo: acme/app}
jobs:
- name: build
  plan: [{get: pr}]
`, "the name is built in"},
		"placed with tags": {`
resources:
- name: pr
  type: github-prs
  tags: [vpc]
  source: {repo: acme/app}
jobs:
- name: build
  plan: [{get: pr}]
`, "tags: is not valid"},
	} {
		_, err := LoadConfig(writeConfig(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// TestGitHubTokenIsARequirement: validate names the one variable the type
// cannot run without, and names the one source.token_env asks for.
func TestGitHubTokenIsARequirement(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("ACME_TOKEN", "")

	cfg, err := LoadConfig(writeConfig(t, githubPipeline("\n    token_env: ACME_TOKEN", "")))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	problems := cfg.CheckEnvironment()

	details := make([]string, 0, len(problems))
	for _, problem := range problems {
		details = append(details, problem.Target+" "+problem.Detail)
	}

	joined := strings.Join(details, "\n")
	if !strings.Contains(joined, `resource "pr" $ACME_TOKEN is not set`) || !strings.Contains(joined, `resource "comment" $GH_TOKEN is not set`) {
		t.Errorf("problems = %q, want the pr resource's own variable and the comment resource's default", joined)
	}

	t.Setenv("GH_TOKEN", "set")
	t.Setenv("ACME_TOKEN", "set")

	if problems := cfg.CheckEnvironment(); len(problems) != 0 {
		t.Errorf("problems with both set = %v, want none", problems)
	}
}
