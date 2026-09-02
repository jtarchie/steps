package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
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
		`href="/p/demo/jobs/build"`,
		`href="/p/demo/jobs/deploy"`,
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

	other, err := store.OpenStore(pipeline.Store.Path(), "neighbour")
	if err != nil {
		t.Fatalf("OpenStore neighbour: %v", err)
	}

	t.Cleanup(func() { _ = other.Close() })

	err = other.StartRun(ctx, "run-foreign", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun foreign: %v", err)
	}

	err = pipeline.Store.StartRun(ctx, "run-mine", "build", "/tmp/ws", "")
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

// TestRunsPageRefreshGuardsOverlappingPolls: the poll loop must not stack
// requests when a response is slow — an older response resolving after a
// newer one rewrote the table with stale rows (the same race the palette
// guards with a generation token), and identical markup must not cost the
// reader their selection or an in-progress click.
func TestRunsPageRefreshGuardsOverlappingPolls(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo/runs")

	if !strings.Contains(body, "inflight") {
		t.Error("refresh script has no in-flight guard")
	}

	if !strings.Contains(body, "current.innerHTML !== next.innerHTML") {
		t.Error("refresh script swaps the region even when the markup is unchanged")
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
