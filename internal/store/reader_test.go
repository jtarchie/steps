package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// sharedFile opens two pipelines onto one state file, which is what
// `steps web app.yml infra.yml --state shared.db` produces.
func sharedFile(t *testing.T, names ...string) []*Store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shared.db")
	stores := make([]*Store, 0, len(names))

	for _, name := range names {
		st, err := OpenStore(path, name)
		if err != nil {
			t.Fatalf("OpenStore(%q): %v", name, err)
		}

		t.Cleanup(func() { _ = st.Close() })

		stores = append(stores, st)
	}

	return stores
}

// TestReaderListsEveryPipelineInTheFile: the pipelines table records name and
// path for exactly this question, and until now nothing asked it.
func TestReaderListsEveryPipelineInTheFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := sharedFile(t, "app", "infra")

	rows, err := stores[0].Reader().Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(rows) != 2 {
		t.Fatalf("Pipelines returned %d rows, want both pipelines in the file: %+v", len(rows), rows)
	}

	// Sorted by name, so a listing is stable rather than in insertion order.
	if rows[0].Name != "app" || rows[1].Name != "infra" {
		t.Errorf("names = %q, %q; want app, infra in order", rows[0].Name, rows[1].Name)
	}

	// A reader built from EITHER handle sees the same file. The scoping lives
	// on the Store, not on the connection.
	other, err := stores[1].Reader().Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(other) != len(rows) {
		t.Errorf("the two handles disagree about the file: %d vs %d", len(other), len(rows))
	}
}

// TestReaderRunsSpanPipelinesNewestFirst is the query the global feed is. It
// is a NEW unscoped method rather than a nullable parameter on ListRuns
// precisely so the scoped path keeps its property: a Store method cannot be
// made to cross pipelines by passing it the wrong argument.
func TestReaderRunsSpanPipelinesNewestFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := sharedFile(t, "app", "infra")

	// Interleaved, so ordering by time is distinguishable from ordering by
	// pipeline — which is the bug a per-pipeline query followed by a merge
	// would have.
	mustStartRun(t, stores[0], "r1", "build")
	mustStartRun(t, stores[1], "r2", "provision")
	mustStartRun(t, stores[0], "r3", "deploy")

	runs, err := stores[0].Reader().RecentRuns(ctx, []string{"app", "infra"}, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}

	if len(runs) != 3 {
		t.Fatalf("RecentRuns returned %d rows, want all 3: %+v", len(runs), runs)
	}

	if runs[0].ID != "r3" || runs[1].ID != "r2" || runs[2].ID != "r1" {
		t.Errorf("order = %q, %q, %q; want r3, r2, r1 (newest first)", runs[0].ID, runs[1].ID, runs[2].ID)
	}

	// Every row says which pipeline it belongs to — without that a feed can
	// show a run it cannot link to.
	want := map[string]string{"r1": "app", "r2": "infra", "r3": "app"}
	for _, run := range runs {
		if run.Pipeline != want[run.ID] {
			t.Errorf("run %s belongs to %q, want %q", run.ID, run.Pipeline, want[run.ID])
		}
	}
}

// TestReaderRunsFilterToTheNamedPipelines is why the filter is in SQL rather
// than applied to the result. A state file may hold a pipeline this process
// does not serve — nothing stops `steps run other.yml --state shared.db` —
// and a feed that fetched a limit and then dropped those rows would show
// fewer runs the busier the pipeline it cannot link to.
func TestReaderRunsFilterToTheNamedPipelines(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := sharedFile(t, "app", "unserved")

	mustStartRun(t, stores[0], "kept", "build")

	for _, id := range []string{"n1", "n2", "n3"} {
		mustStartRun(t, stores[1], id, "noise")
	}

	runs, err := stores[0].Reader().RecentRuns(ctx, []string{"app"}, 2)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}

	if len(runs) != 1 {
		t.Fatalf("RecentRuns returned %d rows, want only app's 1: %+v", len(runs), runs)
	}

	if runs[0].ID != "kept" {
		t.Errorf("run = %q, want the served pipeline's own run", runs[0].ID)
	}

	// Naming nothing asks for nothing, rather than quietly meaning "all" —
	// an empty served list is a configuration to report, not a wildcard.
	empty, err := stores[0].Reader().RecentRuns(ctx, nil, 10)
	if err != nil {
		t.Fatalf("RecentRuns(nil): %v", err)
	}

	if len(empty) != 0 {
		t.Errorf("RecentRuns(nil) returned %d rows, want none", len(empty))
	}
}

func mustStartRun(t *testing.T, st *Store, id, jobName string) {
	t.Helper()

	err := st.StartRun(t.Context(), id, jobName, t.TempDir())
	if err != nil {
		t.Fatalf("StartRun(%q): %v", id, err)
	}
}

