package e2e

// Every test here crosses the HTTP boundary on purpose: set→adopt, compare-and-set, pause and restart are seams a test of either half alone proves nothing about.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestPipelineSetServesAndPollsWhatWasSet: an empty daemon, one set, and the poll that set causes.
func TestPipelineSetServesAndPollsWhatWasSet(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	served := startWeb(t, "--db", fixture.db, "--interval", "100ms")
	defer served.stop(t)

	// An empty daemon must say how to fill it rather than 404 or redirect to nothing.
	code, body := served.get(t, "/")
	if code != http.StatusOK || !strings.Contains(body, "steps pipeline set") {
		t.Fatalf("an empty daemon's root = %d, want 200 saying how to set a pipeline:\n%s", code, body)
	}

	out := captureStdout(t, func() { served.set(t, "items", fixture.pipeline) })
	if !strings.Contains(out, "created") {
		t.Errorf("set did not say it created the pipeline:\n%s", out)
	}

	// The cold start builds the newest version, which proves polling and draining both reached the set pipeline.
	waitForDid(t, fixture, "1")

	out = captureStdout(t, func() { served.pipeline(t, "list") })
	if !strings.Contains(out, "items") {
		t.Errorf("pipeline list does not name the pipeline that was set:\n%s", out)
	}

	// Served under its name.
	code, _ = served.get(t, "/p/items")
	if code != http.StatusOK {
		t.Errorf("/p/items = %d, want 200", code)
	}

	// And -p is an override, not a requirement: without one the name is the
	// YAML's base name, which is what the empty daemon's own instructions and
	// every doc example rely on.
	err := cli.Run([]string{"pipeline", "set", "-c", fixture.pipeline, "-n", "--target", served.target()})
	if err != nil {
		t.Fatalf("steps pipeline set with no -p: %v", err)
	}

	if code, _ := served.get(t, "/p/"+cli.PipelineName(fixture.pipeline)); code != http.StatusOK {
		t.Errorf("a set with no -p did not land under the YAML's base name (%s)", cli.PipelineName(fixture.pipeline))
	}
}

// TestPipelineSetAppliesAnEdit: setting is the ONLY way a served pipeline changes, and a trigger the edit adds is polled after the set.
func TestPipelineSetAppliesAnEdit(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	versions := filepath.Join(dir, "versions.json")
	log := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"1"}]`)
	reloadPipeline(t, path, "build")

	served := startWebFor(t, path, "--interval", "100ms")
	defer served.stop(t)

	// Nothing watches the file any more, so this edit must change nothing served.
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
- name: deploy
  plan:
  - task: ship
    inputs: []
    run: echo shipped
`)

	time.Sleep(300 * time.Millisecond)

	if _, page := served.get(t, "/p/"+cli.PipelineName(path)); strings.Contains(page, "deploy") {
		t.Fatal("the daemon picked up an edit nobody set")
	}

	if fileExists(log) {
		t.Fatal("the daemon polled a trigger nobody set")
	}

	served.set(t, cli.PipelineName(path), path)

	waitForServedPage(t, served, "/p/"+cli.PipelineName(path), "deploy")
	waitForFile(t, log)
}

