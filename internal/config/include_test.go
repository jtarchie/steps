package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSibling writes body to a file named name in the same directory as
// pipelinePath (as returned by writeConfig).
func writeSibling(t *testing.T, pipelinePath, name, body string) {
	t.Helper()

	path := filepath.Join(filepath.Dir(pipelinePath), name)

	err := os.MkdirAll(filepath.Dir(path), 0o755) //nolint:gosec // test fixture directory
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRunFileInlinesTaskRun(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: unit
  run_file: ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`)
	writeSibling(t, path, "ci/unit.sh", "echo from-file\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got, want := cfg.Tasks[0].Run, "echo from-file\n"; got != want {
		t.Errorf("Tasks[0].Run = %q, want %q", got, want)
	}
}

// TestLoadConfigRunFileOnStepInlinesAndStaysInline proves a step's own
// run_file: still short-circuits ResolveTask's inline path (step.Run != "")
// even when a same-named tasks: entry exists — the same guarantee an inline
// run: already gets.
func TestLoadConfigRunFileOnStepInlinesAndStaysInline(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: unit
  run: echo from-tasks-entry
jobs:
- name: build
  plan:
  - task: unit
    run_file: ci/unit.sh
`)
	writeSibling(t, path, "ci/unit.sh", "echo from-step-file\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	rt, err := cfg.ResolveTask(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	if got, want := rt.Run, "echo from-step-file\n"; got != want {
		t.Errorf("ResolveTask(...).Run = %q, want %q (the step's own run_file:, not the tasks: entry)", got, want)
	}
}

func TestLoadConfigSystemFileInlinesPersona(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
  system_file: prompts/reviewer.md
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: look at it
`)
	writeSibling(t, path, "prompts/reviewer.md", "You are a terse reviewer.\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if got, want := ri.Persona, "You are a terse reviewer.\n"; got != want {
		t.Errorf("Persona = %q, want %q", got, want)
	}
}

// TestLoadConfigPromptFileInHook proves resolveFileIncludes' walk reaches a
// hook step's prompt_file: — the walk goes through Job.visitSteps, which
// recurses into Hooks.Each and hands out *Step pointers, so a mutation there
// must land on the same backing Step the hook actually runs.
func TestLoadConfigPromptFileInHook(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - task: unit
    inputs: []
    run: exit 1
    on_failure:
      agent: fixer
      inputs: []
      prompt_file: prompts/fix.md
`)
	writeSibling(t, path, "prompts/fix.md", "Fix the failing task.\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	hook := cfg.Jobs[0].Plan[0].Hooks.OnFailure
	if hook == nil {
		t.Fatal("on_failure hook is nil")
	}

	if got, want := hook.Prompt, "Fix the failing task.\n"; got != want {
		t.Errorf("hook.Prompt = %q, want %q", got, want)
	}
}

func TestLoadConfigPromptFileInFix(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
tasks:
- name: unit
  run: exit 1
  fix:
    agent: fixer
    prompt_file: prompts/fix.md
jobs:
- name: build
  plan:
  - task: unit
`)
	writeSibling(t, path, "prompts/fix.md", "Fix it.\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got, want := cfg.Tasks[0].Fix.Prompt, "Fix it.\n"; got != want {
		t.Errorf("Tasks[0].Fix.Prompt = %q, want %q", got, want)
	}
}

func TestLoadConfigTaskFileWholeDocument(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: unit
  file: ci/unit.yml
  image: alpine
jobs:
- name: build
  plan:
  - task: unit
`)
	writeSibling(t, path, "ci/unit.yml", `
run: echo from-document
image: golang:1.26
timeout: 30s
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	task := cfg.Tasks[0]
	if got, want := task.Run, "echo from-document"; got != want {
		t.Errorf("Run = %q, want %q", got, want)
	}

	if got, want := task.Image, "alpine"; got != want {
		t.Errorf("Image = %q, want %q (the entry's own inline image: must win over the document's)", got, want)
	}

	if got, want := task.Timeout, "30s"; got != want {
		t.Errorf("Timeout = %q, want %q (from the document, since the entry declared none)", got, want)
	}
}

func TestLoadConfigAgentFileWholeDocument(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  file: agents/reviewer.yml
  max_turns: 20
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: look at it
`)
	writeSibling(t, path, "agents/reviewer.yml", `
source: { model: lmstudio/qwen }
system: You are terse.
max_turns: 4
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent := cfg.Agents[0]
	if got, want := agent.Source.Model, "lmstudio/qwen"; got != want {
		t.Errorf("Source.Model = %q, want %q", got, want)
	}

	if got, want := agent.System, "You are terse."; got != want {
		t.Errorf("System = %q, want %q", got, want)
	}

	if got, want := agent.MaxTurns, 20; got != want {
		t.Errorf("MaxTurns = %d, want %d (the entry's own inline max_turns: must win over the document's)", got, want)
	}
}

func TestLoadConfigStepPromptFileScalarInlines(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt_file: prompts/review.md
`)
	writeSibling(t, path, "prompts/review.md", "Review the checked-out code.\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]
	if got, want := step.Prompt, "Review the checked-out code.\n"; got != want {
		t.Errorf("Prompt = %q, want %q", got, want)
	}

	if step.PromptFile != nil {
		t.Errorf("PromptFile = %+v, want nil after inlining", step.PromptFile)
	}
}

