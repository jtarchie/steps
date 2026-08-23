package config

import (
	"strings"
	"testing"
)

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
