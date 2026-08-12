package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A key no field claims is a load error everywhere it can appear: at the top
// level, on a step, on an inline hook, and inside each of the mappings this
// package decodes by hand. Before strict decoding these were silently dropped,
// so a typo produced a pipeline that ran but did something else.
func TestStrictUnknownKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "top level",
			pipeline: `
ressources: []
jobs:
- name: j
  plan: [{ task: t, run: "true" }]
`,
			want: `field ressources not found`,
		},
		{
			name: "step field",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: a
    promt: do it
`,
			want: `field promt not found`,
		},
		{
			// Hooks is a yaml:",inline" struct on Step and Job — the least
			// travelled strict-decode path, and the one most likely to regress.
			name: "inline hook key",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    on_fail: { task: cleanup, run: "true" }
`,
			want: `field on_fail not found`,
		},
		{
			name: "when mapping",
			pipeline: `
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    when: { runs: test -f x }
`,
			want: `step when at line 7: unknown key "runs" (did you mean "run"?)`,
		},
		{
			name: "prompt_file mapping",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: a
    prompt_file: { artifact: repo, paths: x.md }
`,
			want: `prompt_file at line 9: unknown key "paths" (did you mean "path"?)`,
		},
		{
			name: "fix mapping",
			pipeline: `
agents:
- name: fixer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: t
    run: "false"
    fix: { agent: fixer, prompts: retry }
`,
			want: `task fix at line 10: unknown key "prompts" (did you mean "prompt"?)`,
		},
		{
			name: "tools entry mapping",
			pipeline: `
agents:
- name: a
  source: { model: lmstudio/qwen }
  tools:
  - name: build
    run: make
    maxcalls: 2
jobs:
- name: j
  plan: [{ agent: a, prompt: x }]
`,
			want: `agent tool at line 8: unknown key "maxcalls" (did you mean "max_calls"?)`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, test.pipeline), test.want)
		})
	}
}

// An included task/agent document is user-authored too, so it gets the same
// strict treatment as the pipeline that references it.
func TestStrictUnknownKeysInIncludedDocument(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "task.yml"), "runs: make\n")
	path := filepath.Join(dir, "pipeline.yml")
	writeFile(t, path, `
tasks:
- name: t
  file: task.yml
jobs:
- name: j
  plan:
  - task: t
`)

	wantLoadError(t, path, "field runs not found")
}

// Every shipped example must load under strict decoding — the examples are
// the schema's worked reference, so a stale key in one is a documentation bug.
func TestExamplesLoad(t *testing.T) {
	t.Parallel()

	matches, err := filepath.Glob("../../examples/*.yml")
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no examples found to load")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(path)
			if err != nil {
				t.Errorf("LoadConfig(%q): %v", path, err)
			}
		})
	}
}

func TestClosestSuggestion(t *testing.T) {
	t.Parallel()

	candidates := []string{"context", "tool", "prompt", "max_calls"}

	tests := []struct {
		got  string
		want string
	}{
		{got: "contxt", want: "context"},
		{got: "promt", want: "prompt"},
		{got: "maxcalls", want: "max_calls"},
		{got: "tools", want: "tool"},
		// Nothing close enough is worse than no guess at all.
		{got: "artifact", want: ""},
		{got: "", want: ""},
	}

	for _, test := range tests {
		t.Run(test.got, func(t *testing.T) {
			t.Parallel()

			if got := closest(test.got, candidates); got != test.want {
				t.Errorf("closest(%q) = %q, want %q", test.got, got, test.want)
			}
		})
	}
}

func TestStrictUnmarshalEmptyDocument(t *testing.T) {
	t.Parallel()

	var cfg Config

	err := strictUnmarshal(nil, &cfg)
	if err != nil {
		t.Fatalf("strictUnmarshal(empty): %v", err)
	}

	if len(cfg.Jobs) != 0 {
		t.Errorf("expected no jobs, got %d", len(cfg.Jobs))
	}
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}
