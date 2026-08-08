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
			name: "context on a put step",
			pipeline: `
resource_types:
- name: dummy
  config:
    check: "echo '[]'"
    in: "true"
    out: "true"
resources:
- name: results
  type: dummy
  source: {}
jobs:
- name: j
  plan:
  - put: results
    inputs: []
    context: write
`,
			want: "context is only valid on agent and task steps",
		},
		{
			name: "fidelity on a task step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    context: { write: true, fidelity: summary }
`,
			want: "context fidelity is only valid on agent steps",
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)
			wantLoadError(t, path, tc.want)
		})
	}
}

// TestResolveContextFidelity pins the precedence: step, then defaults, then
// compact. First match wins, and a pipeline that declares neither still gets
// a recap — reading is on by default, unlike writing.
func TestResolveContextFidelity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		defaults string
		step     string
		want     ContextFidelity
	}{
		{name: "nothing declared", want: FidelityCompact},
		{name: "defaults only", defaults: "defaults:\n  context:\n    fidelity: summary\n", want: FidelitySummary},
		{name: "step wins over defaults", defaults: "defaults:\n  context:\n    fidelity: summary\n", step: "    context: { fidelity: truncate }\n", want: FidelityTruncate},
		{name: "step opts out", step: "    context: { fidelity: \"off\" }\n", want: FidelityOff},
		{name: "write only leaves the default", step: "    context: write\n", want: FidelityCompact},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.defaults+`
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
`+tc.step)

			cfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}

			if got := cfg.ResolveContextFidelity(cfg.Jobs[0].Plan[0]); got != tc.want {
				t.Errorf("ResolveContextFidelity = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestContextFidelityRejectsUnknownLevels covers both places a level can be
// written, and proves the error names the vocabulary rather than just
// refusing.
func TestContextFidelityRejectsUnknownLevels(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, pipeline, want string }{
		{
			name: "on a step",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    context: { fidelity: verbose }
`,
			want: `unknown fidelity "verbose"`,
		},
		{
			name: "in defaults",
			pipeline: `
defaults:
  context:
    fidelity: brief
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
`,
			want: `unknown fidelity "brief"`,
		},
		{
			name: "a near miss suggests the right one",
			pipeline: `
agents:
- name: writer
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: writer
    prompt: go
    context: { fidelity: truncat }
`,
			want: `did you mean "truncate"?`,
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

// TestContextFidelityOnlyIsValid proves a read-only spec loads: `context:
// {fidelity: off}` enables no tool, and reading it as "enables nothing" would
// reject the opt-out — the one spelling a step most wants.
func TestContextFidelityOnlyIsValid(t *testing.T) {
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
    context: { fidelity: "off" }
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]
	if step.WritesContext() {
		t.Error("WritesContext() = true, want false for a fidelity-only spec")
	}

	if got := cfg.ResolveContextFidelity(step); got != FidelityOff {
		t.Errorf("ResolveContextFidelity = %q, want off", got)
	}
}

// TestContextReadIsLegalInsideABranch proves only WRITES are rejected in a
// concurrent block. A branch step opting out of the recap has nothing to race
// with, and rejecting it would take the opt-out away from exactly the steps
// most likely to want it.
func TestContextReadIsLegalInsideABranch(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
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
        context: { fidelity: "off" }
      - task: work
        inputs: []
        run: "true"
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v, want a read-only context spec to be legal in a branch", err)
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

// TestContextWriteIsLegalOnAMatrix proves the matrix case is NOT the branch
// case. Cells run in declaration order, so two cells writing one key resolve
// the way two sequential steps do — the later wins, in an order the author can
// read off the pipeline. Only concurrent branches have no such order.
func TestContextWriteIsLegalOnAMatrix(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
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
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v, want context: write to be legal on a matrix", err)
	}
}
