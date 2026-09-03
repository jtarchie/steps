package cli

// The small fixtures this package's tests share. They are deliberately
// duplicated from ./e2e rather than exported from it: a helper that captures
// os.Stdout or writes a pipeline file is three lines of policy, and a shared
// one would have to be a non-test package compiled into every build.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
//
// It assigns the os.Stdout GLOBAL, which every fmt.Printf in the code under
// test reads — so a test using it must NOT be parallel. TestNoParallelTestRedirectsStdout
// in ./e2e enforces that across the module, this package included.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()

	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// writePipelineFile writes a pipeline fixture to path.
func writePipelineFile(t *testing.T, path, pipeline string) {
	t.Helper()

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// flagFixture writes the smallest pipeline a command can be pointed at: one
// job, one task, nothing to fetch. What the tests using it assert happens
// before any of it executes.
func flagFixture(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)

	return path
}

// webServerFor opens a read-only server over an already-run pipeline, the way
// `steps web --read-only` would.
func webServerFor(t *testing.T, pipelinePath string) (*web.Server, *web.Pipeline) {
	t.Helper()

	cfg, err := config.LoadConfig(pipelinePath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := store.OpenStore(StatePath(pipelinePath, ""), PipelineName(pipelinePath))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	pipeline := web.NewPipeline(web.Slugify(pipelinePath), pipelinePath, cfg, st, events.New(nil))

	server, err := web.New([]*web.Pipeline{pipeline}, nil)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	return server, pipeline
}

// webPipelineWithVars is webServerFor's pipeline half, loaded under the vars
// the daemon was started with — which is what makes a --vars-file part of
// the configuration a reload compares against.
func webPipelineWithVars(t *testing.T, pipelinePath string, vars VarFlags) *web.Pipeline {
	t.Helper()

	slug := web.Slugify(pipelinePath)

	cfg, err := vars.Load(pipelinePath, slug)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	st, err := store.OpenStore(StatePath(pipelinePath, ""), slug)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	err = RecordRevision(t.Context(), st, cfg)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	return web.NewPipeline(slug, pipelinePath, cfg, st, events.New(nil))
}

// webGet performs a GET against the server and returns status and body.
func webGet(t *testing.T, server *web.Server, target string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}
