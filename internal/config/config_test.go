package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// writeConfig writes pipeline to a temp pipeline.yml and returns its path.
func writeConfig(t *testing.T, pipeline string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// wantLoadError loads path and fails unless it errors with a message
// containing want.
func wantLoadError(t *testing.T, path, want string) {
	t.Helper()

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatalf("LoadConfig(%q): expected an error containing %q, got nil", path, want)
	}

	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("LoadConfig(%q) error = %q, want it to contain %q", path, got, want)
	}
}

func TestConfigValidateWorkspaceStrategy(t *testing.T) {
	t.Parallel()

	t.Run("unknown strategy", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: rsync
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, `workspace.strategy "rsync" must be one of copy, btrfs`)
	})

	t.Run("btrfs without root", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: btrfs
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, "workspace.root is required for strategy: btrfs")
	})

	t.Run("compression on copy strategy errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy
  options:
    compression: zstd
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, "workspace.options.compression is only valid for strategy: btrfs")
	})

	t.Run("invalid compression value", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: btrfs
  root: /mnt/btrfs
  options:
    compression: gzip
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)
		wantLoadError(t, path, `workspace.options.compression "gzip" must be one of zstd, lzo, zlib, none`)
	})

	t.Run("valid copy config loads cleanly", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
`)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})
}

func TestConfigValidateArtifactDeclsRequireWorkspace(t *testing.T) {
	t.Parallel()

	t.Run("step inputs without a workspace: block errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo]
`)
		wantLoadError(t, path, "inputs/outputs require a top-level workspace: block")
	})

	t.Run("top-level task outputs without a workspace: block errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
tasks:
- name: build
  run: echo hi
  outputs: [built]

jobs:
- name: build
  plan:
  - task: build
`)
		wantLoadError(t, path, `task "build": inputs/outputs require a top-level workspace: block`)
	})
}

func TestConfigValidateArtifactDeclsStepKindRestrictions(t *testing.T) {
	t.Parallel()

	t.Run("inputs on a get step errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
    inputs: [repo]
`)
		wantLoadError(t, path, "inputs/outputs are not valid on get steps")
	})

	t.Run("outputs on a put step errors", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    out: "true"

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - put: thing
    outputs: [built]
`)
		wantLoadError(t, path, "outputs are not valid on put steps")
	})
}

func TestConfigValidateArtifactNames(t *testing.T) {
	t.Parallel()

	t.Run("invalid name rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: ["../evil"]
`)
		wantLoadError(t, path, "invalid artifact name")
	})

	t.Run("duplicate input name rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo, repo]
`)
		wantLoadError(t, path, `duplicate input "repo"`)
	})

	t.Run("name in both inputs and outputs rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
workspace:
  strategy: copy

jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    inputs: [repo]
    outputs: [repo]
`)
		wantLoadError(t, path, `"repo" cannot be both an input and an output`)
	})
}

// assertResolvedIO fails the test unless rt's inputs/outputs exactly equal
// wantInputs/wantOutputs.
func assertResolvedIO(t *testing.T, rt ResolvedTask, wantInputs, wantOutputs []string) {
	t.Helper()

	if !slicesEqual(rt.Inputs, wantInputs) {
		t.Errorf("inputs = %v, want %v", rt.Inputs, wantInputs)
	}

	if !slicesEqual(rt.Outputs, wantOutputs) {
		t.Errorf("outputs = %v, want %v", rt.Outputs, wantOutputs)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] { //nolint:gosec // len(a) == len(b) is checked above, so i is always in range for b too
			return false
		}
	}

	return true
}

// TestExampleWorkspaceLoadsCleanly guards examples/workspace.yml against
// silently drifting out of sync with the workspace: schema/validation rules
// it's meant to demonstrate.
func TestExampleWorkspaceLoadsCleanly(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(filepath.Join("..", "..", "examples", "workspace.yml"))
	if err != nil {
		t.Fatalf("LoadConfig(examples/workspace.yml): %v", err)
	}
}

