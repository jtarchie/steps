package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestDelegateBudgetPercentBounds: 1..100 is inclusive at both ends, in both positions the field is accepted.
func TestDelegateBudgetPercentBounds(t *testing.T) {
	t.Parallel()

	for _, percent := range []int{0, 1, 100, 101} {
		for _, where := range []string{"defaults", "agent"} {
			t.Run(fmt.Sprintf("%s %d", where, percent), func(t *testing.T) {
				t.Parallel()

				defaults, agent := "", ""
				if where == "defaults" {
					defaults = fmt.Sprintf("defaults:\n  delegate_budget_percent: %d\n", percent)
				} else {
					agent = fmt.Sprintf("  delegate_budget_percent: %d\n", percent)
				}

				_, err := LoadConfig(writeConfig(t, defaults+`
agents:
- name: lead
  source: { model: openai/test-model }
`+agent+`
jobs:
- name: j
  plan: [{ agent: lead, messages: [go] }]
`))

				valid := percent >= 1 && percent <= 100
				if valid && err != nil {
					t.Fatalf("delegate_budget_percent: %d rejected: %v", percent, err)
				}

				if !valid && (err == nil || !strings.Contains(err.Error(), "delegate_budget_percent must be between 1 and 100")) {
					t.Fatalf("delegate_budget_percent: %d: error = %v, want the 1..100 refusal", percent, err)
				}
			})
		}
	}
}

// A fix: agent is named by no step, so the model check must reach it through the fix or the gap surfaces mid-run.
func TestFixAgentWithNoModelIsRejected(t *testing.T) {
	t.Setenv(stepsModelEnv, "")

	wantLoadError(t, writeConfig(t, `
agents:
- name: fixer
jobs:
- name: j
  plan:
  - task: build
    run: "true"
    fix: fixer
`), `agent "fixer" has no model`)
}

// The width cap is inclusive: exactly MaxAcrossItems shards is a legal matrix.
func TestParallelismAtTheWidthCapLoads(t *testing.T) {
	t.Parallel()

	cfg := loadOK(t, fmt.Sprintf(`
jobs:
- name: j
  plan:
  - task: unit
    parallelism: %d
    run: echo {{ .vars.index }}
`, MaxAcrossItems))

	if got := len(cfg.Jobs[0].Plan[0].Across[0].Values); got != MaxAcrossItems {
		t.Errorf("shards = %d, want %d", got, MaxAcrossItems)
	}
}

// An untagged do: hands its children the job tags, not an empty set of its own.
func TestAnUntaggedDoPassesTheJobTagsThrough(t *testing.T) {
	t.Parallel()

	cfg := loadOK(t, `
jobs:
- name: j
  tags: [gpu]
  plan:
  - do:
    - task: inner
      run: "true"
`)

	if got := strings.Join(cfg.Jobs[0].Plan[0].Do[0].Tags, ","); got != "gpu" {
		t.Errorf("tags = %q, want the job's gpu", got)
	}
}

// A fallback that cannot resolve is refused at load, not discovered during the outage it exists for.
func TestUnresolvableFallbackIsRejected(t *testing.T) {
	t.Parallel()

	wantLoadError(t, writeConfig(t, `
agents:
- name: writer
  source: { model: openai/test-model }
  fallback:
  - source: { model: opencoder/typo }
jobs:
- name: j
  plan: [{ agent: writer, messages: [go] }]
`), `agent "writer" fallback[0]:`)
}

