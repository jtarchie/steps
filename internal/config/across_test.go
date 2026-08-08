package config

import (
	"strings"
	"testing"
)

// TestAcrossRuntimeAxisRules covers the from: axis's load-time rules. The
// values themselves cannot be checked at load — that is the whole point — so
// what a load CAN catch has to be caught here.
func TestAcrossRuntimeAxisRules(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, axes, wantErr string }{
		{
			name: "from alone is valid",
			axes: `
    - var: finding
      from: findings`,
		},
		{
			name: "values and from together",
			axes: `
    - var: finding
      values: [a]
      from: findings`,
			wantErr: "sets both values: and from:",
		},
		{
			name: "neither values nor from",
			axes: `
    - var: finding`,
			wantErr: "has no values: and no from:",
		},
		{
			name: "a runtime axis beside a static one is valid",
			axes: `
    - var: shard
      values: [a, b]
    - var: finding
      from: findings`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, `
jobs:
- name: j
  plan:
  - across:`+tc.axes+`
    task: "work-{{ .vars.finding }}"
    inputs: []
    run: "true"
`)

			_, err := LoadConfig(path)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfig: %v, want a runtime axis to load", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("LoadConfig succeeded, want an error containing %q", tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestExpandAcrossValuesSubstitutesRuntimeValues proves a runtime axis expands
// exactly like a static one once its values are known — the property that lets
// a runtime cell hash identically to the static cell it is indistinguishable
// from.
func TestExpandAcrossValuesSubstitutesRuntimeValues(t *testing.T) {
	t.Parallel()

	step := Step{
		Across: []AcrossVar{{Var: "finding", From: "findings"}},
		Task:   "investigate-{{ .vars.finding }}",
		Run:    "echo {{ .vars.finding }}",
	}

	cells, err := ExpandAcrossValues("job \"j\" step 0", step, map[string][]any{
		"finding": {"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v", err)
	}

	if len(cells) != 2 {
		t.Fatalf("cells = %d, want one per runtime value", len(cells))
	}

	for i, want := range []string{"alpha", "beta"} {
		if cells[i].Task != "investigate-"+want {
			t.Errorf("cell %d task = %q, want investigate-%s", i, cells[i].Task, want)
		}

		if cells[i].Run != "echo "+want {
			t.Errorf("cell %d run = %q, want echo %s", i, cells[i].Run, want)
		}
	}
}

// TestExpandAcrossValuesRefusesAnUnresolvedAxis proves the caller contract is
// enforced rather than silently expanding to nothing: a matrix that ran zero
// cells and a matrix that was never resolved must not look the same.
func TestExpandAcrossValuesRefusesAnUnresolvedAxis(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "finding", From: "findings"}},
		Task:   "work",
		Run:    "true",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "not resolved before expanding") {
		t.Fatalf("error = %v, want it to name the unresolved axis", err)
	}
}

// TestAcrossCellOverSharedTaskResolves is the regression: a matrix cell that
// references a reusable tasks: entry by name.
//
// The coordinates used to be appended to cell.Task, which is the field
// ResolveTask looks the shared task up by, so every such matrix died with
// `no task named "shared [shard=b]"`. Inline run: cells never hit it, because
// ResolveTask short-circuits on run: before the lookup — which is exactly why
// this went unnoticed.
func TestAcrossCellOverSharedTaskResolves(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
tasks:
- name: shared
  run: "true"
jobs:
- name: j
  plan:
  - across:
    - var: shard
      values: [a, b]
    task: shared
    inputs: []
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	cells, err := ExpandAcross("job \"j\" step 0", cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, cell := range cells {
		// The lookup key is untouched...
		if cell.Task != "shared" {
			t.Errorf("cell %d task = %q, want %q — the lookup key must not carry coordinates", i, cell.Task, "shared")
		}

		// ...and resolution therefore succeeds, which is the bug.
		_, err := cfg.ResolveTask(cell)
		if err != nil {
			t.Errorf("cell %d: ResolveTask: %v", i, err)
		}
	}

	// ...while the identities are still distinct.
	if a, b := cells[0].DisplayName(), cells[1].DisplayName(); a == b {
		t.Errorf("both cells are named %q; a matrix's cells must be tellable apart", a)
	}
}

// TestAcrossAgentCellsGetDistinctNames is what the split unblocks: an agent
// cell can finally be named, because the name no longer has to double as the
// FindAgent key. Every agent cell of a matrix used to answer to one name,
// which is why handoff: and context: are rejected on across: steps.
func TestAcrossAgentCellsGetDistinctNames(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "shard", Values: []string{"a", "b"}}},
		Agent:  "reviewer",
		Prompt: "review it",
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"reviewer [shard=a]", "reviewer [shard=b]"} {
		if got := cells[i].DisplayName(); got != want {
			t.Errorf("cell %d name = %q, want %q", i, got, want)
		}

		// The agent to invoke is untouched, or FindAgent would fail.
		if cells[i].Agent != "reviewer" {
			t.Errorf("cell %d agent = %q, want reviewer", i, cells[i].Agent)
		}
	}
}

