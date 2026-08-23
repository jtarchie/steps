package config

import (
	"strings"
	"testing"
)

func TestTransitionsLoadAndDecode(t *testing.T) {
	t.Parallel()

	pipeline := `
agents:
- name: critic
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: draft
    inputs: []
    run: "true"
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts:
      - approve: publish
      - revise: draft
      - escalate: publish
      - failure: publish
    max_visits: 3
  - task: publish
    inputs: []
    run: "true"
`
	path := writeConfig(t, pipeline)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	critic := cfg.Jobs[0].Plan[1]

	target, routed := critic.RouteFor("revise")
	if !routed || target != "draft" {
		t.Errorf("RouteFor(revise) = %q/%v, want draft/true", target, routed)
	}

	if critic.MaxVisits != 3 {
		t.Errorf("max_visits = %d, want 3", critic.MaxVisits)
	}

	// Four entries, but only three the model may choose from: failure: is the
	// runtime's catch, not a choice, so it never reaches the tool enum.
	if len(critic.Verdicts) != 4 {
		t.Errorf("verdicts = %v, want 4 entries", critic.Verdicts)
	}

	if got := strings.Join(critic.VerdictNames(), ","); got != "approve,revise,escalate" {
		t.Errorf("VerdictNames() = %q, want the declared order without failure", got)
	}
}

func TestTransitionsBinaryLoads(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    to:
      success: publish
      failure: work
    max_visits: 2
  - task: publish
    inputs: []
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a binary success/failure loop should load, got: %v", err)
	}
}

func TestTransitionsValidationErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name: "unresolvable target",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    to: { success: nowhere }
`,
			want: "not a step in the same segment",
		},
		{
			name: "backward without max_visits",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
  - task: b
    inputs: []
    run: "true"
    to: { failure: a }
`,
			want: "max_visits must be set",
		},
		{
			name: "backward max_visits exceeds ceiling",
			pipeline: `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
  - task: b
    inputs: []
    run: "true"
    to: { failure: a }
    max_visits: 1001
`,
			want: "exceeds the maximum",
		},
		{
			name: "to on a get step",
			pipeline: `
resource_types:
- name: rt
  config: { check: "echo '[]'", in: "true" }
resources:
- name: r
  type: rt
  source: {}
jobs:
- name: j
  plan:
  - get: r
    to: { success: r }
`,
			want: "not valid on get steps",
		},
		{
			name: "to on a hook step",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    on_failure:
      task: notify
      run: "true"
      to: { success: notify }
`,
			want: "not valid on hook steps",
		},
		{
			name: "unknown binary key",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    to: { done: publish }
  - task: publish
    inputs: []
    run: "true"
`,
			want: `key "done" is not valid`,
		},
		{
			name: "duplicate names in to-using segment",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    to: { success: work }
  - task: work
    inputs: []
    run: "true"
`,
			want: "duplicated within a to:-using segment",
		},
		{
			name: "verdicts on a task",
			pipeline: `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
    verdicts:
      - a: work
      - b: work
    max_visits: 2
`,
			want: "verdicts is only valid on agent steps",
		},
		{
			name: "verdict collides with reserved key",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - success: c
`,
			want: "collides with a reserved key",
		},
		{
			// The hard cutover: the old spelling put the vocabulary in
			// verdicts: and the routing in a to: map beside it.
			name: "verdicts alongside to",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts: [approve, revise]
    to: { approve: done, revise: done }
  - task: done
    inputs: []
    run: "true"
`,
			want: "to: is not valid on a step with verdicts:",
		},
		{
			name: "duplicate verdict",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - approve: done
      - approve: next
  - task: done
    inputs: []
    run: "true"
`,
			want: `verdict "approve" is declared more than once`,
		},
		{
			name: "bare failure catch",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - approve
      - failure
`,
			want: "must name a target",
		},
		{
			name: "verdict target outside the segment",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - approve: nowhere
`,
			want: "not a step in the same segment",
		},
		{
			name: "verdict routes backward without max_visits",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - task: draft
    inputs: []
    run: "true"
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - revise: draft
`,
			want: "max_visits must be set",
		},
		{
			name: "verdicts entry is a multi-key mapping",
			pipeline: `
agents:
- name: c
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:
  - agent: c
    inputs: []
    messages:
      - x
    verdicts:
      - approve: done
        revise: done
  - task: done
    inputs: []
    run: "true"
`,
			want: "must be either a name on its own",
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

// TestTransitionsDuplicateNamesAllowedWithoutTo proves the uniqueness check is
// gated on to: usage — a job with duplicate step names but no routing loads.
func TestTransitionsDuplicateNamesAllowedWithoutTo(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: work
    inputs: []
    run: "true"
  - task: work
    inputs: []
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("duplicate names in a to:-free job should load, got: %v", err)
	}
}

// TestTransitionsForwardNeedsNoMaxVisits proves an all-forward to: is legal
// without max_visits.
func TestTransitionsForwardNeedsNoMaxVisits(t *testing.T) {
	t.Parallel()

	pipeline := `
jobs:
- name: j
  plan:
  - task: a
    inputs: []
    run: "true"
    to: { success: c }
  - task: b
    inputs: []
    run: "true"
  - task: c
    inputs: []
    run: "true"
`
	path := writeConfig(t, pipeline)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("a forward-only to: should load without max_visits, got: %v", err)
	}
}

// TestSegmentAllowsSeveralUnnamedBlocks covers the shape a fan-out pipeline
// takes: a concurrent block and a matrix in one job that also loops.
//
// Containers have no name, and pos — the map to: targets resolve against — was
// keyed by name for every step including them. Two blocks in one routing
// segment therefore collided on "", and the load failed with `step name "" is
// duplicated`: a message that named neither block, and described a conflict
// between two steps that cannot be to: targets in the first place.
func TestSegmentAllowsSeveralUnnamedBlocks(t *testing.T) {
	t.Parallel()

	job := Job{
		Name: "review",
		Plan: []Step{
			{Task: "anatomy", Run: "true"},
			{InParallel: &InParallel{Steps: []Step{
				{Task: "lens-a", Run: "true"},
				{Task: "lens-b", Run: "true"},
			}}},
			{
				Across: []AcrossVar{{Var: "dim", Values: []string{"a", "b"}}},
				Task:   "reviewer",
				Run:    "true",
			},
			{
				Task:      "synthesize",
				Run:       "true",
				To:        map[string]string{"success": "publish", "failure": "anatomy"},
				MaxVisits: 2,
			},
			{Task: "publish", Run: "true"},
		},
	}

	err := validateSegment(job, []int{0, 1, 2, 3, 4})
	if err != nil {
		t.Fatalf("two unnamed blocks in a routing segment: %v", err)
	}
}

// TestSegmentStillRejectsDuplicateNamedSteps is the other half: skipping
// unnamed steps must not weaken the rule for steps that DO have names, since
// a to: target naming two of them has no answer.
func TestSegmentStillRejectsDuplicateNamedSteps(t *testing.T) {
	t.Parallel()

	job := Job{
		Name: "review",
		Plan: []Step{
			{Task: "work", Run: "true", To: map[string]string{"success": "work"}, MaxVisits: 2},
			{Task: "work", Run: "true"},
		},
	}

	err := validateSegment(job, []int{0, 1})
	if err == nil {
		t.Fatal("two steps named \"work\" validated in a to:-using segment")
	}

	if !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("error = %v, want it to name the duplication", err)
	}
}

