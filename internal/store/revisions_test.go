package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
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
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for range 5 {
		err := store.RecordRevision(ctx, "sha-one", pipelineSource(1))
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}
	}

	if rows := countRows(ctx, t, store, "pipeline_revisions"); rows != 1 {
		t.Errorf("one configuration loaded five times recorded %d rows, want 1", rows)
	}

	err := store.RecordRevision(ctx, "sha-two", pipelineSource(2))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	if rows := countRows(ctx, t, store, "pipeline_revisions"); rows != 2 {
		t.Errorf("an edited configuration recorded %d rows in total, want 2", rows)
	}
}

// TestRunsRecordTheRevisionTheyWereGiven is the correction that removed the
// revision from this handle: a run names the configuration IT was handed,
// which is not necessarily the newest one this pipeline has loaded.
//
// Every argument order is exercised, because the defect it replaces was an
// ordering one — the row was written long after the caller took its config,
// so whichever revision happened to be newest at write time won.
func TestRunsRecordTheRevisionTheyWereGiven(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for _, sha := range []string{"sha-one", "sha-two"} {
		err := store.RecordRevision(ctx, sha, pipelineSource(1))
		if err != nil {
			t.Fatalf("RecordRevision(%s): %v", sha, err)
		}
	}

	// Started under the OLDER one, with the newer already interned — the
	// daemon reloaded while this run was getting under way.
	err := store.StartRun(ctx, "run-one", "build", "/tmp/ws-one", "sha-one")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.StartRun(ctx, "run-two", "build", "/tmp/ws-two", "sha-two")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	got := map[string]string{}

	rows, err := store.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	for _, row := range rows {
		got[row.ID] = row.ConfigSHA
	}

	for id, want := range map[string]string{"run-one": "sha-one", "run-two": "sha-two"} {
		if got[id] != want {
			t.Errorf("run %s reports configuration %q, want %q", id, got[id], want)
		}
	}
}

// TestResumeRecordsTheConfigItResumesUnder: a resume continues a failed run
// under the configuration it is being resumed WITH, which is usually the one
// that fixed it. Keeping the original would make the run claim it executed a
// pipeline nothing in it ever ran.
func TestResumeRecordsTheConfigItResumesUnder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for _, sha := range []string{"sha-broken", "sha-fixed"} {
		err := store.RecordRevision(ctx, sha, pipelineSource(1))
		if err != nil {
			t.Fatalf("RecordRevision(%s): %v", sha, err)
		}
	}

	err := store.StartRun(ctx, "run-one", "build", "/tmp/ws", "sha-broken")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.FinishRun(ctx, "run-one", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	err = store.ResumeRun(ctx, "run-one", "/tmp/ws", "sha-fixed")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	rows, err := store.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "sha-fixed" {
		t.Errorf("after a resume the run reports %+v, want sha-fixed", rows)
	}
}

