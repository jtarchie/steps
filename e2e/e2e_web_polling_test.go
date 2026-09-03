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
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
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
// process — which is only safe while every test that backgrounds cli.Run is
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

// TestStatePathIsPerPipelineFile pins the DEFAULT every claim about serving
// several pipelines rests on. Keyed by directory, two pipelines in one folder
// would share a database by accident of layout rather than because anyone
// asked — which is what --state is for, and why it is not the default.
func TestStatePathIsPerPipelineFile(t *testing.T) {
	first := cli.StatePath("/srv/pipelines/app.yml", "")
	second := cli.StatePath("/srv/pipelines/infra.yml", "")

	if first == second {
		t.Fatalf("app.yml and infra.yml share %q", first)
	}

	if filepath.Dir(first) != "/srv/pipelines/.steps" {
		t.Errorf("state moved out from under .steps/: %q", first)
	}

	// --state overrides both, which is the whole feature.
	if got := cli.StatePath("/srv/pipelines/app.yml", "/var/lib/steps.db"); got != "/var/lib/steps.db" {
		t.Errorf("--state was ignored: %q", got)
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

// TestWebRejectsANonPositiveInterval: a value that means "poll never" while
// the process reports itself as serving normally is a daemon that looks alive
// and notices nothing — the exact confusion this command polls by default to
// remove.
func TestWebRejectsANonPositiveInterval(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)

	// Port 1 is unbindable as an ordinary user, so a regression fails in a
	// second instead of serving forever. Mutation testing is what made this
	// non-optional: with the guard weakened, --interval 0 was ACCEPTED and
	// this test blocked until the run's own timeout — one mutant stalled a
	// seven-hour run for an hour and forty minutes.
	err := cli.Run([]string{"web", fixture.pipeline, "--listen", "127.0.0.1:1", "--interval", "0"})
	if err == nil {
		t.Fatal("--interval 0 was accepted; it silently disables polling")
	}

	if !strings.Contains(err.Error(), "--interval") {
		t.Errorf("error %q does not name the flag that is wrong", err)
	}
}
