package e2e

// `steps runs` over a state file holding several pipelines.
//
// The scoped views answer "what did THIS pipeline do", which is every view
// the command had: a `--db shared.db` with three pipelines in it had no
// CLI answer to "what ran, across everything in this file" — only the web
// root did. Naming no pipeline is that question, and it reads through
// store.Reader, which crosses pipelines by construction.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// runIDsOf reads back the run ids one pipeline recorded in a shared file —
// what the cross-pipeline feed has to print for a row to be followable back
// to `steps runs cost -p <pipeline> <id>`.
func runIDsOf(t *testing.T, state, name string) []string {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open shared store as %s: %v", name, err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.ListRuns(t.Context(), "", 10)
	if err != nil {
		t.Fatalf("list runs for %s: %v", name, err)
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}

	if len(ids) == 0 {
		t.Fatalf("%s recorded no runs, so there is nothing for the feed to show", name)
	}

	return ids
}

// sharedRunsFixture runs two pipelines into one state database and returns
// the file. Both jobs are named build over an identical task, so the only
// thing telling their rows apart in the output is the pipeline column.
func sharedRunsFixture(t *testing.T) (state, first, second string) {
	t.Helper()

	dir := t.TempDir()
	state = filepath.Join(dir, "shared.db")

	first = sharedStatePipeline(t, filepath.Join(dir, "first.yml"), filepath.Join(dir, "first.log"))
	second = sharedStatePipeline(t, filepath.Join(dir, "second.yml"), filepath.Join(dir, "second.log"))

	for _, pipeline := range []string{first, second} {
		err := cli.Run([]string{"run", pipeline, "--job", "build", "--db", state})
		if err != nil {
			t.Fatalf("run %s: %v", filepath.Base(pipeline), err)
		}
	}

	return state, first, second
}

// TestRunsAcrossPipelines is the headline: no pipeline argument, one --db,
// and every pipeline in the file reports.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsAcrossPipelines(t *testing.T) {
	state, first, second := sharedRunsFixture(t)

	var runErr error

	out := captureStdout(t, func() {
		runErr = cli.Run([]string{"runs", "--db", state})
	})

	if runErr != nil {
		t.Fatalf("runs --db: %v", runErr)
	}

	// What the file holds, which is the other half of the question --db
	// created: a name alone does not say which YAML is behind it.
	for _, want := range []string{"PIPELINE", "first", "second", first, second} {
		if !strings.Contains(out, want) {
			t.Errorf("cross-pipeline output is missing %q:\n%s", want, out)
		}
	}

	// And every run, from both pipelines, by id. Ids rather than a row count
	// because both pipelines run a job named build: a feed that lost the
	// second pipeline's rows and duplicated the first's would still have the
	// right number of lines saying `build`.
	for _, name := range []string{"first", "second"} {
		for _, id := range runIDsOf(t, state, name) {
			if !strings.Contains(out, id) {
				t.Errorf("run %s of pipeline %s is missing from the feed:\n%s", id, name, out)
			}
		}
	}
}

// TestRunsScopedStaysScoped: naming a pipeline against the same shared file
// still answers for that pipeline alone, with no pipeline column.
//
// The two views read different tables, and this is the guard that adding the
// unscoped one did not quietly route the scoped one through the reader.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsScopedStaysScoped(t *testing.T) {
	state, first, _ := sharedRunsFixture(t)

	var runErr error

	out := captureStdout(t, func() {
		runErr = cli.Run([]string{"runs", "-p", cli.PipelineName(first), "--db", state})
	})

	if runErr != nil {
		t.Fatalf("runs %s: %v", first, runErr)
	}

	if strings.Contains(out, "PIPELINE") {
		t.Errorf("a scoped listing grew a pipeline column:\n%s", out)
	}

	// The second pipeline's runs live in the same file and must not appear.
	for _, id := range runIDsOf(t, state, "second") {
		if strings.Contains(out, id) {
			t.Errorf("scoped listing leaked run %s from another pipeline:\n%s", id, out)
		}
	}
}

// TestScopedViewsRequireAPipeline.
//
// steps, queue, cost and where are questions about one pipeline: a queue
// belongs to a pipeline, a step's job name means nothing without one, and
// cost/where already refuse an id belonging to a neighbour in the same file.
// Answering them across a file would mean inventing a semantic; answering
// them for a pipeline nobody named would mean picking one.
//
// A refusal from the COMMAND rather than the grammar, which is what `-p`
// changed: the pipeline is a flag now, because a served pipeline has no file
// here to name — so the refusal has to name the flag and say where to find
// the names it takes.
func TestScopedViewsRequireAPipeline(t *testing.T) {
	t.Parallel()

	state, _, _ := sharedRunsFixture(t)

	for _, view := range []string{"steps", "queue", "cost", "where"} {
		t.Run(view, func(t *testing.T) {
			err := cli.Run([]string{"runs", view, "--db", state})
			if err == nil {
				t.Fatalf("runs %s answered without a pipeline to answer for", view)
			}

			if !strings.Contains(err.Error(), "-p") {
				t.Errorf("runs %s does not say which flag names a pipeline: %v", view, err)
			}
		})
	}
}