// TestRunWithNoRecordedConfigurationSaysSo pins the NULL: a caller that
// loaded no pipeline file records no revision, and its runs must still be
// insertable — the column is a foreign key, and 0 is not a row id.
func TestRunWithNoRecordedConfigurationSaysSo(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.StartRun(ctx, "run-one", "build", "/tmp/ws-one", "")
	if err != nil {
		t.Fatalf("StartRun with no configuration recorded: %v", err)
	}

	rows, err := store.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "" {
		t.Errorf("a run started with no configuration reports %+v, want one row with an empty ConfigSHA", rows)
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
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const (
		keep    = 10
		builds  = 60
		jobName = "answer-mention"
	)

	for build := 1; build <= builds; build++ {
		// syntheticBuild records a configuration of its own per build, which
		// is the worst case a reloading daemon produces.
		syntheticBuild(ctx, t, store, jobName, build)

		err := store.PruneRuns(ctx, jobName, keep, "")
		if err != nil {
			t.Fatalf("PruneRuns: %v", err)
		}
	}

	// One per retained run, and never more: the run cap is the only bound,
	// which is the whole reason there is no config_history: setting.
	if rows := countRows(ctx, t, store, "pipeline_revisions"); rows > keep {
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
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const (
		keep    = 2
		jobName = "answer-mention"
	)

	for build := 1; build <= 5; build++ {
		syntheticBuild(ctx, t, store, jobName, build)
	}

	// The swap: loaded, and referenced by nothing that has run yet.
	err := store.RecordRevision(ctx, "sha-current", pipelineSource(2))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	// The build that started under the old configuration, finishing now.
	err = store.PruneRuns(ctx, jobName, keep, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	err = store.StartRun(ctx, "run-after-prune", jobName, "/tmp/ws-after", "sha-current")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := store.ListRuns(ctx, jobName, 10)
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
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for edit := 1; edit <= 50; edit++ {
		err := store.RecordRevision(ctx, fmt.Sprintf("sha-%03d", edit), pipelineSource(edit))
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}

		err = store.PruneRevisions(ctx)
		if err != nil {
			t.Fatalf("PruneRevisions: %v", err)
		}
	}

	// One: the newest. Nothing ran, so nothing else is reachable.
	if rows := countRows(ctx, t, store, "pipeline_revisions"); rows != 1 {
		t.Errorf("%d configurations survive 50 saves that ran nothing, want 1", rows)
	}
}

// TestRevisionsAreBoundedWhenRunsAreUnlimited: run_history: 0 means no limit
// on RUNS, and PruneRuns returns early on it — which left the configurations
// unbounded for the life of the file, on the one setting an operator chooses
// when they want to keep everything about their runs and nothing about their
// editor's autosaves.
func TestRevisionsAreBoundedWhenRunsAreUnlimited(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	for edit := 1; edit <= 20; edit++ {
		err := store.RecordRevision(ctx, fmt.Sprintf("sha-%03d", edit), pipelineSource(edit))
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}

		err = store.PruneRuns(ctx, "build", 0, "")
		if err != nil {
			t.Fatalf("PruneRuns: %v", err)
		}
	}

	if rows := countRows(ctx, t, store, "pipeline_revisions"); rows != 1 {
		t.Errorf("%d configurations survive under run_history: 0, want 1", rows)
	}
}

// TestFindRevisionIsScopedToItsPipeline: a state file may hold several
// pipelines, and a hash identifies bytes rather than a pipeline — so an
// unscoped lookup would serve one pipeline's page a configuration another
// pipeline ran, which is the shape of bug every query in this package carries
// a pipeline_id predicate to prevent.
func TestFindRevisionIsScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "shared.db")

	mine := mustOpenStore(t, path)
	defer func() { _ = mine.Close() }()

	theirs, err := OpenStore(path, "other")
	if err != nil {
		t.Fatalf("OpenStore as other: %v", err)
	}

	defer func() { _ = theirs.Close() }()

	err = theirs.RecordRevision(ctxFor(t), "sha-theirs", pipelineSource(1))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, found, err := mine.FindRevision(ctxFor(t), "sha-theirs")
	if err != nil {
		t.Fatalf("FindRevision: %v", err)
	}

	if found {
		t.Error("one pipeline read a configuration another pipeline recorded")
	}

	// And its own is still found, so the scoping is a predicate rather than a
	// lookup that never works.
	err = mine.RecordRevision(ctxFor(t), "sha-mine", pipelineSource(2))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	revision, found, err := mine.FindRevision(ctxFor(t), "sha-mine")
	if err != nil || !found {
		t.Fatalf("FindRevision of its own configuration = (%v, %v, %v)", revision.SHA, found, err)
	}

	if revision.Source != pipelineSource(2) {
		t.Error("FindRevision returned a configuration that is not the one recorded")
	}
}

// ctxFor is the test's context, named so the calls above read as one line.
func ctxFor(t *testing.T) context.Context {
	t.Helper()

	return t.Context()
}
