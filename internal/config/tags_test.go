package config

import (
	"strings"
	"testing"
)

const taggedResourceTypes = `
resource_types:
- name: probe
  config:
    check: echo '[]'
    in: "true"
    out: echo '{}'
- name: remote
  config:
    mcp: { server: tools, check: { tool: list } }
mcp_servers:
- name: tools
  endpoint: http://127.0.0.1:1/mcp
`

// TestTagsOnResources pins the shape of a resource's tags: — one entry, the
// same rule a step's has — and where it is refused.
func TestTagsOnResources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"empty", `
resources:
- name: repo
  type: probe
  tags: []
  source: {}
`, `resource repo: tags: is empty`},
		{"two", `
resources:
- name: repo
  type: probe
  tags: [a, b]
  source: {}
`, `resource repo: tags: names 2 workers (a, b)`},
		{"blank", `
resources:
- name: repo
  type: probe
  tags: [" "]
  source: {}
`, `resource repo: tags: has a blank entry`},
		{"mcp-backed resource", `
resources:
- name: repo
  type: remote
  tags: [vpc]
  source: {}
`, `resource repo: tags: is not valid on a resource of type "remote" — its mcp in/out run inside this process`},
		{"mcp-backed get", `
resources:
- name: repo
  type: remote
  source: {}
jobs:
- name: build
  plan:
  - get: repo
    tags: [vpc]
`, `job "build" step 0 (line 22): tags: is not valid on a resource of type "remote"`},
		{"try wrapper", `
resources:
- name: repo
  type: probe
  source: {}
jobs:
- name: build
  plan:
  - try:
      get: repo
    tags: [vpc]
`, `tags is not valid on a try: step; set it on the step try: wraps`},
		{"env still refused on a get", `
resources:
- name: repo
  type: probe
  source: {}
jobs:
- name: build
  plan:
  - get: repo
    env: [TOKEN]
`, `env is not valid on get steps`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, taggedResourceTypes+tc.yaml), tc.want)
		})
	}
}

// TestGetAndPutInheritResourceTags: a resource's tags: reach every get and
// put of it that names none of its own, resolved at load so every reader of
// a step's tags: sees one answer. A step's own tags: wins.
func TestGetAndPutInheritResourceTags(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, taggedResourceTypes+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}
- name: other
  type: probe
  source: {}
jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - get: other
  - put: repo
    tags: [edge]
  - try:
      put: repo
  on_failure:
    put: repo
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	plan := cfg.Jobs[0].Plan

	for i, want := range []string{"vpc", "", "edge"} {
		if got := strings.Join(plan[i].Tags, ","); got != want {
			t.Errorf("step %d tags = %q, want %q", i, got, want)
		}
	}

	if got := strings.Join(plan[3].Try.Tags, ","); got != "vpc" {
		t.Errorf("the put inside try: has tags %q, want the resource's", got)
	}

	if got := strings.Join(cfg.Jobs[0].Hooks.OnFailure.Tags, ","); got != "vpc" {
		t.Errorf("the put hook has tags %q, want the resource's", got)
	}
}
