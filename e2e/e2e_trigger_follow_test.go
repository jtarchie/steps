package e2e

// steps#294's first slice and #283's leftovers: a terminal that queues a run on the daemon can follow exactly that run and exit as it did, including when it is aborted before it ever starts.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/outcome"
)

func TestRunsTriggerFollowExitsAsTheRunDid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: say
    run: echo triggered-and-followed
- name: breaks
  plan:
  - task: fail
    run: echo about-to-fail && exit 3
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--follow", "--progress", "plain", "--target", served.target()})
	})
	if err != nil {
		t.Fatalf("steps runs trigger --follow: %v\n%s", err, out)
	}

	if !strings.Contains(out, "triggered-and-followed") {
		t.Errorf("the followed run's output never reached the terminal:\n%s", out)
	}

	if id := followedID(t, out); id != newestRun(t, served.state, name, "build").ID {
		t.Errorf("followed %s, want the run the trigger queued", id)
	}

	out = captureStdout(t, func() {
		err = stepsRun(t, []string{"runs", "trigger", "breaks", "-p", name, "-w", "--progress", "plain", "--target", served.target()})
	})
	if outcome.ExitCode(err) != outcome.ExitFailed {
		t.Errorf("steps runs trigger -w of a failing job = %v, want the run's failure as the exit\n%s", err, out)
	}
}

// A plain trigger merges into the job's waiting run, as a second click does, so two terminals that each asked must follow that one run rather than whichever is newest.
func TestRunsTriggerFollowFollowsTheRunItMergedInto(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	gate := filepath.Join(dir, "gate")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: wait
    run: |
      touch `+started+`
      until [ -f `+gate+` ]; do sleep 0.05; done
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	waitForFile(t, started)

	first := newestRun(t, served.state, name, "build").ID

	outs := [2]*lockedBuffer{{}, {}}
	errs := make(chan error, len(outs))

	for _, out := range outs {
		go func() {
			errs <- cli.RunWith(cli.Streams{Out: out, Err: out}, []string{"runs", "trigger", "build", "-p", name, "--follow", "--progress", "plain", "--target", served.target()})
		}()
	}

	queued := regexp.MustCompile(`queued: `)
	for _, out := range outs {
		waitForWritten(t, out, queued)
	}

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for range outs {
		err = <-errs
		if err != nil {
			t.Errorf("a follower: %v", err)
		}
	}

	a, b := followedID(t, outs[0].String()), followedID(t, outs[1].String())
	if a != b || a == first {
		t.Errorf("the followers followed %s and %s, want one run, the one queued behind %s", a, b, first)
	}
}

// #283: a one-off's run aborted before it starts never starts, so the terminal waiting for it must say so and exit rather than wait forever.
func TestRunTargetExitsWhenItsQueuedRunIsAborted(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	gate := filepath.Join(dir, "gate")
	ran := filepath.Join(dir, "ran")

	holdPath := filepath.Join(dir, "hold.yml")
	writePipelineFile(t, holdPath, `
jobs:
- name: hold
  plan:
  - task: wait
    run: |
      touch `+started+`
      until [ -f `+gate+` ]; do sleep 0.05; done
`)

	path := writePipeline(t, dir, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: touch "+ran+"\n")

	// --max-runs 1 holds the one-off's row pending behind hold, where an abort of the queued run is what stops it.
	served := startWebFor(t, holdPath, "--max-runs", "1", "--interval", "1h")
	defer served.stopIfRunning(t)

	defer func() { _ = os.WriteFile(gate, nil, 0o600) }()

	served.trigger(t, "hold", "hold")
	waitForFile(t, started)

	out := &lockedBuffer{}
	done := make(chan error, 1)

	go func() {
		done <- cli.RunWith(cli.Streams{Out: out, Err: out}, []string{"run", path, "--job", "build", "--target", served.target(), "--progress", "plain"})
	}()

	oneOff := waitForWritten(t, out, regexp.MustCompile(`one-off\.[a-z0-9]+`))

	err := stepsRun(t, []string{"runs", "abort", "-p", oneOff, "--queued", "build", "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs abort --queued: %v", err)
	}

	select {
	case err = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("steps run --target still waiting after its queued run was aborted:\n%s", out)
	}

	if outcome.ExitCode(err) != outcome.ExitAborted || !strings.Contains(err.Error(), "aborted") {
		t.Errorf("steps run --target of an aborted one-off = %v, want an aborted exit naming the abort", err)
	}

	if fileExists(ran) {
		t.Error("the aborted one-off ran")
	}
}

// #283: --pin reaches the daemon, which builds the version named rather than the newest.
func TestRunTargetCarriesThePinToTheDaemon(t *testing.T) {
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

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "1h")
	defer served.stopIfRunning(t)

	err := stepsRun(t, []string{"run", path, "--target", served.target(), "--pin", "n=one", "--progress", "plain"})
	if err != nil {
		t.Fatalf("steps run --target --pin: %v", err)
	}

	if got := readTrimmed(t, seen); got != "one" {
		t.Errorf("the pinned one-off built %q, want the pinned version one", got)
	}

	err = stepsRun(t, []string{"run", path, "--target", served.target(), "--pin", "n=one", "--force", "--progress", "plain"})
	if err == nil || !strings.Contains(err.Error(), "--pin") || !strings.Contains(err.Error(), "--force") {
		t.Errorf("steps run --target --pin --force = %v, want refused naming both, as the daemon cannot honor them together", err)
	}
}

// #283: --force reaches the daemon. A one-off is a new pipeline, but the step cache is keyed by the work rather than the pipeline, so a second one-off of the same file is answered from the first's unless forced.
func TestRunTargetCarriesForceToTheDaemon(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: make
    outputs: [out]
    run: echo made >> `+ran+` && echo made > out/made
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "1h")
	defer served.stopIfRunning(t)

	for i, extra := range [][]string{nil, nil, {"--force"}} {
		var err error

		out := captureStdout(t, func() {
			err = stepsRun(t, append([]string{"run", path, "--target", served.target(), "--progress", "plain"}, extra...))
		})
		if err != nil {
			t.Fatalf("one-off %d %v: %v\n%s", i, extra, err, out)
		}
	}

	if got := readTrimmed(t, ran); got != "made\nmade" {
		t.Errorf("the task ran %q across a one-off, its cached repeat and a forced one; want it run by the first and the forced one only", got)
	}
}

// waitForWritten is the first match of pattern an invocation still running writes to out.
func waitForWritten(t *testing.T, out *lockedBuffer, pattern *regexp.Regexp) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for {
		if match := pattern.FindString(out.String()); match != "" {
			return match
		}

		if time.Now().After(deadline) {
			t.Fatalf("never wrote %s:\n%s", pattern, out)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// followedID is the run a follow said it was following.
func followedID(t *testing.T, out string) string {
	t.Helper()

	match := regexp.MustCompile(`following ([A-Za-z0-9]+)`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no run followed:\n%s", out)
	}

	return match[1]
}
