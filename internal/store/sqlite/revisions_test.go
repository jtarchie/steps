package sqlite

// What the revisions table costs and what a sweep leaves in it — measured
// against the rows themselves, which is why these stay with the driver.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// pipelineSource is a plausible pipeline YAML of a few kilobytes, which is
// what a revision row actually costs — the measurements below are meaningless
// against a 20-byte stand-in.
func pipelineSource(edit int) string {
	return fmt.Sprintf("# revision %d\njobs:\n- name: build\n  plan:\n  - task: compile\n    run: |\n      %s\n",
		edit, strings.Repeat("echo building; ", 200))
}

// TestRecordRevisionInternsOneRowPerConfiguration is the dedupe: a daemon
// loads the same file on every poll, and a row per load would grow the
// database with time rather than with change.
func TestRecordRevisionInternsOneRowPerConfiguration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	for range 5 {
		err := st.RecordRevision(ctx, "sha-one", pipelineSource(1), nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	if rows := countRows(ctx, t, st, "pipeline_revisions"); rows != 1 {
		t.Errorf("one configuration loaded five times recorded %d rows, want 1", rows)
	}

	err := st.RecordRevision(ctx, "sha-two", pipelineSource(2), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	if rows := countRows(ctx, t, st, "pipeline_revisions"); rows != 2 {
		t.Errorf("an edited configuration recorded %d rows in total, want 2", rows)
	}
}

// TestRevisionsAreBoundedByRunRetention is the measurement the retention
// decision rests on: the configuration is edited on EVERY build — the worst
// case there is — and the table still stops growing, because a revision no
// surviving run points at is reaped with them.
//
// The alternative designs both fail here rather than in review: revisions
// with a cap of their own would need a third knob to hold this, and revisions
// never reaped would grow one row per edit forever.
func TestRevisionsAreBoundedByRunRetention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	const (
		keep    = 10
		builds  = 60
		jobName = "answer-mention"
	)

	for build := 1; build <= builds; build++ {
		// syntheticBuild records a configuration of its own per build, which
		// is the worst case a reloading daemon produces.
		syntheticBuild(ctx, t, st, jobName, build)

		err := st.Prune(ctx, store.Retention{JobName: jobName, Runs: keep}, "")
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
	}

	// One per retained run, and never more: the run cap is the only bound,
	// which is the whole reason there is no config_history: setting.
	if rows := countRows(ctx, t, st, "pipeline_revisions"); rows > keep {
		t.Errorf("%d configurations survive %d builds under a run cap of %d; retention is not reaching them",
			rows, builds, keep)
	}
}

// TestTheNewestRevisionSurvivesRetention is the exemption, and it is the
// window a reload opens: a configuration is loaded, and a build that STARTED
// under the previous one finishes and prunes before anything has run under
// the new one. Without the exemption that sweep reaps the row the next run is
// about to name, and that run records no configuration at all.
func TestTheNewestRevisionSurvivesRetention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	const (
		keep    = 2
		jobName = "answer-mention"
	)

	for build := 1; build <= 5; build++ {
		syntheticBuild(ctx, t, st, jobName, build)
	}

	// The swap: loaded, and referenced by nothing that has run yet.
	err := st.RecordRevision(ctx, "sha-current", pipelineSource(2), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	// The build that started under the old configuration, finishing now.
	err = st.Prune(ctx, store.Retention{JobName: jobName, Runs: keep}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	err = st.StartRun(ctx, "run-after-prune", jobName, "/tmp/ws-after", "sha-current")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := st.ListRuns(ctx, jobName, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	for _, row := range rows {
		if row.ID == "run-after-prune" && row.ConfigSHA != "sha-current" {
			t.Errorf("the run pinned %q, want sha-current — the sweep reaped the configuration it was about to name", row.ConfigSHA)
		}
	}
}

// TestRevisionsAreBoundedWithoutAnyRunsBeingReaped is the other orphaning
// event, and the one the first bound missed entirely: an operator iterating
// on a pipeline with `steps web` watching it mints a multi-kilobyte row per
// distinct save, and none of those saves has to run anything. Waiting for a
// job to pass run_history: before reclaiming them is not a bound.
func TestRevisionsAreBoundedWithoutAnyRunsBeingReaped(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	for edit := 1; edit <= 50; edit++ {
		err := st.RecordRevision(ctx, fmt.Sprintf("sha-%03d", edit), pipelineSource(edit), nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}

		err = st.Prune(ctx, store.Retention{}, "")
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
	}

	// One: the newest. Nothing ran, so nothing else is reachable.
	if rows := countRows(ctx, t, st, "pipeline_revisions"); rows != 1 {
		t.Errorf("%d configurations survive 50 saves that ran nothing, want 1", rows)
	}
}

// TestRevisionsAreBoundedWhenRunsAreUnlimited: run_history: 0 means no limit
// on RUNS, and the run pass returns early on it — which left the configurations
// unbounded for the life of the file, on the one setting an operator chooses
// when they want to keep everything about their runs and nothing about their
// editor's autosaves.
func TestRevisionsAreBoundedWhenRunsAreUnlimited(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	for edit := 1; edit <= 20; edit++ {
		err := st.RecordRevision(ctx, fmt.Sprintf("sha-%03d", edit), pipelineSource(edit), nil)
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}

		err = st.Prune(ctx, store.Retention{JobName: "build", Runs: 0}, "")
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
	}

	if rows := countRows(ctx, t, st, "pipeline_revisions"); rows != 1 {
		t.Errorf("%d configurations survive under run_history: 0, want 1", rows)
	}
}

// TestARevertedConfigurationSurvivesTheSweep is the row the sweep protects
// getting the wrong answer from MAX(id).
//
// RecordRevision upserts by (pipeline_id, sha), and an upsert keeps the row it
// conflicts with — so reverting an edit re-loads a configuration whose id was
// minted BEFORE the one it supersedes. Keyed by id, the exemption then guarded
// the superseded row nobody was serving and swept the one every subsequent run
// was about to name.
func TestARevertedConfigurationSurvivesTheSweep(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	// The original, and a run under it: without one, the first sweep reclaims
	// it and the revert mints a fresh id, which is the case that already works.
	err := st.RecordRevision(ctx, "sha-original", pipelineSource(1), nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = st.StartRun(ctx, "run-one", "build", "/tmp/ws", "sha-original")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// The edit, then the revert. The daemon is serving sha-original again.
	for _, sha := range []string{"sha-edited", "sha-original"} {
		err = st.RecordRevision(ctx, sha, pipelineSource(2), nil)
		if err != nil {
			t.Fatalf("RecordRevision(%s): %v", sha, err)
		}
	}

	// The run that referenced it ages out, which is what makes the exemption
	// the only thing left holding the served configuration in the table.
	err = st.Prune(ctx, store.Retention{JobName: "build", Runs: 0}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	_, err = st.db.ExecContext(ctx, `DELETE FROM runs WHERE id = 'run-one'`)
	if err != nil {
		t.Fatalf("delete run: %v", err)
	}

	err = st.Prune(ctx, store.Retention{}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	_, found, err := st.FindRevision(ctx, "sha-original")
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if !found {
		t.Error("the sweep reclaimed the configuration being served, so the next run records none")
	}
}

// TestATrimmedChainCacheIsCommitted: the chain cache is capped at a different
// multiple of run_history: than runs and nodes are, so a build can cross it
// while both of those stay under theirs. A commit decision that did not ask
// this pass rolled its deletions back, and the table never shrank.
func TestATrimmedChainCacheIsCommitted(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = st.Close() }()

	const (
		keep   = 10
		chains = keep*chainsPerRetainedRun + 20
	)

	for chain := range chains {
		err := st.RecordChainSucceeded(ctx, "build", fmt.Sprintf("root-%04d", chain))
		if err != nil {
			t.Fatalf("RecordChainSucceeded: %v", err)
		}
	}

	// No runs and no nodes, so this pass is the only one with anything to do.
	err := st.Prune(ctx, store.Retention{JobName: "build", Runs: keep}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if rows := countRows(ctx, t, st, "job_runs"); rows != keep*chainsPerRetainedRun {
		t.Errorf("%d chain cache entries survive a cap of %d — the trim was rolled back",
			rows, keep*chainsPerRetainedRun)
	}
}
