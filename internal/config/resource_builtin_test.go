package config

import (
	"strings"
	"testing"
)

// `type: git` needs no resource_types: block. Cloning a repository is step two
// of essentially every real pipeline, and it used to mean hand-writing
// check/in shell against an undocumented JSON contract.
func TestBuiltinGitResourceTypeRegistered(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: repo
  type: git
  source:
    uri: https://example.com/repo.git
    branch: main
jobs:
- name: j
  plan: [{ get: repo }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType("git")
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Check == "" || resourceType.Config.In == "" {
		t.Error("built-in git is missing a check or in command")
	}

	// Read-only by design: publishing is policy, so there is no out:.
	if resourceType.Config.Out != "" {
		t.Errorf("built-in git declares out: %q, want none", resourceType.Config.Out)
	}
}

// `type: slack-mentions` needs no resource_types: block either — it is
// expr-backed (a JSON HTTP API and nothing else), get-only, and requires
// SLACK_BOT_TOKEN so it can authenticate.
func TestBuiltinSlackMentionsResourceTypeRegistered(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: mentions
  type: slack-mentions
  source: {}
jobs:
- name: j
  plan: [{ get: mentions }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType("slack-mentions")
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Backend() != BackendExpr {
		t.Fatalf("Backend() = %v, want expr", resourceType.Config.Backend())
	}

	if resourceType.Config.Expr.Check == "" || resourceType.Config.Expr.In == "" {
		t.Error("built-in slack-mentions is missing a check or in expression")
	}

	// Get-only by design: there is nothing to publish, so put: against it is
	// a load error rather than a silent no-op.
	if resourceType.Config.Expr.Out != "" {
		t.Errorf("built-in slack-mentions declares expr.out: %q, want none", resourceType.Config.Expr.Out)
	}

	found := false

	for _, name := range resourceType.Env {
		if name == "SLACK_BOT_TOKEN" {
			found = true
		}
	}

	if !found {
		t.Errorf("env = %v, want SLACK_BOT_TOKEN", resourceType.Env)
	}
}

// `type: slack-reply` is the publish-only mirror: no expr.check/in, so
// get: against it is a load error.
func TestBuiltinSlackReplyResourceTypeRegistered(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: reply
  type: slack-reply
  source: {}
jobs:
- name: j
  plan: [{ put: reply }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType("slack-reply")
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Backend() != BackendExpr {
		t.Fatalf("Backend() = %v, want expr", resourceType.Config.Backend())
	}

	if resourceType.Config.Expr.Out == "" {
		t.Error("built-in slack-reply is missing an out expression")
	}

	if resourceType.Config.Expr.Check != "" || resourceType.Config.Expr.In != "" {
		t.Errorf("built-in slack-reply declares expr.check/in, want neither (publish-only)")
	}
}

// A user-defined type of the same name replaces the built-in outright. Command
// templates don't merge — half of one check paired with half of another is not
// a resource type anyone meant to write.
func TestBuiltinResourceTypeShadowedByUserDefinition(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: git
  config:
    check: echo mine
    in: "true"
    out: "true"
resources:
- name: repo
  type: git
  source: {}
jobs:
- name: j
  plan: [{ get: repo }, { put: repo }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType("git")
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Check != "echo mine" {
		t.Errorf("Check = %q, want the user's definition to win", resourceType.Config.Check)
	}

	// Exactly one entry: the built-in did not also get appended.
	count := 0

	for _, rt := range cfg.ResourceTypes {
		if rt.Name == "git" {
			count++
		}
	}

	if count != 1 {
		t.Errorf("found %d resource types named git, want 1", count)
	}
}

// A put against a type that declares no way to publish is a load error, not a
// run that reaches the put and fails obscurely — and not a type carrying an
// `out: "true"` placeholder that succeeds having pushed nothing.
func TestPutRequiresAnOutCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "built-in git has no out",
			pipeline: `
resources:
- name: repo
  type: git
  source: { uri: https://example.com/repo.git }
jobs:
- name: j
  plan: [{ put: repo }]
`,
			want: `put "repo" targets resource type "git", which declares no out: command`,
		},
		{
			name: "user type with no out",
			pipeline: `
resource_types:
- name: readonly
  config:
    check: 'echo ''[{"ref":"1"}]'''
    in: "true"
resources:
- name: thing
  type: readonly
  source: {}
jobs:
- name: j
  plan: [{ put: thing }]
`,
			want: `which declares no out: command`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, test.pipeline), test.want)
		})
	}
}

// The git type reads its optional branch via `index`, since templates render
// with missingkey=error and an absent optional key must answer "nothing"
// rather than failing the render.
func TestBuiltinGitBranchIsOptional(t *testing.T) {
	t.Parallel()

	resourceType, err := ReadBuiltinResourceType("git")
	if err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{resourceType.Config.Check, resourceType.Config.In} {
		if strings.Contains(command, ".source.branch") {
			t.Errorf("command reads .source.branch directly, which errors when branch is omitted:\n%s", command)
		}
	}
}

// `type: slack-reaction` is the third of the Slack trio and the other
// publish-only one: a get: against it is a load error, and its env: names the
// same token the other two read.
func TestBuiltinSlackReactionResourceTypeRegistered(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: reaction
  type: slack-reaction
  source: {}
jobs:
- name: j
  plan: [{ put: reaction, params: { add: eyes } }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resourceType, err := cfg.FindResourceType("slack-reaction")
	if err != nil {
		t.Fatal(err)
	}

	if resourceType.Config.Backend() != BackendExpr {
		t.Fatalf("Backend() = %v, want expr", resourceType.Config.Backend())
	}

	if resourceType.Config.Expr.Out == "" {
		t.Error("built-in slack-reaction is missing an out expression")
	}

	if resourceType.Config.Expr.Check != "" || resourceType.Config.Expr.In != "" {
		t.Errorf("built-in slack-reaction declares expr.check/in, want neither (publish-only)")
	}

	found := false

	for _, name := range resourceType.Env {
		if name == "SLACK_BOT_TOKEN" {
			found = true
		}
	}

	if !found {
		t.Errorf("env = %v, want SLACK_BOT_TOKEN", resourceType.Env)
	}
}

// A resource naming a type that does not exist used to LOAD, and only failed
// when the step ran — so `steps validate` said ok to a pipeline that could
// never work, and a config that was fine on the machine that wrote it broke
// on the one that ran it (a built-in the local binary had and the remote one
// did not). Validation is where that belongs: nothing about the answer needs
// the run.
func TestResourceWithUnknownTypeIsRefused(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: nope
  type: totally-made-up-type
  source: {}
jobs:
- name: j
  plan: [{ put: nope }]
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig succeeded; a resource naming an unknown type must be refused")
	}

	if !strings.Contains(err.Error(), "totally-made-up-type") {
		t.Errorf("error does not name the missing type: %v", err)
	}

	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error does not name the resource that declared it: %v", err)
	}
}

// The built-ins are registered before this check runs, so a resource naming
// one of them with no resource_types: block in sight still loads.
func TestResourceWithBuiltinTypeStillLoads(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resources:
- name: reaction
  type: slack-reaction
  source: {}
jobs:
- name: j
  plan: [{ put: reaction, params: { add: eyes } }]
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
