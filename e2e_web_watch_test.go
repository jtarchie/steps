package main

// `steps web` as a watcher, end to end through the CLI.
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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// webProcess is a backgrounded `steps web` and the address it serves on.
type webProcess struct {
	addr string
	done chan error
}

// startWeb launches the command on a free loopback port and returns once it
// answers, so a test never signals a process that has not installed its
// signal handler yet.
func startWeb(t *testing.T, pipelinePaths []string, args ...string) *webProcess {
	t.Helper()

	served := &webProcess{addr: freeAddr(t), done: make(chan error, 1)}
	argv := append([]string{"web"}, pipelinePaths...)
	argv = append(argv, "--listen", served.addr)
	argv = append(argv, args...)

	go func() { served.done <- run(argv) }()

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
func probe(t *testing.T, addr string) (*http.Response, error) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatal(err)
	}

	return client.Do(req) //nolint:wrapcheck // the caller only asks whether it answered
}

// stop signals the whole test binary, since the served command is this
// process — which is only safe while every test that backgrounds run() is
// serial. If one of them ever takes t.Parallel(), this cancels its context
// too, and the failure surfaces over there as a flake.
//
// The already-exited check is not politeness: signal delivery is untrapped
// once a command finishes (main.go's withSignalCancel stops it on the way
// out), so signalling a dead command kills the test binary outright.
func (w *webProcess) stop(t *testing.T) {
	t.Helper()

	select {
	case exited := <-w.done:
		t.Fatalf("web exited on its own before it was stopped: %v", exited)
	default:
	}

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

	served := startWeb(t, []string{fixture.pipeline}, "--interval", "200ms")
	defer served.stop(t)

	// The first poll is a cold start, which builds the newest version it
	// finds — so this alone proves the served process polled.
	waitForDid(t, fixture, "1")

	// And it keeps polling: a later arrival builds too.
	fixture.items(t, 2)
	waitForDid(t, fixture, "1", "2")
}

// TestWebNoWatchLeavesPollingToWatch: --no-watch must not merely stay quiet,
// it must leave the watcher's LOCK alone, since that is what decides whether
// a `steps watch` can start alongside it.
func TestWebNoWatchLeavesPollingToWatch(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	served := startWeb(t, []string{fixture.pipeline}, "--no-watch", "--interval", "200ms")
	defer served.stop(t)

	assertWatchLockFree(t, fixture.pipeline)

	fixture.items(t, 2)
	assertNothingProcessed(t, fixture)
}

// TestWebSkipsPollingWhenAnotherWatcherHoldsTheLock: two pollers against one
// state.db claim each other's work, so the one whose job is the UI gives way
// — and keeps serving, which is the part that must not become collateral.
func TestWebSkipsPollingWhenAnotherWatcherHoldsTheLock(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	st, err := store.OpenStore(statePath(fixture.pipeline))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	release, held, err := st.AcquireWatchLock()
	if err != nil || held {
		t.Fatalf("could not take the watch lock first: held=%v err=%v", held, err)
	}
	defer release()

	served := startWeb(t, []string{fixture.pipeline}, "--interval", "200ms")
	defer served.stop(t)

	fixture.items(t, 2)
	assertNothingProcessed(t, fixture)

	// Still serving: giving up the poll is not giving up the job.
	resp, err := probe(t, served.addr)
	if err != nil {
		t.Fatalf("web stopped serving after declining to poll: %v", err)
	}

	_ = resp.Body.Close()
}

func assertWatchLockFree(t *testing.T, pipelinePath string) {
	t.Helper()

	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	release, held, err := st.AcquireWatchLock()
	if err != nil {
		t.Fatal(err)
	}

	if held {
		t.Fatal("the watch lock is held, so a steps watch could not start alongside this one")
	}

	release()
}

// assertNothingProcessed is the one time-bounded assertion here: proving a
// poll did NOT happen has no event to wait for. The window is many multiples
// of the 200ms interval every caller passes, so a poller that ran at all had
// several chances to be caught.
func assertNothingProcessed(t *testing.T, fixture *watchFixture) {
	t.Helper()

	time.Sleep(2 * time.Second)

	if did := fixture.did(t); len(did) != 0 {
		t.Errorf("job processed %v, want nothing — this process was not supposed to poll", did)
	}
}