// TestJobFilterIsRefusedAcrossPipelines is the one flag the grammar cannot
// settle: --job is legitimate on `list`, and meaningless when the listing
// spans a file, because the cross-pipeline query does not filter by job and
// two pipelines calling a job `build` are not one job. Silently ignoring it
// would answer a different question than the one typed.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestJobFilterIsRefusedAcrossPipelines(t *testing.T) {
	state, _, _ := sharedRunsFixture(t)

	var err error

	_ = captureStdout(t, func() {
		err = cli.Run([]string{"runs", "--db", state, "--job", "build"})
	})

	if err == nil {
		t.Fatal("--job across a whole state file was answered rather than refused")
	}

	if !strings.Contains(err.Error(), "--job") || !strings.Contains(err.Error(), "steps runs list -p <name>") {
		t.Errorf("refusal does not say what to do instead: %v", err)
	}
}

// TestRunsWithNoPipelineReadsTheDaemonsOwnState: a bare `steps runs` reads
// the database a daemon keeps its pipelines in, which is the whole reason
// that database has a fixed name rather than one derived from a file.
//
// It used to be a refusal, because without a pipeline there was no YAML to
// derive a path from. There is one now, and answering "nothing there" for a
// file that does not exist is the honest answer rather than a usage error.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsWithNoPipelineReadsTheDaemonsOwnState(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "nothing-here.db")

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"runs", "--db", absent})
	})

	if err != nil {
		t.Fatalf("`steps runs` against a database that is not there: %v", err)
	}

	if !strings.Contains(out, absent) {
		t.Errorf("the answer does not name the database it looked in:\n%s", out)
	}

	if fileExists(absent) {
		t.Error("asking about history created the database it was asking about")
	}
}

// TestRunsAcrossMissingStateCreatesNothing: asking about history must never
// leave a database behind, the same promise the scoped path already keeps.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsAcrossMissingStateCreatesNothing(t *testing.T) {
	state := filepath.Join(t.TempDir(), "absent.db")

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"runs", "--db", state})
	})

	if err != nil {
		t.Fatalf("asking about a state file that does not exist is not an error: %v", err)
	}

	if !strings.Contains(out, state) {
		t.Errorf("output does not name the file it looked for:\n%s", out)
	}

	if fileExists(state) {
		t.Error("a read command created the state database it was asked about")
	}
}

// TestRunsAcrossPipelineWithNothingRecorded.
//
// A pipeline exists in the file the moment any command opens it by name, and
// two things about it are then unknown: where its YAML lives (only a command
// that LOADED one records that) and whether it has ever run. Both have to
// read as unanswered rather than as an answer — a blank path column reads as
// a pipeline living at "", and a feed that printed nothing at all reads as a
// broken command.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsAcrossPipelineWithNothingRecorded(t *testing.T) {
	state := filepath.Join(t.TempDir(), "shared.db")

	st, err := sqlite.OpenStore(state, "never-run")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	var runErr error

	out := captureStdout(t, func() {
		runErr = cli.Run([]string{"runs", "--db", state})
	})

	if runErr != nil {
		t.Fatalf("runs --db: %v", runErr)
	}

	// The whole row, not just the name: "never-run" contains a dash of its
	// own, so asserting the two pieces separately would pass on a blank
	// column.
	if !strings.Contains(out, "never-run  -\n") {
		t.Errorf("a pipeline with no recorded source is not listed as unanswered:\n%s", out)
	}

	if !strings.Contains(out, "no runs recorded") {
		t.Errorf("a file whose pipelines have never run says nothing about it:\n%s", out)
	}
}

// TestRunsDoesNotMintThePipelineItWasAskedAbout.
//
// `steps runs` is a read, and reads do not create. It used to open the state
// store the ordinary way, which registers whatever name it was handed — so a
// typo left a pipeline in the file forever, and the answer it gave back ("no
// job runs recorded") was indistinguishable from a pipeline that simply had
// not run yet.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsDoesNotMintThePipelineItWasAskedAbout(t *testing.T) {
	state, _, _ := sharedRunsFixture(t)

	var runErr error

	out := captureStdout(t, func() {
		runErr = cli.Run([]string{"runs", "-p", "typo", "--db", state})
	})

	if runErr == nil {
		t.Fatalf("a pipeline the file has never heard of was answered for:\n%s", out)
	}

	// The message names the file's actual contents, because the whole class
	// of mistake here is a name that is nearly right.
	for _, want := range []string{"typo", "first", "second"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, runErr)
		}
	}

	listing := captureStdout(t, func() {
		err := cli.Run([]string{"runs", "--db", state})
		if err != nil {
			t.Errorf("runs --db: %v", err)
		}
	})

	if strings.Contains(listing, "typo") {
		t.Errorf("the read registered the pipeline it was asked about:\n%s", listing)
	}
}