// TestPipelineSetCarriesIncludes: a run_file: is pipeline-relative and the daemon has no sibling filesystem, so the script must travel with the set.
func TestPipelineSetCarriesIncludes(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	log := filepath.Join(dir, "ran.log")

	err := os.MkdirAll(filepath.Join(dir, "ci"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, filepath.Join(dir, "ci", "build.sh"), "echo from-the-include >> "+log+"\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run_file: ci/build.sh
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	// Removed AFTER the set so a daemon reading its own disk instead of the bundle would fail here.
	err = os.Remove(filepath.Join(dir, "ci", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}

	served.trigger(t, cli.PipelineName(path), "build")
	waitForFile(t, log)

	if got := readFileString(t, log); !strings.Contains(got, "from-the-include") {
		t.Errorf("the job ran %q, want the included script's output", got)
	}

	// What was set must be readable back, or the daemon is the only copy of a configuration nobody can see.
	out := captureStdout(t, func() { served.pipeline(t, "get", "-p", cli.PipelineName(path)) })
	if !strings.Contains(out, "run_file: ci/build.sh") {
		t.Errorf("get does not print the configuration that was set:\n%s", out)
	}
}

// TestPipelineSetRefusesWhatCannotRunHere: the refusal must land in the terminal that asked, which is why the transport is HTTP.
func TestPipelineSetRefusesWhatCannotRunHere(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)

	writePipelineFile(t, path, `
agents:
- name: reviewer
  source:
    model: openrouter/qwen/qwen3.7-flash
    api_key_env: STEPS_TEST_ABSENT_KEY
jobs:
- name: review
  plan:
  - agent: reviewer
    messages: [hi]
`)

	served := startWebFor(t, path, "--interval", "1h", "--skip-set")
	defer served.stop(t)

	err := cli.Run([]string{"pipeline", "set", "-p", "review", "-c", path, "-n", "--target", served.target()})
	if err == nil {
		t.Fatal("set accepted a pipeline whose api_key_env is not set on the daemon's machine")
	}

	if !strings.Contains(err.Error(), "STEPS_TEST_ABSENT_KEY") {
		t.Errorf("the refusal does not name what is missing: %v", err)
	}

	if code, _ := served.get(t, "/p/review"); code != http.StatusNotFound {
		t.Errorf("/p/review = %d after a refused set, want 404", code)
	}
}

// TestPipelineSetIsCompareAndSet: a set that diffed against a revision that has since moved must be refused, not applied over the newer one.
func TestPipelineSetIsCompareAndSet(t *testing.T) {
	path := flagFixture(t)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)

	body, err := json.Marshal(map[string]any{
		"source":     "jobs:\n- name: build\n  plan:\n  - task: compile\n    inputs: []\n    run: echo stale\n",
		"expect_sha": "not-the-current-one",
	})
	if err != nil {
		t.Fatal(err)
	}

	status, reply := served.put(t, "/api/pipelines/"+name, body)
	if status != http.StatusConflict {
		t.Fatalf("a set against a stale sha answered %d, want 409: %s", status, reply)
	}

	// A refused set must leave what is served untouched.
	out := captureStdout(t, func() { served.pipeline(t, "get", "-p", name) })
	if strings.Contains(out, "echo stale") {
		t.Error("a refused set changed what is served")
	}

	// The CLI diffs first and sends the sha it saw, so its set goes through.
	served.set(t, name, path)
}

// TestPipelinePauseIsTheCircuitBreaker: paused means no polling, no
// admission, a webhook that enqueues nothing and a refused trigger, until
// unpause.
//
// Every window here is longer than the drainer's own idle backoff, which is
// what a sabotage pass showed to be load-bearing: a shorter one passes with
// the admission gate removed, because the drain was merely between naps.
func TestPipelinePauseIsTheCircuitBreaker(t *testing.T) {
	t.Setenv("STEPS_TEST_WEBHOOK_TOKEN", "s3cret")

	fixture := newWatchFixture(t, webhookPipeline)
	fixture.items(t, 1)

	served := startWebFor(t, fixture.pipeline, "--interval", "100ms")
	defer served.stop(t)

	name := cli.PipelineName(fixture.pipeline)

	waitForDid(t, fixture, "1")

	served.pipeline(t, "pause", "-p", name)

	// A poll already in flight is allowed to finish before the feed grows.
	time.Sleep(300 * time.Millisecond)
	fixture.items(t, 2)

	hook := fmt.Sprintf("http://%s/p/%s/check/items?token=s3cret", served.addr, name)
	if status := postWebhook(t, hook); status != http.StatusOK {
		t.Errorf("webhook on a paused pipeline answered %d, want 200 (received, not acted on)", status)
	}

	trigger := fmt.Sprintf("http://%s/p/%s/jobs/build/trigger", served.addr, name)
	if status := postWebhook(t, trigger); status != http.StatusConflict {
		t.Errorf("manual trigger on a paused pipeline answered %d, want 409", status)
	}

	// A row queued BEFORE the pause is the drain's own gate, and the only way
	// to see it: with the poller stopped, nothing else puts work in front of
	// the drain to be refused.
	enqueueDirectly(t, served.state, name, "build")

	time.Sleep(drainIdleWindow)

	if did := fixture.did(t); strings.Join(did, " ") != "1" {
		t.Fatalf("a paused pipeline built %v, want nothing after 1", did)
	}

	if !queueHasPending(t, served.state, name) {
		t.Error("a paused pipeline claimed the row that was queued before it was paused")
	}

	if _, page := served.get(t, "/p/"+name); !strings.Contains(page, "paused") {
		t.Error("the pipeline's page does not say it is paused")
	}

	served.pipeline(t, "unpause", "-p", name)

	waitForDid(t, fixture, "1", "2")
}

// drainIdleWindow is longer than the drainer's own idle backoff, so a claim
// that did not happen inside it did not happen because it was refused rather
// than because the worker was asleep.
const drainIdleWindow = 2 * time.Second

// TestPipelineDestroyForgetsEverything: destroy is one DELETE, so the route, the rows and the history all go.
func TestPipelineDestroyForgetsEverything(t *testing.T) {
	path := flagFixture(t)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "build")
	waitForQueueSuccess(t, served.state, name, 1)

	served.pipeline(t, "destroy", "-p", name, "-n")

	if code, _ := served.get(t, "/p/"+name); code != http.StatusNotFound {
		t.Errorf("/p/%s = %d after destroy, want 404", name, code)
	}

	reader, err := sqlite.OpenReader(served.state)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	defer func() { _ = reader.Close() }()

	rows, err := reader.Pipelines(t.Context())
	if err != nil {
		t.Fatalf("Pipelines: %v", err)
	}

	for _, row := range rows {
		if row.Name == name {
			t.Errorf("destroy left the pipelines row behind: %+v", row)
		}
	}

	// Destroyed rather than stopped: a re-set starts with no history.
	served.set(t, name, path)

	if code, page := served.get(t, "/p/"+name+"/runs"); code != http.StatusOK || strings.Contains(page, "passed") {
		t.Errorf("a re-set pipeline still shows the destroyed one's runs (%d):\n%s", code, page)
	}
}

