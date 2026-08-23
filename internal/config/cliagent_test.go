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

// TestBudgetSpellingFollowsTheRunner pins that each budget spelling is
// accepted exactly where something can enforce it. The failure this prevents
// is a pipeline that reads as if it had a spend limit while nothing applies
// one -- the same class of silent no-op every other cli rejection exists for.
func TestBudgetSpellingFollowsTheRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		model         string
		budget        Budget
		wantErrSubstr string
	}{
		{
			name:   "usd on a cli agent",
			model:  "@claude/sonnet",
			budget: Budget{USD: 0.5},
		},
		{
			name:   "tokens on a hosted agent",
			model:  "openai/gpt-4o",
			budget: Budget{Tokens: 1000},
		},
		{
			name:          "tokens on a cli agent",
			model:         "@claude/sonnet",
			budget:        Budget{Tokens: 1000},
			wantErrSubstr: "budget.tokens is not supported with a cli source",
		},
		{
			name:          "usd on a hosted agent",
			model:         "openai/gpt-4o",
			budget:        Budget{USD: 0.5},
			wantErrSubstr: "budget.usd is only supported with a cli source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			budget := tt.budget
			cfg := &Config{
				Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: tt.model}, Budget: &budget}},
				Jobs: []Job{{Name: "review", Plan: []Step{
					{Agent: "reviewer", Messages: []string{"go"}, Inputs: &InputSpec{}},
				}}},
			}

			err := cfg.validate()

			if tt.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErrSubstr)
			}

			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrSubstr)
			}
		})
	}
}

// TestCLIAgentContainerRules covers what image: means for a CLI agent now
// that it containerizes the CLI process itself: allowed, including with the
// step-level override, but not together with network: none, which would
// sever the connection the CLI's steps-provided tools come back over.
func TestCLIAgentContainerRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		agentImage    string
		agentNetwork  string
		stepImage     string
		stepNetwork   string
		wantErrSubstr string
	}{
		{
			name:       "image on a cli agent",
			agentImage: "alpine:3",
		},
		{
			name:      "image on a step using a cli agent",
			stepImage: "alpine:3",
		},
		{
			// Credentials are deliberately NOT a load-time concern: which
			// route is available depends on the machine, and a pipeline must
			// not stop loading because it moved to a Mac. Preflight answers
			// that question instead.
			name:       "image without api_key_env still loads",
			agentImage: "alpine:3",
		},
		{
			name:         "a narrower network is fine",
			agentImage:   "alpine:3",
			agentNetwork: "my-compose-net",
		},
		{
			name:          "network none on a containerized cli agent",
			agentImage:    "alpine:3",
			agentNetwork:  "none",
			wantErrSubstr: "the cli reaches its steps-provided tools",
		},
		{
			name:          "network none set on the step instead",
			agentImage:    "alpine:3",
			stepNetwork:   "none",
			wantErrSubstr: "the cli reaches its steps-provided tools",
		},
		{
			name:          "image on the step, network none on the agent",
			agentNetwork:  "none",
			stepImage:     "alpine:3",
			wantErrSubstr: "the cli reaches its steps-provided tools",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{
				Agents: []Agent{{
					Name:    "reviewer",
					Source:  AgentSource{Model: "@claude/sonnet"},
					Image:   tt.agentImage,
					Network: tt.agentNetwork,
				}},
				Jobs: []Job{{Name: "review", Plan: []Step{{
					Agent:    "reviewer",
					Messages: []string{"go"},
					Inputs:   &InputSpec{},
					Image:    tt.stepImage,
					Network:  tt.stepNetwork,
				}}}},
			}

			err := cfg.validate()

			if tt.wantErrSubstr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("expected an error containing %q", tt.wantErrSubstr)
			}

			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErrSubstr)
			}
		})
	}
}