// TestAcrossAuthorNamedCellsAreLeftAlone proves the naming defers to an author
// who distinguished the cells themselves, and that it decides that from the
// TEMPLATE rather than from the rendered text.
//
// The old check looked for a value as a substring of the rendered name, which
// is a coincidence detector: values [a, b] over a task named "shared" found
// the "a" in "shared" and left that cell unsuffixed while its sibling got one.
func TestAcrossAuthorNamedCellsAreLeftAlone(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross("job \"j\" step 0", Step{
		Across: []AcrossVar{{Var: "shard", Values: []string{"a", "b"}}},
		Task:   "check-{{ .vars.shard }}",
		Run:    "true",
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"check-a", "check-b"} {
		if got := cells[i].DisplayName(); got != want {
			t.Errorf("cell %d name = %q, want %q (an author-named cell gets no suffix)", i, got, want)
		}

		if cells[i].Label != "" {
			t.Errorf("cell %d label = %q, want none", i, cells[i].Label)
		}
	}
}

// TestOrdinaryStepHasNoLabel pins the value-gating: a step outside a matrix
// carries no Label, so it hashes exactly as it did before the field existed.
func TestOrdinaryStepHasNoLabel(t *testing.T) {
	t.Parallel()

	step := Step{Task: "build", Run: "true"}
	if step.Label != "" {
		t.Errorf("Label = %q, want empty on an ordinary step", step.Label)
	}

	if got := step.DisplayName(); got != "build" {
		t.Errorf("DisplayName() = %q, want the task name", got)
	}
}

// TestValidateAcrossTemplatesOnRuntimeMatrix covers the one check a runtime
// matrix used to skip entirely (#40.3): its cells cannot expand at load, and
// template validation was a side effect of expanding.
//
// Parse-only, so the object case must still pass — there is no dummy value set
// that satisfies both a string axis and an object axis, which is why nothing is
// rendered here.
func TestValidateAcrossTemplatesOnRuntimeMatrix(t *testing.T) {
	t.Parallel()

	runtimeStep := func(run string) Step {
		return Step{
			Across: []AcrossVar{{Var: "finding", From: "findings"}},
			Task:   "check",
			Run:    run,
		}
	}

	tests := []struct {
		name    string
		step    Step
		wantErr string
	}{
		{
			name:    "unclosed brace",
			step:    runtimeStep("echo {{ .vars.finding }"),
			wantErr: "could not parse the template",
		},
		{
			name:    "var no axis declares",
			step:    runtimeStep("echo {{ .vars.finidng }}"),
			wantErr: "names no axis",
		},
		{
			name:    "undeclared var inside a field access",
			step:    runtimeStep("echo {{ .vars.findings.file }}"),
			wantErr: "names no axis",
		},
		{
			name: "undeclared var inside an if body",
			step: runtimeStep("{{ if .vars.finding }}echo {{ .vars.other }}{{ end }}"),
			// The walk descends into branch bodies, or the check has the same
			// hole rejectBareObjectRefs closed.
			wantErr: "names no axis",
		},
		{
			name: "through a try: wrapper",
			step: Step{
				Across: []AcrossVar{{Var: "finding", From: "findings"}},
				Try:    &Step{Task: "check", Run: "echo {{ .vars.finding }"},
			},
			wantErr: "could not parse the template",
		},
		{name: "field access on an object item", step: runtimeStep("echo {{ .vars.finding.file }}")},
		{name: "no template at all", step: runtimeStep("true")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := validateAcrossTemplates(`job "j" step 0`, tc.step)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAcrossTemplates: %v, want no error", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("validateAcrossTemplates: no error, want one containing %q", tc.wantErr)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
