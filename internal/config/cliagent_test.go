package config

import (
	"strings"
	"testing"
)

func TestResolveCLITarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		source        AgentSource
		wantModel     string
		wantCLI       string
		wantKeyEnv    string
		wantRequires  bool
		wantErrSubstr string
	}{
		{
			name:      "cli and model",
			source:    AgentSource{Model: "@claude/sonnet"},
			wantModel: "sonnet",
			wantCLI:   "claude",
		},
		{
			name:      "model may carry its own slashes",
			source:    AgentSource{Model: "@claude/anthropic/claude-sonnet-4-5"},
			wantModel: "anthropic/claude-sonnet-4-5",
			wantCLI:   "claude",
		},
		{
			name:         "explicit api_key_env is forwarded and required",
			source:       AgentSource{Model: "@claude/opus", APIKeyEnv: "MY_KEY"},
			wantModel:    "opus",
			wantCLI:      "claude",
			wantKeyEnv:   "MY_KEY",
			wantRequires: true,
		},
		{
			name:          "unknown cli",
			source:        AgentSource{Model: "@cluade/sonnet"},
			wantErrSubstr: "names no known cli",
		},
		{
			name:          "no model after the cli",
			source:        AgentSource{Model: "@claude"},
			wantErrSubstr: "must name a model after the cli",
		},
		{
			name:          "empty model after the slash",
			source:        AgentSource{Model: "@claude/"},
			wantErrSubstr: "must name a model after the cli",
		},
		{
			name:          "endpoint has nothing to configure",
			source:        AgentSource{Model: "@claude/sonnet", Endpoint: "https://example.test/v1/"},
			wantErrSubstr: "remove source.endpoint",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Through resolveAgentTarget, not resolveCLITarget directly: the
			// dispatch on "@" is part of what this pins.
			target, err := resolveAgentTarget(tt.source)

			if tt.wantErrSubstr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q", tt.wantErrSubstr)
				}

				if !strings.Contains(err.Error(), tt.wantErrSubstr) {
					t.Fatalf("error = %q, want it to contain %q", err, tt.wantErrSubstr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveAgentTarget: %v", err)
			}

			assertCLITarget(t, target, tt.wantCLI, tt.wantModel, tt.wantKeyEnv, tt.wantRequires)
		})
	}
}

// assertCLITarget checks a resolved cli target field by field.
func assertCLITarget(t *testing.T, target agentTarget, wantCLI, wantModel, wantKeyEnv string, wantRequires bool) {
	t.Helper()

	if target.CLI != wantCLI {
		t.Errorf("cli = %q, want %q", target.CLI, wantCLI)
	}

	if target.ModelName != wantModel {
		t.Errorf("model = %q, want %q", target.ModelName, wantModel)
	}

	if target.APIKeyEnv != wantKeyEnv {
		t.Errorf("apiKeyEnv = %q, want %q", target.APIKeyEnv, wantKeyEnv)
	}

	if target.RequiresKey != wantRequires {
		t.Errorf("requiresKey = %v, want %v", target.RequiresKey, wantRequires)
	}

	if target.BaseURL != "" {
		t.Errorf("baseURL = %q, want empty — a cli source has no endpoint", target.BaseURL)
	}
}

// TestWithSourceCrossesCLIBoundary pins failover in both directions: a CLI
// agent falling back to a hosted provider must stop running a subprocess, and
// a hosted agent falling back to a CLI must start.
func TestWithSourceCrossesCLIBoundary(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "@claude/sonnet"}}},
	}

	primary, err := cfg.ResolveAgentInvocation(Step{Agent: "reviewer"})
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if primary.CLI != "claude" || primary.BaseURL != "" {
		t.Fatalf("primary = {CLI: %q, BaseURL: %q}, want the cli path", primary.CLI, primary.BaseURL)
	}

	toHTTP, err := primary.WithSource(AgentSource{Model: "openai/gpt-4o"}, nil)
	if err != nil {
		t.Fatalf("WithSource to http: %v", err)
	}

	assertHostedInvocation(t, toHTTP)
}

// TestWithSourceIntoCLI is the other direction: a hosted agent falling back
// onto a cli must start running a subprocess.
func TestWithSourceIntoCLI(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "openai/gpt-4o"}}},
	}

	primary, err := cfg.ResolveAgentInvocation(Step{Agent: "reviewer"})
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	toCLI, err := primary.WithSource(AgentSource{Model: "@claude/opus"}, nil)
	if err != nil {
		t.Fatalf("WithSource to cli: %v", err)
	}

	if toCLI.CLI != "claude" || toCLI.ModelName != "opus" {
		t.Errorf("failover to cli = {CLI: %q, Model: %q}, want {claude, opus}", toCLI.CLI, toCLI.ModelName)
	}

	if toCLI.BaseURL != "" {
		t.Errorf("BaseURL = %q after failing over to a cli, want it cleared", toCLI.BaseURL)
	}

	// The point of WithSource: only the destination moves.
	if toCLI.AgentName != primary.AgentName || toCLI.MaxTurns != primary.MaxTurns {
		t.Error("failover changed something other than the source")
	}
}

// assertHostedInvocation checks an invocation left the cli path entirely.
func assertHostedInvocation(t *testing.T, ri ResolvedInvocation) {
	t.Helper()

	if ri.CLI != "" {
		t.Errorf("CLI = %q after failing over to a hosted provider, want it cleared", ri.CLI)
	}

	if ri.BaseURL == "" {
		t.Error("BaseURL is empty after failing over to a hosted provider")
	}
}

func TestCLIProviderTableIsResolvable(t *testing.T) {
	t.Parallel()

	for _, name := range CLIProviderNames() {
		if CLIBinary(name) == "" {
			t.Errorf("cli %q has no binary", name)
		}

		target, err := resolveAgentTarget(AgentSource{Model: CLISourcePrefix + name + "/some-model"})
		if err != nil {
			t.Errorf("cli %q does not resolve: %v", name, err)

			continue
		}

		if target.CLI != name {
			t.Errorf("cli %q resolved to %q", name, target.CLI)
		}
	}
}
