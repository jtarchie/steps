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
		{"mcp-backed renamed put", `
resources:
- name: repo
  type: remote
  source: {}
jobs:
- name: build
  plan:
  - put: publish
    resource: repo
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
  - put: publish
    resource: repo
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

	if got := strings.Join(plan[4].Tags, ","); got != "vpc" {
		t.Errorf("the renamed put has tags %q, want its resource's", got)
	}

	if got := strings.Join(cfg.Jobs[0].Hooks.OnFailure.Tags, ","); got != "vpc" {
		t.Errorf("the put hook has tags %q, want the resource's", got)
	}
}

const inheritedTagAgents = `
agents:
- name: boxed
  image: alpine:3.20
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
- name: bare
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
- name: claude
  image: alpine:3.20
  source: { model: "@claude/sonnet" }
`

// TestJobAndBlockTagsAreInherited pins the resolved tree: a step's own tags:,
// else its resource's, else the nearest do:/in_parallel:, else the job's — and
// a step's hook takes what the step resolved to.
func TestJobAndBlockTagsAreInherited(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, taggedResourceTypes+inheritedTagAgents+`
resources:
- name: private
  type: probe
  tags: [vpc]
  source: {}
- name: public
  type: probe
  source: {}
jobs:
- name: build
  tags: [gpu]
  plan:
  - get: private
    on_success:
      task: get-hook
      run: "true"
  - get: public
  - task: plain
    run: "true"
    on_failure:
      task: plain-hook
      run: "true"
  - task: own
    tags: [edge]
    run: "true"
    ensure:
      task: own-hook
      run: "true"
  - tags: [disk]
    do:
    - task: in-do
      run: "true"
    - in_parallel:
        steps:
        - task: in-parallel
          run: "true"
      tags: [arm]
    - try:
        task: in-try
        run: "true"
    on_success:
      task: do-hook
      run: "true"
  - race:
      steps:
      - task: racer-a
        run: "true"
      - task: racer-b
        run: "true"
  - ensemble:
      agents:
      - agent: boxed
        messages: [one]
      - agent: boxed
        messages: [two]
      verdicts: [ok, "no"]
      decide: majority
  - approval:
      message: go
  ensure:
    task: job-hook
    run: "true"
- name: escape
  plan:
  - do:
    - task: placed
      tags: [edge]
      run: "true"
    on_failure:
      task: here
      run: "true"
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	job := cfg.Jobs[0]
	plan := job.Plan
	doBlock := plan[4]

	for _, tc := range []struct {
		name string
		step *Step
		want string
	}{
		{"a get of a tagged resource keeps the resource's", &plan[0], "vpc"},
		{"a get of an untagged resource takes the job's", &plan[1], "gpu"},
		{"an untagged task takes the job's", &plan[2], "gpu"},
		{"an untagged step's hook takes the job's", plan[2].Hooks.OnFailure, "gpu"},
		{"a step's own tags win", &plan[3], "edge"},
		{"a tagged step's hook takes the step's", plan[3].Hooks.Ensure, "edge"},
		{"a get's hook takes its resource's", plan[0].Hooks.OnSuccess, "vpc"},
		{"a do: overrides the job", &doBlock.Do[0], "disk"},
		{"an in_parallel: in a do: overrides again", &doBlock.Do[1].InParallel.Steps[0], "arm"},
		{"a try: wrapper carries nothing", &doBlock.Do[2], ""},
		{"what a try: wraps takes the block's", doBlock.Do[2].Try, "disk"},
		{"a declaring block's hook takes the block's", doBlock.Hooks.OnSuccess, "disk"},
		{"a declaring do: is cleared", &doBlock, ""},
		{"a declaring in_parallel: is cleared", &doBlock.Do[1], ""},
		{"a race: passes the job's through", &plan[5].Race.Steps[1], "gpu"},
		{"an ensemble: passes the job's through", &plan[6].Ensemble.Agents[0], "gpu"},
		{"an approval runs nothing to place", &plan[7], ""},
		{"a job hook takes the job's", job.Hooks.Ensure, "gpu"},
		{"an untagged do:'s hook stays here though its step is placed", cfg.Jobs[1].Plan[0].Hooks.OnFailure, ""},
	} {
		if got := strings.Join(tc.step.Tags, ","); got != tc.want {
			t.Errorf("%s: tags = %q, want %q", tc.name, got, tc.want)
		}
	}

	plan[1].Tags[0] = "mutated"
	if plan[2].Tags[0] != "gpu" || job.Tags[0] != "gpu" {
		t.Error("two steps inheriting one tag share its backing array")
	}
}

