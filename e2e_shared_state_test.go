package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// End-to-end proof for --state: several pipelines recording into ONE sqlite
// file while staying strangers to each other.
//
// The fixture is deliberately hostile. Both pipelines declare a job named
// `build` running a byte-identical task, so their merkle chains hash to the
// same root — which is exactly the collision a shared database makes
// reachable, and the reason nodes and job_runs carry a pipeline rather than
// relying on the hash to be unique. Only the log path each task appends to
// differs, and that lives outside the hashed content.

// sharedStatePipeline writes a pipeline whose single job is named build and
// whose only step appends a line to log. Two of these differ in no hashed
// byte.
func sharedStatePipeline(t *testing.T, path, log string) string {
	t.Helper()

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo built >> %s
`, log))

	return path
}

// TestSharedStateKeepsPipelinesApart is the headline: two pipeline files, one
// --state database, and neither one's cache answers for the other.
func TestSharedStateKeepsPipelinesApart(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "shared.db")

	first := sharedStatePipeline(t, filepath.Join(dir, "first.yml"), filepath.Join(dir, "first.log"))
	second := sharedStatePipeline(t, filepath.Join(dir, "second.yml"), filepath.Join(dir, "second.log"))

	for _, pipeline := range []string{first, second} {
		err := run([]string{"run", pipeline, "--job", "build", "--state", state})
		if err != nil {
			t.Fatalf("run %s: %v", filepath.Base(pipeline), err)
		}
	}

	// The second pipeline's job must actually have RUN. If the merkle cache
	// were shared, first.yml's identical chain would have marked it done and
	// this file would never be written — the whole failure this scoping
	// exists to prevent, and one that is invisible from the exit code.
	for _, log := range []string{"first.log", "second.log"} {
		got := readFileString(t, filepath.Join(dir, log))
		if !strings.Contains(got, "built") {
			t.Errorf("%s: job did not run; log says %q", log, got)
		}
	}

	// Each pipeline sees its own single run and nothing of its neighbor's,
	// even though both jobs are named build.
	for _, pipeline := range []string{first, second} {
		assertRunCount(t, state, pipelineName(pipeline), 1)
	}
}

// TestSharedStateWritesOneFile: --state means one database, and no .steps/
// beside either YAML. The layout is half of what the flag promises.
func TestSharedStateWritesOneFile(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "shared.db")

	first := sharedStatePipeline(t, filepath.Join(dir, "first.yml"), filepath.Join(dir, "first.log"))
	second := sharedStatePipeline(t, filepath.Join(dir, "second.yml"), filepath.Join(dir, "second.log"))

	for _, pipeline := range []string{first, second} {
		err := run([]string{"run", pipeline, "--job", "build", "--state", state})
		if err != nil {
			t.Fatalf("run %s: %v", filepath.Base(pipeline), err)
		}
	}

	_, err := os.Stat(state)
	if err != nil {
		t.Fatalf("shared state db was not created: %v", err)
	}

	_, err = os.Stat(filepath.Join(dir, ".steps"))
	if !os.IsNotExist(err) {
		t.Error("--state was given, but a default .steps/ directory was created anyway")
	}
}

// assertRunCount opens the shared database as one pipeline and checks how many
// runs of a job it can see — the read that proves scoping from the outside.
func assertRunCount(t *testing.T, state, name string, want int) {
	t.Helper()

	st, err := store.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open shared store as %s: %v", name, err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}

	if len(runs) != want {
		t.Errorf("%s: got %d runs of build, want %d", name, len(runs), want)
	}
}

// TestSharedStateRerunStillSkips is the other half of the same claim: scoping
// the cache must not DEFEAT it. Running one pipeline twice against a shared
// database skips the second time, exactly as it does with its own file.
func TestSharedStateRerunStillSkips(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "shared.db")
	log := filepath.Join(dir, "build.log")

	pipeline := sharedStatePipeline(t, filepath.Join(dir, "only.yml"), log)

	for range 2 {
		err := run([]string{"run", pipeline, "--job", "build", "--state", state})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if got := strings.Count(readFileString(t, log), "built"); got != 1 {
		t.Errorf("the task ran %d times across two invocations, want 1 (the second is a cache hit)", got)
	}
}

// TestSharedStateNameFlagOverridesTheFilename covers the collision --name
// exists for: two repos each with a pipeline.yml, one shared database.
// Without the flag they are the same name; with it they are two pipelines.
func TestSharedStateNameFlagOverridesTheFilename(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "shared.db")

	pipelines := map[string]string{"alpha": "a", "beta": "b"}
	for name, sub := range pipelines {
		repo := filepath.Join(dir, sub)
		err := os.MkdirAll(repo, 0o750)
		if err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		path := sharedStatePipeline(t,
			filepath.Join(repo, "pipeline.yml"),
			filepath.Join(dir, sub+".log"))

		err = run([]string{"run", path, "--job", "build", "--state", state, "--name", name + "=" + path})
		if err != nil {
			t.Fatalf("run %s: %v", name, err)
		}
	}

	// Both ran: the names kept the identical chains apart even though both
	// files are called pipeline.yml.
	for _, sub := range pipelines {
		if got := readFileString(t, filepath.Join(dir, sub+".log")); !strings.Contains(got, "built") {
			t.Errorf("%s.log: job did not run; log says %q", sub, got)
		}
	}

	for name := range pipelines {
		assertRunCount(t, state, name, 1)
	}
}

// TestSharedStateRefusesAForeignRunID pins the one place a shared database
// makes a run id dangerous. Ids are globally unique and carry no pipeline, so
// without scoping FindRun, `steps run a.yml --resume <id-from-b>` would
// cheerfully continue b's run — reusing b's workspace and step indexes against
// a's plan.
func TestSharedStateRefusesAForeignRunID(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "shared.db")

	first := sharedStatePipeline(t, filepath.Join(dir, "first.yml"), filepath.Join(dir, "first.log"))
	second := sharedStatePipeline(t, filepath.Join(dir, "second.yml"), filepath.Join(dir, "second.log"))

	err := run([]string{"run", first, "--job", "build", "--state", state})
	if err != nil {
		t.Fatalf("run first: %v", err)
	}

	st, err := store.OpenStore(state, pipelineName(first))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("list runs: %d rows, %v", len(runs), err)
	}

	foreign := runs[0].ID

	err = st.Close()
	if err != nil {
		t.Fatalf("close: %v", err)
	}

	err = run([]string{"run", second, "--job", "build", "--state", state, "--resume", foreign})
	if err == nil {
		t.Fatal("second.yml resumed a run belonging to first.yml")
	}

	if !strings.Contains(err.Error(), foreign) {
		t.Errorf("error %q does not name the run id that was refused", err)
	}
}
