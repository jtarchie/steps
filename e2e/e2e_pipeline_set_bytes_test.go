package e2e

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/web"
)

// TestPipelineSetCarriesIncludeBytesExactly: an include is whatever bytes the file holds, and encoding/json turns each invalid UTF-8 byte of a string into U+FFFD — so a Latin-1 run_file: ran on the daemon as bytes nobody sent, under a sha no local load computes, and every later set saw a changed include and uploaded it again.
func TestPipelineSetCarriesIncludeBytesExactly(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	log := filepath.Join(dir, "ran.log")

	err := os.MkdirAll(filepath.Join(dir, "ci"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, filepath.Join(dir, "ci", "build.sh"), "printf 'caf\xe9' > "+log+"\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: ci/build.sh
`)

	name := cli.PipelineName(path)

	local, err := config.Load(path, name, nil)
	if err != nil {
		t.Fatal(err)
	}

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	code, body := served.get(t, "/api/pipelines/"+name)
	if code != http.StatusOK {
		t.Fatalf("GET /api/pipelines/%s = %d: %s", name, code, body)
	}

	var current web.PipelineConfig

	err = json.Unmarshal([]byte(body), &current)
	if err != nil {
		t.Fatal(err)
	}

	if current.SHA != local.Revision.SHA {
		t.Errorf("the daemon serves config %s, want %s: what a local load of the same files computes", current.SHA, local.Revision.SHA)
	}

	served.trigger(t, name, "build")
	waitForQueueSuccess(t, served.state, name, 1)

	if got := readFileString(t, log); got != "caf\xe9" {
		t.Errorf("the job printed %q, want %q: the bytes the include holds", got, "caf\xe9")
	}

	out := captureStdout(t, func() { served.set(t, name, path) })
	if !strings.Contains(out, "unchanged") {
		t.Errorf("setting the same files again was not reported unchanged:\n%s", out)
	}
}