// newWatchFixtureIn writes a fixture into a directory that may already hold
// another one, which is how pipelines actually sit next to each other —
// `steps web app.yml infra.yml nightly.yml` is one repo folder, not three.
func newWatchFixtureIn(t *testing.T, dir, name, pipelineYAML string) *watchFixture {
	t.Helper()

	fixture := &watchFixture{
		dir:       dir,
		pipeline:  filepath.Join(dir, name+".yml"),
		feed:      filepath.Join(dir, name+"-feed.txt"),
		processed: filepath.Join(dir, name+"-processed.txt"),
	}

	body := strings.NewReplacer("FEED", fixture.feed, "PROCESSED", fixture.processed).Replace(pipelineYAML)

	err := os.WriteFile(fixture.pipeline, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return fixture
}

// TestStatePathIsPerPipelineFile pins the invariant every claim about serving
// several pipelines rests on. Keyed by directory, two pipelines in one folder
// shared a database whose every row is keyed by a name they each chose
// independently — job, resource, queue row.
func TestStatePathIsPerPipelineFile(t *testing.T) {
	first := statePath("/srv/pipelines/app.yml")
	second := statePath("/srv/pipelines/infra.yml")

	if first == second {
		t.Fatalf("app.yml and infra.yml share %q", first)
	}

	if filepath.Dir(first) != "/srv/pipelines/.steps" {
		t.Errorf("state moved out from under .steps/: %q", first)
	}
}

// TestWebPollsEveryPipelineItServes is the multi-pipeline case the routing
// already implies, in the layout people actually use: both pipelines in ONE
// directory. Each gets its own state.db, its own poller, its own watch lock
// and its own store handle — so one pipeline's version change builds that
// pipeline's job, and not the other's.
func TestWebPollsEveryPipelineItServes(t *testing.T) {
	dir := t.TempDir()
	first := newWatchFixtureIn(t, dir, "app", cursorFeed)
	second := newWatchFixtureIn(t, dir, "infra", cursorFeed)

	first.items(t, 1)
	second.items(t, 1)

	served := startWeb(t, []string{first.pipeline, second.pipeline}, "--interval", "200ms")
	defer served.stop(t)

	waitForDid(t, first, "1")
	waitForDid(t, second, "1")

	first.items(t, 2)
	second.items(t, 2)

	waitForDid(t, first, "1", "2")
	waitForDid(t, second, "1", "2")
}

// TestWebLeavesAForeignWatchersClaimedJobAlone: the recovery that re-queues
// stranded work treats every running row as an abandoned leftover, which is
// only true when no other watcher is alive. Serving alongside `steps watch`
// is the pairing --no-watch exists for, so the drainer must not reach into
// that watcher's in-flight job and run it a second time.
func TestWebLeavesAForeignWatchersClaimedJobAlone(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	st, err := store.OpenStore(statePath(fixture.pipeline))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	// Pose as the `steps watch` that is already running: hold the lock, and
	// hold a job in flight.
	release, held, err := st.AcquireWatchLock()
	if err != nil || held {
		t.Fatalf("could not take the watch lock first: held=%v err=%v", held, err)
	}

	defer release()

	err = st.EnqueueJob(t.Context(), "build", "test")
	if err != nil {
		t.Fatal(err)
	}

	_, _, claimed, err := st.ClaimNextJob(t.Context())
	if err != nil || !claimed {
		t.Fatalf("could not put a job in flight: claimed=%v err=%v", claimed, err)
	}

	served := startWeb(t, []string{fixture.pipeline}, "--no-watch")
	defer served.stop(t)

	assertNothingProcessed(t, fixture)

	// Still in flight, and still the other watcher's: a reset would have made
	// it claimable here (or run it, which the line above already rules out).
	_, _, claimable, err := st.ClaimNextJob(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if claimable {
		t.Error("the running job was re-queued: web recovered a row belonging to a live watcher")
	}
}

// TestWebRejectsANonPositiveInterval: `steps watch` refuses one loudly, and a
// flag that means "poll never" while the process reports itself as serving
// normally is the same mistake with no error attached. --no-watch is the
// spelling for not polling.
func TestWebRejectsANonPositiveInterval(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)

	err := run([]string{"web", fixture.pipeline, "--listen", freeAddr(t), "--interval", "0"})
	if err == nil {
		t.Fatal("--interval 0 was accepted; it silently disables polling")
	}

	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error %q does not name the flag that is wrong", err)
	}
}
