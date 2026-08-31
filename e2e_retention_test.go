package main

// Retention, end to end: does `defaults.run_history:` actually bound what a
// pipeline leaves on disk when it is run through run(), rather than only when
// store.PruneRuns is called directly?
//
// internal/store proves the prune itself (see its footprint_test.go, which
// measures the bytes). This proves the WIRING — the config field, the flag, the
// resolution order, and the call at the end of every build — because a
// retention policy nothing invokes is indistinguishable from no policy, and
// that is exactly the state this repo was in: the code to bound run history did
// not exist, and the only capped table in the schema was resource_versions.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
)

// runDistinctBuilds runs a job `count` times, rewriting the pipeline each time
// so every build has DIFFERENT content.
//
// The rewrite is the point, not incidental. Nodes are content-addressed, so
// running the identical pipeline N times — even under --force, which only
// bypasses the skip — produces the same hashes and therefore the same handful of
// rows. A retention assertion made that way passes whether or not retention
// exists, which is how two of the tests below were vacuous the first time.
// Varying the script is what makes each build accumulate its own nodes.
func runDistinctBuilds(t *testing.T, dir, template string, count int) string {
	t.Helper()

	var path string

	for build := range count {
		path = writePipeline(t, dir, fmt.Sprintf(template, build))

		err := run([]string{"run", path, "--job", "build", "--force"})
		if err != nil {
			t.Fatalf("run %d: %v", build, err)
		}
	}

	return path
}

// countTableRows is how many rows a pipeline's state database holds in a table.
// The table name is a literal from this file, never caller input.
func countTableRows(t *testing.T, pipelinePath, table string) int {
	t.Helper()

	var count int

	err := openStateDB(t, pipelinePath).
		QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&count)
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}

	return count
}

// countRunRows is how many run rows a pipeline's state database holds.
func countRunRows(t *testing.T, pipelinePath string) int {
	t.Helper()

	return countTableRows(t, pipelinePath, "runs")
}

