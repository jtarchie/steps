package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/web"
)

// TestColdTabReachesAFailingTranscriptInTwoClicks: the whole reason for the
// overview's job chips. A daemon holding several pipelines used to answer a
// cold tab with a table that said which pipeline last ran and nothing about
// which JOB is red — so finding a failure was switcher, board, job, run.
// Now each row carries its jobs colored by their latest run, a red one is a
// link, and the link lands on the transcript with the broken step named.
func TestColdTabReachesAFailingTranscriptInTwoClicks(t *testing.T) {
	dir := t.TempDir()
	green := filepath.Join(dir, "green.yml")
	red := filepath.Join(dir, "red.yml")

	writePipelineFile(t, green, `
jobs:
- name: ok
  plan:
  - task: work
    run: "true"
`)
	writePipelineFile(t, red, `
jobs:
- name: fine
  plan:
  - task: work
    run: "true"
- name: broken
  plan:
  - task: work
    run: exit 1
`)

	mustRun(t, "run", green, "--job", "ok")
	mustRun(t, "run", red, "--job", "fine")

	err := cli.Run([]string{"run", red, "--job", "broken"})
	if err == nil {
		t.Fatal("the broken job ran green")
	}

	server := webServerForAll(t, green, red)

	code, root := webGet(t, server, "/")
	if code != 200 {
		t.Fatalf("GET / = %d, want the overview", code)
	}

	for _, want := range []struct{ chip, cost string }{
		{`<a class="st st-passed" href="/p/green/jobs/ok">ok</a>`, "a green job is not a chip"},
		{`<a class="st st-passed" href="/p/red/jobs/fine">fine</a>`, "a green job beside a red one is not a chip"},
		{`<a class="st st-failed" href="/p/red/jobs/broken">broken</a>`, "the red job is not a red chip"},
	} {
		if !strings.Contains(root, want.chip) {
			t.Errorf("%s: missing %s", want.cost, want.chip)
		}
	}

	if fine, broken := strings.Index(root, `jobs/fine">`), strings.Index(root, `jobs/broken">`); fine > broken {
		t.Error("chips are not in the pipeline's own order")
	}

	// Click one: the chip is the job link, which is the latest run.
	runs, err := openStoreFor(t, red).ListRuns(t.Context(), "broken", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d rows)", err, len(runs))
	}

	run := "/p/red/runs/" + runs[0].ID
	expectRedirect(t, server, "/p/red/jobs/broken", run)

	transcript := expectPage(t, server, run, "steps run broken")
	if !strings.Contains(transcript, `failed at <a href="#step-`) {
		t.Error("the transcript a red chip lands on does not name the step that broke")
	}
}

// webServerForAll is webServerFor over several pipeline files, which is
// what makes the root an overview rather than a redirect.
func webServerForAll(t *testing.T, paths ...string) *web.Server {
	t.Helper()

	pipelines := make([]*web.Pipeline, 0, len(paths))

	for _, path := range paths {
		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", path, err)
		}

		pipelines = append(pipelines, web.NewPipeline(cli.PipelineName(path), path, cfg, openStoreFor(t, path), events.New(nil)))
	}

	server, err := web.New(pipelines, nil)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	return server
}

// TestRootRowsSayOnlyWhatIsNotNormal: the root is the glance, so a healthy
// pipeline's row is its mark, its name, its jobs and its last run — no
// column of dashes and "active" — and the one thing that is not normal, a
// pause, is said once, by the mark, on that row alone. A daemon's pipeline is
// the name it was set under, so its file is not drawn, and the mark is the
// switcher's own, so the two cannot disagree.
func TestRootRowsSayOnlyWhatIsNotNormal(t *testing.T) {
	dir := t.TempDir()
	running := filepath.Join(dir, "running.yml")
	stopped := filepath.Join(dir, "stopped.yml")

	for _, path := range []string{running, stopped} {
		writePipelineFile(t, path, `
jobs:
- name: ok
  plan:
  - task: work
    run: "true"
`)
		mustRun(t, "run", path, "--job", "ok")
	}

	err := openStoreFor(t, stopped).Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, root := webGet(t, webServerForAll(t, running, stopped), "/")

	table := between(t, root, `id="pipelines-table"`, "</table>")
	for _, gone := range []string{">active<", ">Waiting</th>", ">Queued</th>", ">State</th>", ">File</th>", "running.yml", "stopped.yml"} {
		if strings.Contains(table, gone) {
			t.Errorf("the root still draws %s", gone)
		}
	}

	rows := map[string]string{"running": rootRow(t, table, "running"), "stopped": rootRow(t, table, "stopped")}

	if strings.Contains(rows["running"], "st-paused") || !strings.Contains(rows["stopped"], `<span class="st-paused">⏸</span>`) || strings.Contains(rows["stopped"], `class="st st-paused"`) {
		t.Errorf("the pause is not said on the paused row alone:\nrunning: %s\nstopped: %s", rows["running"], rows["stopped"])
	}

	for slug, row := range rows {
		switcher := between(t, root, `href="/p/`+slug+`">`, `<span class="slug">`)
		if !strings.Contains(row, switcher) {
			t.Errorf("%s's row does not wear the switcher's mark %q:\n%s", slug, switcher, row)
		}
	}
}

// rootRow is the root table's row for one pipeline.
func rootRow(t *testing.T, table, slug string) string {
	t.Helper()

	for _, row := range strings.Split(table, "<tr") {
		if strings.Contains(row, `href="/p/`+slug+`"`) {
			return row
		}
	}

	t.Fatalf("no row for %s in:\n%s", slug, table)

	return ""
}

// between is the text after start up to the first end that follows it.
func between(t *testing.T, text, start, end string) string {
	t.Helper()

	_, after, found := strings.Cut(text, start)
	if !found {
		t.Fatalf("no %q in:\n%s", start, text)
	}

	inside, _, _ := strings.Cut(after, end)

	return inside
}
