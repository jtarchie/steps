package config

// The load-time half of `from_file:` — the axis whose values a step writes.
// What a load CAN check is checked here; the values themselves cannot be, and
// that is the whole point of the feature.

import (
	"strings"
	"testing"
)

const fromFileJob = `
tasks:
- name: scan
  run: "true"

jobs:
- name: j
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
  - across:`

func TestAcrossFromFileAxisRules(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, axes, wantErr string }{
		{
			name: "from_file alone is valid",
			axes: `
    - var: item
      from_file: findings/items.json`,
		},
		{
			name: "beside a static axis is valid",
			axes: `
    - var: shard
      values: [a, b]
    - var: item
      from_file: findings/items.json`,
		},
		{
			name: "two file axes are valid",
			axes: `
    - var: item
      from_file: findings/items.json
    - var: lens
      from_file: findings/lenses.json`,
		},
		{
			name: "values and from_file together",
			axes: `
    - var: item
      values: [a]
      from_file: findings/items.json`,
			wantErr: "sets both values: and from_file:",
		},
		{
			name: "neither values nor from_file",
			axes: `
    - var: item`,
			wantErr: "has no values: and no from_file:",
		},
		{
			name: "from_file names no artifact",
			axes: `
    - var: item
      from_file: items.json`,
			wantErr: "names no artifact",
		},
		{
			name: "from_file is absolute",
			axes: `
    - var: item
      from_file: /etc/passwd`,
			wantErr: "is absolute",
		},
		{
			name: "from_file escapes the workspace",
			axes: `
    - var: item
      from_file: ../secrets/items.json`,
			wantErr: "escapes the workspace",
		},
		// An axis naming an artifact nothing produces is checked by
		// workspace.ValidateArtifactFlow, where dir:'s identical rule lives —
		// see TestAcrossFromFileArtifactMustBeAvailable there.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, fromFileJob+tc.axes+`
    task: "work-{{ .vars.item }}"
    inputs: [findings]
    run: "true"
`)

			_, err := LoadConfig(path)

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadConfig: %v, want a from_file: axis to load", err)
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

// TestExpandAcrossValuesSubstitutesFileValues proves a file axis expands
// exactly like a static one once its values are known — the property that lets
// a file-driven cell hash identically to the static cell it is
// indistinguishable from.
func TestExpandAcrossValuesSubstitutesFileValues(t *testing.T) {
	t.Parallel()

	step := Step{
		Across: []AcrossVar{{Var: "item", FromFile: "findings/items.json"}},
		Task:   "investigate-{{ .vars.item }}",
		Run:    "echo {{ .vars.item }}",
	}

	cells, err := ExpandAcrossValues(`job "j" step 0`, step, map[string][]string{
		"item": {"alpha", "beta"},
	})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v", err)
	}

	if len(cells) != 2 {
		t.Fatalf("cells = %d, want one per value", len(cells))
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

// TestExpandAcrossValuesTakesTheProductWithAStaticAxis pins full symmetry: a
// file axis is an ordinary axis, so it multiplies with a static one exactly as
// two static axes do.
func TestExpandAcrossValuesTakesTheProductWithAStaticAxis(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcrossValues(`job "j" step 0`, Step{
		Across: []AcrossVar{
			{Var: "item", FromFile: "findings/items.json"},
			{Var: "model", Values: []string{"fast", "thorough"}},
		},
		Task: "check",
		Run:  "echo {{ .vars.item }}/{{ .vars.model }}",
	}, map[string][]string{"item": {"alpha", "beta"}})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v", err)
	}

	// Row-major: the LAST axis varies fastest.
	want := []string{"alpha/fast", "alpha/thorough", "beta/fast", "beta/thorough"}
	if len(cells) != len(want) {
		t.Fatalf("cells = %d, want %d", len(cells), len(want))
	}

	for i, w := range want {
		if cells[i].Run != "echo "+w {
			t.Errorf("cell %d run = %q, want echo %s", i, cells[i].Run, w)
		}
	}
}

// TestExpandAcrossValuesEmptyFileYieldsNoCells pins the decided answer to an
// empty array: zero cells, no error. The block runs nothing and the plan
// carries on; the runner is what says so out loud.
func TestExpandAcrossValuesEmptyFileYieldsNoCells(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcrossValues(`job "j" step 0`, Step{
		Across: []AcrossVar{{Var: "item", FromFile: "findings/items.json"}},
		Task:   "work",
		Run:    "true",
	}, map[string][]string{"item": {}})
	if err != nil {
		t.Fatalf("ExpandAcrossValues: %v, want an empty axis to be legal", err)
	}

	if len(cells) != 0 {
		t.Errorf("cells = %d, want none", len(cells))
	}
}

// TestExpandAcrossValuesRefusesAnUnreadAxis proves the caller contract is
// enforced rather than silently expanding to nothing: a matrix that ran zero
// cells and a matrix that was never read must not look the same.
func TestExpandAcrossValuesRefusesAnUnreadAxis(t *testing.T) {
	t.Parallel()

	_, err := ExpandAcrossValues(`job "j" step 0`, Step{
		Across: []AcrossVar{{Var: "item", FromFile: "findings/items.json"}},
		Task:   "work",
		Run:    "true",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "not read before expanding") {
		t.Fatalf("error = %v, want it to name the unread axis", err)
	}
}

// TestAcrossFromFileTemplatesAreCheckedAtLoad is what a file matrix would
// otherwise never get: a static matrix has its templates validated as a side
// effect of expanding at load, and a file one cannot expand until the step
// that writes its source has run.
func TestAcrossFromFileTemplatesAreCheckedAtLoad(t *testing.T) {
	t.Parallel()

	fileStep := func(run string) Step {
		return Step{
			Across: []AcrossVar{{Var: "item", FromFile: "findings/items.json"}},
			Task:   "check",
			Run:    run,
		}
	}

	cases := []struct {
		name    string
		step    Step
		wantErr string
	}{
		{
			name:    "unclosed brace",
			step:    fileStep("echo {{ .vars.item }"),
			wantErr: "could not parse the template",
		},
		{
			name:    "var no axis declares",
			step:    fileStep("echo {{ .vars.itme }}"),
			wantErr: "names no axis",
		},
		{
			name: "undeclared var inside an if body",
			step: fileStep("{{ if .vars.item }}echo {{ .vars.other }}{{ end }}"),
			// The walk descends into branch bodies, or a misspelling inside one
			// reaches run time unchecked.
			wantErr: "names no axis",
		},
		{
			name: "through a try: wrapper",
			step: Step{
				Across: []AcrossVar{{Var: "item", FromFile: "findings/items.json"}},
				Try:    &Step{Task: "check", Run: "echo {{ .vars.item }"},
			},
			wantErr: "could not parse the template",
		},
		{name: "a declared var is fine", step: fileStep("echo {{ .vars.item }}")},
		{name: "no template at all", step: fileStep("true")},
	}

	for _, tc := range cases {
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

// TestAcrossSourceArtifact pins the one rule both the plan-time availability
// check and the run-time read depend on: which artifact a path names.
func TestAcrossSourceArtifact(t *testing.T) {
	t.Parallel()

	cases := []struct{ from, want string }{
		{"findings/items.json", "findings"},
		{"findings/nested/items.json", "findings"},
		{"./findings/items.json", "findings"},
		{"", ""},
	}

	for _, tc := range cases {
		if got := (AcrossVar{Var: "x", FromFile: tc.from}).SourceArtifact(); got != tc.want {
			t.Errorf("SourceArtifact(%q) = %q, want %q", tc.from, got, tc.want)
		}
	}
}
