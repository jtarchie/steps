package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
    messages:
      - look at it
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
// hook step's message_files: — the walk goes through Job.visitSteps, which
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
      message_files: [prompts/fix.md]
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

	if got, want := hook.Messages, []string{"Fix the failing task.\n"}; !slices.Equal(got, want) {
		t.Errorf("hook.Messages = %q, want %q", got, want)
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
    message_files: [prompts/fix.md]
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

	if got, want := cfg.Tasks[0].Fix.Messages, []string{"Fix it.\n"}; !slices.Equal(got, want) {
		t.Errorf("Tasks[0].Fix.Messages = %q, want %q", got, want)
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

// TestLoadConfigTaskFileDocumentReachesTheStep asserts through ResolveTask, the seam hashing and execution both read, not the merged entry.
func TestLoadConfigTaskFileDocumentReachesTheStep(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
tasks:
- name: unit
  file: ci/unit.yml
jobs:
- name: build
  plan:
  - task: unit
`)
	writeSibling(t, path, "ci/unit.yml", `
run: echo from-document
image: alpine
fix: fixer
inputs: [src]
outputs: [bin]
env: [GOPATH]
network: none
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	resolved, err := cfg.ResolveTask(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveTask: %v", err)
	}

	for field, ok := range map[string]bool{
		"fix":     resolved.Fix != nil && resolved.Fix.Agent == "fixer",
		"inputs":  slices.Equal(resolved.Inputs, []string{"src"}),
		"outputs": slices.Equal(resolved.Outputs, []string{"bin"}),
		"env":     slices.Equal(resolved.Env, []string{"GOPATH"}),
		"network": resolved.Network == "none",
	} {
		if !ok {
			t.Errorf("%s: the document's value did not reach the step: %+v", field, resolved)
		}
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
    messages:
      - look at it
`)
	writeSibling(t, path, "agents/reviewer.yml", `
source: { model: lmstudio/qwen }
system: You are terse.
max_turns: 4
image: alpine
env: [GOPATH]
network: none
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

	if got, want := agent.MaxTurns, 20; got == nil || *got != want {
		t.Errorf("MaxTurns = %v, want %d (the entry's own inline max_turns: must win over the document's)", got, want)
	}

	ri, err := cfg.ResolveAgentInvocation(cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ResolveAgentInvocation: %v", err)
	}

	if !slices.Equal(ri.Env, []string{"GOPATH"}) || ri.Network != "none" {
		t.Errorf("the agent's tools run with env %q on network %q, want the document's [GOPATH] on none", ri.Env, ri.Network)
	}
}

var documentFieldSkips = map[string]bool{"name": true, "file": true, "run_file": true, "system_file": true}

// TestMergeDocumentCarriesEveryField is the guard for the next field: a Task or Agent field the merge forgets decodes from a file: document and is then silently dropped.
func TestMergeDocumentCarriesEveryField(t *testing.T) {
	t.Parallel()

	carries := func(t *testing.T, typ reflect.Type, merge func(doc reflect.Value) reflect.Value) {
		t.Helper()

		for i := range typ.NumField() {
			tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if tag == "" || tag == "-" || documentFieldSkips[tag] {
				continue
			}

			doc := reflect.New(typ).Elem()
			doc.Field(i).Set(nonZeroValue(t, typ.Field(i).Type))

			if got := merge(doc).Field(i); !reflect.DeepEqual(got.Interface(), doc.Field(i).Interface()) {
				t.Errorf("%s.%s: a file: document's value did not reach an entry that set none", typ.Name(), tag)
			}
		}
	}

	carries(t, reflect.TypeFor[Task](), func(doc reflect.Value) reflect.Value {
		var entry Task

		mergeTaskDocument(&entry, doc.Interface().(Task)) //nolint:forcetypeassert // doc is built from reflect.TypeFor[Task]

		return reflect.ValueOf(entry)
	})

	carries(t, reflect.TypeFor[Agent](), func(doc reflect.Value) reflect.Value {
		var entry Agent

		mergeAgentDocument(&entry, doc.Interface().(Agent)) //nolint:forcetypeassert // doc is built from reflect.TypeFor[Agent]

		return reflect.ValueOf(entry)
	})
}

func TestMergeDocumentInlineWins(t *testing.T) {
	t.Parallel()

	one, two := 1, 2

	task := Task{Image: "entry", Fix: &FixSpec{Agent: "entry"}, Outputs: []string{"entry"}}
	mergeTaskDocument(&task, Task{Image: "doc", Fix: &FixSpec{Agent: "doc"}, Outputs: []string{"doc"}, Privileged: true})

	if task.Image != "entry" || task.Fix.Agent != "entry" || !slices.Equal(task.Outputs, []string{"entry"}) {
		t.Errorf("the document overrode the task entry's own fields: %+v", task)
	}

	if !task.Privileged {
		t.Error("privileged: true in the shared document was loosened by an entry that never mentioned it")
	}

	agent := Agent{Description: "entry", MaxTurns: &one, Env: []string{"ENTRY"}}
	mergeAgentDocument(&agent, Agent{Description: "doc", MaxTurns: &two, Env: []string{"DOC"}})

	if agent.Description != "entry" || *agent.MaxTurns != 1 || !slices.Equal(agent.Env, []string{"ENTRY"}) {
		t.Errorf("the document overrode the agent entry's own fields: %+v", agent)
	}
}

// nonZeroValue sets only a struct's first exported field, which is enough to make it non-zero without recursing into self-referential types.
func nonZeroValue(t *testing.T, typ reflect.Type) reflect.Value {
	t.Helper()

	value := reflect.New(typ).Elem()

	switch typ.Kind() { //nolint:exhaustive // default fails, naming a kind a new field introduced
	case reflect.String:
		value.SetString("x")
	case reflect.Int, reflect.Int64:
		value.SetInt(1)
	case reflect.Float64:
		value.SetFloat(0.5)
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Pointer:
		value.Set(reflect.New(typ.Elem()))
		value.Elem().Set(nonZeroValue(t, typ.Elem()))
	case reflect.Slice:
		value.Set(reflect.Append(value, nonZeroValue(t, typ.Elem())))
	case reflect.Struct:
		setFirstExportedField(t, value)
	default:
		t.Fatalf("nonZeroValue: no value for %s — teach it this kind", typ)
	}

	return value
}

func setFirstExportedField(t *testing.T, value reflect.Value) {
	t.Helper()

	for i := range value.NumField() {
		if field := value.Type().Field(i); field.IsExported() {
			value.Field(i).Set(nonZeroValue(t, field.Type))

			return
		}
	}

	t.Fatalf("nonZeroValue: %s has no exported field", value.Type())
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
    message_files: [prompts/review.md]
`)
	writeSibling(t, path, "prompts/review.md", "Review the checked-out code.\n")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]
	if got, want := step.Messages, []string{"Review the checked-out code.\n"}; !slices.Equal(got, want) {
		t.Errorf("Messages = %q, want %q", got, want)
	}

	if step.MessageFiles != nil {
		t.Errorf("PromptFile = %+v, want nil after inlining", step.MessageFiles)
	}
}

// TestLoadConfigStepPromptFileDeferredFormLeftUnresolved proves the
// {artifact, path} mapping form of message_files: is deliberately NOT resolved
// at load time (there is no artifact on disk yet — see FileRef's doc
// comment) and survives LoadConfig untouched, for internal/agent to resolve
// at run time.
func TestLoadConfigStepPromptFileDeferredFormLeftUnresolved(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: mock
  config:
    check: 'echo ''[{"ref":"1"}]'''
    in: "true"
resources:
- name: repo
  type: mock
  source: {}
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - get: repo
  - agent: reviewer
    inputs: [repo]
    message_files: [{ artifact: repo, path: PROMPT.md }]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[1]
	if len(step.Messages) != 0 {
		t.Errorf("Messages = %q, want empty (unresolved at load time)", step.Messages)
	}

	if len(step.MessageFiles) != 1 {
		t.Fatalf("MessageFiles = %+v, want one deferred entry", step.MessageFiles)
	}

	ref := step.MessageFiles[0]
	if !ref.Deferred() {
		t.Fatalf("MessageFiles[0].Deferred() = false, want true")
	}

	if ref.Artifact != "repo" || ref.Path != "PROMPT.md" {
		t.Errorf("MessageFiles[0] = %+v, want {Artifact: repo, Path: PROMPT.md}", ref)
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
    messages:
      - look
    run_file: ci/unit.sh
`,
			sibling: map[string]string{"ci/unit.sh": "echo hi\n"},
			want:    `run_file: is only valid on task steps`,
		},
		{
			name: "message_files on a non-agent step rejected",
			pipeline: `
tasks:
- name: unit
  run: echo hi
jobs:
- name: build
  plan:
  - task: unit
    message_files: [prompts/x.md]
`,
			sibling: map[string]string{"prompts/x.md": "hello\n"},
			want:    `message_files: is only valid on agent steps`,
		},
		{
			name: "prompt and message_files both set",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - inline prompt
    message_files: [prompts/review.md]
`,
			sibling: map[string]string{"prompts/review.md": "file prompt\n"},
			want:    `messages: and message_files: are mutually exclusive`,
		},
		{
			name: "empty message_files entry rejected",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    message_files: [prompts/review.md, ""]
`,
			sibling: map[string]string{"prompts/review.md": "file prompt\n"},
			want:    `message_files: entry 2 is empty`,
		},
		{
			name: "message_files mixing both forms rejected",
			pipeline: `
agents:
- name: reviewer
  source: { model: lmstudio/qwen }
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    message_files: [prompts/review.md, { artifact: repo, path: review.md }]
`,
			sibling: map[string]string{"prompts/review.md": "file prompt\n"},
			want:    `message_files: mixes file paths with {artifact, path} entries`,
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
    messages:
      - hi
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
    messages:
      - build it
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

	// defaults.model is all a bare @builtin/ reference needs: no agents:
	// entry, no per-agent connection string.
	path := writeConfig(t, `
defaults:
  model: lmstudio/qwen
jobs:
- name: build
  plan:
  - agent: "@builtin/reviewer"
    inputs: []
    messages:
      - review the code
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
    messages:
      - review it
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
    message_files: ["@builtin/explorer"]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]

	if len(step.Messages) == 0 {
		t.Fatal("message_files @builtin/explorer resolved to empty string")
	}
}

func TestReadBuiltinAgentProfiles(t *testing.T) {
	t.Parallel()

	names, err := ListBuiltinAgentNames()
	if err != nil {
		t.Fatal(err)
	}

	if len(names) < 4 {
		t.Fatalf("expected at least 4 built-in agent profiles, got %d: %v", len(names), names)
	}

	for _, name := range names {
		agent, err := ReadBuiltinAgent(name)
		if err != nil {
			t.Errorf("ReadBuiltinAgent(%q): %v", name, err)
			continue
		}

		if agent.System == "" && agent.SystemFile == "" {
			t.Errorf("built-in agent %q has no system prompt or system_file", name)
		}

		if agent.MaxTurns == nil {
			t.Errorf("built-in agent %q sets no max_turns", name)
		}
	}
}

func TestReadBuiltinBuilderProfile(t *testing.T) {
	t.Parallel()

	builder, err := ReadBuiltinAgent("builder")
	if err != nil {
		t.Fatalf("ReadBuiltinAgent(builder): %v", err)
	}

	if builder.MaxTurns == nil || *builder.MaxTurns != 50 {
		t.Errorf("builder MaxTurns = %v, want 50", builder.MaxTurns)
	}

	if builder.Description == "" {
		t.Error("builder agent has no description")
	}

	hasExplorer := false
	for _, spec := range builder.Tools {
		if spec.Agent == "@builtin/explorer" {
			hasExplorer = true
			break
		}
	}
	if !hasExplorer {
		t.Error("builder agent is missing explorer sub-agent tool")
	}
}

func TestLoadConfigSubAgentDescriptionResolvedFromChild(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: lead
  source: { model: lmstudio/qwen }
  description: Lead reviewer agent
  tools:
  - read_file
  - agent: helper
- name: helper
  source: { model: lmstudio/qwen }
  description: Helper agent for delegated work
  tools:
  - read_file
jobs:
- name: build
  plan:
  - agent: lead
    inputs: []
    messages:
      - review
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	lead, err := cfg.FindAgent("lead")
	if err != nil {
		t.Fatalf("FindAgent(lead): %v", err)
	}

	for _, spec := range lead.Tools {
		if spec.Agent == "helper" {
			if spec.Description == "" {
				t.Error("sub-agent helper description was not resolved from child agent")
			}
			return
		}
	}

	t.Error("lead agent is missing helper sub-agent tool")
}

func TestLoadConfigSubAgentMissingDescriptionFails(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: lead
  source: { model: lmstudio/qwen }
  tools:
  - read_file
  - agent: helper
- name: helper
  source: { model: lmstudio/qwen }
  tools:
  - read_file
jobs:
- name: build
  plan:
  - agent: lead
    inputs: []
    messages:
      - review
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for sub-agent with no description")
	}

	if !strings.Contains(err.Error(), "no description") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestLoadConfigAgentFileMergesContextCeilings pins that BOTH byte/window
// ceilings survive a file: include. context_window: was merged and
// max_context_bytes: was not — the field sitting next to it on Agent, added in
// the same change — so an included ceiling was silently replaced by the
// compiled-in default, with nothing to say it had been ignored.
func TestLoadConfigAgentFileMergesContextCeilings(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  file: agents/reviewer.yml
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - look at it
`)
	writeSibling(t, path, "agents/reviewer.yml", `
source: { model: lmstudio/qwen }
context_window: 64000
max_context_bytes: 5000
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent := cfg.Agents[0]
	if got, want := agent.ContextWindow, 64_000; got != want {
		t.Errorf("ContextWindow = %d, want %d", got, want)
	}

	if got, want := agent.MaxContextBytes, 5000; got == nil || *got != want {
		t.Errorf("MaxContextBytes = %v, want %d (the document's ceiling was dropped)", got, want)
	}
}

// TestLoadConfigAgentFileMergesEveryDocumentField: a field the merge forgets is silently dropped, not refused — the document decoded it fine.
func TestLoadConfigAgentFileMergesEveryDocumentField(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
agents:
- name: reviewer
  file: agents/reviewer.yml
jobs:
- name: build
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - look at it
`)
	writeSibling(t, path, "agents/reviewer.yml", `
source: { model: lmstudio/qwen }
description: reviews code
image: alpine
temperature: 0.2
top_p: 0.9
max_tokens: 512
reasoning_effort: low
compact_after_tokens: 7000
timeout: 20m
attempts: 3
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	agent := cfg.Agents[0]

	for field, ok := range map[string]bool{
		"description":          agent.Description == "reviews code",
		"image":                agent.Image == "alpine",
		"temperature":          agent.Temperature != nil && *agent.Temperature == 0.2,
		"top_p":                agent.TopP != nil && *agent.TopP == 0.9,
		"max_tokens":           agent.MaxTokens == 512,
		"reasoning_effort":     agent.ReasoningEffort == "low",
		"compact_after_tokens": agent.CompactAfterTokens != nil && *agent.CompactAfterTokens == 7000,
		"timeout":              agent.Timeout == "20m",
		"attempts":             agent.Attempts != nil && *agent.Attempts == 3,
	} {
		if !ok {
			t.Errorf("%s: was not taken from the document: %+v", field, agent)
		}
	}
}
