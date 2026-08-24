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