// TestPipelineRenameKeepsHistory: a rename is one UPDATE, so the old name's runs become the new name's.
func TestPipelineRenameKeepsHistory(t *testing.T) {
	path := flagFixture(t)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "build")
	waitForQueueSuccess(t, served.state, name, 1)

	served.pipeline(t, "rename", "-p", name, "--to", "renamed")

	if code, _ := served.get(t, "/p/"+name); code != http.StatusNotFound {
		t.Errorf("/p/%s = %d after rename, want 404", name, code)
	}

	code, page := served.get(t, "/p/renamed/runs")
	if code != http.StatusOK || !strings.Contains(page, "passed") {
		t.Errorf("/p/renamed/runs = %d, want the run recorded under the old name:\n%s", code, page)
	}

	// The read commands must follow the name, or history is reachable from the browser and not the terminal.
	out := captureStdout(t, func() {
		err := cli.Run([]string{"runs", "-p", "renamed", "--db", served.state})
		if err != nil {
			t.Fatalf("steps runs under the new name: %v", err)
		}
	})

	if !strings.Contains(out, "succeeded") {
		t.Errorf("steps runs -p renamed does not list the run:\n%s", out)
	}
}

// TestPipelineSetSurvivesARestart: a restart serves what was set from the database, with nobody setting it again.
func TestPipelineSetSurvivesARestart(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	served := startWebFor(t, fixture.pipeline, "--interval", "100ms")
	name := cli.PipelineName(fixture.pipeline)

	waitForDid(t, fixture, "1")
	served.stop(t)

	// With the file gone, only the daemon's own record can serve the pipeline.
	err := os.Remove(fixture.pipeline)
	if err != nil {
		t.Fatal(err)
	}

	served = startWeb(t, "--db", fixture.db, "--interval", "100ms")
	defer served.stop(t)

	if code, _ := served.get(t, "/p/"+name); code != http.StatusOK {
		t.Fatalf("/p/%s = %d after a restart, want the pipeline served from the database", name, code)
	}

	fixture.items(t, 2)
	waitForDid(t, fixture, "1", "2")
}

