package e2e

// steps#283: `steps run --target` hands the job to a daemon, which runs it as a one-off under a pipeline of its own, and the terminal follows it there over HTTP — the same path for a daemon on this machine or another.

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/outcome"
)

func TestRunTargetRunsTheJobOnTheDaemonAndFollowsIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	built := filepath.Join(dir, "built")
	polled := filepath.Join(dir, "polled")

	path := writePipeline(t, dir, `
resource_types:
- name: counter
  config:
    check: echo '[{"n":"one"}]'
    in: "true"

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - task: say
    run: echo built-on-the-daemon && touch `+built+`
- name: breaks
  plan:
  - task: fail
    run: echo about-to-fail && exit 3
- name: auto
  plan:
  - get: ticks
    trigger: true
  - task: note
    run: touch `+polled+`
`)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "50ms")
	defer served.stopIfRunning(t)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "build", "--target", served.target(), "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("steps run --target: %v\n%s", err, out)
	}

	if !strings.Contains(out, "built-on-the-daemon") {
		t.Errorf("the terminal never showed the task's output:\n%s", out)
	}

	if !fileExists(built) {
		t.Error("the job did not run")
	}

	// The run is the daemon's, so its page is there to open.
	page := regexp.MustCompile(`/p/[^/\s]+/runs/[A-Za-z0-9]+`).FindString(out)
	if page == "" {
		t.Fatalf("the terminal never named the run's page on the daemon:\n%s", out)
	}

	if code, body := served.get(t, page); code != http.StatusOK || !strings.Contains(body, "built-on-the-daemon") {
		t.Errorf("GET %s = %d, want the daemon's page for the run that printed built-on-the-daemon", page, code)
	}

	// A one-off runs the job it was handed and nothing its pipeline would trigger on its own: many polls at 50ms have passed by now.
	time.Sleep(500 * time.Millisecond)

	if fileExists(polled) {
		t.Error("the one-off pipeline polled, and its trigger: true job ran")
	}

	out = captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "breaks", "--target", served.target(), "--progress", "plain"})
	})
	if outcome.ExitCode(err) != outcome.ExitFailed {
		t.Errorf("steps run --target of a failing job = %v, want the run's failure as the exit", err)
	}

	if !strings.Contains(out, "about-to-fail") {
		t.Errorf("the failing run's output never reached the terminal:\n%s", out)
	}
}

// A daemon on another host has no state file here, so following one of its runs goes over HTTP too.
func TestRunsFollowTargetFollowsARunOnTheDaemon(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: say
    run: echo followed-from-afar
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	err := stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--target", served.target()})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	newestRun(t, served.state, name, "build")

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"runs", "follow", "-p", name, "--job", "build", "--target", served.target(), "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("steps runs follow --target: %v\n%s", err, out)
	}

	if !strings.Contains(out, "followed-from-afar") {
		t.Errorf("the followed run's output never reached the terminal:\n%s", out)
	}
}

// The reads a remote follower makes are a pipeline's pages by another route, so a pipeline's access: hides them exactly as it hides the pages.
func TestRunReadsOverTheAPIRespectAccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writePipeline(t, dir, "access:\n  viewer: UserID != \"eve\"\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--role", `admin=UserID == "alex"`,
		"--local-user", "alex:"+hashed(t, "alex"), "--local-user", "sam:"+hashed(t, "sam"), "--local-user", "eve:"+hashed(t, "eve"))
	defer served.stopIfRunning(t)

	err := stepsRun(t, []string{"pipeline", "set", "-p", "secret", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "alex")})
	if err != nil {
		t.Fatalf("set: %v", err)
	}

	err = stepsRun(t, []string{"runs", "trigger", "build", "-p", "secret", "--target", authTarget(served.addr, "alex", "alex")})
	if err != nil {
		t.Fatalf("trigger: %v", err)
	}

	run := newestRun(t, filepath.Join(dir, "daemon.db"), "secret", "build").ID

	for _, path := range []string{"/api/pipelines/secret/runs", "/api/pipelines/secret/runs/" + run, "/api/pipelines/secret/runs/" + run + "/events"} {
		if code, body := authBody(t, served.addr, "sam", "sam", path); code != http.StatusOK || !strings.Contains(body, run) {
			t.Errorf("sam, a viewer the pipeline admits, got %d for %s:\n%s", code, path, body)
		}

		if code, body := authBody(t, served.addr, "eve", "eve", path); code != http.StatusNotFound || strings.Contains(body, run) {
			t.Errorf("eve, whom the pipeline hides, got %d for %s, want told it does not exist:\n%s", code, path, body)
		}
	}
}

// A one-off is arbitrary commands on the daemon's machine, which is a set's power, so it asks what a set asks — and an operator, who may only trigger what an admin set, is refused.
func TestRunTargetAsksWhatASetAsks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ran := filepath.Join(dir, "ran")

	path := writePipeline(t, dir, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: touch "+ran+"\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"),
		"--role", `admin=UserID == "alex"`, "--role", `operator=UserID == "dan"`,
		"--local-user", "alex:"+hashed(t, "alex"), "--local-user", "dan:"+hashed(t, "dan"))
	defer served.stopIfRunning(t)

	err := stepsRun(t, []string{"run", path, "--target", authTarget(served.addr, "dan", "dan"), "--progress", "plain"})
	if err == nil || !strings.Contains(err.Error(), "admin") {
		t.Errorf("an operator's one-off = %v, want refused naming the admin role", err)
	}

	if fileExists(ran) {
		t.Fatal("an operator's refused one-off ran anyway")
	}

	err = stepsRun(t, []string{"run", path, "--target", authTarget(served.addr, "alex", "alex"), "--progress", "plain"})
	if err != nil {
		t.Fatalf("an admin's one-off: %v", err)
	}

	if !fileExists(ran) {
		t.Error("an admin's one-off did not run")
	}
}