// TestResolveTaskInputsOutputsOverride mirrors
// TestRunJobTaskReferenceStepFixOverridesTaskFix (tasks_test.go): a step's
// own inputs:/outputs:, when set (including an explicit empty list),
// override the referenced task's for that step only.
func TestResolveTaskInputsOutputsOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Workspace: &WorkspaceConfig{Strategy: "copy"},
		Tasks: []Task{
			{Name: "build", Run: "echo hi", Inputs: []string{"repo"}, Outputs: []string{"built"}},
		},
	}

	t.Run("step with no inputs/outputs of its own inherits the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build"})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{"repo"}, []string{"built"})
	})

	t.Run("step's own explicit empty inputs overrides the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build", Inputs: []string{}})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{}, []string{"built"})
	})

	t.Run("step's own outputs overrides the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build", Outputs: []string{"artifact"}})
		if err != nil {
			t.Fatal(err)
		}

		assertResolvedIO(t, rt, []string{"repo"}, []string{"artifact"})
	})
}

func TestFindAgent(t *testing.T) {
	t.Parallel()

	cfg := &Config{Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "openai/gpt-4o"}}}}

	_, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Errorf("FindAgent(reviewer): %v", err)
	}

	_, err = cfg.FindAgent("nope")
	if err == nil {
		t.Error("expected an error for a missing agent")
	}
}

func TestResolveAgentTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                     string
		source                   AgentSource
		wantBaseURL              string
		wantModel                string
		wantAPIKeyEnv            string
		wantRequiresKey          bool
		wantStringOnlyToolChoice bool
		wantErr                  bool
	}{
		{ //nolint:gosec // wantAPIKeyEnv values are env-var *names*, not credential values
			name:            "openrouter prefix with slashed model id",
			source:          AgentSource{Model: "openrouter/anthropic/claude-3.5-sonnet"},
			wantBaseURL:     "https://openrouter.ai/api/v1/",
			wantModel:       "anthropic/claude-3.5-sonnet",
			wantAPIKeyEnv:   "OPENROUTER_API_KEY",
			wantRequiresKey: true,
			// cloud provider: default to the precise named tool_choice.
			wantStringOnlyToolChoice: false,
		},
		{
			name:            "lmstudio prefix requires no key",
			source:          AgentSource{Model: "lmstudio/qwen2.5-coder"},
			wantBaseURL:     "http://localhost:1234/v1/",
			wantModel:       "qwen2.5-coder",
			wantAPIKeyEnv:   "",
			wantRequiresKey: false,
			// local/no-auth provider: default to the string-only fallback.
			wantStringOnlyToolChoice: true,
		},
		{
			name:                     "ollama prefix requires no key",
			source:                   AgentSource{Model: "ollama/llama3.1"},
			wantBaseURL:              "http://localhost:11434/v1/",
			wantModel:                "llama3.1",
			wantAPIKeyEnv:            "",
			wantRequiresKey:          false,
			wantStringOnlyToolChoice: true,
		},
		{
			name:    "bare model with no endpoint errors",
			source:  AgentSource{Model: "gpt-4o"},
			wantErr: true,
		},
		{
			name:                     "explicit endpoint and api_key_env override derived values",
			source:                   AgentSource{Model: "openrouter/anthropic/claude-3.5-sonnet", Endpoint: "https://gateway.internal/v1", APIKeyEnv: "CUSTOM_KEY"},
			wantBaseURL:              "https://gateway.internal/v1/",
			wantModel:                "anthropic/claude-3.5-sonnet",
			wantAPIKeyEnv:            "CUSTOM_KEY",
			wantRequiresKey:          true,
			wantStringOnlyToolChoice: false,
		},
		{
			name:                     "unrecognized prefix requires explicit endpoint",
			source:                   AgentSource{Model: "foo/bar", Endpoint: "https://foo.example/v1/"},
			wantBaseURL:              "https://foo.example/v1/",
			wantModel:                "foo/bar",
			wantAPIKeyEnv:            "",
			wantRequiresKey:          false,
			wantStringOnlyToolChoice: false,
		},
		{ //nolint:gosec // wantAPIKeyEnv values are env-var *names*, not credential values
			name:                     "explicit string_tool_choice true overrides cloud default",
			source:                   AgentSource{Model: "openrouter/anthropic/claude-3.5-sonnet", StringToolChoice: boolPtr(true)},
			wantBaseURL:              "https://openrouter.ai/api/v1/",
			wantModel:                "anthropic/claude-3.5-sonnet",
			wantAPIKeyEnv:            "OPENROUTER_API_KEY",
			wantRequiresKey:          true,
			wantStringOnlyToolChoice: true,
		},
		{
			name:                     "explicit string_tool_choice false overrides local default",
			source:                   AgentSource{Model: "lmstudio/qwen2.5-coder", StringToolChoice: boolPtr(false)},
			wantBaseURL:              "http://localhost:1234/v1/",
			wantModel:                "qwen2.5-coder",
			wantAPIKeyEnv:            "",
			wantRequiresKey:          false,
			wantStringOnlyToolChoice: false,
		},
		{
			name:                     "explicit string_tool_choice true on an unrecognized prefix",
			source:                   AgentSource{Model: "foo/bar", Endpoint: "https://foo.example/v1/", StringToolChoice: boolPtr(true)},
			wantBaseURL:              "https://foo.example/v1/",
			wantModel:                "foo/bar",
			wantAPIKeyEnv:            "",
			wantRequiresKey:          false,
			wantStringOnlyToolChoice: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			baseURL, modelName, apiKeyEnv, requiresKey, stringOnlyToolChoice, err := resolveAgentTarget(tt.source)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveAgentTarget: %v", err)
			}

			if baseURL != tt.wantBaseURL {
				t.Errorf("baseURL = %q, want %q", baseURL, tt.wantBaseURL)
			}

			if modelName != tt.wantModel {
				t.Errorf("modelName = %q, want %q", modelName, tt.wantModel)
			}

			if apiKeyEnv != tt.wantAPIKeyEnv {
				t.Errorf("apiKeyEnv = %q, want %q", apiKeyEnv, tt.wantAPIKeyEnv)
			}

			if requiresKey != tt.wantRequiresKey {
				t.Errorf("requiresKey = %v, want %v", requiresKey, tt.wantRequiresKey)
			}

			if stringOnlyToolChoice != tt.wantStringOnlyToolChoice {
				t.Errorf("stringOnlyToolChoice = %v, want %v", stringOnlyToolChoice, tt.wantStringOnlyToolChoice)
			}
		})
	}
}