// TestOpenReaderReadsAFileItDoesNotOwn: `steps runs --state shared.db` has no
// pipeline to name, so there is no Store to borrow a connection from — and
// opening one the ordinary way would register the very pipeline the caller
// could not name.
func TestOpenReaderReadsAFileItDoesNotOwn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := sharedFile(t, "app", "infra")
	path := stores[0].Path()

	mustStartRun(t, stores[0], "r1", "build")

	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	defer func() { _ = reader.Close() }()

	pipelines, err := reader.Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(pipelines) != 2 {
		t.Fatalf("reader sees %d pipelines, want the file's 2: %+v", len(pipelines), pipelines)
	}

	runs, err := reader.RecentRuns(ctx, []string{"app", "infra"}, 10)
	if err != nil {
		t.Fatalf("RecentRuns: %v", err)
	}

	if len(runs) != 1 || runs[0].ID != "r1" {
		t.Errorf("runs = %+v, want the one recorded run", runs)
	}
}

// TestOpenReaderInventsNoPipeline: the difference from OpenStore, and the
// reason this exists at all. Opening a file to ask about it must not add a
// row to the very list it is about to print.
func TestOpenReaderInventsNoPipeline(t *testing.T) {
	t.Parallel()

	stores := sharedFile(t, "app")
	path := stores[0].Path()

	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	defer func() { _ = reader.Close() }()

	pipelines, err := reader.Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	if len(pipelines) != 1 || pipelines[0].Name != "app" {
		t.Errorf("pipelines = %+v, want only the one that was actually opened", pipelines)
	}
}

// TestOpenReaderRefusesAFileThatIsNotThere.
//
// The driver connects lazily, so an absent path otherwise becomes an empty
// database at the first query: a command asking about history would create
// the state file it was asking about, and answer "nothing ran" for a typo.
func TestOpenReaderRefusesAFileThatIsNotThere(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "absent.db")

	reader, err := OpenReader(path)
	if err == nil {
		_ = reader.Close()

		t.Fatal("OpenReader opened a database that does not exist")
	}

	_, statErr := os.Stat(path)
	if statErr == nil {
		t.Error("OpenReader created the database it was asked to read")
	}
}

// TestOpenReaderRefusesAnOlderSchema: a reader gets the same refusal a writer
// does. A stale file's tables are missing columns, and a SELECT naming one
// fails in a way that reads exactly like "nothing ever ran here".
func TestOpenReaderRefusesAnOlderSchema(t *testing.T) {
	t.Parallel()

	stores := sharedFile(t, "app")
	path := stores[0].Path()

	mustStartRun(t, stores[0], "r1", "build")

	err := stores[0].Close()
	if err != nil {
		t.Fatalf("closing: %v", err)
	}

	stampVersion(t, path, schemaVersion-1)

	reader, err := OpenReader(path)
	if err == nil {
		_ = reader.Close()

		t.Fatal("a database written by an older build was read as if this build understood it")
	}

	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("error = %v, want a schema-version refusal", err)
	}
}

// TestBorrowedReaderCloseLeavesTheStoreOpen: Store.Reader() shares the
// store's connection, so closing the reader must not close the store's handle
// out from under it — the store outlives it and its own Close is what
// checkpoints the WAL.
func TestBorrowedReaderCloseLeavesTheStoreOpen(t *testing.T) {
	t.Parallel()

	stores := sharedFile(t, "app")

	err := stores[0].Reader().Close()
	if err != nil {
		t.Fatalf("closing a borrowed reader: %v", err)
	}

	_, err = stores[0].ListRuns(t.Context(), "", 10)
	if err != nil {
		t.Fatalf("the store's connection was closed by its reader: %v", err)
	}
}

// TestSourcePathIsPerPipelineNotPerFile.
//
// The pipelines table records a path so a name can be traced back to a
// checkout, and it was being filled with the STATE FILE — the same string for
// every pipeline sharing one, which answers nothing. Only a command that
// loaded the YAML knows, so only it writes.
func TestSourcePathIsPerPipelineNotPerFile(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	stores := sharedFile(t, "app", "infra")

	err := stores[0].SetSourcePath(ctx, "/src/app/pipeline.yml")
	if err != nil {
		t.Fatalf("SetSourcePath: %v", err)
	}

	err = stores[1].SetSourcePath(ctx, "/src/infra/pipeline.yml")
	if err != nil {
		t.Fatalf("SetSourcePath: %v", err)
	}

	rows, err := stores[0].Reader().Pipelines(ctx)
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	want := map[string]string{"app": "/src/app/pipeline.yml", "infra": "/src/infra/pipeline.yml"}
	for _, row := range rows {
		if row.Path != want[row.Name] {
			t.Errorf("pipeline %s reports path %q, want %q", row.Name, row.Path, want[row.Name])
		}
	}
}
