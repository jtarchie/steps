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

	if cell.Parallelism != nil {
		t.Errorf("cell %d still carries parallelism=%d; a cell is an ordinary step", i, *cell.Parallelism)
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
			// An explicit 0 must not silently mean "sugar off": the step
			// would run once with its {{ .vars.index }} text unrendered, and
			// the schema's minimum of 1 would disagree with the loader.
			name: "zero",
			yaml: `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: 0
    run: echo {{ .vars.index }}
`,
			want: "must be positive",
		},
		{
			// One mistyped digit is 9 characters that expand at load —
			// make(N) at int64 scale panics before any error could say why.
			name: "over the width cap",
			yaml: `
jobs:
- name: j
  plan:
  - task: unit
    parallelism: 5000000
    run: echo {{ .vars.index }}
`,
			want: "above the limit of 1000",
		},
		{
			// runHookStep never expands a matrix, so a hook shard count
			// would load clean, run ONCE with literal {{ .vars.* }} text,
			// and stay green.
			name: "on a hook",
			yaml: `
jobs:
- name: j
  plan:
  - task: main
    run: echo hi
  on_success:
    task: cleanup
    parallelism: 2
    run: echo {{ .vars.index }}
`,
			want: "parallelism: is not valid on hook steps",
		},
		{
			// across: on a try: wrapper renders through to the task inside,
			// so the near-miss points there instead of asserting a visible
			// task step is not one.
			name: "on a try wrapper",
			yaml: `
jobs:
- name: j
  plan:
  - try:
      task: unit
      run: echo {{ .vars.index }}
    parallelism: 2
`,
			want: "belongs on the task inside the try:",
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
			// The dangling put: reference is reported too (Load joins
			// desugar and validate) — this case asserts only its own half.
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

// TestParallelismCountOnlyNameStillGetsCoordinates is the naming half of the
// count contract: count renders identically in every shard, so a name
// interpolating ONLY count is one name N times — and identical labels are
// identical cell hashes, which let shard 2 cache-skip against shard 1's
// success on the FIRST build. Interpolating the per-cell index, by contrast,
// is the author distinguishing the cells themselves.
func TestParallelismCountOnlyNameStillGetsCoordinates(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - task: unit-of-{{ .vars.count }}
    parallelism: 2
    run: echo copy
  - task: unit-{{ .vars.index }}
    parallelism: 2
    run: echo copy
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	countNamed, err := ExpandAcross(`job "j" step 0`, cfg.Jobs[0].Plan[0])
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"unit-of-2 [index=1]", "unit-of-2 [index=2]"} {
		if got := countNamed[i].DisplayName(); got != want {
			t.Errorf("count-named cell %d = %q, want %q — count does not distinguish cells", i, got, want)
		}
	}

	indexNamed, err := ExpandAcross(`job "j" step 1`, cfg.Jobs[0].Plan[1])
	if err != nil {
		t.Fatalf("ExpandAcross: %v", err)
	}

	for i, want := range []string{"unit-1", "unit-2"} {
		if got := indexNamed[i].DisplayName(); got != want {
			t.Errorf("index-named cell %d = %q, want %q — the author distinguished these", i, got, want)
		}
	}
}

// TestLoadJoinsParallelismAndValidateErrors: a desugar rejection must not
// hide validate()'s independent findings behind an extra load.
func TestLoadJoinsParallelismAndValidateErrors(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: a
  plan:
  - put: dest
    parallelism: 2
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig: expected an error")
	}

	for _, want := range []string{"only valid on a task step", `no resource named "dest"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig error = %q, missing %q — desugar and validate findings arrive in one load", err, want)
		}
	}
}

// TestParallelismErrorsNamedAcrossJobs pins the walk's own contract: a
// misplaced parallelism: in two jobs takes one load to name both.
func TestParallelismErrorsNamedAcrossJobs(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: a
  plan:
  - put: dest
    parallelism: 2
- name: c
  plan:
  - task: unit
    parallelism: -1
    run: echo hi
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig: expected an error")
	}

	for _, want := range []string{`job "a"`, `job "c"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("LoadConfig error = %q, missing %q — both jobs named in one load", err, want)
		}
	}
}