// TestRouteTargetNext covers the reserved positional target — the one to:
// value that is not a step name.
//
// Its reason to exist is the case at the top: a verdict must name a target and
// a container has none, so an author whose next step is an approval: gate had
// to route PAST it, which does not skip a formality but the human gate the
// pipeline exists to have.
func TestRouteTargetNext(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, plan, wantErr string }{
		{
			name: "verdict falls through into an approval gate",
			plan: `
  - agent: critic
    inputs: []
    messages:
      - judge it
    verdicts:
      - approve: next
      - revise: draft
    max_visits: 2
  - approval:
      message: "publish it?"
  - task: draft
    inputs: []
    run: "true"`,
		},
		{
			// Forward by construction, so it can never be the backward jump
			// max_visits: bounds.
			name: "needs no max_visits",
			plan: `
  - task: work
    inputs: []
    run: "true"
    to: { success: next, failure: next }
  - task: after
    inputs: []
    run: "true"`,
		},
		{
			// One past the end of a segment is where an unrouted final step
			// goes anyway, so "carry on" is meaningful even with nothing after.
			name: "on the last step of a segment",
			plan: `
  - task: work
    inputs: []
    run: "true"
    to: { failure: next }`,
		},
		{
			name: "a step named next collides",
			plan: `
  - task: work
    inputs: []
    run: "true"
    to: { success: next }
  - task: next
    inputs: []
    run: "true"`,
			wantErr: "collides with the reserved to: target",
		},
		{
			// The word is only read where to: is, so a pipeline that routes
			// nowhere may still call a step whatever it likes.
			name: "a step named next is fine without routing",
			plan: `
  - task: next
    inputs: []
    run: "true"`,
		},
		{
			name: "a near miss is suggested",
			plan: `
  - task: work
    inputs: []
    run: "true"
    to: { success: nex }
  - task: after
    inputs: []
    run: "true"`,
			wantErr: `did you mean "next"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, `
agents:
- name: critic
  source: { model: lmstudio/qwen }
jobs:
- name: j
  plan:`+tc.plan+"\n")

			_, err := LoadConfig(path)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfig: %v, want it to load", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("LoadConfig: no error, want one containing %q", tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