// boolPtr returns a pointer to b, for AgentSource.StringToolChoice test
// fixtures that need to distinguish "unset" from an explicit false.
func boolPtr(b bool) *bool { return &b }

//nolint:gochecknoglobals // shared read-only fixture for the effective-tools tests
var effectiveToolsAgentGrant = []ToolSpec{
	{Builtin: "read_file"},
	{Builtin: "list_dir"},
	{Name: "post_review", Run: "gh pr review {{ .args.action }}"},
}

func TestResolveEffectiveToolsSelection(t *testing.T) {
	t.Parallel()

	t.Run("empty step selection inherits all agent tools", func(t *testing.T) {
		t.Parallel()

		got, err := resolveEffectiveTools(effectiveToolsAgentGrant, nil)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 3 {
			t.Errorf("got %d tools, want 3", len(got))
		}
	})

	t.Run("step selects a granted subset by name", func(t *testing.T) {
		t.Parallel()

		got, err := resolveEffectiveTools(effectiveToolsAgentGrant, []ToolSpec{{Builtin: "read_file"}, {Builtin: "post_review"}})
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 2 {
			t.Fatalf("got %d tools, want 2", len(got))
		}

		if got[1].Name != "post_review" || got[1].Run == "" {
			t.Errorf("expected the agent's post_review definition, got %+v", got[1])
		}
	})

	t.Run("empty agent grant treats all built-ins as granted", func(t *testing.T) {
		t.Parallel()

		got, err := resolveEffectiveTools(nil, []ToolSpec{{Builtin: "run_shell"}})
		if err != nil {
			t.Fatalf("a step should be able to select a built-in when the agent grants none explicitly: %v", err)
		}

		if len(got) != 1 || got[0].Builtin != "run_shell" {
			t.Errorf("got %+v, want [run_shell]", got)
		}
	})
}