// TestLoadConfigStepPromptFileDeferredFormLeftUnresolved proves the
// {artifact, path} mapping form of prompt_file: is deliberately NOT resolved
// at load time (there is no artifact on disk yet — see FileRef's doc
// comment) and survives LoadConfig untouched, for internal/agent to resolve
// at run time.
func TestLoadConfigStepPromptFileDeferredFormLeftUnresolved(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - get: repo
  - agent: reviewer
    inputs: [repo]
    prompt_file: { artifact: repo, path: PROMPT.md }
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[1]
	if step.Prompt != "" {
		t.Errorf("Prompt = %q, want empty (unresolved at load time)", step.Prompt)
	}

	if !step.PromptFile.Deferred() {
		t.Fatalf("PromptFile.Deferred() = false, want true")
	}

	if step.PromptFile.Artifact != "repo" || step.PromptFile.Path != "PROMPT.md" {
		t.Errorf("PromptFile = %+v, want {Artifact: repo, Path: PROMPT.md}", step.PromptFile)
	}
}

func TestLoadConfigRunFileParentDirectory(t *testing.T) {
	t.Parallel()

	// A shared ../tasks/ directory next to a pipelines/ directory is a
	// legitimate layout: the pipeline file is trusted input, so ".." is
	// deliberately not confined here (contrast internal/agent's
	// resolveAgentPath, which confines a *model*-supplied path). This test
	// pins that decision so it isn't "hardened" away later.
	root := t.TempDir()

	sharedDir := filepath.Join(root, "shared")

	err := os.MkdirAll(sharedDir, 0o755) //nolint:gosec // test fixture directory
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(sharedDir, "build.sh"), []byte("echo shared\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	pipelineDir := filepath.Join(root, "pipelines")

	err = os.MkdirAll(pipelineDir, 0o755) //nolint:gosec // test fixture directory
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(pipelineDir, "pipeline.yml")

	err = os.WriteFile(path, []byte(`
tasks:
- name: unit
  run_file: ../shared/build.sh
jobs:
- name: build
  plan:
  - task: unit
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got, want := cfg.Tasks[0].Run, "echo shared\n"; got != want {
		t.Errorf("Run = %q, want %q", got, want)
	}
}

func TestLoadConfigRunFileConcourseHabitHint(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: unit
  run_file: repo/ci/build.sh
jobs:
- name: build
  plan:
  - task: unit
`)

	wantLoadError(t, path, "a path naming a fetched artifact is not supported here")
}

func TestLoadConfigFileIncludeErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		sibling  map[string]string
		want     string
	}{
		{
			name: "run and run_file both set",
			pipeline: `
tasks:
- name: unit
  run: echo inline
  run_file: ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`,
			sibling: map[string]string{"ci/unit.sh": "echo file\n"},
			want:    `run: and run_file: are mutually exclusive`,
		},
		{
			name: "absolute path rejected",
			pipeline: `
tasks:
- name: unit
  run_file: /etc/passwd
jobs:
- name: build
  plan:
  - task: unit
`,
			want: `must be a path relative to the pipeline file's directory`,
		},
		{
			name: "empty included file rejected",
			pipeline: `
tasks:
- name: unit
  run_file: ci/unit.sh
jobs:
- name: build
  plan:
  - task: unit
`,
			sibling: map[string]string{"ci/unit.sh": "   \n"},
			want:    `is empty`,
		},
		{
			name: "run_file on a non-task step rejected",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: look
    run_file: ci/unit.sh
`,
			sibling: map[string]string{"ci/unit.sh": "echo hi\n"},
			want:    `run_file: is only valid on task steps`,
		},
		{
			name: "prompt_file on a non-agent step rejected",
			pipeline: `
tasks:
- name: unit
  run: echo hi
jobs:
- name: build
  plan:
  - task: unit
    prompt_file: prompts/x.md
`,
			sibling: map[string]string{"prompts/x.md": "hello\n"},
			want:    `prompt_file: is only valid on agent steps`,
		},
		{
			name: "prompt and prompt_file both set",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: inline prompt
    prompt_file: prompts/review.md
`,
			sibling: map[string]string{"prompts/review.md": "file prompt\n"},
			want:    `prompt: and prompt_file: are mutually exclusive`,
		},
		{
			name: "nested file: in an included task document rejected",
			pipeline: `
tasks:
- name: unit
  file: ci/unit.yml
jobs:
- name: build
  plan:
  - task: unit
`,
			sibling: map[string]string{"ci/unit.yml": "run_file: ci/nested.sh\n"},
			want:    `an included task document may not itself set file or run_file`,
		},
		{
			name: "nested file: in an included agent document rejected",
			pipeline: `
agents:
- name: reviewer
  file: agents/reviewer.yml
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    prompt: hi
`,
			sibling: map[string]string{"agents/reviewer.yml": "source: { model: lmstudio/qwen }\nsystem_file: p.md\n"},
			want:    `an included agent document may not itself set file or system_file`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			for name, body := range tc.sibling {
				writeSibling(t, path, name, body)
			}

			wantLoadError(t, path, tc.want)
		})
	}
}