// TestPipelineSetVarsArePerPipeline: one file set twice under two vars is two pipelines, each running under its own.
func TestPipelineSetVarsArePerPipeline(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)
	log := filepath.Join(dir, "greetings.log")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: greet
    inputs: []
    run: echo ((greeting)) >> `+log+`
`)

	served := startWeb(t, "--db", filepath.Join(dir, "state.db"), "--interval", "1h")
	defer served.stop(t)

	served.set(t, "hello", path, "-v", "greeting=hello")
	served.set(t, "goodbye", path, "-v", "greeting=goodbye")

	served.trigger(t, "hello", "build")
	served.trigger(t, "goodbye", "build")

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		if fileExists(log) && strings.Count(readFileString(t, log), "\n") == 2 {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	got := readFileString(t, log)
	if !strings.Contains(got, "hello") || !strings.Contains(got, "goodbye") {
		t.Errorf("the two pipelines ran with %q, want one greeting each", got)
	}
}

// TestWebTakesNoPipelineArguments pins the pure-server decision: set configures the daemon and nothing else does.
func TestWebTakesNoPipelineArguments(t *testing.T) {
	t.Parallel()

	err := cli.Run([]string{"web", flagFixture(t), "--listen", "127.0.0.1:1"})
	if err == nil {
		t.Fatal("steps web accepted a pipeline file argument")
	}

	if !strings.Contains(err.Error(), "could not parse flags") {
		t.Errorf("the argument was rejected by the command rather than by the grammar: %v", err)
	}

	for _, flag := range []string{"--once", "--var", "--vars-file", "--name"} {
		err = cli.Run([]string{"web", flag, "x", "--listen", "127.0.0.1:1"})
		if err == nil || !strings.Contains(err.Error(), "could not parse flags") {
			t.Errorf("steps web still accepts %s: %v", flag, err)
		}
	}
}

func (w *webProcess) set(t *testing.T, name, path string, extra ...string) {
	t.Helper()

	args := append([]string{"pipeline", "set", "-p", name, "-c", path, "-n", "--target", w.target()}, extra...)

	err := cli.Run(args)
	if err != nil {
		t.Fatalf("steps pipeline set %s: %v", name, err)
	}
}

func (w *webProcess) pipeline(t *testing.T, args ...string) {
	t.Helper()

	err := cli.Run(append(append([]string{"pipeline"}, args...), "--target", w.target()))
	if err != nil {
		t.Fatalf("steps pipeline %v: %v", args, err)
	}
}

// Through the UI's own route, so the trigger is the one a person's click sends.
//
// Redirects are not followed: a trigger answers 303 to the waiting room, and
// following it would report the waiting room's own 200 instead.
func (w *webProcess) trigger(t *testing.T, name, job string) {
	t.Helper()

	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	url := fmt.Sprintf("http://%s/p/%s/jobs/%s/trigger", w.addr, name, job)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("trigger %s/%s answered %d, want 303", name, job, resp.StatusCode)
	}
}

func (w *webProcess) target() string { return "http://" + w.addr }

func (w *webProcess) get(t *testing.T, path string) (int, string) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+w.addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// Raw HTTP rather than the CLI, because the CLI always sends the sha it just fetched and cannot send a stale one.
func (w *webProcess) put(t *testing.T, path string, body []byte) (int, string) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "http://"+w.addr+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	reply, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(reply)
}

// reloadPipeline writes a pipeline whose job names are the caller's, so a set is visible on the pages the daemon serves.
func reloadPipeline(t *testing.T, path string, jobs ...string) {
	t.Helper()

	body := &strings.Builder{}
	body.WriteString("jobs:\n")

	for _, job := range jobs {
		body.WriteString("- name: " + job + "\n  plan:\n  - task: compile\n    inputs: []\n    run: echo " + job + "\n")
	}

	writePipelineFile(t, path, body.String())
}

// waitForServedPage polls a real backgrounded daemon's page until it says what the caller expects.
func waitForServedPage(t *testing.T, served *webProcess, path, want string) {
	t.Helper()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
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
