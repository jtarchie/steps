package config

import (
	"strconv"
	"strings"
	"testing"
)

// TestParallelismDesugarsToAcross pins the whole contract: parallelism: N is
// exactly an across: over a 1-based index axis, concurrent by default, with a
// render-only count var the cells can interpolate.
func TestParallelismDesugarsToAcross(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: 3
    run: sh -c 'echo "slot {{ .vars.index }} of {{ .vars.count }}"'
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	step := cfg.Jobs[0].Plan[0]

	if len(step.Across) != 1 || step.Across[0].Var != "index" {
		t.Fatalf("across = %+v, want one implicit index axis", step.Across)
	}

	if got, want := strings.Join(step.Across[0].Values, ","), "1,2,3"; got != want {
		t.Errorf("index values = %q, want %q — 1-based, one per shard", got, want)
	}

	if step.MaxInFlight != 3 {
		t.Errorf("max_in_flight = %d, want 3 — shards run concurrently by default", step.MaxInFlight)
	}

	cells, err := ExpandAcross(`job "j" step 0`, step)
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	if len(cells) != 3 {
		t.Fatalf("expanded to %d cells, want 3", len(cells))
	}

	for i, cell := range cells {
		assertShardCell(t, i, cell)
	}
}

// assertShardCell holds one shard to the cell contract: coordinate-named,
// count rendered, and an ordinary step once expanded.
func assertShardCell(t *testing.T, i int, cell Step) {
	t.Helper()

	if want := `unit [index=` + strconv.Itoa(i+1) + `]`; cell.DisplayName() != want {
		t.Errorf("cell %d named %q, want %q", i, cell.DisplayName(), want)
	}

	if !strings.Contains(cell.Run, `of 3`) {
		t.Errorf("cell %d run = %q; {{ .vars.count }} did not render to the shard count", i, cell.Run)
	}

	if cell.Parallelism != 0 {
		t.Errorf("cell %d still carries parallelism=%d; a cell is an ordinary step", i, cell.Parallelism)
	}
}

// TestParallelismKeepsAuthoredMaxInFlight: an explicit max_in_flight: narrows
// the default rather than being overwritten by it.
func TestParallelismKeepsAuthoredMaxInFlight(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: 6
    max_in_flight: 2
    run: echo {{ .vars.index }}
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := cfg.Jobs[0].Plan[0].MaxInFlight; got != 2 {
		t.Errorf("max_in_flight = %d, want the authored 2", got)
	}
}

// TestParallelismRejects covers the placements that cannot mean anything.
func TestParallelismRejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "negative",
			yaml: `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: -2
    run: echo hi
`,
			want: "must be positive",
		},
		{
			name: "beside across",
			yaml: `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: 2
    across:
    - var: shard
      values: [a, b]
    run: echo {{ .vars.shard }}
`,
			want: "beside across:",
		},
		{
			name: "not a task",
			// The dangling put: reference never matters: desugar rejects
			// before validate() would look the resource up.
			yaml: `
jobs:
- name: j
  plan:
  - put: dest
    parallelism: 2
`,
			want: "only valid on a task step",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadConfig(writeConfig(t, tc.yaml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestAcrossHasNoCountVar: count belongs to parallelism: alone — on a
// hand-written matrix the reference is the load error any misspelled var is.
func TestAcrossHasNoCountVar(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: unit
    across:
    - var: shard
      values: [a, b]
    run: echo {{ .vars.count }}
`)

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("LoadConfig error = %v, want the count reference refused", err)
	}
}
