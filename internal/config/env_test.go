package config

import (
	"strings"
	"testing"
)

func TestValidateEnvValuesRejectsLiterals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		cfg  *Config
	}{
		{"resource_type", &Config{ResourceTypes: []ResourceType{{Name: "git", Env: []string{"TOKEN=abc"}}}}},
		{"resource", &Config{Resources: []Resource{{Name: "repo", Type: "git", Env: []string{"TOKEN=abc"}}}}},
		{"agent", &Config{Agents: []Agent{{Name: "reviewer", Env: []string{"TOKEN=abc"}}}}},
		{"task", &Config{Tasks: []Task{{Name: "build", Env: []string{"TOKEN=abc"}}}}},
		{"step", &Config{Jobs: []Job{{Name: "j", Plan: []Step{{Task: "build", Run: "true", Env: []string{"TOKEN=abc"}}}}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.cfg.validateEnvValues()
			if err == nil {
				t.Fatal("expected a KEY=VALUE literal to be rejected")
			}

			if !strings.Contains(err.Error(), "must be a variable NAME") {
				t.Errorf("error = %v, want it to explain the name-not-value rule", err)
			}
		})
	}
}

func TestValidateEnvValuesRejectsEmptyName(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build", Env: []string{""}}}}

	err := cfg.validateEnvValues()
	if err == nil {
		t.Fatal("expected an empty variable name to be rejected")
	}
}

func TestValidateEnvValuesAcceptsNames(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{Name: "git", Env: []string{"GIT_TOKEN"}}},
		Resources:     []Resource{{Name: "repo", Type: "git", Env: []string{"SECOND_GIT_TOKEN"}}},
		Agents:        []Agent{{Name: "reviewer", Env: []string{"GH_TOKEN"}}},
		Tasks:         []Task{{Name: "build", Env: []string{"GOFLAGS", "GOPATH"}}},
	}

	err := cfg.validateEnvValues()
	if err != nil {
		t.Errorf("validateEnvValues: %v", err)
	}
}

// TestValidateEnvValuesRejectsResourceEnvOnMCPBackedType closes the footgun
// an mcp-backed resource type would otherwise leave open: env: has no effect
// there (mcpCheckVersions/mcpRunIn/mcpRunOut never consult it), so silently
// accepting it would let someone copy the reply-as-support-bot-style
// env: + source.token_env pattern onto an mcp-backed resource, pass
// validation, run green, and never actually widen anything.
func TestValidateEnvValuesRejectsResourceEnvOnMCPBackedType(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		ResourceTypes: []ResourceType{{
			Name:   "issues",
			Config: ResourceTypeConfig{MCP: &MCPResourceConfig{Server: "s"}},
		}},
		Resources: []Resource{{Name: "tracker", Type: "issues", Env: []string{"SECOND_TOKEN"}}},
	}

	err := cfg.validateEnvValues()
	if err == nil {
		t.Fatal("expected env: on an mcp-backed resource to be rejected")
	}

	if !strings.Contains(err.Error(), "mcp-backed") {
		t.Errorf("error = %v, want it to explain the mcp-backed type has no use for env:", err)
	}
}

// An unknown resource type is a different validator's error — this one must
// not itself panic or mask it by reporting something misleading.
func TestValidateEnvValuesToleratesUnknownResourceType(t *testing.T) {
	t.Parallel()

	cfg := &Config{Resources: []Resource{{Name: "tracker", Type: "does-not-exist", Env: []string{"TOKEN"}}}}

	err := cfg.validateEnvValues()
	if err != nil {
		t.Errorf("validateEnvValues: %v, want nil (an unresolvable resource type is caught elsewhere)", err)
	}
}

func TestValidateEnvPlacementRejectsGetAndPut(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		step Step
		want string
	}{
		{"get", Step{Get: "repo", Env: []string{"TOKEN"}}, "not valid on get steps"},
		{"put", Step{Put: "results", Env: []string{"TOKEN"}}, "not valid on put steps"},
		{"try", Step{Try: &Step{Task: "build", Run: "true"}, Env: []string{"TOKEN"}}, "not valid on a try: step"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Jobs: []Job{{Name: "j", Plan: []Step{tc.step}}}}

			err := cfg.rejectOnGetAndPut("env", func(s *Step) bool { return s.Env != nil })
			if err == nil {
				t.Fatalf("expected env: on a %s step to be rejected", tc.name)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestValidateEnvPlacementAllowsEmptyOnTaskAndAgent pins that an explicit
// `env: []` is a legal declaration, not an accidental one — it is how a step
// says "nothing beyond the baseline" over a task that sets env:.
func TestValidateEnvPlacementAllowsEmptyOnTaskAndAgent(t *testing.T) {
	t.Parallel()

	cfg := &Config{Jobs: []Job{{Name: "j", Plan: []Step{
		{Task: "build", Run: "true", Env: []string{}},
		{Agent: "reviewer", Messages: []string{"x"}, Env: []string{}},
	}}}}

	err := cfg.rejectOnGetAndPut("env", func(s *Step) bool { return s.Env != nil })
	if err != nil {
		t.Errorf("validateEnvPlacement: %v", err)
	}
}

// TestResolveTaskEnvOverride covers the declared-wins rule: a step's env:
// replaces its tasks: entry's, and an explicit empty list is a real override
// rather than an absent one. A non-empty test here would silently keep the
// task's variables for a step that asked for none.
func TestResolveTaskEnvOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{Tasks: []Task{{Name: "build", Run: "true", Env: []string{"FROM_TASK"}}}}

	cases := []struct {
		name string
		step Step
		want []string
	}{
		{"inherits when the step declares none", Step{Task: "build"}, []string{"FROM_TASK"}},
		{"step overrides", Step{Task: "build", Env: []string{"FROM_STEP"}}, []string{"FROM_STEP"}},
		{"explicit empty overrides to nothing", Step{Task: "build", Env: []string{}}, []string{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rt, err := cfg.ResolveTask(tc.step)
			if err != nil {
				t.Fatalf("ResolveTask: %v", err)
			}

			if strings.Join(rt.Env, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Env = %v, want %v", rt.Env, tc.want)
			}
		})
	}
}

func TestResolveAgentInvocationEnvOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agents: []Agent{{
		Name:   "reviewer",
		Source: AgentSource{Model: "openai/gpt-4o"},
		Env:    []string{"FROM_AGENT"},
	}}}

	ri, err := cfg.ResolveAgentInvocation(Step{Agent: "reviewer", Messages: []string{"x"}})
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if strings.Join(ri.Env, ",") != "FROM_AGENT" {
		t.Errorf("Env = %v, want [FROM_AGENT]", ri.Env)
	}

	ri, err = cfg.ResolveAgentInvocation(Step{Agent: "reviewer", Messages: []string{"x"}, Env: []string{"FROM_STEP"}})
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if strings.Join(ri.Env, ",") != "FROM_STEP" {
		t.Errorf("Env = %v, want [FROM_STEP]", ri.Env)
	}
}