// TestPreflightDurationBounds: zero is not positive, and the cache window may equal the pin lifetime but not exceed it.
func TestPreflightDurationBounds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, preflight, want string
	}{
		{"zero timeout", "timeout: 0s", "defaults.preflight.timeout: \"0s\" is not a positive duration"},
		{"cache at the pin lifetime", "cache: " + PinLifetime.String(), ""},
		{"cache past the pin lifetime", "cache: 16m", "longer than the"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, `
defaults:
  preflight:
    `+tc.preflight+`
jobs:
- name: j
  plan: [{ task: t, run: "true" }]
`))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("LoadConfig: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestMalformedTimeoutsAreRejected: a typo in any timeout: position costs a load rather than reading as set.
func TestMalformedTimeoutsAreRejected(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, pipeline, want string
	}{
		{
			name: "job, unparsable",
			pipeline: `
jobs:
- name: j
  timeout: soon
  plan: [{ task: t, run: "true" }]
`,
			want: `job "j": timeout: invalid timeout "soon"`,
		},
		{
			name: "job, zero",
			pipeline: `
jobs:
- name: j
  timeout: 0s
  plan: [{ task: t, run: "true" }]
`,
			want: `job "j": timeout must be a positive duration`,
		},
		{
			name: "tasks: entry",
			pipeline: `
tasks:
- name: t
  run: "true"
  timeout: soon
jobs:
- name: j
  plan: [{ task: t }]
`,
			want: `task "t": invalid timeout "soon"`,
		},
		{
			name: "tasks: entry's fix:",
			pipeline: `
agents:
- name: fixer
  source: { model: openai/test-model }
tasks:
- name: t
  run: "true"
  fix: { agent: fixer, timeout: soon }
jobs:
- name: j
  plan: [{ task: t }]
`,
			want: `task "t" fix: invalid timeout "soon"`,
		},
		{
			name: "step's fix:",
			pipeline: `
agents:
- name: fixer
  source: { model: openai/test-model }
jobs:
- name: j
  plan:
  - task: t
    run: "true"
    fix: { agent: fixer, timeout: soon }
`,
			want: `fix: invalid timeout "soon"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, tc.pipeline), tc.want)
		})
	}
}

// max_in_flight: 1 is as serial as leaving it unset, so a reservation there decides nothing too.
func TestReservePerCellRefusedAtMaxInFlightOne(t *testing.T) {
	t.Parallel()

	wantLoadError(t, writeConfig(t, `
agents:
- name: reviewer
  source: { model: openai/test-model }
jobs:
- name: j
  plan:
  - across:
    - var: item
      values: [a, b]
    agent: reviewer
    messages: ["{{ .vars.item }}"]
    max_in_flight: 1
    budget:
      tokens: 500
      reserve_per_cell: 100
`), "budget.reserve_per_cell needs max_in_flight: above 1")
}

// A fix: agent's provider prefix is checked at load like a step agent's.
func TestFixAgentWithUnknownProviderIsRejected(t *testing.T) {
	t.Parallel()

	wantLoadError(t, writeConfig(t, `
agents:
- name: fixer
  source: { model: opencoder/typo }
jobs:
- name: j
  plan:
  - task: build
    run: "true"
    fix: fixer
`), `agent "fixer"`)
}

// More messages than varFields' base capacity still load.
func TestAgentStepWithManyMessagesLoads(t *testing.T) {
	t.Parallel()

	cfg := loadOK(t, `
agents:
- name: a
  source: { model: openai/test-model }
jobs:
- name: j
  plan:
  - agent: a
    messages: [one, two, three, four, five]
`)

	if got := len(cfg.Jobs[0].Plan[0].Messages); got != 5 {
		t.Errorf("messages = %d, want 5", got)
	}
}

// Only load_var: reads file:, so on a task (Concourse's task-config spelling) or a get it would load and be ignored.
func TestFileWithoutLoadVarIsRejectedOnEveryStep(t *testing.T) {
	t.Parallel()

	for name, step := range map[string]string{
		"task": "task: t\n    run: \"true\"",
		"get":  "get: src",
		"put":  "put: src",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			wantLoadError(t, writeConfig(t, `
resource_types:
- name: probe
  config:
    check: echo '[]'
    in: "true"
    out: echo '{}'
resources:
- name: src
  type: probe
  source: {}
jobs:
- name: j
  plan:
  - `+step+`
    file: ci/build.yml
`), "file: without load_var: has no meaning here")
		})
	}
}
