package storetest

// Reading across the pipelines one state file holds.

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestSourcePathIsWhatAReaderReports: which checkout a pipeline was loaded
// from, for a reader telling two apart.
func (s suite) TestSourcePathIsWhatAReaderReports(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.SetSourcePath(ctx, "/src/app/pipeline.yml")
	if err != nil {
		t.Fatalf("SetSourcePath: %v", err)
	}

	rows, err := st.Reader().Pipelines(ctx)
	if err != nil || len(rows) != 1 || rows[0].Path != "/src/app/pipeline.yml" {
		t.Fatalf("Pipelines = %+v, %v; want test at /src/app/pipeline.yml", rows, err)
	}
}

// TestPipelinesReportWhatADaemonWouldServe: the listing carries the current sha, where it was set from and the pause state, so a daemon starting up knows which rows to serve without opening each one.
func (s suite) TestPipelinesReportWhatADaemonWouldServe(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	set := s.open(t, "test")
	unset := s.open(t, "other")

	setRevision(t, set, "sha-one", 1, nil)

	err := set.Pause(ctx)
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	rows, err := unset.Reader().Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	byName := map[string]store.PipelineRow{}
	for _, row := range rows {
		byName[row.Name] = row
	}

	if byName["test"].CurrentSHA != "sha-one" || !byName["test"].Paused {
		t.Errorf("the set, paused pipeline reports %+v", byName["test"])
	}

	if byName["test"].Path != "/src/app/pipeline.yml" {
		t.Errorf("the set pipeline reports it was set from %q, want the path SetCurrentRevision was given", byName["test"].Path)
	}

	if byName["other"].CurrentSHA != "" || byName["other"].Paused {
		t.Errorf("a pipeline nobody set or paused reports %+v", byName["other"])
	}
}

// pipelineNames is what the file holds, by name.
func pipelineNames(t *testing.T, st store.Store) []string {
	t.Helper()

	rows, err := st.Reader().Pipelines(ctxFor(t))
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	return names
}

// TestReaderRunsFilterToTheNamedPipelines is why the filter belongs to the
// read rather than to the caller. A state file may hold a pipeline this
// process does not serve, and a feed that fetched a limit and then dropped
// those rows would show fewer runs the busier the pipeline it cannot link to.
func (s suite) TestReaderRunsFilterToTheNamedPipelines(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	app := s.open(t, "app")
	unserved := s.open(t, "unserved")

	mustStartRuns(ctx, t, app, "build", "kept")
	mustStartRuns(ctx, t, unserved, "noise", "n1", "n2", "n3")

	runs, err := app.Reader().RecentRuns(ctx, []string{"app"}, 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "kept" || runs[0].Pipeline != "app" {
		t.Fatalf("RecentRuns returned %+v, want only app's one run", runs)
	}

	both, err := app.Reader().RecentRuns(ctx, []string{"app", "unserved"}, 0)
	if err != nil {
		t.Fatalf("RecentRuns(both, 0): %v", err)
	}

	if len(both) != 4 {
		t.Fatalf("RecentRuns over both pipelines with limit 0 returned %d rows, want all 4", len(both))
	}

	// Naming nothing asks for nothing, rather than quietly meaning "all" —
	// an empty served list is a configuration to report, not a wildcard.
	empty, err := app.Reader().RecentRuns(ctx, nil, 10)
	if err != nil {
		t.Fatalf("RecentRuns(nil): %v", err)
	}

	if len(empty) != 0 {
		t.Errorf("RecentRuns(nil) returned %d rows, want none", len(empty))
	}
}