func TestLoadConfigBuiltinPrompt(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: coder
  source: { model: lmstudio/qwen }
  system_file: "@builtin/builder"
jobs:
- name: build
  plan:
  - agent: coder
    inputs: []
    prompt: build it
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if ri.Persona == "" {
		t.Fatal("built-in prompt resolved to empty string")
	}
}

func TestLoadConfigBuiltinAgentRegistration(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: build
  plan:
  - agent: "@builtin/reviewer"
    inputs: []
    prompt: review the code
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	a, err := cfg.FindAgent("@builtin/reviewer")
	if err != nil {
		t.Fatalf("FindAgent(@builtin/reviewer): %v", err)
	}

	if a.System == "" {
		t.Fatal("built-in agent has empty system persona")
	}

	if len(a.Tools) == 0 {
		t.Fatal("built-in agent has no tool specs")
	}
}

func TestLoadConfigBuiltinAgentUserOverride(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: "@builtin/reviewer"
  source: { model: openrouter/anthropic/claude-3.5-sonnet }
  max_turns: 42
jobs:
- name: build
  plan:
  - agent: "@builtin/reviewer"
    inputs: []
    prompt: review it
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if ri.AgentName != "@builtin/reviewer" {
		t.Errorf("AgentName = %q, want @builtin/reviewer", ri.AgentName)
	}

	if ri.MaxTurns != 42 {
		t.Errorf("MaxTurns = %d, want 42 (user override)", ri.MaxTurns)
	}

	if ri.BaseURL == "" {
		t.Error("BaseURL = empty, expected openrouter override")
	}
}

func TestLoadConfigBuiltinPromptOnStep(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: coder
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: coder
    inputs: []
    prompt_file: "@builtin/explorer"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]

	if step.Prompt == "" {
		t.Fatal("prompt_file @builtin/explorer resolved to empty string")
	}
}
