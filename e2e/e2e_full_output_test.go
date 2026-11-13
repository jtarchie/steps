package e2e

// A step's record keeps a head and a tail; when it drops the middle, the whole output is kept in the state database and a person can get it back.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// chattyTask prints a line nobody will find in the record: it sits between 40,000 lines either side.
const chattyTask = `
  - task: chatty
    run: |
      seq 1 20000
      echo needle-in-the-middle
      seq 20001 40000
`

// chattyOutput is every byte chattyTask prints.
func chattyOutput() string {
	var b strings.Builder

	for i := 1; i <= 40000; i++ {
		if i == 20001 {
			b.WriteString("needle-in-the-middle\n")
		}

		fmt.Fprintf(&b, "%d\n", i)
	}

	return b.String()
}

// stepOutput is the newest run's step_output event for the step named name.
func stepOutput(t *testing.T, rows []store.RunEventRow, name string) store.RunEventRow {
	t.Helper()

	for _, row := range rows {
		if row.Type == events.TypeStepOutput && row.StepName == name {
			return row
		}
	}

	t.Fatalf("no output was recorded for %s", name)

	return store.RunEventRow{}
}

// keptOf is the whole output the store kept for the step an output event belongs to, and whether it kept one.
func keptOf(t *testing.T, path string, output store.RunEventRow) (string, bool) {
	t.Helper()

	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	n, err := strconv.Atoi(output.Detail)
	if err != nil {
		n = 0
	}

	var whole strings.Builder

	found, err := st.StepOutput(t.Context(), output.RunID, output.StepID, n, func(chunk []byte) error {
		_, err := whole.Write(chunk)

		return err //nolint:wrapcheck // a strings.Builder never fails
	})
	if err != nil {
		t.Fatalf("StepOutput: %v", err)
	}

	return whole.String(), found
}

// The record elides the middle and says the whole was kept; the database holds every byte the step printed, and a step that printed little stores nothing.
func TestAnElidedOutputIsKeptWholeInTheDatabase(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:`+chattyTask+`
  - task: quiet
    run: echo small
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	rows := recordedEvents(t, path)
	chatty := stepOutput(t, rows, "chatty")

	if strings.Contains(chatty.Text, "needle-in-the-middle") {
		t.Fatal("the record kept the middle line; this test needs it elided")
	}

	if !strings.Contains(chatty.Text, "full output kept") {
		t.Errorf("the record's marker does not say the full output was kept:\n%.8200s", chatty.Text)
	}

	if whole, found := keptOf(t, path, chatty); !found || whole != chattyOutput() {
		t.Errorf("the stored output is not the whole one: found %v, %d bytes, want %d", found, len(whole), len(chattyOutput()))
	}

	quiet := stepOutput(t, rows, "quiet")
	if whole, found := keptOf(t, path, quiet); quiet.Detail != "" || found {
		t.Errorf("a step that printed little stored %q (detail %q)", whole, quiet.Detail)
	}

	_, err = os.Stat(strings.TrimSuffix(string(cli.StatePath(path, "")), ".db") + ".logs")
	if !os.IsNotExist(err) {
		t.Errorf("a log directory sits beside the state file: %v", err)
	}
}

