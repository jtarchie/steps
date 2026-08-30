package main

// `steps runs` over a state file holding several pipelines.
//
// The scoped views answer "what did THIS pipeline do", which is every view
// the command had: a `--state shared.db` with three pipelines in it had no
// CLI answer to "what ran, across everything in this file" — only the web
// root did. Naming no pipeline is that question, and it reads through
// store.Reader, which crosses pipelines by construction.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// runIDsOf reads back the run ids one pipeline recorded in a shared file —
// what the cross-pipeline feed has to print for a row to be followable back
// to `steps runs <pipeline> --run <id>`.
func runIDsOf(t *testing.T, state, name string) []string {
	t.Helper()

	st, err := store.OpenStore(state, name)
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
		err := run([]string{"run", pipeline, "--job", "build", "--state", state})
		if err != nil {
			t.Fatalf("run %s: %v", filepath.Base(pipeline), err)
		}
	}

	return state, first, second
}

// TestRunsAcrossPipelines is the headline: no pipeline argument, one --state,
// and every pipeline in the file reports.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsAcrossPipelines(t *testing.T) {
	state, first, second := sharedRunsFixture(t)

	var runErr error

	out := captureStdout(t, func() {
		runErr = run([]string{"runs", "--state", state})
	})

	if runErr != nil {
		t.Fatalf("runs --state: %v", runErr)
	}

	// What the file holds, which is the other half of the question --state
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
		runErr = run([]string{"runs", first, "--state", state})
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

// TestRunsAcrossRefusesScopedQuestions.
//
// --job, --queue, --steps, --cost, --where and --run are all questions about
// one pipeline: a queue is per pipeline, a step's job name means nothing
// without one, and --run refuses an id belonging to another pipeline by
// design. Answering them across the file would mean inventing a semantic;
// answering them for a pipeline nobody named would mean picking one. Both are
// worse than saying so.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsAcrossRefusesScopedQuestions(t *testing.T) {
	state, _, _ := sharedRunsFixture(t)

	for _, flag := range [][]string{
		{"--job", "build"},
		{"--queue"},
		{"--steps"},
		{"--cost"},
		{"--where"},
		{"--run", "SOMERUNID"},
	} {
		t.Run(flag[0], func(t *testing.T) {
			var err error

			_ = captureStdout(t, func() {
				err = run(append([]string{"runs", "--state", state}, flag...))
			})

			if err == nil {
				t.Fatalf("%s across pipelines was answered instead of refused", flag[0])
			}

			// The message has to name the way out, which is the pipeline
			// argument the caller left off.
			if !strings.Contains(err.Error(), flag[0]) || !strings.Contains(err.Error(), "steps runs <pipeline>") {
				t.Errorf("refusal does not say what to do instead: %v", err)
			}
		})
	}
}

// TestRunsWithNoPipelineNeedsAState: without a pipeline there is no YAML to
// derive .steps/<name>.db from, so a bare `steps runs` has no file to read —
// and must say that rather than reading whatever `.steps/.db` resolves to in
// the current directory.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsWithNoPipelineNeedsAState(t *testing.T) {
	var err error

	_ = captureStdout(t, func() {
		err = run([]string{"runs"})
	})

	if err == nil {
		t.Fatal("`steps runs` with neither a pipeline nor --state was answered")
	}

	if !strings.Contains(err.Error(), "--state") {
		t.Errorf("refusal does not name the flag that would make it answerable: %v", err)
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
		err = run([]string{"runs", "--state", state})
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

	st, err := store.OpenStore(state, "never-run")
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	var runErr error

	out := captureStdout(t, func() {
		runErr = run([]string{"runs", "--state", state})
	})

	if runErr != nil {
		t.Fatalf("runs --state: %v", runErr)
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
