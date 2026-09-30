package e2e

// `steps web` as the daemon, end to end through the CLI.
//
// The UI's own pages are covered by e2e_web_test.go; this file is about the
// half that has nothing to do with HTML — whether the served process CHECKS
// trigger: true resources, and how it behaves when something else already is.
//
// The command blocks, so every test starts it in the background, waits until
// it actually answers, and stops it with the SIGINT a person would press —
// which is also the only thing that exercises its shutdown path.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// webProcess is a backgrounded `steps web` and the address it serves on.
type webProcess struct {
	addr string
	// state is the database this daemon holds its pipelines in, so a test can
	// read back what it recorded without deriving a path from a file.
	state string
	done  chan error
	// stopped guards the second stop. A stop SIGNALS THIS PROCESS, so calling
	// it once nothing is trapping the signal kills the test binary outright —
	// which is what a deferred cleanup after an explicit stop would do.
	stopped bool
}

// startWeb launches the command on a free loopback port and returns once it
// answers, so a test never signals a process that has not installed its
// signal handler yet.
func startWeb(t *testing.T, args ...string) *webProcess {
	t.Helper()

	served := &webProcess{addr: freeAddr(t), done: make(chan error, 1)}
	argv := append([]string{"web", "--listen", served.addr}, args...)

	go func() { served.done <- cli.Run(argv) }()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		// Any answer proves the listener is up; which status it is belongs to
		// the page tests, not this one. The timeout matters: a handler that
		// accepts and then hangs would otherwise park this loop past its own
		// deadline and fail as a suite timeout instead of a message.
		resp, err := probe(t, served.addr)
		if err == nil {
			_ = resp.Body.Close()

			return served
		}

		select {
		case exited := <-served.done:
			t.Fatalf("web exited before it served: %v", exited)
		default:
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("web never answered on %s", served.addr)

	return nil
}

// probe is one bounded GET against the served address.
//
// Keep-alives off, and that is not tidiness: a pooled connection is one
// http.Server.Shutdown waits the full grace period for, so every daemon a test
// stops would cost five seconds — which is most of what the end-to-end suite's
// wall clock became once a poll was a daemon rather than an invocation.
func probe(t *testing.T, addr string) (*http.Response, error) {
	t.Helper()

	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	return client.Do(req) //nolint:wrapcheck // the caller only asks whether it answered
}

// stop signals the whole test binary, since the served command is this
// process — which is only safe while every test that backgrounds cli.Run is
// serial. If one of them ever takes t.Parallel(), this cancels its context
// too, and the failure surfaces over there as a flake.
//
// The already-exited check is not politeness: signal delivery is untrapped
// once a command finishes (main.go's withSignalCancel stops it on the way
// out), so signalling a dead command kills the test binary outright.
// stopIfRunning is stop for a deferred cleanup that may run after an explicit stop.
func (w *webProcess) stopIfRunning(t *testing.T) {
	t.Helper()

	if w.stopped {
		return
	}

	w.stop(t)
}

func (w *webProcess) stop(t *testing.T) {
	t.Helper()

	select {
	case exited := <-w.done:
		t.Fatalf("web exited on its own before it was stopped: %v", exited)
	default:
	}

	w.stopped = true

	err := syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	if err != nil {
		t.Fatalf("could not signal the web process: %v", err)
	}

	select {
	case exited := <-w.done:
		if exited != nil {
			t.Errorf("web exited with %v, want a clean shutdown", exited)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("web did not shut down after SIGINT")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := listener.Addr().String()
	_ = listener.Close()

	return addr
}

// startWebFor is the ordinary shape of an end-to-end test now: a daemon on its own state database, with one pipeline set into it.
func startWebFor(t *testing.T, path string, args ...string) *webProcess {
	t.Helper()

	skip := false

	kept := make([]string, 0, len(args))

	for _, arg := range args {
		// --skip-set starts the daemon without setting anything, for a test whose subject is the set itself.
		if arg == "--skip-set" {
			skip = true

			continue
		}

		kept = append(kept, arg)
	}

	state := filepath.Join(filepath.Dir(path), "daemon.db")

	served := startWeb(t, append([]string{"--db", state}, kept...)...)
	served.state = state

	if !skip {
		served.set(t, cli.PipelineName(path), path)
	}

	return served
}

// settlePolls is how many whole polls must land after settle starts before an
// idle queue counts. A poll counts once EVERY resource has been checked since
// the last one counted: the poller runs one poll at a time, checking each
// resource and then enqueueing, so a full set of newer checks means every
// earlier poll has finished enqueueing. Two, because the first is the poll that
// saw what the test wrote, and only the one after it proves its enqueue landed.
const settlePolls = 2

// settle is what `steps web --once` returning used to mean: one whole
// poll-and-drain cycle, finished.
//
// Both halves are load-bearing. Waiting only for an idle queue answers
// immediately — before the poll that would fill it has run — so it waits for
// checks to LAND after this call started, which is what proves the daemon has
// seen whatever the test just wrote. Then it waits for the drain behind them.
//
// Counted per whole poll, never per check or per tick: a poll records its
// checks BEFORE it enqueues, so a queue read between the two is empty, and
// counting ticks let a loaded machine stretch that gap past the quiet window —
// the cold-start build then landed after the test had cleared its output.
//
// ONE handle for the whole wait, deliberately: opening a sqlite database is
// the expensive part of a probe, and at two opens per 60ms tick this put the
// end-to-end suite past its own timeout under -race.
func settle(t *testing.T, state, name string, resources ...string) {
	t.Helper()

	st := waitForStore(t, state, name)
	defer func() { _ = st.Close() }()

	seen := lastCheck(t, st, resources)
	deadline := time.Now().Add(60 * time.Second)
	polls := 0

	for time.Now().Before(deadline) {
		if oldestCheck(t, st, resources).After(seen) {
			polls++
			seen = lastCheck(t, st, resources)
		}

		if polls >= settlePolls && queueIsIdle(t, st) {
			return
		}

		time.Sleep(60 * time.Millisecond)
	}

	t.Fatalf("the daemon never completed a poll-and-drain cycle for %s", name)
}

// waitForStore opens the daemon's database once the pipeline set into it is
// actually there, which a set has to land before.
func waitForStore(t *testing.T, state, name string) store.Store {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		st, err := sqlite.OpenExisting(state, name)
		if err == nil {
			return st
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("the daemon never recorded a pipeline called %s in %s", name, state)

	return nil
}

// lastCheck is the newest moment any of these resources was checked, zero
// until the daemon has checked one.
func lastCheck(t *testing.T, st store.Store, resources []string) time.Time {
	t.Helper()

	var newest time.Time

	for _, resource := range resources {
		checked, found, err := st.LastChecked(t.Context(), resource)
		if err != nil {
			t.Fatalf("LastChecked(%s): %v", resource, err)
		}

		if found && checked.CheckedAt.After(newest) {
			newest = checked.CheckedAt
		}
	}

	return newest
}

// oldestCheck is when the least recently checked of resources was last checked; zero while any never has been.
func oldestCheck(t *testing.T, st store.Store, resources []string) time.Time {
	t.Helper()

	var oldest time.Time

	for i, resource := range resources {
		checked, found, err := st.LastChecked(t.Context(), resource)
		if err != nil {
			t.Fatalf("LastChecked(%s): %v", resource, err)
		}

		if !found {
			return time.Time{}
		}

		if i == 0 || checked.CheckedAt.Before(oldest) {
			oldest = checked.CheckedAt
		}
	}

	return oldest
}

// queueIsIdle reports a pipeline with nothing pending and nothing running.
func queueIsIdle(t *testing.T, st store.Store) bool {
	t.Helper()

	rows, err := st.ListTriggerQueue(t.Context(), 50)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.Status == "pending" || row.Status == "running" {
			return false
		}
	}

	return true
}

// queueHasFailure reports a pipeline any of whose queued jobs failed, which is where a daemon records what an exit code used to say.
func queueHasFailure(t *testing.T, state, name string) bool {
	t.Helper()

	st, err := sqlite.OpenExisting(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.ListTriggerQueue(t.Context(), 50)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.Status == "failed" {
			return true
		}
	}

	return false
}

// enqueueDirectly puts a row on a pipeline's queue without going through the
// daemon, which is how a test arranges work that was queued before a pause.
func enqueueDirectly(t *testing.T, state, name, job string) {
	t.Helper()

	st, err := sqlite.OpenExisting(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	err = st.EnqueueJob(t.Context(), job, "test")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}
}

// queueHasPending reports a row still waiting to be claimed.
func queueHasPending(t *testing.T, state, name string) bool {
	t.Helper()

	st, err := sqlite.OpenExisting(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.ListTriggerQueue(t.Context(), 50)
	if err != nil {
		t.Fatalf("ListTriggerQueue: %v", err)
	}

	for _, row := range rows {
		if row.Status == "pending" {
			return true
		}
	}

	return false
}

// waitForDid blocks until the job has processed exactly these versions.
func waitForDid(t *testing.T, fixture *watchFixture, want ...string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	wanted := strings.Join(want, " ")

	for time.Now().Before(deadline) {
		if strings.Join(fixture.did(t), " ") == wanted {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("job processed %v, want %v", fixture.did(t), want)
}

// TestWebPollsTriggerResourcesByDefault is the whole point of the flag's
// default: one command both notices a new version and builds it, because
// "serving the UI" and "nothing is watching" was the surprise worth removing.
func TestWebPollsTriggerResourcesByDefault(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	served := startWebFor(t, fixture.pipeline, "--interval", "200ms")
	defer served.stop(t)

	// The first poll is a cold start, which builds the newest version it
	// finds — so this alone proves the served process polled.
	waitForDid(t, fixture, "1")

	// And it keeps polling: a later arrival builds too.
	fixture.items(t, 2)
	waitForDid(t, fixture, "1", "2")
}

// SIGINT mid-deploy: a build of a job that did not say interruptible: true finishes rather than being cut off and re-run by the next start — and the daemon's half of that, handing the runner the signal's context, is a seam no package test reaches.
func TestWebShutdownWaitsForANonInterruptibleBuild(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	finished := filepath.Join(dir, "finished")

	path := writePipeline(t, dir, `
jobs:
- name: deploy
  plan:
  - task: apply
    inputs: []
    run: |
      touch `+started+`
      sleep 2
      touch `+finished+`
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "deploy")
	waitForFile(t, started)

	served.stop(t)

	if !fileExists(finished) {
		t.Error("the deploy was cut off by the shutdown: a job that did not opt into interruptible: must be allowed to finish")
	}

	if got := queueStatuses(t, served.state, name); got != "deploy:succeeded" {
		t.Errorf("queue = %s, want the build finished and recorded: left running, the next start re-runs a deploy that already happened", got)
	}
}

// newWatchFixtureIn writes a fixture into a directory that may already hold
// another one, which is how pipelines actually sit next to each other — one
// daemon serving several is one repo folder, not three.
func newWatchFixtureIn(t *testing.T, dir, name string) *watchFixture {
	t.Helper()

	fixture := &watchFixture{
		dir:       dir,
		pipeline:  filepath.Join(dir, name+".yml"),
		feed:      filepath.Join(dir, name+"-feed.txt"),
		processed: filepath.Join(dir, name+"-processed.txt"),
		db:        filepath.Join(dir, "daemon.db"),
		name:      name,
		resources: []string{"items"},
	}

	body := strings.NewReplacer("FEED", fixture.feed, "PROCESSED", fixture.processed).Replace(cursorFeed)

	err := os.WriteFile(fixture.pipeline, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return fixture
}

// TestStatePathIsPerPipelineFile pins the DEFAULT the file-driven commands
// rest on. Keyed by directory, two pipelines in one folder would share a
// database by accident of layout rather than because anyone asked.
func TestStatePathIsPerPipelineFile(t *testing.T) {
	first := string(cli.StatePath("/srv/pipelines/app.yml", ""))
	second := string(cli.StatePath("/srv/pipelines/infra.yml", ""))

	if first == second {
		t.Fatalf("app.yml and infra.yml share %q", first)
	}

	if filepath.Dir(first) != "/srv/pipelines/.steps" {
		t.Errorf("state moved out from under .steps/: %q", first)
	}

	// --db overrides both, which is the whole feature.
	if got := string(cli.StatePath("/srv/pipelines/app.yml", "/var/lib/steps.db")); got != "/var/lib/steps.db" {
		t.Errorf("--db was ignored: %q", got)
	}
}

// TestDaemonStatePathHasNoFileToDeriveFrom: a daemon is handed pipelines by
// name, so its database cannot be named after a YAML — and the read commands
// have to look in the same place or they answer about nothing.
func TestDaemonStatePathHasNoFileToDeriveFrom(t *testing.T) {
	if got := string(cli.DaemonStatePath("")); got != cli.DefaultDaemonState {
		t.Errorf("the daemon's default state is %q, want %q", got, cli.DefaultDaemonState)
	}

	if got := string(cli.DaemonStatePath("/var/lib/steps.db")); got != "/var/lib/steps.db" {
		t.Errorf("--db was ignored: %q", got)
	}
}

// TestWebPollsEveryPipelineItServes is the multi-pipeline case the routing
// already implies, in the layout people actually use: both pipelines in ONE
// directory. Each gets its own state.db, its own poller, its own watch lock
// and its own store handle — so one pipeline's version change builds that
// pipeline's job, and not the other's.
func TestWebPollsEveryPipelineItServes(t *testing.T) {
	dir := t.TempDir()
	first := newWatchFixtureIn(t, dir, "app")
	second := newWatchFixtureIn(t, dir, "infra")

	first.items(t, 1)
	second.items(t, 1)

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--interval", "200ms")
	defer served.stop(t)

	served.set(t, "app", first.pipeline)
	served.set(t, "infra", second.pipeline)

	waitForDid(t, first, "1")
	waitForDid(t, second, "1")

	first.items(t, 2)
	second.items(t, 2)

	waitForDid(t, first, "1", "2")
	waitForDid(t, second, "1", "2")
}

// TestWebRejectsANonPositiveInterval: a value that means "poll never" while
// the process reports itself as serving normally is a daemon that looks alive
// and notices nothing — the exact confusion this command polls by default to
// remove.
func TestWebRejectsANonPositiveInterval(t *testing.T) {
	// Port 1 is unbindable as an ordinary user, so a regression fails in a
	// second instead of serving forever. Mutation testing is what made this
	// non-optional: with the guard weakened, --interval 0 was ACCEPTED and
	// this test blocked until the run's own timeout — one mutant stalled a
	// seven-hour run for an hour and forty minutes.
	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--interval", "0"})
	if err == nil {
		t.Fatal("--interval 0 was accepted; it silently disables polling")
	}

	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error %q does not name the flag that is wrong", err)
	}
}

// TestWebRejectsDeliver: web embeds the run flags, and a --deliver it parsed and never recorded would read as a delivery made.
func TestWebRejectsDeliver(t *testing.T) {
	err := cli.Run([]string{"web", "--listen", "127.0.0.1:1", "--deliver", "push=push.http"})
	if err == nil || !strings.Contains(err.Error(), "--deliver") {
		t.Fatalf("err = %v, want --deliver refused", err)
	}
}

// readArgs names the pipeline a file-driven `steps run` recorded, for the read
// commands — which take a name and a database, because a pipeline set into a
// daemon has no file here to derive either from.
func readArgs(path string) []string {
	return []string{"-p", cli.PipelineName(path), "--db", string(cli.StatePath(path, ""))}
}

// contextLogKeys are the attributes events.LogHandler stamps from the context.
var contextLogKeys = []string{"pipeline", "revision", "run", "job", "step", "resource", "index", "kind"}

// TestDaemonLogLinesNameTheirPipeline: a daemon serving two pipelines logs
// each line with the pipeline, revision, and job/resource/run it concerns.
// Two pipelines are what make the attribution provable rather than
// coincidental — with one, every line naming it would be a guess that
// happened to be right.
func TestDaemonLogLinesNameTheirPipeline(t *testing.T) {
	dir := t.TempDir()
	fixtures := []*watchFixture{newWatchFixtureIn(t, dir, "app"), newWatchFixtureIn(t, dir, "infra")}

	logs := captureStderr(t)
	state := filepath.Join(dir, "daemon.db")
	served := startWeb(t, "--db", state, "--interval", "200ms")
	served.state = state

	defer served.stopIfRunning(t)

	for _, fixture := range fixtures {
		fixture.items(t, 1)
		served.set(t, fixture.name, fixture.pipeline)
	}

	shas := map[string]string{}

	for _, fixture := range fixtures {
		waitForDid(t, fixture, "1")

		st := waitForStore(t, state, fixture.name)

		revision, found, err := st.CurrentRevision(t.Context())
		_ = st.Close()

		if err != nil || !found || revision.SHA == "" {
			t.Fatalf("%s: current revision %q found=%v err=%v", fixture.name, revision.SHA, found, err)
		}

		shas[fixture.name] = revision.SHA
	}

	for name := range shas {
		// The drain logs web.job.done after the task that wrote processed.txt returns.
		waitForLogLine(t, logs, "web.job.done", "pipeline", name)
	}

	served.stop(t)

	out := logs()

	for name, sha := range shas {
		assertDaemonLines(t, out, name, sha)
	}

	assertNoDuplicateKeys(t, out)
}

// assertNoDuplicateKeys: a key the call site passed beats the context's, so
// no line says the same thing twice.
func assertNoDuplicateKeys(t *testing.T, out string) {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		for _, key := range contextLogKeys {
			if strings.Count(" "+line, " "+key+"=") > 1 {
				t.Errorf("%s= appears twice: %s", key, line)
			}
		}
	}
}

// assertDaemonLines checks one pipeline's poll and build lines name it, its
// revision, and the resource/job/run they concern.
func assertDaemonLines(t *testing.T, out, name, sha string) {
	t.Helper()

	// A check with no run= is the poll's, not a get inside a build — which gets its pipeline from RunJob instead.
	checked := findLogLineWith(t, out, "resource.checked", func(line string) bool {
		return logField(line, "pipeline") == name && logField(line, "run") == ""
	})
	if logField(checked, "resource") != "items" || logField(checked, "revision") != sha {
		t.Errorf("%s: poll's resource.checked lacks resource=items revision=%s: %s", name, sha, checked)
	}

	done := findLogLineWith(t, out, "job.done", func(line string) bool { return logField(line, "pipeline") == name })
	if logField(done, "job") != "build" || logField(done, "run") == "" || logField(done, "revision") != sha {
		t.Errorf("%s: job.done lacks job=build, run= or revision=%s: %s", name, sha, done)
	}

	webDone := findLogLineWith(t, out, "web.job.done", func(line string) bool { return logField(line, "pipeline") == name })
	if logField(webDone, "revision") != sha {
		t.Errorf("%s: web.job.done lacks revision=%s: %s", name, sha, webDone)
	}

	// Identity first, ahead of what the call site said.
	run := findLogLineWith(t, out, "job.run", func(line string) bool { return logField(line, "pipeline") == name })
	if strings.Index(run, "pipeline=") > strings.Index(run, "steps=") {
		t.Errorf("%s: job.run puts its own attributes before pipeline=: %s", name, run)
	}
}

// findLogLineWith is findLogLine for the first line with this message that
// also satisfies match.
func findLogLineWith(t *testing.T, out, msg string, match func(string) bool) string {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		if slices.Contains(strings.Fields(line), msg) && match(line) {
			return line
		}
	}

	t.Fatalf("no %q log line matched in: %s", msg, out)

	return ""
}

// waitForLogLine blocks until a line with this message carries key=value.
func waitForLogLine(t *testing.T, logs func() string, msg, key, value string) {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for time.Now().Before(deadline) {
		for line := range strings.SplitSeq(logs(), "\n") {
			if slices.Contains(strings.Fields(line), msg) && logField(line, key) == value {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("no %s line with %s=%s: %s", msg, key, value, logs())
}
