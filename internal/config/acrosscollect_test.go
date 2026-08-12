package config

// Collected outputs on across: — the load-time half. A matrix that declares
// outputs: captures each cell's under the cell's own coordinates; what a load
// can check about that is checked here.

import (
	"strings"
	"testing"
)

// TestAcrossCollectStampsOutputSubdir pins the coordinate path each cell
// captures under: one segment per axis value, declaration order — NOT the
// sorted order cell names use, because the path is a contract consumers walk
// and reordering axes alphabetically would silently reshape it.
func TestAcrossCollectStampsOutputSubdir(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross(`job "j" step 0`, Step{
		Across: []AcrossVar{
			{Var: "zone", Values: []string{"alpha", "beta"}},
			{Var: "mode", Values: []string{"fast"}},
		},
		Task:    "work",
		Run:     "true",
		Outputs: []string{"findings"},
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	want := []string{"alpha/fast", "beta/fast"}
	for i, w := range want {
		if cells[i].OutputSubdir != w {
			t.Errorf("cell %d OutputSubdir = %q, want %q", i, cells[i].OutputSubdir, w)
		}
	}
}

// TestAcrossCollectStampsThroughTry: the template's fields live on the step a
// try: wraps, and so does the capture — the runtime hands internal/agent and
// executeTask the INNER step, so a subdir stamped on the wrapper would never
// be read.
func TestAcrossCollectStampsThroughTry(t *testing.T) {
	t.Parallel()

	cells, err := ExpandAcross(`job "j" step 0`, Step{
		Across: []AcrossVar{{Var: "dim", Values: []string{"alpha"}}},
		Try:    &Step{Task: "work", Run: "true", Outputs: []string{"findings"}},
	})
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	if got := cells[0].Try.OutputSubdir; got != "alpha" {
		t.Errorf("wrapped cell OutputSubdir = %q, want alpha", got)
	}
}

// TestAcrossCollectValueRules: a collecting matrix turns axis values into
// directory names of the collected artifact, so they get the checks a name
// that becomes a path needs. A matrix that does NOT collect keeps taking any
// value, since interpolation has no path to protect.
func TestAcrossCollectValueRules(t *testing.T) {
	t.Parallel()

	collecting := func(values ...string) Step {
		return Step{
			Across:  []AcrossVar{{Var: "dim", Values: values}},
			Task:    "work",
			Run:     "true",
			Outputs: []string{"findings"},
		}
	}

	t.Run("a path-hostile value is refused", func(t *testing.T) {
		t.Parallel()

		_, err := ExpandAcross(`job "j" step 0`, collecting("has space"))
		if err == nil || !strings.Contains(err.Error(), "cannot name a directory") {
			t.Fatalf("error = %v, want the path-segment refusal", err)
		}
	})

	t.Run("a traversal value is refused", func(t *testing.T) {
		t.Parallel()

		_, err := ExpandAcross(`job "j" step 0`, collecting("../escape"))
		if err == nil || !strings.Contains(err.Error(), "cannot name a directory") {
			t.Fatalf("error = %v, want the path-segment refusal", err)
		}
	})

	t.Run("a duplicate value is refused", func(t *testing.T) {
		t.Parallel()

		_, err := ExpandAcross(`job "j" step 0`, collecting("alpha", "alpha"))
		if err == nil || !strings.Contains(err.Error(), "lists \"alpha\" twice") {
			t.Fatalf("error = %v, want the duplicate refusal", err)
		}
	})

	t.Run("a non-collecting matrix keeps taking any value", func(t *testing.T) {
		t.Parallel()

		_, err := ExpandAcross(`job "j" step 0`, Step{
			Across: []AcrossVar{{Var: "dim", Values: []string{"has space", "has space"}}},
			Task:   "work",
			Run:    "echo {{ .vars.dim }}",
		})
		if err != nil {
			t.Fatalf("ExpandAcross: %v, want interpolation-only values to stay unrestricted", err)
		}
	})
}

// TestAcrossOutputsValidation covers the block-level rules: isolation is
// required (collection IS the capture step), btrfs is refused rather than
// silently corrupting, and the outputs must be declared on the step rather
// than inherited from a tasks: entry.
func TestAcrossOutputsValidation(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, pipeline, want string }{
		{
			name: "no workspace",
			pipeline: `
jobs:
- name: j
  plan:
  - across:
    - var: dim
      values: [a, b]
    task: work
    inputs: []
    outputs: [findings]
    run: "true"
`,
			want: "requires workspace isolation",
		},
		{
			name: "btrfs",
			pipeline: `
workspace:
  strategy: btrfs
  root: /var/lib/steps
jobs:
- name: j
  plan:
  - across:
    - var: dim
      values: [a, b]
    task: work
    inputs: []
    outputs: [findings]
    run: "true"
`,
			want: `not supported under workspace strategy "btrfs"`,
		},
		{
			name: "outputs inherited from the tasks: entry",
			pipeline: `
workspace:
  strategy: copy
tasks:
- name: work
  run: "true"
  inputs: []
  outputs: [findings]
jobs:
- name: j
  plan:
  - across:
    - var: dim
      values: [a, b]
    task: work
`,
			want: "declare outputs: on the across: step itself",
		},
		{
			name: "declared on the step is fine",
			pipeline: `
workspace:
  strategy: copy
jobs:
- name: j
  plan:
  - across:
    - var: dim
      values: [a, b]
    task: work
    inputs: []
    outputs: [findings]
    run: "true"
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, tc.pipeline)

			_, err := LoadConfig(path)

			if tc.want == "" {
				if err != nil {
					t.Fatalf("LoadConfig: %v, want a collecting matrix under copy to load", err)
				}

				return
			}

			if err == nil {
				t.Fatalf("LoadConfig succeeded, want an error containing %q", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCollectedOutputMapping pins the one rule both cell kinds capture
// through: the declared name (through any author-written output_mapping) plus
// the cell's coordinates — and a pass-through everywhere else.
func TestCollectedOutputMapping(t *testing.T) {
	t.Parallel()

	t.Run("an ordinary step passes its mapping through", func(t *testing.T) {
		t.Parallel()

		mapping := map[string]string{"out": "renamed"}

		got := CollectedOutputMapping([]string{"out"}, mapping, "")
		if len(got) != 1 || got["out"] != "renamed" {
			t.Errorf("mapping = %v, want the input unchanged", got)
		}
	})

	t.Run("a cell captures under its coordinates", func(t *testing.T) {
		t.Parallel()

		got := CollectedOutputMapping([]string{"findings", "logs"}, nil, "alpha/fast")

		for out, want := range map[string]string{
			"findings": "findings/alpha/fast",
			"logs":     "logs/alpha/fast",
		} {
			if got[out] != want {
				t.Errorf("mapping[%s] = %q, want %q", out, got[out], want)
			}
		}
	})

	t.Run("output_mapping renames first, coordinates append after", func(t *testing.T) {
		t.Parallel()

		got := CollectedOutputMapping([]string{"out"}, map[string]string{"out": "findings"}, "alpha")
		if got["out"] != "findings/alpha" {
			t.Errorf("mapping[out] = %q, want findings/alpha", got["out"])
		}
	})
}
