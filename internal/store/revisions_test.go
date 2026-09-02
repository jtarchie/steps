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

// TestRunsPinTheRevisionTheyStartedUnder is the pin, read back through the
// row a history view reads: a run started under one configuration keeps
// naming it after the handle has moved on to another.
//
// This is the half the watcher will stand on. A run that read the CURRENT
// revision at display time instead of the one it started under would report
// every historical run as having executed today's file.
func TestRunsPinTheRevisionTheyStartedUnder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	err := store.RecordRevision(ctx, "sha-one", pipelineSource(1))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = store.StartRun(ctx, "run-one", "build", "/tmp/ws-one")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.RecordRevision(ctx, "sha-two", pipelineSource(2))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = store.StartRun(ctx, "run-two", "build", "/tmp/ws-two")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := store.ListRuns(ctx, "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	got := map[string]string{}
	for _, row := range rows {
		got[row.ID] = row.ConfigSHA
	}

	for id, want := range map[string]string{"run-one": "sha-one", "run-two": "sha-two"} {
		if got[id] != want {
			t.Errorf("run %s reports configuration %q, want %q", id, got[id], want)
		}
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

	err := store.StartRun(ctx, "run-one", "build", "/tmp/ws-one")
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
		err := store.RecordRevision(ctx, fmt.Sprintf("sha-%03d", build), pipelineSource(build))
		if err != nil {
			t.Fatalf("RecordRevision: %v", err)
		}

		syntheticBuild(ctx, t, store, jobName, build)

		err = store.PruneRuns(ctx, jobName, keep, "")
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

// TestCurrentRevisionSurvivesRetention is the exemption, and it is the window
// a swap opens: a configuration is loaded, and a build that STARTED under the
// previous one finishes and prunes before anything has run under the new one.
// Without the exemption that sweep reaps a revision referenced by nothing
// yet, and the next run fails its foreign key on a row that was about to be
// pinned.
//
// Constructed by pruning once at the end rather than per build, which is what
// puts runs past the cap in the same sweep that sees the fresh revision
// unreferenced.
func TestCurrentRevisionSurvivesRetention(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	const (
		keep    = 2
		jobName = "build"
	)

	err := store.RecordRevision(ctx, "sha-before", pipelineSource(1))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	for build := 1; build <= 5; build++ {
		syntheticBuild(ctx, t, store, jobName, build)
	}

	// The swap: loaded, and referenced by nothing that has run yet.
	err = store.RecordRevision(ctx, "sha-current", pipelineSource(2))
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	// The build that started under the old configuration, finishing now.
	err = store.PruneRuns(ctx, jobName, keep, "")
	if err != nil {
		t.Fatalf("PruneRuns: %v", err)
	}

	err = store.StartRun(ctx, "run-after-prune", jobName, "/tmp/ws-after")
	if err != nil {
		t.Fatalf("a run started after retention swept the loaded configuration: %v", err)
	}

	rows, err := store.ListRuns(ctx, jobName, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	for _, row := range rows {
		if row.ID == "run-after-prune" && row.ConfigSHA != "sha-current" {
			t.Errorf("the run pinned %q, want sha-current", row.ConfigSHA)
		}
	}
}
