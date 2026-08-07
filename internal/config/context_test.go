package config

import (
	"strings"
	"testing"
)

// TestContextScalarAndMappingDecode proves both spellings reach the same
// ContextSpec, and that the scalar rejects anything but the one word it takes.
func TestContextScalarAndMappingDecode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		step      string
		wantWrite bool
	}{
		{name: "scalar write", step: "context: write", wantWrite: true},
		{name: "mapping write true", step: "context: { write: true }", wantWrite: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    `+tc.step+`
`)

			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			step := cfg.Jobs[0].Plan[0]
			if step.Context == nil {
				t.Fatal("step.Context is nil")
			}

			if step.Context.Write != tc.wantWrite {
				t.Errorf("Write = %v, want %v", step.Context.Write, tc.wantWrite)
			}

			if got := step.WritesContext(); got != tc.wantWrite {
				t.Errorf("WritesContext() = %v, want %v", got, tc.wantWrite)
			}
		})
	}
}

// TestContextValidationErrors covers every load-time rejection context: has.
// The branch cases are the load-bearing ones: a write inside a concurrent
// block has nowhere to land until the join exists, and shipping it as a
// silent no-op is exactly the class of bug this codebase keeps finding.
func TestContextValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "context on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    context: write
`,
			want: "context is only valid on agent steps",
		},
		{
			name: "context enables nothing",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    context: { write: false }
`,
			want: "context enables nothing",
		},
		{
			name: "unknown scalar mode",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    context: read
`,
			want: `unknown mode "read"`,
		},
		{
			name: "unknown mapping key",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    context: { writes: true }
`,
			want: "writes",
		},
		{
			name: "context on a hook step",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    on_failure:
      agent: writer
      prompt: notify
      context: write
`,
			want: "context is not valid on hook steps",
		},
		{
			name: "context inside an in_parallel branch",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - in_parallel:
      steps:
      - agent: writer
        prompt: go
        context: write
      - task: work
        inputs: []
        run: "true"
`,
			want: "not supported inside a concurrent block",
		},
		{
			name: "context on an across step",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - across:
    - var: shard
      values: [a, b]
    agent: writer
    prompt: "go {{ .vars.shard }}"
    context: write
`,
			want: "not supported on an across: step",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			wantLoadError(t, path, tc.want)
		})
	}
}

// TestValidateContextKey pins the tool boundary's key rule. The reserved
// prefix and the charset are what stop a model from overwriting engine
// bookkeeping or minting a key that renders one way and matches another.
func TestValidateContextKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, key, wantErr string
	}{
		{name: "plain", key: "failure_cause"},
		{name: "dotted", key: "review.result"},
		{name: "hyphenated", key: "build-id"},
		{name: "empty", key: "", wantErr: "must not be empty"},
		{name: "reserved prefix", key: "internal.run_id", wantErr: "reserved"},
		{name: "space", key: "failure cause", wantErr: "contains"},
		{name: "quote", key: `a"b`, wantErr: "contains"},
		{name: "too long", key: strings.Repeat("k", MaxContextKeyLen+1), wantErr: "above the limit"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateContextKey(tc.key)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateContextKey(%q) = %v, want nil", tc.key, err)
				}

				return
			}

			if err == nil {
				t.Fatalf("ValidateContextKey(%q) = nil, want an error naming %q", tc.key, tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ValidateContextKey(%q) = %v, want it to mention %q", tc.key, err, tc.wantErr)
			}
		})
	}
}
