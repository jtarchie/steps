package e2e

// steps#235: a run is queued on the daemon from the terminal — by job, pinned to a version, or as a retry of a recorded run — and builds there like any other queued run.

import (
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

func TestRunsTriggerQueuesAPinnedRunOnTheDaemon(t *testing.T) {
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

	err := cli.Run([]string{"runs", "trigger", "build", "-p", name, "--pin", "n=one", "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger --pin: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 1)

	if got := readTrimmed(t, seen); got != "one" {
		t.Fatalf("the pinned run built %q, want the pinned version one", got)
	}

	pinned := newestRun(t, served.state, name, "build").ID

	err = cli.Run([]string{"runs", "trigger", "build", "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 2)

	if got := readTrimmed(t, seen); got != "one\nthree" {
		t.Fatalf("after an unpinned trigger seen = %q, want the newest version built second", got)
	}

	err = cli.Run([]string{"runs", "retry", pinned, "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs retry: %v", err)
	}

	waitForQueueSuccess(t, served.state, name, 3)

	if got := readTrimmed(t, seen); got != "one\nthree\none" {
		t.Errorf("after the retry seen = %q, want the retried run's own version built again", got)
	}

	served.stop(t)
}

func TestRunsTriggerRefusesWhatTheDaemonCannotQueue(t *testing.T) {
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
		if cli.Run(args) == nil {
			t.Errorf("%s was accepted", label)
		}
	}

	served.stop(t)
}