// TestRunHistoryBoundsWhatABuildLeavesBehind is the happy path: run a job more
// times than the cap and the database keeps the cap, not the count.
//
// The steps are deliberately trivial and the pipeline uncacheable-free, so what
// is measured is retention rather than the merkle cache skipping work: --force
// makes every invocation do the same work again and record it again.
func TestRunHistoryBoundsWhatABuildLeavesBehind(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
defaults:
  run_history: 3

jobs:
- name: build
  plan:
  - task: work
    run: echo working
`)

	const invocations = 8

	for range invocations {
		err := run([]string{"run", path, "--job", "build", "--force"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := countRunRows(t, path); got != 3 {
		t.Errorf("runs = %d after %d invocations under run_history: 3, want 3", got, invocations)
	}
}

// TestRunHistoryReapsAWholeRun: a reaped run takes its events, its per-step
// records and its cached nodes with it. Without the foreign keys those tables
// gained, the run row would vanish and everything hanging off it would stay
// forever as rows nothing can reach — which is the leak, not the run row.
func TestRunHistoryReapsAWholeRun(t *testing.T) {
	dir := t.TempDir()

	path := runDistinctBuilds(t, dir, `
defaults:
  run_history: 1

jobs:
- name: build
  plan:
  - task: first
    run: echo one %[1]d
  - task: second
    run: echo two %[1]d
`, 5)

	db := openStateDB(t, path)

	// Every child table must be scoped to the one surviving run. A count that
	// still reflects five builds is a table the cascade missed.
	for _, table := range []string{"run_events", "run_steps"} {
		var orphans int

		err := db.QueryRowContext(context.Background(), fmt.Sprintf(
			`SELECT COUNT(*) FROM %s t WHERE NOT EXISTS (SELECT 1 FROM runs r WHERE r.id = t.run_id)`,
			table)).Scan(&orphans)
		if err != nil {
			t.Fatalf("count orphaned %s: %v", table, err)
		}

		if orphans != 0 {
			t.Errorf("%s holds %d rows whose run was reaped", table, orphans)
		}
	}

	// Node reaping is asserted separately, below: the cache is bounded by count at
	// a multiple of run_history:, so five builds of a two-step plan are nowhere
	// near the cap and none of their nodes is a candidate. That is the design
	// rather than a gap in this test — see pruneNodes.
}

// TestRunHistoryReapsCachedNodes covers the biggest table, the merkle cache.
//
// It is bounded by COUNT rather than by age, at a multiple of run_history:, and
// that is the correction of a real bug rather than a detail. Under an age rule a
// fully-cached pipeline lost the cache it was relying on: a cache hit records no
// node, never reaches recordChainSucceeded, and publishes its skip events with an
// empty hash, so nothing about a HIT refreshes any timestamp and the entries just
// looked older every poll. A count rule cannot do that — no new work means no new
// entries means no eviction — which is also why this test needs no sleep, where
// the age-based version did.
func TestRunHistoryReapsCachedNodes(t *testing.T) {
	dir := t.TempDir()

	// run_history: 1 makes the node cap 1 * nodesPerRetainedRun, so this has to
	// run more than that many distinct builds for the cap to bind at all.
	const builds = 40

	path := runDistinctBuilds(t, dir, `
defaults:
  run_history: 1

jobs:
- name: build
  plan:
  - task: work
    run: echo working %[1]d
`, builds)

	nodes := countTableRows(t, path, "nodes")

	if nodes >= builds {
		t.Errorf("nodes = %d after %d distinct builds; the cache is not bounded", nodes, builds)
	}

	// The interned content went with them, since nothing points at it any more —
	// its reference declares RESTRICT, so a missed sweep would surface as rows
	// outliving every node that used them.
	if content := countTableRows(t, path, "node_content"); content > nodes {
		t.Errorf("node_content = %d rows for %d nodes; interned content outlived every node using it",
			content, nodes)
	}

	// And nothing points at a node that is gone.
	if orphans := countTableRows(t, path,
		"nodes c WHERE c.parent_hash IS NOT NULL AND c.parent_hash NOT IN (SELECT hash FROM nodes)"); orphans != 0 {
		t.Errorf("%d nodes still name a parent the prune deleted", orphans)
	}
}

// TestRunHistoryZeroKeepsEverything pins the zero convention this repo uses
// everywhere else. It matters more than it looks: the difference between "unset,
// so use the default" and "set to zero, so keep everything" is the difference
// between a bounded database and an unbounded one, and a plain int field cannot
// carry it.
func TestRunHistoryZeroKeepsEverything(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
defaults:
  run_history: 0

jobs:
- name: build
  plan:
  - task: work
    run: echo working
`)

	const invocations = 4

	for range invocations {
		err := run([]string{"run", path, "--job", "build", "--force"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := countRunRows(t, path); got != invocations {
		t.Errorf("runs = %d under run_history: 0, want all %d — zero must mean no limit", got, invocations)
	}
}

// TestRunHistoryFlagLosesToThePipeline is the precedence rule, which is the same
// one --version-history follows: the pipeline wins, because it is the thing that
// knows how much its jobs write.
func TestRunHistoryFlagLosesToThePipeline(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
defaults:
  run_history: 0

jobs:
- name: build
  plan:
  - task: work
    run: echo working
`)

	for range 4 {
		err := run([]string{"run", path, "--job", "build", "--force", "--run-history", "1"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := countRunRows(t, path); got != 4 {
		t.Errorf("runs = %d, want 4 — the pipeline's run_history: 0 must beat --run-history 1", got)
	}
}

// TestRunHistoryFlagAppliesWhenThePipelineIsSilent is the other half: the flag
// is a DEFAULT, so it has to actually take effect on a pipeline that declares
// nothing. This is also the regression test for --version-history having been
// declared on `steps run` and never applied at all.
func TestRunHistoryFlagAppliesWhenThePipelineIsSilent(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: work
    run: echo working
`)

	for range 5 {
		err := run([]string{"run", path, "--job", "build", "--force", "--run-history", "2"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := countRunRows(t, path); got != 2 {
		t.Errorf("runs = %d, want 2 — --run-history must apply when the pipeline says nothing", got)
	}
}

// TestRetentionLeavesTriggerStateAlone is the safety property, and the one worth
// the most: bounding history must never make a watch re-answer something it has
// already handled.
//
// A version cursor or a recorded version lost to retention would do exactly
// that — the pipeline this work came from is a Slack bot, and re-answering is
// the failure mode that costs money and looks broken in public. So those tables
// are deliberately outside everything PruneRuns touches.
func TestRetentionLeavesTriggerStateAlone(t *testing.T) {
	dir := t.TempDir()

	template := `
defaults:
  run_history: 1

resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"},{"n":"2"},{"n":"3"}]'
    in: echo ` + "{{ .version.n | shellquote }}" + ` > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  # version: every is what creates a cursor at all, and it is the exact shape
  # the Slack bot uses: answer each mention once. A plain get takes "latest" and
  # records no cursor, so a test using one would assert nothing here.
  - get: ticks
    version: every
  - task: work
    run: echo working %[1]d
`

	path := writePipeline(t, dir, fmt.Sprintf(template, 0))

	// SEVERAL invocations, because one is not enough to prune anything: a
	// version: every fan-out reuses ONE run id for every version it builds
	// (pipeline/get.go re-upserts the same row), so a single invocation leaves one
	// run and a cap of 1 deletes nothing. Asserted against a prune that did no
	// work, this test passed while testing nothing — the trap this file's header
	// warns about.
	for build := range 4 {
		invocation := writePipeline(t, dir, fmt.Sprintf(template, build))

		err := run([]string{"run", invocation, "--job", "build", "--force"})
		if err != nil {
			t.Fatalf("run %d: %v", build, err)
		}
	}

	db := openStateDB(t, path)

	// The runs are capped, so retention definitely ran and deleted something...
	if got := countRunRows(t, path); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}

	// ...and the state that decides what to build next is untouched.
	for _, intact := range []struct {
		what  string
		query string
		want  int
	}{
		{"recorded resource versions", `SELECT COUNT(*) FROM resource_versions WHERE resource_name = 'ticks'`, 3},
		{"the per-job version cursor", `SELECT COUNT(*) FROM job_version_cursor`, 1},
		// The value, not just the row: a cursor reset to 0 would survive a count
		// and re-answer every version on the next poll, which is the failure this
		// whole test exists to rule out.
		{"the cursor's high-water mark", `SELECT COALESCE(MAX(check_order), 0) FROM job_version_cursor`, 3},
	} {
		var got int

		err := db.QueryRowContext(context.Background(), intact.query).Scan(&got)
		if err != nil {
			t.Fatalf("%s: %v", intact.what, err)
		}

		if got != intact.want {
			t.Errorf("%s = %d, want %d — retention must not touch what decides the next build",
				intact.what, got, intact.want)
		}
	}
}

// TestRetentionKeepsTheDatabaseInternallyConsistent runs sqlite's own referential
// check after a retention pass, which is the broadest statement available that
// the cascades and the explicit sweeps agree with each other.
func TestRetentionKeepsTheDatabaseInternallyConsistent(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
defaults:
  run_history: 2

jobs:
- name: build
  plan:
  - task: work
    run: echo working
  - do:
    - task: inner
      run: echo inner
`)

	for range 6 {
		err := run([]string{"run", path, "--job", "build", "--force"})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	rows, err := openStateDB(t, path).
		QueryContext(context.Background(), `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}

	defer func() { _ = rows.Close() }()

	var violations []string

	for rows.Next() {
		var (
			table, parent string
			rowid         sql.NullInt64
			constraint    sql.NullInt64
		)

		err = rows.Scan(&table, &rowid, &parent, &constraint)
		if err != nil {
			t.Fatalf("foreign_key_check: %v", err)
		}

		violations = append(violations, fmt.Sprintf("%s -> %s", table, parent))
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}

	if len(violations) != 0 {
		t.Errorf("foreign_key_check found %d violations after retention: %v", len(violations), violations)
	}
}

// TestStateDatabaseStaysSmallAcrossManyBuilds is the whole change in one
// assertion, at the only level a user experiences it: the size of the file on
// disk after a pipeline has run many times.
func TestStateDatabaseStaysSmallAcrossManyBuilds(t *testing.T) {
	dir := t.TempDir()

	path := runDistinctBuilds(t, dir, `
defaults:
  run_history: 5

jobs:
- name: build
  plan:
  - task: work
    run: echo working %[1]d
`, 40)

	// Generous, because sqlite allocates in pages and a schema this wide costs
	// a page per table and index before a single row is written. The point is
	// the ORDER of magnitude: unbounded, forty builds of even a one-step plan
	// grew this without limit, and a real pipeline's builds are far bigger.
	const ceiling = 1 << 20

	info, err := os.Stat(statePath(path, ""))
	if err != nil {
		t.Fatalf("stat state.db: %v", err)
	}

	if info.Size() > ceiling {
		t.Errorf("state.db is %d bytes after 40 builds under run_history: 5, want under %d", info.Size(), ceiling)
	}

	t.Logf("state.db after 40 builds under run_history: 5 = %d bytes", info.Size())
}

// TestVersionHistoryFlagAppliesWhenThePipelineIsSilent.
//
// --version-history is the flag whose whole story is that it was declared and
// applied nowhere: it read as configured on `steps run` and bound nothing, and
// that bug is what the flag embeds exist to prevent. It still had no test of
// its own — the run-history sibling above was doing the work — so mutation
// testing walked straight through the line that applies it.
func TestVersionHistoryFlagAppliesWhenThePipelineIsSilent(t *testing.T) {
	dir := t.TempDir()
	versions := dir + "/versions.json"

	writePipelineFile(t, versions, `[{"ref":"v1"},{"ref":"v2"},{"ref":"v3"},{"ref":"v4"},{"ref":"v5"}]`)

	path := writePipeline(t, dir, `
resource_types:
- name: listing
  config:
    check: cat `+versions+`
    in: echo {{ .version.ref | shellquote }} > ref.txt
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: repo
  - task: work
    inputs: [repo]
    run: cat repo/ref.txt
`)

	err := run([]string{"run", path, "--job", "build", "--version-history", "2"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Nothing is pruned yet, and that is the documented rule rather than the
	// flag failing: the cap bounds what has SCROLLED AWAY, and a window still
	// being reported is kept whole however small the cap (see
	// store.RecordVersions — pruning a version the check still reports makes
	// the next poll rediscover it at a fresh order, and the table oscillates).
	if got := countTableRows(t, path, "resource_versions"); got != 5 {
		t.Fatalf("resource_versions = %d, want the 5 the check still reports", got)
	}

	// Now they scroll away: the check reports only the newest, so the older
	// four are finally prunable and the cap is what decides how many survive.
	writePipelineFile(t, versions, `[{"ref":"v5"}]`)

	err = run([]string{"run", path, "--job", "build", "--force", "--version-history", "2"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := countTableRows(t, path, "resource_versions"); got != 2 {
		t.Errorf("resource_versions = %d, want 2 — --version-history must apply when the pipeline says nothing", got)
	}
}
