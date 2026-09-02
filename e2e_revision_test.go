package main

// Which configuration a run actually executed.
//
// A run recorded what it did and never what it was told to do, so a job that
// started behaving differently had no answer to "did the pipeline change?" —
// the file on disk is only ever its newest version, and the run that broke
// may have executed a different one. These record the configuration a run was
// started from, keyed by the content it was parsed from, so two runs of one
// config share a row and an edit mints a new one.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// cellSplit separates the columns of a tab-written table.
var cellSplit = regexp.MustCompile(`\s{2,}`)

// revisionPipeline writes a pipeline whose task command is the caller's, so a
// change to it is a change to the parsed configuration.
func revisionPipeline(t *testing.T, path, command string) {
	t.Helper()

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: %s
`, command))
}

// configColumn reads the CONFIG cell of every row `steps runs` printed.
//
// Positional rather than a substring search: a sha that appeared anywhere in
// the output would satisfy a test that never checked the runs were told
// apart, which is the whole claim.
//
// Split on runs of two or more spaces, which is what the tab writer pads
// with — WHEN holds a date and a time separated by ONE space, so splitting on
// whitespace reads every row one column to the left and compares run ids
// while claiming to compare configurations.
func configColumn(t *testing.T, out string) []string {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("no run rows printed:\n%s", out)
	}

	header := cellSplit.Split(strings.TrimSpace(lines[0]), -1)

	column := -1

	for i, name := range header {
		if name == "CONFIG" {
			column = i
		}
	}

	if column < 0 {
		t.Fatalf("no CONFIG column in the header %q:\n%s", lines[0], out)
	}

	var cells []string

	// The table ends at the first blank line; what follows is the footer
	// pointing at `steps runs steps`, whose words would otherwise be read as
	// cells of a run that never happened.
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			break
		}

		fields := cellSplit.Split(strings.TrimSpace(line), -1)
		if len(fields) <= column {
			continue
		}

		cells = append(cells, fields[column])
	}

	return cells
}

// TestRunsReportTheConfigTheyRan is the headline: three runs, two
// configurations, and the listing says which run used which.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsReportTheConfigTheyRan(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.db")
	pipeline := filepath.Join(dir, "pipeline.yml")
	log := filepath.Join(dir, "build.log")

	revisionPipeline(t, pipeline, "echo one >> "+log)

	// Twice against the same file: an unchanged configuration is one
	// revision however many runs execute it, so the second run must NOT mint
	// a row of its own.
	for range 2 {
		err := run([]string{"run", pipeline, "--job", "build", "--state", state, "--force"})
		if err != nil {
			t.Fatalf("run against the first config: %v", err)
		}
	}

	revisionPipeline(t, pipeline, "echo two >> "+log)

	err := run([]string{"run", pipeline, "--job", "build", "--state", state, "--force"})
	if err != nil {
		t.Fatalf("run against the edited config: %v", err)
	}

	var runErr error

	out := captureStdout(t, func() {
		runErr = run([]string{"runs", pipeline, "--state", state})
	})

	if runErr != nil {
		t.Fatalf("steps runs: %v", runErr)
	}

	cells := configColumn(t, out)
	if len(cells) != 3 {
		t.Fatalf("three runs executed, %d rows carry a config:\n%s", len(cells), out)
	}

	// Newest first, so the edited config leads and the two runs of the
	// original follow it, agreeing with each other.
	if cells[0] == cells[1] {
		t.Errorf("the edited config recorded the same revision as the one before it (%s):\n%s", cells[0], out)
	}

	if cells[1] != cells[2] {
		t.Errorf("two runs of ONE config recorded different revisions (%s, %s):\n%s", cells[1], cells[2], out)
	}
}

// TestRunsSeparateConfigsThatDifferOnlyByVars pins the seam the watcher will
// stand on: ((var)) substitution happens BEFORE the parse, so the same file
// under different vars is a different configuration, and a revision computed
// from the file on disk rather than from what was parsed would call these one.
func TestRunsSeparateConfigsThatDifferOnlyByVars(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state.db")
	pipeline := filepath.Join(dir, "pipeline.yml")
	log := filepath.Join(dir, "build.log")

	revisionPipeline(t, pipeline, "echo ((greeting)) >> "+log)

	for _, greeting := range []string{"hello", "goodbye"} {
		err := run([]string{
			"run", pipeline, "--job", "build",
			"--state", state, "--force",
			"--var", "greeting=" + greeting,
		})
		if err != nil {
			t.Fatalf("run with greeting=%s: %v", greeting, err)
		}
	}

	var runErr error

	out := captureStdout(t, func() {
		runErr = run([]string{"runs", pipeline, "--state", state})
	})

	if runErr != nil {
		t.Fatalf("steps runs: %v", runErr)
	}

	cells := configColumn(t, out)
	if len(cells) != 2 {
		t.Fatalf("two runs executed, %d rows carry a config:\n%s", len(cells), out)
	}

	if cells[0] == cells[1] {
		t.Errorf("one file under two vars recorded one revision (%s), so a vars change would swap nothing:\n%s", cells[0], out)
	}
}

// TestRecordRevisionSkipsAConfigThatWasNeverLoaded covers the branch every
// e2e above takes the other side of: a Config built in memory has no source
// to hash, and recording one anyway would write a row describing nothing.
func TestRecordRevisionSkipsAConfigThatWasNeverLoaded(t *testing.T) {
	t.Parallel()

	st, err := store.OpenStore(filepath.Join(t.TempDir(), "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = recordRevision(t.Context(), st, &config.Config{})
	if err != nil {
		t.Fatalf("recording a config that was never loaded: %v", err)
	}

	err = st.StartRun(t.Context(), "run-one", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(rows) != 1 || rows[0].ConfigSHA != "" {
		t.Fatalf("got %+v, want one run reporting no configuration", rows)
	}

	// And that is what the column prints, which is the claim docs/README.md
	// makes about a dash.
	if got := shortConfig(rows[0].ConfigSHA); got != "-" {
		t.Errorf("a run with no configuration prints %q, want %q", got, "-")
	}
}

// TestShortConfigKeepsAHashThatAlreadyFits pins the third branch: a hash
// shorter than the column is printed whole rather than sliced, which is what
// a naive prefix would panic on.
func TestShortConfigKeepsAHashThatAlreadyFits(t *testing.T) {
	t.Parallel()

	if got := shortConfig("abc123"); got != "abc123" {
		t.Errorf("shortConfig(%q) = %q, want it printed whole", "abc123", got)
	}
}

// TestResumeRecordsTheConfigurationThatFixedIt: a resume continues a failed
// run under the pipeline it is resumed WITH, which is usually the edit that
// fixed it — so the row must name that one. Leaving the original would have
// the run claim it executed a pipeline nothing in it ever ran, on the page
// whose whole job is answering what a run was told to do.
func TestResumeRecordsTheConfigurationThatFixedIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	state := filepath.Join(dir, "state.db")

	revisionPipeline(t, path, "exit 1")

	out := captureStdout(t, func() {
		err := run([]string{"run", path, "--job", "build", "--state", state})
		if err == nil {
			t.Fatal("the pipeline was supposed to fail")
		}
	})

	runID := resumeID(t, out)

	broken := configColumn(t, captureStdout(t, func() {
		err := run([]string{"runs", path, "--state", state})
		if err != nil {
			t.Fatalf("steps runs: %v", err)
		}
	}))

	// The fix.
	revisionPipeline(t, path, "echo fixed")

	err := run([]string{"run", path, "--resume", runID, "--state", state})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	fixed := configColumn(t, captureStdout(t, func() {
		err := run([]string{"runs", path, "--state", state})
		if err != nil {
			t.Fatalf("steps runs: %v", err)
		}
	}))

	if len(broken) != 1 || len(fixed) != 1 {
		t.Fatalf("expected one run before and after the resume, got %d and %d", len(broken), len(fixed))
	}

	if fixed[0] == broken[0] {
		t.Errorf("the resumed run still reports the configuration that failed (%s)", fixed[0])
	}

	// Named, not merely changed: a resume that recorded NO configuration
	// would also differ from the broken one, and would be worse than either.
	after, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if want := shortConfig(after.Revision.SHA); fixed[0] != want {
		t.Errorf("the resumed run reports %q, want the configuration it resumed with (%q)", fixed[0], want)
	}
}
