package e2e

// A command on a docker+ worker survives the connection to it dropping: steps reaches the worker again, shows what was missed and takes the exit code, never running the command twice (steps#306).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// reattachPipeline is a task that counts its runs in a cache, prints, sleeps for nap, prints again and exits 3, with hooks naming which outcome it had.
func reattachPipeline(t *testing.T, dir, nap, extra string) string {
	t.Helper()

	return writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - do:
    - task: long
      tags: [box]
      image: `+dockerE2EImage+`
      caches:
      - path: counter
`+extra+`      run: |
        echo run >> counter/runs
        echo "runs=$(wc -l < counter/runs | tr -d ' ')"
        echo "id=$(hostname)"
        echo before
        sleep `+nap+`
        echo after
        echo to-stderr >&2
        exit 3
    on_failure:
      task: failed
      run: touch `+filepath.Join(dir, "failed")+`
    on_error:
      task: errored
      run: touch `+filepath.Join(dir, "errored")+`
`)
}

// runUntil runs path on worker in the background and calls during once the step has printed before, returning everything printed and the run's error.
func runUntil(t *testing.T, path, worker string, during func(containerID string)) (string, error) {
	t.Helper()

	out, errOut := &lockedBuffer{}, &lockedBuffer{}
	done := make(chan error, 1)

	go func() {
		done <- cli.RunWith(cli.Streams{Out: out, Err: errOut}, []string{"run", path, "--worker", "box=" + worker})
	}()

	idLine := regexp.MustCompile(`id=([0-9a-f]+)`)
	deadline := time.Now().Add(60 * time.Second)

	for !strings.Contains(out.String(), "before") {
		select {
		case err := <-done:
			t.Fatalf("the run ended before the step printed: %v\n%s\n%s", err, out.String(), errOut.String())
		case <-time.After(50 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			t.Fatalf("the step never printed before:\n%s", out.String())
		}
	}

	during(idLine.FindStringSubmatch(out.String())[1])

	select {
	case err := <-done:
		return out.String() + errOut.String(), err
	case <-time.After(120 * time.Second):
		t.Fatalf("the run never ended:\n%s", out.String())

		return "", nil
	}
}

func assertOnce(t *testing.T, output string, lines ...string) {
	t.Helper()

	for _, line := range lines {
		if n := strings.Count(output, "[long] "+line+"\n"); n != 1 {
			t.Errorf("%q printed %d times, want once:\n%s", line, n, output)
		}
	}
}

func assertHook(t *testing.T, dir, fired, silent string) {
	t.Helper()

	_, err := os.Stat(filepath.Join(dir, fired))
	if err != nil {
		t.Errorf("on_%s did not fire", fired)
	}

	_, err = os.Stat(filepath.Join(dir, silent))
	if err == nil {
		t.Errorf("on_%s fired", silent)
	}
}

// The issue's test: the connection drops mid-sleep, the command goes on, and its exit 3 is the step's verdict, with every line shown once and the command run once.
func TestDockerSSHWorkerCommandSurvivesADroppedConnection(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	worker, server := dockerSSHWorker(t)
	dir := t.TempDir()
	path := reattachPipeline(t, dir, "3", "")

	output, err := runUntil(t, path, worker, func(string) { server.DropAll() })
	if err == nil {
		t.Fatal("the run succeeded; the command exits 3")
	}

	assertOnce(t, output, "runs=1", "before", "after", "to-stderr")
	assertHook(t, dir, "failed", "errored")
}

// The command finishes while nothing can reach the worker: what it printed meanwhile, and its exit code, are still there when the connection is back.
func TestDockerSSHWorkerCommandFinishedDuringTheOutage(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	worker, server := dockerSSHWorker(t)
	dir := t.TempDir()
	path := reattachPipeline(t, dir, "1", "")

	output, err := runUntil(t, path, worker, func(string) { server.DropFor(4 * time.Second) })
	if err == nil {
		t.Fatal("the run succeeded; the command exits 3")
	}

	assertOnce(t, output, "runs=1", "before", "after", "to-stderr")
	assertHook(t, dir, "failed", "errored")
}

// A command whose container is gone when the worker answers again is lost, which is the machinery failing, not the command saying no.
func TestDockerSSHWorkerCommandRemovedDuringTheOutageErrors(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	worker, server := dockerSSHWorker(t)
	dir := t.TempDir()
	path := reattachPipeline(t, dir, "30", "      timeout: 60s\n")

	output, err := runUntil(t, path, worker, func(id string) {
		server.DropFor(3 * time.Second)

		removed := exec.CommandContext(context.Background(), "docker", "rm", "-f", id).Run() //nolint:gosec // an id the container printed
		if removed != nil {
			t.Errorf("removing the command's container: %v", removed)
		}
	})
	if err == nil {
		t.Fatal("the run succeeded with its command's container removed")
	}

	if strings.Contains(output, "[long] after") {
		t.Errorf("the removed command finished:\n%s", output)
	}

	assertHook(t, dir, "errored", "failed")
}

// timeout: stops the command when it fires, even while the worker cannot be reached: once it answers again the container is gone, not left running to teardown.
func TestDockerSSHWorkerTimeoutDuringTheOutageStopsTheCommand(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	worker, server := dockerSSHWorker(t)
	dir := t.TempDir()
	path := reattachPipeline(t, dir, "60", "      timeout: 10s\n")

	var id string

	output, err := runUntil(t, path, worker, func(container string) {
		id = container

		server.DropFor(14 * time.Second)
	})
	if err == nil {
		t.Fatal("the run succeeded past its timeout")
	}

	if strings.Contains(output, "[long] after") {
		t.Errorf("the timed-out command finished:\n%s", output)
	}

	state, _ := exec.CommandContext(context.Background(), "docker", "inspect", "-f", "{{.State.Status}}", id).CombinedOutput() //nolint:gosec // an id the container printed
	if !strings.Contains(strings.ToLower(string(state)), "no such") {
		t.Errorf("the timed-out command's container is still there: %s", state)
	}
}

// A command runs in a container of its own, so a second attempt starts from the image's filesystem, not the first attempt's leftovers; only the tree, outputs: and caches: carry over (steps#306).
func TestAContainerAttemptStartsFromACleanImage(t *testing.T) {
	t.Parallel()
	dockerSlot(t)
	requireDockerE2E(t)

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: install
    image: `+dockerE2EImage+`
    attempts: 2
    run: |
      if [ -f /opt/installed ]; then echo carried; exit 0; fi
      touch /opt/installed
      echo fresh
      exit 1
`)

	var err error

	output := captureStdout(t, func() { err = stepsRun(t, []string{"run", path}) })
	if err == nil {
		t.Fatalf("the second attempt saw the first's /opt/installed:\n%s", output)
	}

	if n := strings.Count(output, "fresh"); n != 2 {
		t.Errorf("fresh printed %d times, want once per attempt:\n%s", n, output)
	}
}

// A resource's in: is reattachable like a task's run:: it is its container's main process, not an exec beside a keepalive.
func TestAResourceInRunsAsItsContainersMainProcess(t *testing.T) {
	t.Parallel()
	dockerSlot(t)
	requireDockerE2E(t)

	dir := t.TempDir()
	report := filepath.Join(dir, "pid1.txt")
	path := writePipeline(t, dir, `
resource_types:
- name: probe
  image: `+dockerE2EImage+`
  config:
    check: echo '[{"ref":"v1"}]'
    in: cat /proc/1/cmdline > pid1.txt

resources:
- name: repo
  type: probe
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: report
    inputs: [repo]
    run: cp repo/pid1.txt `+report+`
`)

	mustRun(t, path)

	if pid1 := readFileString(t, report); strings.Contains(pid1, "sleep") || !strings.Contains(pid1, "pid1.txt") {
		t.Errorf("in: ran beside %q, want as the main process", pid1)
	}
}
