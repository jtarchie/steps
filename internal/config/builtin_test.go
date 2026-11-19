package config

import (
	"slices"
	"strings"
	"testing"
)

// A declared built-in name with only image: is the built-in run there, still native (its source: is still checked); with commands of its own it replaces the built-in, as before.
func TestABuiltinTypeGivenAnImageIsTheBuiltinRunThere(t *testing.T) {
	t.Parallel()

	cfg, err := Parse([]byte(`
resource_types:
- name: git
  image: alpine/git
  env: [EXTRA]
- name: github-comments
  image: alpine/git
- name: cron
  image: alpine:3
  config: {check: "true"}
`), "image", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	for name, want := range map[string]string{"git": gitType, "github-comments": GitHubCommentsType, "cron": ""} {
		if got := cfg.nativeType(name); got != want {
			t.Errorf("nativeType(%q) = %q, want %q", name, got, want)
		}
	}

	git, _ := cfg.FindResourceType("git")
	if git.Image != "alpine/git" || !slices.Equal(git.Env, []string{"EXTRA"}) {
		t.Errorf("git = image %q env %q, want alpine/git and EXTRA", git.Image, git.Env)
	}

	// The slack-* built-ins evaluate in this process, so an image is refused rather than ignored.
	_, err = Parse([]byte("resource_types:\n- {name: slack-reply, image: alpine:3}\n"), "image", nil)
	if err == nil || !strings.Contains(err.Error(), "image: cannot apply") {
		t.Errorf("slack-reply given an image = %v, want it refused as having nothing to run there", err)
	}

	_, err = Parse([]byte(`
resource_types:
- name: git
  image: alpine/git
resources:
- name: repo
  type: git
  source: {uri: https://example.com/a.git, fecth: true}
`), "image", nil)
	if err == nil || !strings.Contains(err.Error(), "fecth") {
		t.Errorf("a typo in a built-in-in-an-image's source loaded: %v", err)
	}
}

// The Go-backed built-ins with no command run in this process, as slack-* do; given an image, the built-in was dropped for an empty shell type that loaded with no check.
func TestABuiltinWithNoCommandIsRefusedAnImage(t *testing.T) {
	t.Parallel()

	for _, name := range []string{WebhookType, CronType, RegistryImageType, DockerServiceType, GitHubIssuesType, LinearTypes()[0]} {
		_, err := Parse([]byte("resource_types:\n- {name: "+name+", image: alpine:3}\n"), "image", nil)
		if err == nil || !strings.Contains(err.Error(), "image: cannot apply to "+name) {
			t.Errorf("%s given an image = %v, want it refused as having nothing to run there", name, err)
		}
	}

	_, err := Parse([]byte("resource_types:\n- {name: mine, image: alpine:3}\n"), "image", nil)
	if err == nil || !strings.Contains(err.Error(), `resource_type "mine": sets none of check/in/out`) {
		t.Errorf("a shell type with no commands = %v, want it refused", err)
	}
}

// A user entry for a built-in name supplies what it sets and inherits the
// rest. Built-in profiles carry no source: — only the user knows which model
// to call — so before this, naming @builtin/reviewer to supply one discarded
// the persona and tool grant that were the reason to reference it.
func TestBuiltinAgentPartialOverride(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: "@builtin/reviewer"
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan: [{ agent: "@builtin/reviewer", messages: [review] }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent, err := cfg.FindAgent("@builtin/reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if agent.Source.Model != "lmstudio/qwen" {
		t.Errorf("Source.Model = %q, want the user's model", agent.Source.Model)
	}

	if agent.System == "" {
		t.Error("System is empty, want the built-in persona inherited")
	}

	if len(agent.Tools) == 0 {
		t.Error("Tools is empty, want the built-in tool grant inherited")
	}

	// The profile's dials come along too.
	if agent.MaxTurns == nil {
		t.Error("MaxTurns is unset, want the built-in value inherited")
	}
}

// TestBuiltinAgentDialsAreWellFormed holds every shipped profile to the shape
// the others rely on: a turn count to correlate against, and no dial set to a
// value that caps nothing.
func TestBuiltinAgentDialsAreWellFormed(t *testing.T) {
	t.Parallel()

	names, err := ListBuiltinAgentNames()
	if err != nil {
		t.Fatalf("ListBuiltinAgentNames: %v", err)
	}

	if len(names) == 0 {
		t.Fatal("no built-in agents, want the shipped profiles")
	}

	for _, name := range names {
		agent, err := ReadBuiltinAgent(name)
		if err != nil {
			t.Fatalf("ReadBuiltinAgent(%q): %v", name, err)
		}

		// Without a turn count there is nothing for the other two dials to be
		// correlated WITH, which is the whole reason a profile states them.
		if agent.MaxTurns == nil {
			t.Errorf("@builtin/%s sets no max_turns", name)
		}

		// A dial a profile does set must be a real value rather than the zero
		// one, which reads as "no cap" and is never what a role means.
		if agent.MaxContextBytes != nil && *agent.MaxContextBytes <= 0 {
			t.Errorf("@builtin/%s sets max_context_bytes: %d, which caps nothing", name, *agent.MaxContextBytes)
		}

		if agent.Timeout == "" {
			continue
		}

		_, err = ParseTimeout(agent.Timeout)
		if err != nil {
			t.Errorf("@builtin/%s sets timeout: %q, which does not parse: %v", name, agent.Timeout, err)
		}
	}
}

// TestBuiltinReadingRolesRaiseTheContextCeiling pins the finding the dials
// encode. A turn count on its own is not an opinion about a role: the default
// 100_000 bytes is the wrong ceiling for the roles whose whole job is to see
// something entire, and a profile that left it there would describe a workload
// nobody has.
func TestBuiltinReadingRolesRaiseTheContextCeiling(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"planner", "reviewer", "builder"} {
		agent, err := ReadBuiltinAgent(name)
		if err != nil {
			t.Fatalf("ReadBuiltinAgent(%q): %v", name, err)
		}

		if agent.MaxContextBytes == nil || *agent.MaxContextBytes <= DefaultMaxContextBytes {
			t.Errorf("@builtin/%s does not raise max_context_bytes above the %d default; its job is to read a lot",
				name, DefaultMaxContextBytes)
		}
	}
}

// Anything the entry sets still wins over the profile — the same "inline wins"
// rule a file: include follows.
func TestBuiltinAgentOverrideWins(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: "@builtin/reviewer"
  source: { model: lmstudio/qwen }
  system: only mine
  tools: [read_file]
  max_turns: 3
jobs:
- name: j
  plan: [{ agent: "@builtin/reviewer", messages: [review] }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent, err := cfg.FindAgent("@builtin/reviewer")
	if err != nil {
		t.Fatal(err)
	}

	if agent.System != "only mine" {
		t.Errorf("System = %q, want the entry's own value", agent.System)
	}

	if len(agent.Tools) != 1 {
		t.Errorf("Tools = %v, want only the entry's grant", agent.Tools)
	}

	if agent.MaxTurns == nil || *agent.MaxTurns != 3 {
		t.Errorf("MaxTurns = %v, want 3", agent.MaxTurns)
	}
}

// Every built-in is registered and carries a resolved persona, so referencing
// one never depends on a file the pipeline author has to supply.
func TestBuiltinAgentsRegistered(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan: [{ task: t, run: "true" }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	names, err := ListBuiltinAgentNames()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) == 0 {
		t.Fatal("no built-in agents are embedded")
	}

	for _, name := range names {
		agent, findErr := cfg.FindAgent("@builtin/" + name)
		if findErr != nil {
			t.Errorf("built-in %q: %v", name, findErr)

			continue
		}

		if agent.System == "" {
			t.Errorf("built-in %q has no resolved system prompt", name)
		}

		if strings.HasPrefix(agent.SystemFile, "@builtin/") {
			t.Errorf("built-in %q left system_file unresolved", name)
		}
	}
}