func TestResolveEffectiveToolsBoundary(t *testing.T) {
	t.Parallel()

	t.Run("step cannot select a tool the agent did not grant", func(t *testing.T) {
		t.Parallel()

		// agent grants no run_shell; a read-only step must not be able to add it
		_, err := resolveEffectiveTools([]ToolSpec{{Builtin: "read_file"}}, []ToolSpec{{Builtin: "run_shell"}})
		if err == nil {
			t.Error("expected an error selecting a tool outside the agent's grant")
		}
	})

	t.Run("step may add an inline custom tool even under a restricted agent", func(t *testing.T) {
		t.Parallel()

		got, err := resolveEffectiveTools(
			[]ToolSpec{{Builtin: "read_file"}},
			[]ToolSpec{{Builtin: "read_file"}, {Name: "notify", Run: "echo {{ .args.msg }}"}},
		)
		if err != nil {
			t.Fatal(err)
		}

		if len(got) != 2 || got[1].Name != "notify" {
			t.Errorf("expected an inline notify tool to be allowed, got %+v", got)
		}
	})
}

func TestResolveAgentInvocation(t *testing.T) {
	t.Parallel()

	baseCfg := func(agent Agent) *Config {
		return &Config{Agents: []Agent{agent}}
	}

	t.Run("step sets its own attempts; agent max_turns applies", func(t *testing.T) {
		t.Parallel()

		cfg := baseCfg(Agent{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}, MaxTurns: 20})

		ri, err := cfg.ResolveAgentInvocation(Step{Agent: "a", Attempts: 2})
		if err != nil {
			t.Fatal(err)
		}

		if ri.Attempts != 2 {
			t.Errorf("attempts = %d, want 2 (step value)", ri.Attempts)
		}

		if ri.MaxTurns != 20 {
			t.Errorf("maxTurns = %d, want 20 (agent value)", ri.MaxTurns)
		}
	})

	t.Run("defaults apply when unset", func(t *testing.T) {
		t.Parallel()

		cfg := baseCfg(Agent{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}})

		ri, err := cfg.ResolveAgentInvocation(Step{Agent: "a"})
		if err != nil {
			t.Fatal(err)
		}

		if ri.Attempts != 1 {
			t.Errorf("attempts = %d, want 1", ri.Attempts)
		}

		if ri.MaxTurns != defaultMaxAgentTurns {
			t.Errorf("maxTurns = %d, want %d", ri.MaxTurns, defaultMaxAgentTurns)
		}
	})

	t.Run("invalid reasoning_effort errors", func(t *testing.T) {
		t.Parallel()

		cfg := baseCfg(Agent{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}, ReasoningEffort: "turbo"})

		_, err := cfg.ResolveAgentInvocation(Step{Agent: "a"})
		if err == nil {
			t.Error("expected an error for an invalid reasoning_effort")
		}
	})
}

func TestToolSpecUnmarshalYAML(t *testing.T) {
	t.Parallel()

	const doc = `
tools:
- read_file
- name: post_review
  description: post a review
  run: gh pr review {{ .args.action }}
`

	var v struct {
		Tools []ToolSpec `yaml:"tools"`
	}

	err := yaml.Unmarshal([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}

	if len(v.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(v.Tools))
	}

	if v.Tools[0].Builtin != "read_file" {
		t.Errorf("Tools[0].Builtin = %q, want %q", v.Tools[0].Builtin, "read_file")
	}

	if v.Tools[1].Name != "post_review" || v.Tools[1].Run != "gh pr review {{ .args.action }}" {
		t.Errorf("Tools[1] = %+v, want a custom post_review tool", v.Tools[1])
	}
}

func TestToolSpecUnmarshalYAMLInvalid(t *testing.T) {
	t.Parallel()

	const doc = `
tools:
- [not, a, valid, entry]
`

	var v struct {
		Tools []ToolSpec `yaml:"tools"`
	}

	err := yaml.Unmarshal([]byte(doc), &v)
	if err == nil {
		t.Error("expected an error for a sequence-node tool entry")
	}
}

func TestFixSpecUnmarshalScalar(t *testing.T) {
	t.Parallel()

	var v struct {
		Fix *FixSpec `yaml:"fix"`
	}

	err := yaml.Unmarshal([]byte("fix: fixer\n"), &v)
	if err != nil {
		t.Fatal(err)
	}

	if v.Fix == nil || v.Fix.Agent != "fixer" {
		t.Fatalf("Fix = %+v, want agent=fixer", v.Fix)
	}
}

