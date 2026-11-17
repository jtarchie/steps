package e2e

// steps#235: a run is queued on the daemon from the terminal — by job, pinned to a version, or as a retry of a recorded run — and builds there like any other queued run.

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

func TestRunsTriggerQueuesAPinnedRunOnTheDaemon(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen")

	path := writePipeline(t, dir, `
resource_types:
- name: counter
  config:
    check: echo '[{"n":"one"},{"n":"two"},{"n":"three"}]'
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: note
    inputs: [ticks]
    run: cat ticks/n.txt >> `+seen+`
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--pin", "n=one", "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger --pin: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 1)

	if got := readTrimmed(t, seen); got != "one" {
		t.Fatalf("the pinned run built %q, want the pinned version one", got)
	}

	pinned := newestRun(t, served.state, name, "build").ID

	err = stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 2)

	if got := readTrimmed(t, seen); got != "one\nthree" {
		t.Fatalf("after an unpinned trigger seen = %q, want the newest version built second", got)
	}

	err = stepsRun(t, []string{"runs", "retry", pinned, "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs retry: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 3)

	if got := readTrimmed(t, seen); got != "one\nthree\none" {
		t.Errorf("after the retry seen = %q, want the retried run's own version built again", got)
	}

	served.stop(t)
}

// steps#274: a webhook has no check, so a pin can only be answered from the deliveries already recorded — and must be, or a past delivery could never be rebuilt.
func TestRunsTriggerPinsARecordedWebhookDelivery(t *testing.T) {
	t.Setenv("STEPS_TEST_GITHUB_SECRET", githubSecret)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: STEPS_TEST_GITHUB_SECRET
jobs:
- name: triage
  plan:
  - get: push
    trigger: true
  - task: note
    inputs: [push]
    run: cat push/body
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)
	url := fmt.Sprintf("http://%s/p/%s/hooks/push", served.addr, name)

	for i, id := range []string{"first-delivery", "second-delivery"} {
		body := []byte(`{"n":"` + id + `"}`)

		status := postDelivery(t, url, githubDelivery(githubSecret, "push", id, body), body)
		if status != http.StatusOK {
			t.Fatalf("delivery %s answered %d, want 200", id, status)
		}

		waitForQueueSuccess(t, served.state, name, i+1)
	}

	err := stepsRun(t, []string{"runs", "trigger", "triage", "-p", name, "--pin", "id=first-delivery", "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger --pin: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 3)
	served.stop(t)

	st, err := sqlite.OpenStore(served.state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	// The pinned run's task is a step-cache hit on the first delivery's build, so what it was created with is the evidence, not a second line of output.
	got := runVersions(t, st, "triage", "push")
	if len(got) != 3 || len(got[0]) != 1 || !strings.Contains(got[0][0], `"first-delivery"`) {
		t.Fatalf("runs were created with %q, newest first; want the pinned run on the first delivery", got)
	}
}

func TestRunsTriggerRefusesWhatTheDaemonCannotQueue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: note
    inputs: []
    run: "true"
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	for label, args := range map[string][]string{
		"an unknown job": {"runs", "trigger", "nope", "-p", name, "--target", served.target()},
		"an unknown run": {"runs", "retry", "NOSUCHRUN", "-p", name, "--target", served.target()},
	} {
		if stepsRun(t, args) == nil {
			t.Errorf("%s was accepted", label)
		}
	}

	served.stop(t)
}
