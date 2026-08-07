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

	cells, err := ExpandAcrossValues("job \"j\" step 0", step, map[string][]string{
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