// What is kept is what the record would have held without its bound, after the scrubber: a secret printed in the middle is *** in the database.
func TestAKeptOutputIsScrubbed(t *testing.T) {
	dir, path, config, _ := cliSecretsFixture(t, nil)
	writeVault(t, filepath.Join(dir, "vault"), map[string]string{cli.PipelineName(path) + "/gh_token": e2eSecretValue + "\n"})

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: leak
    secrets:
      TOKEN: ((ci:gh_token))
    run: |
      seq 1 20000
      echo "middle=$TOKEN"
      head=${TOKEN%??????????}
      printf 'split=%s' "$head"
      sleep 0.2
      printf '%s done\n' "${TOKEN#"$head"}"
      seq 20001 40000
`)

	// Not runAndCapture: its pipe is read only once the run returns, and 40,000 lines fill it first.
	orig := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	os.Stdout = devnull
	err = cli.Run([]string{"run", "--config", config, "--progress", "plain", path})
	os.Stdout = orig
	_ = devnull.Close()

	if err != nil {
		t.Fatalf("run --config: %v", err)
	}

	whole, found := keptOf(t, path, stepOutput(t, recordedEvents(t, path), "leak"))
	if !found {
		t.Fatal("the elided output was not kept")
	}

	if strings.Contains(whole, e2eSecretValue) || strings.Contains(whole, e2eSecretValue[:10]) {
		t.Error("the kept output holds the secret")
	}

	if !strings.Contains(whole, "middle=***\n") || !strings.Contains(whole, "split=*** done\n") {
		t.Errorf("the kept output lost the line instead of masking it")
	}
}

// A runaway step cannot fill the database: past the bound the output stops and says how much it dropped.
func TestAKeptOutputIsBounded(t *testing.T) {
	defer func(bound int64) { store.MaxStepOutputBytes = bound }(store.MaxStepOutputBytes)

	store.MaxStepOutputBytes = 100_000

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: runaway
    run: yes x | head -c 400000
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	whole, found := keptOf(t, path, stepOutput(t, recordedEvents(t, path), "runaway"))
	head, trailer, cut := strings.Cut(whole, "\n... [")

	switch {
	case !found || !cut:
		t.Fatalf("the output does not say it was cut (found %v, %d bytes)", found, len(whole))
	case head != strings.Repeat("x\n", 50_000):
		t.Errorf("the output kept %d bytes before its trailer, want exactly the %d-byte bound of what was printed", len(head), 100_000)
	case !strings.Contains(trailer, "300000 further bytes"):
		t.Errorf("the trailer does not count what was dropped: %q", trailer)
	}
}

// A run reaped by run_history: takes its kept output with it, and the run that survives keeps its own.
func TestPruningARunRemovesItsKeptOutput(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
defaults:
  run_history: 1
jobs:
- name: build
  plan:`+chattyTask)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	first := stepOutput(t, recordedEvents(t, path), "chatty")
	if _, found := keptOf(t, path, first); !found {
		t.Fatal("the first run kept nothing")
	}

	err = cli.Run([]string{"run", path, "--job", "build", "--force", "--progress", "plain"})
	if err != nil {
		t.Fatal(err)
	}

	second := stepOutput(t, recordedEvents(t, path), "chatty")

	if second.RunID == first.RunID {
		t.Fatal("the second run is the first")
	}

	if _, found := keptOf(t, path, first); found {
		t.Error("a reaped run's output outlived it")
	}

	if _, found := keptOf(t, path, second); !found {
		t.Error("the surviving run lost its kept output")
	}
}

// runEvents is one run's events, out of a daemon's state file.
func runEvents(t *testing.T, state, name, runID string) []store.RunEventRow {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.RunEvents(t.Context(), runID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	return rows
}

// rawGet is one authenticated GET, keeping the body and the content type as sent.
func rawGet(t *testing.T, addr, user, pass, path string) (int, string, string) {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}

	req.SetBasicAuth(user, pass)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// keptOutputDaemon serves a pipeline eve may not see beside one she may, runs it once, and returns the elided step's output event.
func keptOutputDaemon(t *testing.T) (*webProcess, string, store.RunEventRow) {
	t.Helper()

	dir := t.TempDir()
	state := filepath.Join(dir, "daemon.db")
	path := pipelinePath(t, dir)
	writePipelineFile(t, path, "access:\n  viewer: UserID != \"eve\"\njobs:\n- name: build\n  plan:"+chattyTask)

	other := filepath.Join(dir, "other.yml")
	writePipelineFile(t, other, "jobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", state, "--role", `admin=UserID == "alex"`, "--role", "viewer=true", "--local-user", "alex:"+hashed(t, "a"), "--local-user", "eve:"+hashed(t, "e"))
	name := cli.PipelineName(path)

	for slug, file := range map[string]string{name: path, "other": other} {
		err := cli.Run([]string{"pipeline", "set", "-p", slug, "-c", file, "-n", "--target", authTarget(served.addr, "alex", "a")})
		if err != nil {
			t.Fatalf("set %s: %v", slug, err)
		}
	}

	if code := authPost(t, served.addr, "alex", "a", "/p/"+name+"/jobs/build/trigger"); code != http.StatusSeeOther {
		t.Fatalf("trigger answered %d, want 303", code)
	}

	run := waitForNthRun(t, state, name, "build", 1)
	waitForRunStatus(t, state, name, run.ID, string(store.RunSucceeded))

	var chatty store.RunEventRow

	// The run reads succeeded before its bus has drained into the store.
	for deadline := time.Now().Add(10 * time.Second); chatty.Detail == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		for _, row := range runEvents(t, state, name, run.ID) {
			if row.Type == events.TypeStepOutput {
				chatty = row
			}
		}
	}

	return served, name, chatty
}

