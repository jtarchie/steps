package cli

import (
	"errors"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/web"
)

// fakeEC2Endpoint points the AWS SDK at an EC2 with nothing running, for the sweep a daemon's load runs.
func fakeEC2Endpoint(t *testing.T) {
	t.Helper()

	server := httptest.NewServer(&recordingEC2{})
	t.Cleanup(server.Close)

	t.Setenv("AWS_ENDPOINT_URL_EC2", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "fake")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
}

// twoPools is a job that takes a machine from a and from b.
const twoPools = `jobs:
- name: build
  plan:
  - {task: one, tags: [a], image: alpine:3, run: "true"}
  - {task: two, tags: [b], image: alpine:3, run: "true"}
`

func poolDaemon(t *testing.T, state State, workers map[string]string) *daemon {
	t.Helper()

	ctx, err := pipeline.WithWorkers(t.Context(), workers)
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	local := scheduler.NewLocalRunner(nil, pipeline.Run{}, 1)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	held := newDaemon(ctx, server, local, state, ExecFlags{}, HistoryFlags{}, time.Hour)
	t.Cleanup(held.Close)

	return held
}

// The deadlock refusal is decided on the daemon, against its own --worker flags, and read at the terminal that ran steps pipeline set.
func TestASetTouchingTwoCappedPoolsIsRefusedAtTheTerminal(t *testing.T) {
	t.Parallel()

	held := poolDaemon(t, State(filepath.Join(t.TempDir(), "steps.db")), map[string]string{
		"a": "aws://launch/lt-0aaa4567890abcde?exclusive&max=2",
		"b": "gcp://launch/tmpl?zone=us-central1-a&exclusive&max=1",
	})
	held.server.SetManager(held)

	daemon := httptest.NewServer(held.server.Handler())
	t.Cleanup(daemon.Close)

	_, err := newDaemonClient(daemon.URL).set("app", web.SetRequest{Source: twoPools})
	if err == nil {
		t.Fatal("a job touching two capped pools was accepted")
	}

	if msg := err.Error(); !strings.Contains(msg, web.ErrRefused.Error()) || !strings.Contains(msg, `job "build"`) || !strings.Contains(msg, "a, b") {
		t.Errorf("the terminal was told %q; want the job and both tags named", msg)
	}

	if held.server.Lookup("app") != nil {
		t.Error("a refused set is served")
	}
}

// The --worker flags can change across a restart: a stored pipeline that would now deadlock is held and logged, never started, and never stops the daemon. Not t.Parallel(): the sweep at load reads AWS settings from the environment.
func TestARestartUnderNewWorkersHoldsAPipelineThatWouldDeadlock(t *testing.T) {
	fakeEC2Endpoint(t)

	state := State(filepath.Join(t.TempDir(), "steps.db"))

	first := poolDaemon(t, state, map[string]string{
		"a": "aws://launch/lt-0aaa4567890abcde?exclusive&max=2",
		"b": "aws://launch/lt-0bbb4567890abcde?exclusive",
	})
	setPipeline(t, first, "deadlocks", twoPools)
	setPipeline(t, first, "plain", idlePipeline)
	first.Close()

	var logged lockedBuffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	restarted := poolDaemon(t, state, map[string]string{
		"a": "aws://launch/lt-0aaa4567890abcde?exclusive&max=2",
		"b": "aws://launch/lt-0bbb4567890abcde?exclusive&max=1",
	})

	err := restarted.load(t.Context())
	if err != nil {
		t.Fatalf("load refused the whole daemon over one pipeline: %v", err)
	}

	assertServedApartFrom(t, restarted, "plain", "deadlocks")

	broken := restarted.server.Broken()
	if len(broken) != 1 || broken[0].Name != "deadlocks" {
		t.Fatalf("broken = %+v, want deadlocks held", broken)
	}

	if reason := broken[0].Reason; !strings.Contains(reason, "a, b") || !strings.Contains(reason, "restart the daemon") {
		t.Errorf("reason = %q, want both tags and a restart suggested", reason)
	}

	if log := logged.String(); !strings.Contains(log, "web.not_serving") || !strings.Contains(log, "deadlocks") {
		t.Errorf("logged %q, want the held pipeline in the daemon's log", log)
	}

	_, err = restarted.Set(t.Context(), "deadlocks", web.SetRequest{Source: twoPools})
	if !errors.Is(err, web.ErrRefused) {
		t.Errorf("setting it again under the new flags = %v, want it refused", err)
	}
}

func assertServedApartFrom(t *testing.T, held *daemon, served, notServed string) {
	t.Helper()

	if held.server.Lookup(served) == nil {
		t.Errorf("%s is not served", served)
	}

	if held.server.Lookup(notServed) != nil {
		t.Errorf("%s is served", notServed)
	}
}
