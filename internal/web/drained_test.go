package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// drainable is one pipeline with one queued build of "build", for a page test that needs a run the real drain produced.
func drainable(t *testing.T, dir, yaml string) (*scheduler.LocalRunner, *Pipeline, store.Store) {
	t.Helper()

	path := filepath.Join(dir, "demo.yml")
	writeFile(t, path, yaml)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	target := NewPipeline("demo", path, cfg, st, events.New(nil))

	scheduler.PrepareQueue(t.Context(), target.Pipeline)
	scheduler.SyncQueueLimits(t.Context(), target.Pipeline)

	err = st.EnqueueJob(t.Context(), "build", "test", "")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	return scheduler.NewLocalRunner(map[string]workspace.Provider{"demo": provider}, pipeline.Run{}, 1), target, st
}

func countLines(t *testing.T, path string) int {
	t.Helper()

	//nolint:gosec // the path is this test's own t.TempDir()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tally: %v", err)
	}

	return len(strings.Fields(string(body)))
}

// A hook's output is drawn on a named row of its own that finishes with the
// run. It used to land in a nameless row nothing closed — the one thing
// still ticking on a failed run's page — or under the step it guards.
func TestAFailedRunDrawsItsHooksAsFinishedRows(t *testing.T) {
	t.Parallel()

	page := drainedRunPage(t, `
jobs:
  - name: build
    plan:
      - task: build
        inputs: []
        run: exit 1
        on_failure:
          task: tell
          run: echo step hook ran
    on_failure:
      task: explain-failure
      run: echo see steps runs
`)

	hookHead := regexp.MustCompile(`id="(step-\d+-[^"]*)_head"[^>]*>\s*<span class="chev[^"]*"></span>\s*<span class="stmark"[^>]*>[^<]*</span>\s*<span class="kind">hook</span>\s*<span class="name">on_failure · task explain-failure</span>`).FindStringSubmatch(page)
	if hookHead == nil {
		t.Fatalf("no row drawn for the job hook, kind hook, named after what it ran:\n%s", page)
	}

	hook := hookHead[1]

	if tag := openingTag(t, page, hook); !strings.Contains(tag, "passed") || strings.Contains(tag, "running") {
		t.Errorf("job hook row = %s, want a finished, passed row", tag)
	}

	if !strings.Contains(between(t, page, `id="`+hook+`"`, "</body>"), "see steps runs") {
		t.Error("the job hook's output is not on its row")
	}

	build := assertStepHookUnderBuild(t, page, hook)

	if strings.Contains(page, `data-elapsed-since="`) {
		t.Error("a finished run's page still has a running clock")
	}

	if !strings.Contains(page, `failed at <a href="#`+build+`">build</a>`) {
		t.Error(`the header's "failed at" does not name build`)
	}
}

// assertStepHookUnderBuild checks the step hook drew inside build's substeps,
// which end where the job hook's row begins, and not in build's own body.
// It returns build's anchor.
func assertStepHookUnderBuild(t *testing.T, page, jobHook string) string {
	t.Helper()

	build := regexp.MustCompile(`id="(step-\d+-build)_substeps"`).FindStringSubmatch(page)
	if build == nil {
		t.Fatal("build drew no substeps, so its hook is not under it")
	}

	substeps := between(t, page, `id="`+build[1]+`_substeps"`, `id="`+jobHook+`"`)
	if !strings.Contains(substeps, "on_failure · task tell") || !strings.Contains(substeps, "step hook ran") {
		t.Error("the step hook's row and output are not inside build's substeps")
	}

	if strings.Contains(between(t, page, `id="`+build[1]+`"`, `id="`+build[1]+`_substeps"`), "step hook ran") {
		t.Error("the step hook's output is filed in build's own body")
	}

	return build[1]
}

// drainedRunPage runs the pipeline's one queued build and returns its run
// page, with the events persisted the way the daemon persists them.
func drainedRunPage(t *testing.T, yaml string) string {
	t.Helper()

	runner, target, st := drainable(t, t.TempDir(), yaml)

	bus := events.New(pipeline.StoreSink(st))
	target = NewPipeline("demo", target.Path(), target.Config(), st, bus)

	runner.DrainOne(t.Context(), target.Pipeline)
	bus.Close()

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %+v, %v", runs, err)
	}

	server, err := New([]*Pipeline{target}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/"+runs[0].ID)

	return page
}
