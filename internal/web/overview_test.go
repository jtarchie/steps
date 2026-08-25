package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// testPipelines builds a server over several pipelines sharing one state
// file, which is what `steps web app.yml infra.yml --state shared.db`
// produces. Each gets one job named after itself, so a page can be checked
// for having reached the right one.
func testPipelines(t *testing.T, names ...string) (*Server, []*Pipeline) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, ".steps", "shared.db")
	pipelines := make([]*Pipeline, 0, len(names))

	for _, name := range names {
		path := filepath.Join(dir, name+".yml")
		writeFile(t, path, `
jobs:
  - name: `+name+`-job
    plan:
      - task: work
        run: "true"
`)

		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", name, err)
		}

		st, err := store.OpenStore(statePath, name)
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", name, err)
		}

		t.Cleanup(func() { _ = st.Close() })

		pipelines = append(pipelines, &Pipeline{
			Slug: name, Path: path, Cfg: cfg, Store: st, Bus: events.New(nil),
		})
	}

	server, err := New(pipelines, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipelines
}

// TestRootRedirectsForASinglePipeline is the common case, and the reason the
// root adapts rather than always becoming a feed: with one pipeline served,
// an overview is a list of one and a click in front of everything useful.
func TestRootRedirectsForASinglePipeline(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	code, _ := get(t, server, "/")
	if code != http.StatusFound {
		t.Fatalf("GET / = %d, want a redirect straight to the only pipeline", code)
	}
}

// TestRootListsWhatTheStateFileHolds: with several pipelines served, the root
// stops guessing. It used to redirect to whichever slug sorted first, which
// is an arbitrary answer to a question the operator did not ask.
func TestRootListsEveryServedPipeline(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	code, body := get(t, server, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d, want an overview rather than a redirect", code)
	}

	for _, slug := range []string{"app", "infra"} {
		if !strings.Contains(body, `/p/`+slug) {
			t.Errorf("the overview does not link to /p/%s", slug)
		}
	}
}

// TestRootFeedSpansPipelines is the view #85 exists for: one feed, newest
// first, every row saying which pipeline it belongs to. A row that cannot
// name its pipeline is a row nobody can follow.
func TestRootFeedSpansPipelines(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")
	ctx := t.Context()

	for _, seed := range []struct {
		pipeline *Pipeline
		id       string
		job      string
	}{
		{pipelines[0], "app-1", "app-job"},
		{pipelines[1], "infra-1", "infra-job"},
		{pipelines[0], "app-2", "app-job"},
	} {
		err := seed.pipeline.Store.StartRun(ctx, seed.id, seed.job, t.TempDir())
		if err != nil {
			t.Fatalf("StartRun(%s): %v", seed.id, err)
		}
	}

	_, body := get(t, server, "/")

	for _, id := range []string{"app-1", "infra-1", "app-2"} {
		if !strings.Contains(body, id) {
			t.Errorf("the feed is missing run %s", id)
		}
	}

	// Newest first, and interleaved — ordering by pipeline would put both of
	// app's runs together and still contain all three ids.
	first := strings.Index(body, "app-2")
	second := strings.Index(body, "infra-1")
	third := strings.Index(body, "app-1")

	if first >= second || second >= third {
		t.Errorf("feed order is app-2@%d, infra-1@%d, app-1@%d; want newest first", first, second, third)
	}

	// Each run has to be reachable, which means the row carries its pipeline.
	for _, want := range []string{"/p/app/runs/app-2", "/p/infra/runs/infra-1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the feed does not link %s", want)
		}
	}
}

// TestRootFeedIgnoresUnservedPipelines: a state file may hold a pipeline this
// process was not given. Showing its runs would put rows on the page with no
// route behind them, and — because the feed is bounded — would crowd out runs
// of pipelines the operator IS looking at.
func TestRootFeedIgnoresUnservedPipelines(t *testing.T) {
	t.Parallel()

	server, pipelines := testPipelines(t, "app", "infra")

	// A third pipeline writing into the same file, served by nobody here.
	other, err := store.OpenStore(filepath.Join(filepath.Dir(pipelines[0].Path), ".steps", "shared.db"), "unserved")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = other.Close() })

	err = other.StartRun(t.Context(), "unserved-1", "secret", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	_, body := get(t, server, "/")

	if strings.Contains(body, "unserved-1") {
		t.Error("the feed shows a run from a pipeline this process does not serve")
	}
}
