package main

// End-to-end coverage for concurrent matrix cells (#41): `max_in_flight:` on an
// across: step, which runs that many cells at once instead of walking them in
// declaration order.

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAcrossMaxInFlightRunsCellsConcurrently proves the cells actually overlap,
// without depending on wall-clock timing.
//
// Each cell writes a start marker and then waits for its siblings' markers to
// appear. Run concurrently, every cell sees all three almost immediately and
// records "overlapped". Run serially, the first cell waits out its budget alone
// and records "alone" — so a regression to the serial walk FAILS this test with
// a readable diagnosis rather than hanging it.
func TestAcrossMaxInFlightRunsCellsConcurrently(t *testing.T) {
	dir := t.TempDir()
	marks := filepath.Join(dir, "marks")
	log := filepath.Join(dir, "overlap.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: cell
      values: [a, b, c]
    max_in_flight: 3
    task: wait
    inputs: []
    run: |
      mkdir -p %[1]s
      touch %[1]s/{{ .vars.cell }}
      for _ in $(seq 1 100); do
        if [ "$(ls %[1]s | wc -l)" -ge 3 ]; then
          echo overlapped >> %[2]s
          exit 0
        fi
        sleep 0.1
      done
      echo alone >> %[2]s
`, marks, log))

	mustRun(t, path)

	got := readFileString(t, log)
	if strings.Contains(got, "alone") {
		t.Fatalf("a cell ran with no sibling in flight — max_in_flight did not take effect:\n%s", got)
	}

	if n := strings.Count(got, "overlapped"); n != 3 {
		t.Errorf("overlapped cells = %d, want 3:\n%s", n, got)
	}
}

// TestAcrossMaxInFlightKeepsExecutionOrderDeterministic pins the property that
// makes a concurrent matrix testable at all: cells finish in whatever order
// they finish, but assert.execution sees them in DECLARATION order.
//
// The cells below finish in reverse — c is quickest, a slowest — so a
// runner that recorded completions as they happened would produce c/b/a and
// fail this assert every time.
func TestAcrossMaxInFlightKeepsExecutionOrderDeterministic(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: cell
      values: [a, b, c]
    max_in_flight: 3
    task: "wait-{{ .vars.cell }}"
    inputs: []
    run: |
      case "{{ .vars.cell }}" in
        a) sleep 0.6 ;;
        b) sleep 0.3 ;;
      esac
  assert:
    execution: [wait-a, wait-b, wait-c]
`)

	mustRun(t, path)
}

// TestAcrossMaxInFlightScopesContextPerCell covers the hazard concurrency
// introduces for the run context store: two cells recording the same key.
//
// Serial cells resolve that the way two sequential steps do — the later wins,
// in an order readable off the pipeline. Concurrent cells have no order, so
// each writes to a scope only it touches and the join merges them under a key
// naming the cell, exactly as in_parallel: branches do. Without that, this
// records one `finding` belonging to whichever cell happened to finish last.
func TestAcrossMaxInFlightScopesContextPerCell(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: cell
      values: [alpha, beta]
    max_in_flight: 2
    task: "record-{{ .vars.cell }}"
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf '{{ .vars.cell }} found something' > context/finding
`)

	mustRun(t, path)

	got := runContextKeys(t, path)
	want := []string{"record-alpha.finding", "record-beta.finding"}

	if len(got) != len(want) {
		t.Fatalf("run context keys = %v, want one per cell %v (a lost key means two cells shared a scope)", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestAcrossMaxInFlightReportsEveryFailure pins that concurrency does not
// change what a failing cell means: a matrix asks which combinations work, so
// every cell runs and every failure is reported. There is deliberately no
// fail_fast: here — cancelling the siblings of the first red cell would answer
// that question for exactly one cell.
func TestAcrossMaxInFlightReportsEveryFailure(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: cell
      values: [a, b, c]
    max_in_flight: 3
    task: "check-{{ .vars.cell }}"
    inputs: []
    run: |
      echo {{ .vars.cell }} >> %[1]s
      test "{{ .vars.cell }}" = b
`, log))

	err := run([]string{path})
	if err == nil {
		t.Fatal("run succeeded with two failing cells")
	}

	for _, cell := range []string{"check-a", "check-c"} {
		if !strings.Contains(err.Error(), cell) {
			t.Errorf("error does not name failing cell %q: %v", cell, err)
		}
	}

	// Every cell ran, including the ones declared after the first failure.
	assertLineCount(t, log, 3)
}

// TestAcrossMaxInFlightIsNotHashed pins the caching contract. max_in_flight
// only decides how many cells run at once — the cell set is identical at any
// width — so changing it must not invalidate a single cached cell. This is the
// opposite of in_parallel:'s limit:/fail_fast:, which change which steps run at
// all and are therefore hashed.
func TestAcrossMaxInFlightIsNotHashed(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := func(width int) string {
		return fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: cell
      values: [a, b]
    max_in_flight: %[1]d
    task: "check-{{ .vars.cell }}"
    inputs: []
    run: echo {{ .vars.cell }} >> %[2]s
`, width, log)
	}

	path := writePipeline(t, dir, pipeline(1))
	mustRun(t, path)
	assertLineCount(t, log, 2)

	// Same cells, wider. Every cell is still a cache hit.
	path = writePipeline(t, dir, pipeline(2))
	mustRun(t, path)
	assertLineCount(t, log, 2)
}

// runContextKeys returns the keys recorded in the run context store, sorted, so
// a test can assert on what a run established without going through an agent
// (reads are agent-only by design).
func runContextKeys(t *testing.T, pipelinePath string) []string {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	// Only the run's OWN scope. A concurrent cell records into a per-cell scope
	// (branchContextScope spells it "<run>#<index>:<name>") and the join copies
	// those rows up under a prefixed key; the originals are left where they are,
	// the same as an in_parallel: branch's. Counting both would double every
	// key and say nothing about whether the merge worked.
	rows, err := db.QueryContext(t.Context(), `SELECT key FROM run_context WHERE run_id NOT LIKE '%#%'`)
	if err != nil {
		t.Fatalf("query run_context: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var keys []string

	for rows.Next() {
		var key string

		err = rows.Scan(&key)
		if err != nil {
			t.Fatalf("scan run_context: %v", err)
		}

		keys = append(keys, key)
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("iterate run_context: %v", err)
	}

	sort.Strings(keys)

	return keys
}
