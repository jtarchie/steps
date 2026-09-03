package e2e

// Hot-reloading the pipeline file, through the daemon.
//
// The watcher's own behaviour — what one check does to a served pipeline and
// to the store — is pinned white-box beside it in internal/cli. What only a
// real `steps web` can show is that the command starts the watcher at all,
// and that a poller picks up a trigger an edit introduced. Both of these run
// the actual process, edit the file underneath it, and read the page back.

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/web"
)

// reloadPipeline writes a pipeline whose job names are the caller's, so an
// edit is visible on the pages the daemon serves.
func reloadPipeline(t *testing.T, path string, jobs ...string) {
	t.Helper()

	body := &strings.Builder{}
	body.WriteString("jobs:\n")

	for _, job := range jobs {
		body.WriteString("- name: " + job + "\n  plan:\n  - task: compile\n    inputs: []\n    run: echo " + job + "\n")
	}

	writePipelineFile(t, path, body.String())
}

// TestReloadStartsPollingATriggerAnEditAdded is the seam between the reload
// and the poll loop, and it replaces a stated limitation rather than
// documenting one.
//
// Whether a pipeline had anything to poll used to be decided once, at
// startup, so a pipeline that gained its FIRST `trigger: true` get was served
// with the new configuration and checked by nothing until a restart. The
// decision is now the loop's, taken per configuration.
//
// Driven through the real daemon: this is exactly the kind of claim that
// passes when asserted against a watcher and a poller separately.
func TestReloadStartsPollingATriggerAnEditAdded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	versions := filepath.Join(dir, "versions.json")
	log := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"1"}]`)

	// Nothing to poll: one job, run by hand.
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: echo built
`)

	served := startWeb(t, []string{path}, "--interval", "100ms")
	defer served.stop(t)

	// The edit that gives it something to check.
	writePipelineFile(t, path, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat `+versions+`
    in: "true"
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - task: compile
    inputs: [items]
    run: echo built >> `+log+`
`)

	// The run the poller enqueued and the drain executed — end to end, from a
	// file that was saved while the daemon was already running.
	waitForFile(t, log)
}

// TestWebActuallyWatchesTheFileItWasStartedWith crosses the seam the rest of
// this file stops short of: every test above drives configWatcher.check by
// hand, so deleting the one line in WebCmd.serve that starts a watcher at all
// shipped a daemon that never reloads with the whole suite green.
//
// Through run(["web", ...]) — the real command, a real listener, a real edit —
// because that line is the only thing between a working watcher and a
// feature nobody has.
func TestWebActuallyWatchesTheFileItWasStartedWith(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	reloadPipeline(t, path, "build")

	served := startWeb(t, []string{path})
	defer served.stop(t)

	reloadPipeline(t, path, "build", "deploy")

	waitForServedPage(t, served, "/p/"+web.Slugify(path), "deploy")
}

// waitForServedPage polls a real backgrounded daemon's page until it says
// what the caller expects.
func waitForServedPage(t *testing.T, served *webProcess, path, want string) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+served.addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}

		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()

			if strings.Contains(string(body), want) {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the served page %s never said %q", path, want)
}