func TestFixSpecUnmarshalMapping(t *testing.T) {
	t.Parallel()

	const doc = `
fix:
  agent: fixer
  prompt: only touch parser.go
  dir: repo
  attempts: 2
  tools: [read_file, run_shell]
`

	var v struct {
		Fix *FixSpec `yaml:"fix"`
	}

	err := yaml.Unmarshal([]byte(doc), &v)
	if err != nil {
		t.Fatal(err)
	}

	got := v.Fix
	if got == nil {
		t.Fatal("Fix is nil")
	}

	if got.Agent != "fixer" || got.Prompt != "only touch parser.go" || got.Dir != "repo" || got.Attempts != 2 {
		t.Errorf("Fix = %+v, want the mapping's values", got)
	}

	if len(got.Tools) != 2 || got.Tools[0].Builtin != "read_file" || got.Tools[1].Builtin != "run_shell" {
		t.Errorf("Fix.Tools = %+v, want [read_file run_shell]", got.Tools)
	}
}

func TestFixSpecUnmarshalSequenceErrors(t *testing.T) {
	t.Parallel()

	var v struct {
		Fix *FixSpec `yaml:"fix"`
	}

	err := yaml.Unmarshal([]byte("fix: [a, b]\n"), &v)
	if err == nil {
		t.Error("expected an error for a sequence-node fix entry")
	}
}

func TestStableStrings(t *testing.T) {
	t.Parallel()

	got := StableStrings(nil)
	if got == nil || len(got) != 0 {
		t.Errorf("StableStrings(nil) = %#v, want a non-nil empty slice", got)
	}

	in := []string{"a", "b"}
	out := StableStrings(in)
	out[0] = "mutated"

	if in[0] != "a" {
		t.Error("StableStrings did not return an independent copy")
	}
}

func TestValidateArtifactName(t *testing.T) {
	t.Parallel()

	valid := []string{"repo", "built-output", "a.b_c", "R2D2"}
	for _, name := range valid {
		err := ValidateArtifactName(name)
		if err != nil {
			t.Errorf("ValidateArtifactName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{"", "..", "../evil", "a/b", ".hidden", "-leading-dash", "with space"}
	for _, name := range invalid {
		err := ValidateArtifactName(name)
		if err == nil {
			t.Errorf("ValidateArtifactName(%q) = nil, want an error", name)
		}
	}
}

func TestLoadConfigAndLookups(t *testing.T) {
	t.Parallel()

	const pipeline = `
resource_types:
- name: pull-request
  config:
    check: gh pr list
resources:
- name: prs
  type: pull-request
  source:
    repo: jtarchie/ci
jobs:
- name: review
  plan:
  - get: prs
  - task: review
    run: echo hi
`

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	if got := cfg.JobNames(); !slicesEqual(got, []string{"review"}) {
		t.Errorf("JobNames() = %v, want [review]", got)
	}

	_, err = cfg.FindResource("prs")
	if err != nil {
		t.Errorf("FindResource(prs): %v", err)
	}

	_, err = cfg.FindResourceType("pull-request")
	if err != nil {
		t.Errorf("FindResourceType(pull-request): %v", err)
	}

	_, err = cfg.FindJob("review")
	if err != nil {
		t.Errorf("FindJob(review): %v", err)
	}

	missingLookups := []func() error{
		func() error { _, err := cfg.FindResource("nope"); return err },
		func() error { _, err := cfg.FindResourceType("nope"); return err },
		func() error { _, err := cfg.FindJob("nope"); return err },
	}
	for _, lookup := range missingLookups {
		err := lookup()
		if err == nil {
			t.Error("expected error looking up a missing name")
		}
	}
}

func TestLoadConfigAgentToolRequired(t *testing.T) {
	t.Parallel()

	const pipeline = `
agents:
- name: reviewer
  source:
    model: lmstudio/qwen2.5-coder
  tools:
  - name: post_review
    description: Post a review.
    run: gh pr review --{{ .args.action }}
    required: true
  - name: notify
    description: Not required.
    run: echo hi
jobs:
- name: review
  plan:
  - agent: reviewer
    prompt: go
`

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	agent, err := cfg.FindAgent("reviewer")
	if err != nil {
		t.Fatal(err)
	}

	required := make(map[string]bool, len(agent.Tools))
	for _, tool := range agent.Tools {
		required[tool.Name] = tool.Required
	}

	if !required["post_review"] {
		t.Error("post_review.Required = false, want true")
	}

	if required["notify"] {
		t.Error("notify.Required = true, want false")
	}
}

func TestLoadConfigErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()

		_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.yml"))
		if err == nil {
			t.Error("expected error for missing file")
		}
	})

	t.Run("invalid yaml", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "bad.yml")

		err := os.WriteFile(path, []byte("jobs: [oops"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		_, err = LoadConfig(path)
		if err == nil {
			t.Error("expected error for invalid YAML")
		}
	})
}

func TestResolveTaskImageOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Tasks: []Task{{Name: "build", Run: "echo hi", Image: "golang:1.26"}},
	}

	t.Run("inline step (its own run:) uses its own image, never consulting tasks:", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build", Run: "echo inline", Image: "alpine"})
		if err != nil {
			t.Fatal(err)
		}

		if rt.Image != "alpine" {
			t.Errorf("image = %q, want %q", rt.Image, "alpine")
		}
	})

	t.Run("referenced step with no image of its own inherits the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build"})
		if err != nil {
			t.Fatal(err)
		}

		if rt.Image != "golang:1.26" {
			t.Errorf("image = %q, want %q (inherited from the task)", rt.Image, "golang:1.26")
		}
	})

	t.Run("referenced step's own image overrides the task's", func(t *testing.T) {
		t.Parallel()

		rt, err := cfg.ResolveTask(Step{Task: "build", Image: "alpine"})
		if err != nil {
			t.Fatal(err)
		}

		if rt.Image != "alpine" {
			t.Errorf("image = %q, want %q (step override)", rt.Image, "alpine")
		}
	})

	t.Run("no image anywhere resolves to empty (host execution)", func(t *testing.T) {
		t.Parallel()

		noImageCfg := &Config{Tasks: []Task{{Name: "plain", Run: "echo hi"}}}

		rt, err := noImageCfg.ResolveTask(Step{Task: "plain"})
		if err != nil {
			t.Fatal(err)
		}

		if rt.Image != "" {
			t.Errorf("image = %q, want empty", rt.Image)
		}
	})
}

func TestResolveAgentInvocationImageOverride(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Agents: []Agent{{Name: "reviewer", Source: AgentSource{Model: "openai/gpt-4o"}, Image: "python:3.12"}},
	}

	t.Run("step with no image of its own inherits the agent's", func(t *testing.T) {
		t.Parallel()

		ri, err := cfg.ResolveAgentInvocation(Step{Agent: "reviewer"})
		if err != nil {
			t.Fatal(err)
		}

		if ri.Image != "python:3.12" {
			t.Errorf("image = %q, want %q (inherited from the agent)", ri.Image, "python:3.12")
		}
	})

	t.Run("step's own image overrides the agent's", func(t *testing.T) {
		t.Parallel()

		ri, err := cfg.ResolveAgentInvocation(Step{Agent: "reviewer", Image: "node:22"})
		if err != nil {
			t.Fatal(err)
		}

		if ri.Image != "node:22" {
			t.Errorf("image = %q, want %q (step override)", ri.Image, "node:22")
		}
	})

	t.Run("no image anywhere resolves to empty (host execution)", func(t *testing.T) {
		t.Parallel()

		noImageCfg := &Config{Agents: []Agent{{Name: "a", Source: AgentSource{Model: "openai/gpt-4o"}}}}

		ri, err := noImageCfg.ResolveAgentInvocation(Step{Agent: "a"})
		if err != nil {
			t.Fatal(err)
		}

		if ri.Image != "" {
			t.Errorf("image = %q, want empty", ri.Image)
		}
	})
}