// TestInheritedTagRefusals: a step that cannot be placed whole is refused
// whether it wrote tags: or inherited them, and an inherited refusal names
// where the tag came from and how to scope it.
func TestInheritedTagRefusals(t *testing.T) {
	t.Parallel()

	const hint = "move tags: onto a do: around only the steps that belong on the worker"

	cases := []struct {
		name string
		yaml string
		want []string
	}{
		{"agent without image", `
jobs:
- name: build
  tags: [gpu]
  plan:
  - agent: bare
    messages: [hi]
`, []string{`job "build" step 0 (line 29) (tags: [gpu] inherited from job "build") (agent "bare"): tags: needs image:`, hint}},
		{"cli agent", `
jobs:
- name: build
  tags: [gpu]
  plan:
  - agent: claude
    messages: [hi]
`, []string{`inherited from job "build"`, "tags: is not valid on a CLI agent", hint}},
		{"task with a tasks: entry's fix:", `
tasks:
- name: compile
  run: exit 1
  fix: { agent: bare }
jobs:
- name: build
  tags: [gpu]
  plan:
  - task: compile
`, []string{`inherited from job "build"`, "tags: is not valid on a task with fix:", hint}},
		{"agent hook of a tagged step", `
jobs:
- name: build
  plan:
  - task: work
    tags: [gpu]
    run: "true"
    on_failure:
      agent: bare
      messages: [hi]
`, []string{`(on_failure hook) (tags: [gpu] inherited from job "build" step 0 (line 28)) (agent "bare"): tags: needs image:`, hint}},
		{"mcp put inheriting from a do:", `
resources:
- name: tracker
  type: remote
  source: {}
jobs:
- name: build
  plan:
  - tags: [gpu]
    do:
    - put: tracker
`, []string{`(tags: [gpu] inherited from job "build" step 0 (line 32))`, `tags: is not valid on a resource of type "remote"`, hint}},
		{"ensemble member without image", `
jobs:
- name: build
  tags: [gpu]
  plan:
  - ensemble:
      agents:
      - agent: bare
        messages: [one]
      - agent: boxed
        messages: [two]
      verdicts: [ok, "no"]
      decide: majority
`, []string{`(ensemble branch 0) (tags: [gpu] inherited from job "build")`, "tags: needs image:"}},
		{"job tags empty", `
jobs:
- name: build
  tags: []
  plan:
  - task: a
    run: "true"
`, []string{`job "build" (line 26): tags: is empty`}},
		{"job tags two", `
jobs:
- name: build
  tags: [a, b]
  plan:
  - task: a
    run: "true"
`, []string{`job "build" (line 26): tags: names 2 workers (a, b)`}},
		{"job tags blank", `
jobs:
- name: build
  tags: [" "]
  plan:
  - task: a
    run: "true"
`, []string{`job "build" (line 26): tags: has a blank entry`}},
		{"race", `
jobs:
- name: build
  plan:
  - race:
      steps:
      - task: a
        run: "true"
      - task: b
        run: "true"
    tags: [gpu]
`, []string{"tags is not valid on a race step", "wrap the block in a do: that declares them"}},
		{"ensemble", `
jobs:
- name: build
  plan:
  - ensemble:
      agents:
      - agent: boxed
        messages: [one]
      - agent: boxed
        messages: [two]
      verdicts: [ok, "no"]
      decide: majority
    tags: [gpu]
`, []string{"tags is not valid on an ensemble step", "wrap the block in a do: that declares them"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			for _, want := range tc.want {
				wantLoadError(t, writeConfig(t, taggedResourceTypes+inheritedTagAgents+tc.yaml), want)
			}
		})
	}
}

// TestAnInheritedTagOnAPlaceableAgentLoads is the other side of the refusal:
// an agent with image: can be placed whole, so inheriting a tag is fine.
func TestAnInheritedTagOnAPlaceableAgentLoads(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig(writeConfig(t, inheritedTagAgents+`
jobs:
- name: build
  tags: [gpu]
  plan:
  - agent: boxed
    messages: [hi]
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := strings.Join(cfg.Jobs[0].Plan[0].Tags, ","); got != "gpu" {
		t.Errorf("tags = %q, want the job's", got)
	}
}

// TestPlacementsWalkAMalformedConfig: validation runs every checker and joins
// what they find, so the placement walk meets configs no other rule has
// accepted yet. It must report, not panic.
func TestPlacementsWalkAMalformedConfig(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(writeConfig(t, taggedResourceTypes+`
jobs:
- name: build
  tags: [gpu]
  plan:
  - get: nowhere
  - race:
      steps: []
  - run: "true"
`))
	if err == nil {
		t.Fatal("LoadConfig accepted a malformed config")
	}
}
