package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestAHeldJobRunsWhenAPersonTriggersIt: the breaker stops a job that keeps failing from being triggered AUTOMATICALLY — the page has always said "will not auto-trigger" — so a person pressing Trigger on a held job, most likely to try a fix, gets a run. It used to be accepted, queued, and then dropped at claim as "skipped" with nothing on screen saying why.
func TestAHeldJobRunsWhenAPersonTriggersIt(t *testing.T) {
	dir := t.TempDir()
	tally := filepath.Join(dir, "ran.txt")
	path := filepath.Join(dir, "held.yml")

	err := os.WriteFile(path, []byte(`
jobs:
  - name: build
    max_consecutive_failures: 1
    plan:
      - task: append
        inputs: []
        run: echo ran >> `+tally+`
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	served := startWebFor(t, path)
	defer served.stop(t)

	name := cli.PipelineName(path)

	st, err := sqlite.OpenExisting(served.state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	held, _, err := st.RecordJobOutcome(t.Context(), "build", false, 1)
	_ = st.Close()

	if err != nil || !held {
		t.Fatalf("RecordJobOutcome = held %v, %v: the fixture did not hold the job", held, err)
	}

	served.trigger(t, name, "build")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && countLines(t, tally) < 1 {
		time.Sleep(20 * time.Millisecond)
	}

	if got := countLines(t, tally); got != 1 {
		t.Fatalf("a manual trigger on a held job ran %d times, want 1", got)
	}
}

// TestRetryOnARunPageRebuildsThatBuild: the browser's Retry is the CLI's --rerun, carried across the queue — the claim has to hand the recorded run and build to RunJob, which a test of either half alone never shows.
func TestRetryOnARunPageRebuildsThatBuild(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	tally := filepath.Join(dir, "ran.txt")
	path := filepath.Join(dir, "retry.yml")

	writePipelineFile(t, versions, `[{"n":"one"}]`)

	err := os.WriteFile(path, []byte(`
resource_types:
- name: counter
  config:
    check: cat `+versions+`
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
  - name: build
    plan:
      - get: ticks
      - task: note
        inputs: [ticks]
        run: cat ticks/n.txt >> `+tally+`
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	served := startWebFor(t, path)
	defer served.stop(t)

	name := cli.PipelineName(path)
	served.trigger(t, name, "build")
	waitForLines(t, tally, 1)

	st, err := sqlite.OpenExisting(served.state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v", err)
	}

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	if status := postEmpty(t, "http://"+served.addr+"/p/"+name+"/runs/"+runs[0].ID+"/rerun"); status != http.StatusOK {
		t.Fatalf("POST rerun answered %d, want the follow page", status)
	}

	waitForLines(t, tally, 2)

	if got := strings.Fields(readFileString(t, tally)); strings.Join(got, " ") != "one one" {
		t.Errorf("builds read %v, want [one one]: the retry took the newer version", got)
	}

	latest, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(latest) != 1 || latest[0].RerunOf != runs[0].ID {
		t.Errorf("newest run = %+v (%v), want a rerun of %s", latest, err, runs[0].ID)
	}
}

func waitForLines(t *testing.T, path string, want int) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && countLines(t, path) < want {
		time.Sleep(20 * time.Millisecond)
	}

	if got := countLines(t, path); got != want {
		t.Fatalf("%s has %d lines, want %d", path, got, want)
	}
}
