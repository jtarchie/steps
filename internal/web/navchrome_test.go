package web

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

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
		{"/p/demo/jobs/build/detail", `href="/p/demo">jobs`},
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

	err := pipeline.Store.RecordRevision(context.Background(), sha, "jobs: []\n", nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, body := get(t, server, "/p/demo/config/"+sha)

	if !strings.Contains(body, `aria-current="page" href="/p/demo/runs">runs`) {
		t.Error("config page does not light the runs tab")
	}
}

// TestErrorPageKeepsNavAlive: an error outside any pipeline (a bad slug, a
// stray 404) still renders the shell — the switcher offers a real pipeline,
// and nothing on it is a /p//… link that 404s into the same error page again.
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

	menu := between(t, body, `id="pipemenu"`, "</div>")
	if !strings.Contains(menu, `href="/p/demo"`) {
		t.Error("error page switcher does not offer a served pipeline")
	}
}

// TestThePipelineIsNamedBeforeItsTabs: every tab is relative to ONE pipeline,
// so the pipeline's name has to come first in reading order — it sat at the
// far right of the bar, dim, after six tabs that never said whose they were,
// and a reader clicking "jobs" from / landed on a pipeline they never chose.
// The bar reads as a path now: steps / <pipeline> / jobs.
func TestThePipelineIsNamedBeforeItsTabs(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "alpha", "beta")

	_, body := get(t, server, "/p/beta/runs")

	switcher := strings.Index(body, `id="pipebtn"`)
	firstTab := strings.Index(body, `class="tab"`)

	if switcher < 0 || firstTab < 0 {
		t.Fatalf("switcher at %d, first tab at %d", switcher, firstTab)
	}

	if switcher > firstTab {
		t.Error("the pipeline switcher comes after the tabs it scopes")
	}

	button := between(t, body, `id="pipebtn"`, "</button>")
	if !strings.Contains(button, ">beta<") {
		t.Errorf("the switcher does not name the current pipeline:\n%s", button)
	}
}

// TestPagesAboveAPipelineDrawNoTabs: the root, /docs and an error page sit
// over several pipelines, and tabs borrowed from the first one were the
// confusion — they looked like the root's own sections. Above a pipeline the
// bar offers the switcher and nothing that pretends to be scoped.
func TestPagesAboveAPipelineDrawNoTabs(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "alpha", "beta")

	for _, page := range []string{"/", "/docs/README.md", "/p/no-such-pipeline"} {
		_, body := get(t, server, page)

		if strings.Contains(body, `class="tab"`) {
			t.Errorf("%s draws section tabs for a pipeline it did not resolve", page)
		}

		button := between(t, body, `id="pipebtn"`, "</button>")
		if !strings.Contains(button, "pipelines") {
			t.Errorf("%s switcher does not read as a picker:\n%s", page, button)
		}

		menu := between(t, body, `id="pipemenu"`, "</div>")
		if strings.Contains(menu, `aria-selected="true"`) {
			t.Errorf("%s switcher marks a pipeline as current on a page above all of them", page)
		}
	}

	_, body := get(t, server, "/p/alpha")
	if !strings.Contains(body, `class="tab"`) {
		t.Error("a pipeline's own page lost its tabs")
	}
}

// TestDocsAndTheVersionLiveInTheFooter: docs are not a section of a pipeline
// and did not belong among its tabs; the footer is where a reference link,
// the source and the running version sit on every page, so a bug report can
// name the build.
func TestDocsAndTheVersionLiveInTheFooter(t *testing.T) {
	t.Parallel()

	server, err := New(nil, nil, WithVersion("v9.9.9-test"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, page := range []string{"/", "/docs/README.md"} {
		_, body := get(t, server, page)

		footer := between(t, body, "<footer", "</footer>")

		for _, want := range []string{`href="/docs"`, `href="https://github.com/jtarchie/steps"`, "© " + strconv.Itoa(time.Now().Year()) + " JT Archie", "v9.9.9-test"} {
			if !strings.Contains(footer, want) {
				t.Errorf("%s footer lacks %q:\n%s", page, want, footer)
			}
		}

		if head, _, _ := strings.Cut(body, "<main>"); strings.Contains(head, `href="/docs"`) {
			t.Errorf("%s still offers docs in the header", page)
		}
	}

	_, docs := get(t, server, "/docs/README.md")
	if !strings.Contains(between(t, docs, "<footer", "</footer>"), `aria-current="page" href="/docs"`) {
		t.Error("the docs page does not light its own footer link")
	}
}

// TestTheJumpPaletteHasAButton: the palette opened on "/" and nothing else,
// which on a phone is no way in at all. The button is the way in; the key
// stays as the hint beside it.
func TestTheJumpPaletteHasAButton(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, body := get(t, server, "/p/demo")

	if !strings.Contains(body, `<button class="jumpbtn" id="jumpbtn" type="button"`) {
		t.Error("no button opens the jump palette")
	}

	if !strings.Contains(body, `<kbd>/</kbd>`) {
		t.Error("the / hint is gone")
	}
}

// TestEveryPageSaysThePipelineIsPaused: a board of green jobs that stopped moving looks exactly like a quiet day, so the pause has to be on the page, not only in the CLI.
func TestEveryPageSaysThePipelineIsPaused(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	const banner = `class="actionbar paused"`

	if _, body := get(t, server, "/p/demo"); strings.Contains(body, banner) {
		t.Fatal("a running pipeline claims to be paused")
	}

	err := pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs")
	if !strings.Contains(body, banner) {
		t.Error("a paused pipeline's pages do not say so")
	}
}

// TestNavCountsPendingApprovals: a gate waiting on a person is invisible unless the tab they would open says something is there.
func TestNavCountsPendingApprovals(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	_, err := pipeline.Store.RequestApproval(t.Context(), "deploy", "ship it?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	if _, body := get(t, server, "/p/demo"); !strings.Contains(body, `approvals<span class="badge" title="1 approval is waiting for a decision">`) {
		t.Error("the approvals tab does not count the pending gate")
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

	if !strings.Contains(runBody, `href="/p/demo/jobs/build/detail"`) {
		t.Error("run page breadcrumbs do not link the job's detail page")
	}

	_, jobBody := get(t, server, "/p/demo/jobs/build/detail")
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

	_, jobBody := get(t, server, "/p/demo/jobs/build/detail")
	if !strings.Contains(jobBody, "steps job build") {
		t.Error("job page H1 is not 'steps job <name>'")
	}

	_, runBody := get(t, server, "/p/demo/runs/run-1")
	if !strings.Contains(runBody, "steps run build #run-1") {
		t.Error("run page H1 does not carry the run id")
	}

	_, followBody := get(t, server, "/p/demo/jobs/build/follow")
	if !strings.Contains(followBody, `<b>steps run build</b> <span class="st st-queued">queued</span></h1>`) {
		t.Error("follow page H1 is not 'steps run <name>' marked queued")
	}
}