// expectGet asks path as user and requires the status, a body holding want, and one without wantNot.
func expectGet(t *testing.T, addr, user, path string, status int, want, wantNot string) string {
	t.Helper()

	code, kind, body := rawGet(t, addr, user, user[:1], path)
	if code != status || !strings.Contains(body, want) || (wantNot != "" && strings.Contains(body, wantNot)) {
		t.Errorf("%s GET %s = %d %s; want %d holding %q and not %q: %q", user, path, code, kind, status, want, wantNot, body)
	}

	return kind
}

// The run page links an elided output to the whole of it, served as text to whoever may see the run and to nobody else; an output nothing was stored for is a sentence, not an error page. Not t.Parallel(), for startWeb's reason.
func TestTheRunPageServesAKeptOutput(t *testing.T) {
	served, name, chatty := keptOutputDaemon(t)
	defer served.stopIfRunning(t)

	runPage := "/p/" + name + "/runs/" + chatty.RunID
	link := fmt.Sprintf("%s/steps/%d/output/0", runPage, chatty.StepID)

	expectGet(t, served.addr, "alex", runPage, http.StatusOK, `href="`+link+`"`, "")

	if kind := expectGet(t, served.addr, "alex", link, http.StatusOK, "\nneedle-in-the-middle\n", ""); !strings.HasPrefix(kind, "text/plain") {
		t.Errorf("the kept output is served as %s, want text/plain", kind)
	}

	// Eve is told the pipeline does not exist, by the run page and by the link alike.
	expectGet(t, served.addr, "eve", runPage, http.StatusNotFound, "", "needle")
	expectGet(t, served.addr, "eve", link, http.StatusNotFound, "", "needle")
	expectGet(t, served.addr, "alex", fmt.Sprintf("/p/other/runs/%s/steps/%d/output/0", chatty.RunID, chatty.StepID), http.StatusNotFound, "", "needle")
	expectGet(t, served.addr, "alex", fmt.Sprintf("/p/%s/runs/NOSUCHRUN0000000/steps/%d/output/0", name, chatty.StepID), http.StatusNotFound, "no such run", "")

	if kind := expectGet(t, served.addr, "alex", fmt.Sprintf("%s/steps/%d/output/7", runPage, chatty.StepID), http.StatusNotFound, "no longer stored", "<html"); !strings.HasPrefix(kind, "text/plain") {
		t.Errorf("an output nothing was stored for answers as %s, want a plain sentence", kind)
	}
}

