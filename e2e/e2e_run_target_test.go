package e2e

// steps#283: `steps run --target` hands the job to a daemon, which runs it as a one-off under a pipeline of its own, and the terminal follows it there over HTTP — the same path for a daemon on this machine or another.

import (
	"net/http"
	"net/http/httptest"
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

	var err error

	queued := captureStdout(t, func() {
		err = stepsRun(t, []string{"runs", "trigger", "build", "-p", name, "--target", served.target()})
	})
	if err != nil {
		t.Fatalf("steps runs trigger: %v", err)
	}

	newestRun(t, served.state, name, "build")

	// Followed by the command the trigger printed, which must find the daemon it was queued on.
	out := captureStdout(t, func() {
		err = stepsRun(t, append(followHint(t, queued), "--progress", "plain"))
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

// run and follow take --target as a bare flag rather than TargetFlags, and a target url.Parse refuses still carries its credentials, so the refusal must not quote it.
func TestAnUnparseableTargetNeverPrintsItsPassword(t *testing.T) {
	t.Parallel()

	// url.Parse cuts at the #, so its error quotes the half before it: neither half may appear.
	const target = "http://u:hunter#22@127.0.0.1:1"

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`)

	for _, args := range [][]string{
		{"run", path, "--target", target},
		{"runs", "follow", "-p", "demo", "--target", target},
	} {
		t.Run(args[0], func(t *testing.T) {
			stderr := captureStderr(t)

			var err error

			out := captureStdout(t, func() {
				err = stepsRun(t, args)
			})

			if err == nil {
				t.Fatalf("steps %s accepted an unparseable target", strings.Join(args, " "))
			}

			if !strings.Contains(err.Error(), "--target") {
				t.Errorf("steps %s = %v, want --target named", strings.Join(args, " "), err)
			}

			for where, text := range map[string]string{"error": err.Error(), "stdout": out, "stderr": stderr()} {
				if strings.Contains(text, "hunter") || strings.Contains(text, "#22") {
					t.Errorf("the %s repeated the password: %s", where, text)
				}
			}
		})
	}
}

// followHint is the `steps runs follow` command a verb printed, as args, up to the line's prose.
func followHint(t *testing.T, out string) []string {
	t.Helper()

	_, hint, ok := strings.Cut(out, "steps runs follow ")
	if !ok {
		t.Fatalf("no follow command printed:\n%s", out)
	}

	hint, _, _ = strings.Cut(strings.TrimSpace(hint), " watches")

	return append([]string{"runs", "follow"}, strings.Fields(hint)...)
}

// A queue or a resume on a daemon other than the default must print a follow that reaches it, and never with the password it was given.
func TestTheQueuedHintsNameTheDaemon(t *testing.T) {
	t.Parallel()

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) }))
	defer daemon.Close()

	target := "http://alex:hunter2@" + strings.TrimPrefix(daemon.URL, "http://")

	for verb, args := range map[string][]string{
		"trigger": {"runs", "trigger", "build", "-p", "app", "--target", target},
		"resume":  {"runs", "resume", "RUN", "-p", "app", "--target", target},
	} {
		t.Run(verb, func(t *testing.T) {
			var err error

			out := captureStdout(t, func() { err = stepsRun(t, args) })
			if err != nil {
				t.Fatalf("steps runs %s: %v", verb, err)
			}

			hint := strings.Join(followHint(t, out), " ")
			if !strings.Contains(hint, "--target "+daemon.URL+" ") || strings.Contains(out, "hunter2") {
				t.Errorf("steps runs %s printed %q, want the follow to name --target %s without its password", verb, out, daemon.URL)
			}
		})
	}
}

// A one-off carries the source, its includes and the job; any other run flag would be dropped on the way, so it is refused rather than silently ignored.
func TestRunTargetRefusesTheFlagsADaemonWouldIgnore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writePipeline(t, dir, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	for flag, args := range map[string][]string{
		"db":                 {"--db", filepath.Join(dir, "x.db")},
		"name":               {"--name", "x=" + path},
		"resume":             {"--resume", "RUN"},
		"replay":             {"--replay", "RUN"},
		"from":               {"--from", "say"},
		"rerun":              {"--rerun", "RUN"},
		"answer":             {"--answer", "which bump=minor"},
		"deliver":            {"--deliver", "push=push.http"},
		"worker":             {"--worker", "gpu=ssh://jt@box"},
		"config":             {"--config", filepath.Join(dir, "steps.yml")},
		"no-preflight":       {"--no-preflight"},
		"artifact-store":     {"--artifact-store", "s3://bucket/prefix"},
		"keep-workspace":     {"--keep-workspace"},
		"workspace-root":     {"--workspace-root", dir},
		"resource-cache-max": {"--resource-cache-max", "5"},
		"version-history":    {"--version-history", "5"},
		"run-history":        {"--run-history", "5"},
	} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()

			err := stepsRun(t, append([]string{"run", path, "--target", "http://127.0.0.1:1"}, args...))
			if err == nil || !strings.Contains(err.Error(), "--"+flag+" does not apply with --target") {
				t.Errorf("steps run --target %s = %v, want --%s refused", strings.Join(args, " "), err, flag)
			}
		})
	}
}

// Not t.Parallel(): t.Setenv. An exported default is a standing preference for local runs, not something this one-off asked for.
func TestRunTargetIgnoresEnvironmentDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	t.Setenv("STEPS_KEEP_WORKSPACE", "true")
	t.Setenv("STEPS_WORKSPACE_ROOT", dir)

	err := stepsRun(t, []string{"run", path, "--target", "http://127.0.0.1:1", "--progress", "plain"})
	if err == nil || strings.Contains(err.Error(), "does not apply with --target") {
		t.Errorf("steps run --target with STEPS_KEEP_WORKSPACE and STEPS_WORKSPACE_ROOT exported = %v, want it sent and refused only by the unreachable daemon", err)
	}
}

// A finished one-off keeps its page, transcript and steps runs entry until --one-off-history newer ones have finished, across a restart, which serves it unpolled because the state database says it is a one-off.
func TestRunTargetKeepsTheLastFinishedOneOffs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
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
    run: echo kept-on-the-daemon
- name: auto
  plan:
  - get: ticks
    trigger: true
  - task: note
    run: touch `+polled+`
`)

	served := startWeb(t, "--db", state, "--interval", "50ms", "--one-off-history", "1")
	defer served.stopIfRunning(t)

	first := runTargetPage(t, path, served)

	// Several reap polls, and the one-off still alone under its bound.
	time.Sleep(3 * time.Second)

	assertOneOffKept(t, served, state, first)

	served.stop(t)

	served = startWeb(t, "--db", state, "--interval", "50ms", "--one-off-history", "1")
	defer served.stopIfRunning(t)

	assertOneOffKept(t, served, state, first)

	second := runTargetPage(t, path, served)

	deadline := time.Now().Add(30 * time.Second)
	for code, _ := served.get(t, first); code != http.StatusNotFound; code, _ = served.get(t, first) {
		if time.Now().After(deadline) {
			t.Fatalf("GET %s = %d after a newer one-off finished past --one-off-history 1, want it gone", first, code)
		}

		time.Sleep(50 * time.Millisecond)
	}

	assertOneOffKept(t, served, state, second)

	if fileExists(polled) {
		t.Error("a restored one-off was polled, and its trigger: true job ran")
	}
}

// runTargetPage runs build as a one-off and answers its run's page on the daemon.
func runTargetPage(t *testing.T, path string, served *webProcess) string {
	t.Helper()

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "build", "--target", served.target(), "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("steps run --target: %v\n%s", err, out)
	}

	page := regexp.MustCompile(`/p/[^/\s]+/runs/[A-Za-z0-9]+`).FindString(out)
	if page == "" {
		t.Fatalf("the terminal never named the run's page on the daemon:\n%s", out)
	}

	return page
}

func assertOneOffKept(t *testing.T, served *webProcess, state, page string) {
	t.Helper()

	if code, body := served.get(t, page); code != http.StatusOK || !strings.Contains(body, "kept-on-the-daemon") {
		t.Errorf("GET %s = %d, want the finished one-off's run still served with its transcript", page, code)
	}

	pipelineName := strings.Split(page, "/")[2]

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"runs", "-p", pipelineName, "--db", state})
	})
	if err != nil || !strings.Contains(out, "succeeded") {
		t.Errorf("steps runs -p %s = %v, want its finished run listed:\n%s", pipelineName, err, out)
	}
}