func TestUsesImages(t *testing.T) {
	t.Parallel()

	t.Run("no image anywhere", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{
			ResourceTypes: []ResourceType{{Name: "git"}},
			Tasks:         []Task{{Name: "build", Run: "echo hi"}},
			Jobs:          []Job{{Name: "main", Plan: []Step{{Task: "build"}}}},
		}

		if cfg.UsesImages() {
			t.Error("UsesImages() = true, want false")
		}
	})

	t.Run("resource_type image", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{ResourceTypes: []ResourceType{{Name: "git", Image: "alpine/git"}}}

		if !cfg.UsesImages() {
			t.Error("UsesImages() = false, want true (resource_type sets image)")
		}
	})

	t.Run("agent image", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Agents: []Agent{{Name: "a", Image: "python:3.12"}}}

		if !cfg.UsesImages() {
			t.Error("UsesImages() = false, want true (agent sets image)")
		}
	})

	t.Run("top-level task image", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Tasks: []Task{{Name: "build", Image: "golang:1.26"}}}

		if !cfg.UsesImages() {
			t.Error("UsesImages() = false, want true (task sets image)")
		}
	})

	t.Run("step-level image only", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Jobs: []Job{{Name: "main", Plan: []Step{{Task: "t", Run: "echo hi", Image: "alpine"}}}}}

		if !cfg.UsesImages() {
			t.Error("UsesImages() = false, want true (step sets image)")
		}
	})
}

func TestValidateImagesRejectsGetAndPutSteps(t *testing.T) {
	t.Parallel()

	t.Run("image on a get step is rejected", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Jobs: []Job{{Name: "main", Plan: []Step{{Get: "repo", Image: "alpine"}}}}}

		err := cfg.validateImages()
		if err == nil {
			t.Error("expected an error for image: on a get step")
		}
	})

	t.Run("image on a put step is rejected", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Jobs: []Job{{Name: "main", Plan: []Step{{Put: "repo", Image: "alpine"}}}}}

		err := cfg.validateImages()
		if err == nil {
			t.Error("expected an error for image: on a put step")
		}
	})

	t.Run("image on a task step is fine", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Jobs: []Job{{Name: "main", Plan: []Step{{Task: "t", Run: "echo hi", Image: "alpine"}}}}}

		err := cfg.validateImages()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("image on an agent step is fine", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Jobs: []Job{{Name: "main", Plan: []Step{{Agent: "a", Image: "alpine"}}}}}

		err := cfg.validateImages()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestValidateFixAgentImages(t *testing.T) {
	t.Parallel()

	t.Run("a fix agent with its own image is rejected, from a top-level task", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{
			Agents: []Agent{{Name: "fixer", Image: "python:3.12"}},
			Tasks:  []Task{{Name: "build", Run: "echo hi", Fix: &FixSpec{Agent: "fixer"}}},
		}

		err := cfg.validateFixAgentImages()
		if err == nil {
			t.Error("expected an error for a fix agent with its own image:")
		}
	})

	t.Run("a fix agent with its own image is rejected, from a step-level fix override", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{
			Agents: []Agent{{Name: "fixer", Image: "python:3.12"}},
			Jobs: []Job{{Name: "main", Plan: []Step{
				{Task: "t", Run: "echo hi", Fix: &FixSpec{Agent: "fixer"}},
			}}},
		}

		err := cfg.validateFixAgentImages()
		if err == nil {
			t.Error("expected an error for a fix agent with its own image:")
		}
	})

	t.Run("a fix agent with no image of its own is fine", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{
			Agents: []Agent{{Name: "fixer"}},
			Tasks:  []Task{{Name: "build", Run: "echo hi", Fix: &FixSpec{Agent: "fixer"}}},
		}

		err := cfg.validateFixAgentImages()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("no fix: anywhere is fine", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Tasks: []Task{{Name: "build", Run: "echo hi"}}}

		err := cfg.validateFixAgentImages()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("a fix: agent name that doesn't resolve is left for run-time FindAgent, not rejected here", func(t *testing.T) {
		t.Parallel()

		cfg := &Config{Tasks: []Task{{Name: "build", Run: "echo hi", Fix: &FixSpec{Agent: "nonexistent"}}}}

		err := cfg.validateFixAgentImages()
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})
}
