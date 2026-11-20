package cli

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// Not t.Parallel(), here and below: a trim is a destroy, which forgets mcp logins in the process environment.
func TestTheOldestFinishedOneOffIsForgottenPastTheHistory(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffHistory = 1
	gate := filepath.Join(t.TempDir(), "gate")

	running := oneOff(t, held, gated(gate))
	waitRunning(t, held, running.Pipeline)

	quick := oneOff(t, held, onRoot("echo hi"))

	if status := finishedRun(t, held, quick.Pipeline); status != "succeeded" {
		t.Fatalf("the quick one-off's run %s, want succeeded", status)
	}

	time.Sleep(3 * oneOffPoll)

	if held.server.Lookup(running.Pipeline) == nil || held.server.Lookup(quick.Pipeline) == nil {
		t.Fatal("a one-off was forgotten while only one had finished, under a history of one")
	}

	err := os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	waitUnserved(t, held, quick.Pipeline)
	// Unserved is the destroy's first step, and its rows go after the loops end.
	held.reaping.Wait()

	if slices.Contains(pipelineNames(t, string(held.state)), quick.Pipeline) {
		t.Errorf("the forgotten one-off %s is still in the state database", quick.Pipeline)
	}

	if held.server.Lookup(running.Pipeline) == nil {
		t.Error("the one-off that finished last was forgotten instead of the one that finished first")
	}
}

// A restart reads a one-off back from its column: restored unpolled, its interrupted run resumed and never counted while it runs, and the bound applied once it ends.
func TestARestartedDaemonKeepsTheBoundOverItsOneOffs(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	gate := filepath.Join(t.TempDir(), "gate")

	done := oneOff(t, held, onRoot("echo hi"))
	if status := finishedRun(t, held, done.Pipeline); status != "succeeded" {
		t.Fatalf("the finished one-off's run %s, want succeeded", status)
	}

	interrupted := oneOff(t, held, gated(gate))
	waitRunning(t, held, interrupted.Pipeline)

	held.Close()

	local := scheduler.NewLocalRunner(nil, pipeline.Run{}, 1)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	restarted := newDaemon(t.Context(), server, local, held.state, held.exec, HistoryFlags{}, time.Hour)
	restarted.oneOffHistory = 1
	t.Cleanup(restarted.Close)

	err = restarted.load(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	waitRunning(t, restarted, interrupted.Pipeline)
	time.Sleep(3 * oneOffPoll)

	if server.Lookup(done.Pipeline) == nil {
		t.Fatal("a restart forgot the one finished one-off while the other still ran")
	}

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	waitUnserved(t, restarted, done.Pipeline)

	if server.Lookup(interrupted.Pipeline) == nil {
		t.Error("the resumed one-off, the newest to finish, was forgotten")
	}
}

// A failed queue whose undo cannot delete leaves a one-off served with nothing queued, as a crash between the record and the queue does for a restart; it counts as the oldest finished, and goes first.
func TestAOneOffWithNothingQueuedIsForgottenFirst(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffHistory = 1

	kept := oneOff(t, held, onRoot("true"))
	if status := finishedRun(t, held, kept.Pipeline); status != "succeeded" {
		t.Fatalf("the one-off's run %s, want succeeded", status)
	}

	var faulty atomic.Bool
	faulty.Store(true)

	deleted := make(chan struct{}, 1)
	held.wrapStore = func(st store.Store) store.Store {
		return neverQueued{Store: st, faulty: &faulty, deleted: deleted}
	}

	_, err := held.OneOff(t.Context(), web.OneOffRequest{Source: onRoot("true"), Job: "build"})
	if err == nil {
		t.Fatal("a one-off whose job could not be queued was accepted")
	}

	held.mu.Lock()
	names := slices.DeleteFunc(slices.Collect(maps.Keys(held.served)), func(name string) bool { return name == kept.Pipeline })
	restoredOneOff := len(names) == 1 && held.served[names[0]].oneOff
	held.mu.Unlock()

	if !restoredOneOff {
		t.Fatalf("served %v besides %s, want the one-off the failed undo restored, still a one-off", names, kept.Pipeline)
	}

	faulty.Store(false)

	select {
	case <-deleted:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s, served with nothing queued, was never forgotten", names[0])
	}

	held.reaping.Wait()

	if held.server.Lookup(kept.Pipeline) == nil {
		t.Error("the one-off that ran was forgotten instead of the one with nothing queued")
	}
}

func gated(gate string) string {
	return onRoot("while [ ! -e " + strconv.Quote(gate) + " ]; do sleep 0.02; done")
}

func waitRunning(t *testing.T, held *daemon, name string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for len(held.runner.Running(name)) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s never started", name)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

type neverQueued struct {
	store.Store
	faulty  *atomic.Bool
	deleted chan<- struct{}
}

func (s neverQueued) EnqueueManualJob(ctx context.Context, jobName, reason, by string) error {
	if s.faulty.Load() {
		return errors.New("queue on fire")
	}

	return s.Store.EnqueueManualJob(ctx, jobName, reason, by) //nolint:wrapcheck // test double
}

func (s neverQueued) Delete(ctx context.Context) error {
	if s.faulty.Load() {
		return errors.New("disk on fire")
	}

	err := s.Store.Delete(ctx)
	if err == nil {
		select {
		case s.deleted <- struct{}{}:
		default:
		}
	}

	return err //nolint:wrapcheck // test double
}

// The prefix keeps a minted name from ever being one somebody set, so nothing else may take it on.
func TestTheOneOffPrefixIsTheDaemonsOwn(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)

	_, err := held.Set(t.Context(), oneOffPrefix+"mine", web.SetRequest{Source: onRoot("true")})
	if !errors.Is(err, web.ErrRefused) {
		t.Errorf("a set under the one-off prefix = %v, want refused", err)
	}

	setPipeline(t, held, "app", onRoot("true"))

	err = held.Rename(t.Context(), "app", oneOffPrefix+"app")
	if !errors.Is(err, web.ErrRefused) {
		t.Errorf("a rename onto the one-off prefix = %v, want refused", err)
	}

	_, err = held.OneOff(t.Context(), web.OneOffRequest{Source: onRoot("true"), Job: "nosuch"})
	if !errors.Is(err, web.ErrRefused) {
		t.Errorf("a one-off of a job the pipeline does not have = %v, want refused", err)
	}
}

func oneOff(t *testing.T, held *daemon, source string) web.OneOffResult {
	t.Helper()

	created, err := held.OneOff(t.Context(), web.OneOffRequest{Source: source, Job: "build"})
	if err != nil {
		t.Fatalf("one-off: %v", err)
	}

	if !isOneOff(created.Pipeline) {
		t.Fatalf("the one-off was named %q, without the prefix a restart knows it by", created.Pipeline)
	}

	return created
}

func waitUnserved(t *testing.T, held *daemon, name string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for held.server.Lookup(name) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("%s was never reaped", name)
		}

		time.Sleep(20 * time.Millisecond)
	}
}
