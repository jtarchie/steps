package e2e

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestResumeOnARunPageContinuesFromTheStepThatFailed: the page names where a failed run would pick up, and pressing Resume carries that run across the queue into the daemon's runner, which skips what the run already finished — the step before the failure runs once across both attempts, and the run that succeeds is the same run, not a new one.
func TestResumeOnARunPageContinuesFromTheStepThatFailed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	tally := filepath.Join(dir, "ran.txt")
	gate := filepath.Join(dir, "gate")
	path := filepath.Join(dir, "resume.yml")

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
      - task: expensive
        run: echo expensive >> `+tally+`
      - task: flaky
        run: test -f `+gate+` && echo flaky >> `+tally+`
      - task: after
        run: echo after >> `+tally+`
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

	runID := runs[0].ID
	waitForRunStatus(t, served.state, name, runID, "failed")

	resume := "/p/" + name + "/runs/" + runID + "/resume"

	requireResumeFrom(t, pageGet(t, served.addr, "/p/"+name+"/runs/"+runID), resume, "flaky")

	writePipelineFile(t, gate, "")

	if status := postEmpty(t, "http://"+served.addr+resume); status != http.StatusOK {
		t.Fatalf("POST resume answered %d, want the run page", status)
	}

	// The run reads failed until the claim puts it back in flight, which a status wait alone would take as the answer.
	waitForLines(t, tally, 3)
	waitForRunStatus(t, served.state, name, runID, "succeeded")

	if got := strings.Fields(readFileString(t, tally)); strings.Join(got, " ") != "expensive flaky after" {
		t.Errorf("steps ran %v, want [expensive flaky after]: a resume repeats nothing the run finished", got)
	}

	latest, err := st.ListRuns(t.Context(), "build", 5)
	if err != nil || len(latest) != 1 {
		t.Errorf("runs = %+v (%v), want the one run, continued", latest, err)
	}

	if page := pageGet(t, served.addr, "/p/"+name+"/runs/"+runID); strings.Contains(page, `action="`+resume+`"`) {
		t.Errorf("a succeeded run still offers Resume")
	}
}

// TestResumeIsNotOfferedOnceTheTreeIsGone: a resume continues in the run's own tree, so with the tree gone the page offers only Retry, and a POST anyway is refused with Retry named rather than quietly building from the top.
func TestResumeIsNotOfferedOnceTheTreeIsGone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	path := filepath.Join(dir, "gone.yml")

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
      - task: fine
        run: "true"
      - task: broken
        run: "false"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	served := startWebFor(t, path)
	defer served.stop(t)

	name := cli.PipelineName(path)
	served.trigger(t, name, "build")

	run := newestRun(t, served.state, name, "build")
	waitForRunStatus(t, served.state, name, run.ID, "failed")

	resume := "/p/" + name + "/runs/" + run.ID + "/resume"

	if page := pageGet(t, served.addr, "/p/"+name+"/runs/"+run.ID); !strings.Contains(page, `action="`+resume+`"`) {
		t.Fatalf("the failed run offers no Resume while its tree is there, so its absence below proves nothing")
	}

	// The first read can predate the get re-pointing the row from the job's tree to the build's.
	run = newestRun(t, served.state, name, "build")
	if !strings.HasPrefix(run.Workspace, dir) {
		t.Fatalf("the run's tree %q is outside this test's directory; refusing to remove it", run.Workspace)
	}

	err = os.RemoveAll(run.Workspace)
	if err != nil {
		t.Fatal(err)
	}

	if page := pageGet(t, served.addr, "/p/"+name+"/runs/"+run.ID); strings.Contains(page, `action="`+resume+`"`) {
		t.Errorf("the page offers Resume for a run whose tree is gone")
	}

	if status := postEmpty(t, "http://"+served.addr+resume); status != http.StatusConflict {
		t.Errorf("POST resume answered %d, want %d", status, http.StatusConflict)
	}

	err = stepsRun(t, []string{"runs", "resume", run.ID, "-p", name, "--target", served.target()})
	if err == nil || !strings.Contains(err.Error(), "Retry runs it from the top") {
		t.Errorf("steps runs resume = %v, want the daemon's refusal naming Retry", err)
	}
}

// requireResumeFrom fails unless page draws a Resume posting to resume that names from as where the run picks up.
func requireResumeFrom(t *testing.T, page, resume, from string) {
	t.Helper()

	if !strings.Contains(page, `action="`+resume+`"`) {
		t.Fatalf("the failed run's page draws no Resume posting to %s:\n%s", resume, page)
	}

	if !strings.Contains(page, `data-resume-from="`+from+`"`) {
		t.Fatalf("the Resume button does not name %s as where the run picks up:\n%s", from, page)
	}
}
