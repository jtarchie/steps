package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestRunsPageListsRunsAcrossJobs is the view a single-pipeline deployment
// never had: one history over every job, newest first, each row linking to
// its run and its job.
func TestRunsPageListsRunsAcrossJobs(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	startFinishedRun(t, pipeline, "run-old", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-new", "deploy", "failed")

	code, body := get(t, server, "/p/demo/runs")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/runs = %d", code)
	}

	for _, want := range []string{
		`href="/p/demo/runs/run-old"`,
		`href="/p/demo/runs/run-new"`,
		`href="/p/demo/runs?job=build"`,
		`href="/p/demo/runs?job=deploy"`,
		"passed",
		"failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runs page missing %q", want)
		}
	}

	// Newest first: deploy's failure happened after build's pass.
	if strings.Index(body, "run-new") > strings.Index(body, "run-old") {
		t.Error("runs page is not newest-first")
	}
}

// TestRunsPageScopedToItsPipeline shares one state FILE between two pipelines
// and asserts the page never shows a neighbour's history — the trap named in
// CLAUDE.md: identical content hashes across pipelines make an unscoped query
// look correct until the day it isn't.
func TestRunsPageScopedToItsPipeline(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	other, err := sqlite.OpenStore(pipeline.Store.Description(), "neighbour")
	if err != nil {
		t.Fatalf("OpenStore neighbour: %v", err)
	}

	t.Cleanup(func() { _ = other.Close() })

	err = other.StartRun(ctx, store.NewRun{ID: "run-foreign", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun foreign: %v", err)
	}

	err = pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-mine", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun mine: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/runs = %d", code)
	}

	if !strings.Contains(body, "run-mine") {
		t.Error("runs page missing this pipeline's own run")
	}

	if strings.Contains(body, "run-foreign") {
		t.Error("runs page leaked a run from a pipeline sharing the state file")
	}
}

// TestRunsPageCountLivesInsideTheRefreshRegion: the "N recent runs" count
// must sit inside #runs-region, or the page's own 2.5s refresh fills the
// table while the header keeps saying whatever it said at load.
func TestRunsPageCountLivesInsideTheRefreshRegion(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/runs")

	region := strings.Index(body, `id="runs-region"`)
	count := strings.Index(body, "recent runs")

	if region < 0 || count < 0 {
		t.Fatalf("region at %d, count at %d — one is missing", region, count)
	}

	if count < region {
		t.Error("run count is outside #runs-region, so the refresh never updates it")
	}
}

// TestRunsPageEmptyState says something rather than rendering a bare table.
func TestRunsPageEmptyState(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, body := get(t, server, "/p/demo/runs")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo/runs = %d", code)
	}

	if !strings.Contains(body, "No runs recorded yet") {
		t.Error("runs page has no empty state")
	}
}

// TestRunsPageNarrowsByJobAndWord: ?job= and ?q= combine, the word reaches an input's version (the fault a run handled), and the word a reader SEES for a status finds what is stored under another.
func TestRunsPageNarrowsByJobAndWord(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	startFinishedRun(t, pipeline, "run-build-ok", "build", "succeeded")
	startFinishedRun(t, pipeline, "run-build-bad", "build", "failed")
	startFinishedRun(t, pipeline, "run-deploy-ok", "deploy", "succeeded")

	err := pipeline.Store.RecordRunInput(t.Context(), "run-build-bad", store.RunInput{Input: "fault", Resource: "fault", Version: `{"id":"98765"}`})
	if err != nil {
		t.Fatalf("RecordRunInput: %v", err)
	}

	for _, tc := range []struct {
		url  string
		want []string
		not  []string
	}{
		{"/p/demo/runs?job=build", []string{"run-build-ok", "run-build-bad"}, []string{"run-deploy-ok"}},
		{"/p/demo/runs?q=98765", []string{"run-build-bad"}, []string{"run-build-ok", "run-deploy-ok"}},
		{"/p/demo/runs?q=passed", []string{"run-build-ok", "run-deploy-ok"}, []string{"run-build-bad"}},
		{"/p/demo/runs?job=deploy&q=passed", []string{"run-deploy-ok"}, []string{"run-build-ok", "run-build-bad"}},
	} {
		_, body := get(t, server, tc.url)

		for _, id := range tc.want {
			if !strings.Contains(body, "/runs/"+id+`"`) {
				t.Errorf("%s: missing %s", tc.url, id)
			}
		}

		for _, id := range tc.not {
			if strings.Contains(body, "/runs/"+id+`"`) {
				t.Errorf("%s: shows %s", tc.url, id)
			}
		}
	}

	_, body := get(t, server, "/p/demo/runs?job=build&q=nothing-has-this")
	for _, want := range []string{"No runs match.", `<b>build</b>`, `<b>nothing-has-this</b>`, `href="/p/demo/runs?q=nothing-has-this"`, `href="/p/demo/runs?job=build"`, `aria-current="page">build</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("a narrowed, empty history is missing %q", want)
		}
	}

	// The search box keeps the job it was opened under, so typing narrows WITHIN it.
	if !strings.Contains(body, `<input type="hidden" name="job" value="build">`) {
		t.Error("the search form drops the job filter it was shown under")
	}
}

// TestRunsPagePagesOlderByCursor: past one page there is an "older" link, it carries the narrowing, and the page it leads to starts exactly after the last row shown.
func TestRunsPagePagesOlderByCursor(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	for i := range runsPage + 2 {
		startFinishedRun(t, pipeline, fmt.Sprintf("run-%03d", i), "build", "succeeded")
	}

	_, first := get(t, server, "/p/demo/runs?job=build")

	// Newest first: the page holds 051..002, and the oldest it shows is the cursor.
	if strings.Contains(first, "/runs/run-001\"") || !strings.Contains(first, "/runs/run-002\"") {
		t.Fatal("first page is not the newest runsPage runs")
	}

	older := `href="/p/demo/runs?before=run-002&amp;job=build"`
	if !strings.Contains(first, older) {
		t.Fatalf("first page has no older link %s", older)
	}

	_, second := get(t, server, "/p/demo/runs?before=run-002&job=build")
	if !strings.Contains(second, "/runs/run-001\"") || !strings.Contains(second, "/runs/run-000\"") || strings.Contains(second, "/runs/run-002\"") {
		t.Error("the older page does not start after the cursor")
	}

	if strings.Contains(second, ">older</a>") {
		t.Error("the last page still offers an older one")
	}

	if !strings.Contains(second, `href="/p/demo/runs?job=build">newest</a>`) {
		t.Error("an older page has no way back to the newest, keeping the filter")
	}
}

// TestRunsPageRetriesARun: the job page's retry, on every finished row, and none on a running one or a read-only server.
func TestRunsPageRetriesARun(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)

	startFinishedRun(t, pipeline, "run-done", "build", "failed")

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "run-live", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs")

	if !strings.Contains(body, `action="/p/demo/runs/run-done/rerun"`) {
		t.Error("a finished run has no retry")
	}

	if strings.Contains(body, `action="/p/demo/runs/run-live/rerun"`) {
		t.Error("a running run offers a retry")
	}

	readOnly, ro := testPipeline(t)
	startFinishedRun(t, ro, "run-done", "build", "failed")

	if _, page := get(t, readOnly, "/p/demo/runs"); strings.Contains(page, "/rerun") {
		t.Error("a read-only server offers a retry")
	}
}
