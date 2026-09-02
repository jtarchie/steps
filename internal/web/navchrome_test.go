package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// startFinishedRun records one completed run so detail pages have something
// to render.
func startFinishedRun(t *testing.T, pipeline *Pipeline, id, job, status string) {
	t.Helper()

	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, id, job, "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun %s: %v", id, err)
	}

	err = pipeline.Store.FinishRun(ctx, id, status)
	if err != nil {
		t.Fatalf("FinishRun %s: %v", id, err)
	}
}

// TestBrandLinksHome: the wordmark is the way back up. Before this it was
// plain text, and the multi-pipeline overview was reachable only by editing
// the URL.
func TestBrandLinksHome(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, page := range []string{"/p/demo", "/p/demo/runs", "/p/demo/resources"} {
		_, body := get(t, server, page)
		if !strings.Contains(body, `class="brand" href="/"`) {
			t.Errorf("%s: brand does not link home", page)
		}
	}
}

// TestActiveTabFollowsSection: entering a detail page must not unlight the
// whole nav. A job page lives under jobs; a run transcript lives under runs.
func TestActiveTabFollowsSection(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")

	cases := []struct{ page, currentTab string }{
		{"/p/demo", `href="/p/demo">jobs`},
		{"/p/demo/jobs/build", `href="/p/demo">jobs`},
		{"/p/demo/jobs/build/follow", `href="/p/demo">jobs`},
		{"/p/demo/runs", `href="/p/demo/runs">runs`},
		{"/p/demo/runs/run-1", `href="/p/demo/runs">runs`},
		{"/p/demo/resources", `href="/p/demo/resources">resources`},
	}

	for _, tc := range cases {
		_, body := get(t, server, tc.page)

		want := `aria-current="page" ` + tc.currentTab
		if !strings.Contains(body, want) {
			t.Errorf("%s: expected current tab %q", tc.page, want)
		}
	}
}

// TestNodePageLightsTheRunsTab: the node page is a cache receipt reached from
// run transcripts, so it lives under runs — the branch TestActiveTabFollowsSection
// cannot reach without a recorded node.
func TestNodePageLightsTheRunsTab(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	record := store.NodeRecord{
		Hash:     "cccc111122223333",
		Kind:     "task",
		Resource: "compile",
		Content:  map[string]any{"run": "true"},
	}

	err := pipeline.Store.RecordNode(context.Background(), record, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	_, body := get(t, server, "/p/demo/nodes/"+record.Hash)

	if !strings.Contains(body, `aria-current="page" href="/p/demo/runs">runs`) {
		t.Error("node page does not light the runs tab")
	}
}

// TestConfigPageLightsTheRunsTab: the recorded-configuration page is reached
// from a run transcript, so it belongs under runs too. Its own branch, for the
// reason the node page has one — sectionOf's default returns the page name,
// so a detail page nobody added leaves the WHOLE bar unlit rather than
// lighting the wrong tab, which is a state neither of these tests would catch
// for the other.
func TestConfigPageLightsTheRunsTab(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	const sha = "dddd111122223333"

	err := pipeline.Store.RecordRevision(context.Background(), sha, "jobs: []\n")
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, body := get(t, server, "/p/demo/config/"+sha)

	if !strings.Contains(body, `aria-current="page" href="/p/demo/runs">runs`) {
		t.Error("config page does not light the runs tab")
	}
}

// TestErrorPageKeepsNavAlive: an error outside any pipeline (a bad slug, a
// stray 404) still renders the layout's tab bar — anchored to a real
// pipeline, not to /p//… links that 404 into the same error page again.
func TestErrorPageKeepsNavAlive(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, body := get(t, server, "/p/no-such-pipeline")
	if code != http.StatusNotFound {
		t.Fatalf("GET /p/no-such-pipeline = %d, want 404", code)
	}

	if strings.Contains(body, `href="/p//`) {
		t.Error("error page renders dead /p//… links")
	}

	if !strings.Contains(body, `href="/p/demo/runs"`) {
		t.Error("error page tabs are not anchored to a served pipeline")
	}
}

// TestBreadcrumbsOnDetailPages: a transcript names its job as a LINK, not as
// text a reader retypes into the palette.
func TestBreadcrumbsOnDetailPages(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")

	_, runBody := get(t, server, "/p/demo/runs/run-1")
	if !strings.Contains(runBody, `class="crumbs"`) {
		t.Error("run page has no breadcrumbs")
	}

	if !strings.Contains(runBody, `href="/p/demo/jobs/build"`) {
		t.Error("run page breadcrumbs do not link the job")
	}

	_, jobBody := get(t, server, "/p/demo/jobs/build")
	if !strings.Contains(jobBody, `class="crumbs"`) {
		t.Error("job page has no breadcrumbs")
	}
}

// TestDistinctHeadings: three pages used to share the identical H1
// "steps run <job>", leaving the job page, one run of it, and the waiting
// room indistinguishable.
func TestDistinctHeadings(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-1", "build", "succeeded")

	_, jobBody := get(t, server, "/p/demo/jobs/build")
	if !strings.Contains(jobBody, "steps job build") {
		t.Error("job page H1 is not 'steps job <name>'")
	}

	_, runBody := get(t, server, "/p/demo/runs/run-1")
	if !strings.Contains(runBody, "steps run build #run-1") {
		t.Error("run page H1 does not carry the run id")
	}

	_, followBody := get(t, server, "/p/demo/jobs/build/follow")
	if !strings.Contains(followBody, "steps trigger build") {
		t.Error("follow page H1 is not 'steps trigger <name>'")
	}
}