// A step placed on a worker streams its output back to the process that runs the job, which keeps it.
func TestAPlacedStepsElidedOutputIsKept(t *testing.T) {
	requireDockerE2E(t)

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: chatty
    tags: [gpu]
    image: `+dockerE2EImage+`
    run: |
      seq 1 20000
      echo needle-in-the-middle
      seq 20001 40000
`)

	mustRun(t, "run", path, "--job", "build", "--progress", "plain", "--worker", "gpu=local:")

	whole, found := keptOf(t, path, stepOutput(t, recordedEvents(t, path), "chatty"))
	if !found || !strings.Contains(whole, "\nneedle-in-the-middle\n") {
		t.Errorf("the placed step's full output was not kept (found %v, %d bytes)", found, len(whole))
	}
}

// stdoutOf runs fn with stdout in a file: a kept output is larger than a pipe holds unread.
func stdoutOf(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}

	orig := os.Stdout
	os.Stdout = file
	err = fn()
	os.Stdout = orig

	_ = file.Close()

	data, readErr := os.ReadFile(file.Name())
	if readErr != nil {
		t.Fatal(readErr)
	}

	return string(data), err
}

// The whole output is a command away, not only a link: a script or an agent reading a failed gate has no browser.
func TestRunsOutputPrintsAStepsWholeOutput(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:`+chattyTask+`
  - task: quiet
    run: echo small
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	run := stepOutput(t, recordedEvents(t, path), "chatty").RunID

	got, err := stdoutOf(t, func() error {
		return cli.Run(append([]string{"runs", "output", run, "chatty"}, readArgs(path)...))
	})
	if err != nil || got != chattyOutput() {
		t.Errorf("runs output chatty = %d bytes, %v; want the %d the step printed", len(got), err, len(chattyOutput()))
	}

	got, err = stdoutOf(t, func() error {
		return cli.Run(append([]string{"runs", "output", run, "quiet"}, readArgs(path)...))
	})
	if err != nil || got != "small\n" {
		t.Errorf("runs output quiet = %q, %v; want the record of a step that stored nothing", got, err)
	}

	_, err = stdoutOf(t, func() error {
		return cli.Run(append([]string{"runs", "output", run, "nobody"}, readArgs(path)...))
	})
	if err == nil || !strings.Contains(err.Error(), "chatty") || !strings.Contains(err.Error(), "quiet") {
		t.Errorf("runs output nobody = %v, want the steps that did record output named", err)
	}
}

// Held a run id and not the step names: the names come back one per line, in the order each first recorded, and nothing else on stdout, so a shell can loop over them.
func TestRunsOutputWithNoStepListsTheStepsThatPrinted(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: chatty
    run: echo first
  - task: quiet
    run: echo second
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	run := stepOutput(t, recordedEvents(t, path), "chatty").RunID

	got, err := stdoutOf(t, func() error {
		return cli.Run(append([]string{"runs", "output", run}, readArgs(path)...))
	})
	if err != nil || got != "chatty\nquiet\n" {
		t.Errorf("runs output with no step = %q, %v; want the steps that recorded output, one per line", got, err)
	}
}

// An id nobody recorded is not a run that printed nothing: the sentence names the run and pipeline the way `runs steps` does, under both spellings of the command.
func TestRunsOutputWithAnUnknownRunSaysSo(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: chatty
    run: echo first
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"runs", "output", "NOSUCHRUN"}, {"runs", "output", "NOSUCHRUN", "chatty"}} {
		out, err := stdoutOf(t, func() error {
			return cli.Run(append(args, readArgs(path)...))
		})
		if err == nil {
			t.Errorf("steps %s = nil, want an unknown run refused", strings.Join(args, " "))

			continue
		}

		if got := outcome.ExitCode(err); got != outcome.ExitErrored {
			t.Errorf("steps %s exited %d, want %d: an unknown id is not a red build", strings.Join(args, " "), got, outcome.ExitErrored)
		}

		if !strings.Contains(err.Error(), `no run "NOSUCHRUN" was recorded`) {
			t.Errorf("steps %s = %v, want the run named unrecorded", strings.Join(args, " "), err)
		}

		if strings.Contains(err.Error(), "recorded no step output") {
			t.Errorf("steps %s = %v, want the unknown run said so, not a run that printed nothing", strings.Join(args, " "), err)
		}

		if out != "" {
			t.Errorf("steps %s printed %q to stdout, want nothing", strings.Join(args, " "), out)
		}
	}
}

// No step named is not a way around the refusal: a run that recorded nothing is an error under both spellings, not an empty list and exit 0.
func TestRunsOutputWithNothingRecordedIsStillAnError(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: silent
    run: "true"
`)

	err := runSilently(path)
	if err != nil {
		t.Fatal(err)
	}

	run := newestRun(t, string(cli.StatePath(path, "")), cli.PipelineName(path), "build").ID

	for _, args := range [][]string{{"runs", "output", run}, {"runs", "output", run, "silent"}} {
		_, err := stdoutOf(t, func() error {
			return cli.Run(append(args, readArgs(path)...))
		})
		if err == nil || !strings.Contains(err.Error(), "recorded no step output") {
			t.Errorf("steps %s = %v, want the run's recorded no step output", strings.Join(args, " "), err)
		}
	}
}
