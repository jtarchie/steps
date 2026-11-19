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

// Not t.Parallel(), here and below: a reap is a destroy, which forgets mcp logins in the process environment.
func TestAOneOffIsForgottenOnceItsRunIsOverAndTheGraceHasPassed(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffGrace = 0
	gate := filepath.Join(t.TempDir(), "gate")

	created := oneOff(t, held, onRoot("while [ ! -e "+strconv.Quote(gate)+" ]; do sleep 0.02; done"))

	time.Sleep(3 * oneOffPoll)

	if held.server.Lookup(created.Pipeline) == nil {
		t.Fatal("a one-off was reaped while its run was still going")
	}

	err := os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if status := finishedRun(t, held, created.Pipeline); status != "succeeded" {
		t.Fatalf("the one-off's run %s, want succeeded", status)
	}

	waitUnserved(t, held, created.Pipeline)
	// Unserved is the destroy's first step, and its rows go after the loops end.
	held.reaping.Wait()

	if slices.Contains(pipelineNames(t, string(held.state)), created.Pipeline) {
		t.Errorf("the reaped one-off %s is still in the state database", created.Pipeline)
	}
}

// The grace is what lets a dropped terminal reconnect and a browser read the end, so a finished one-off is served through it.
func TestAFinishedOneOffIsServedThroughItsGrace(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffGrace = time.Hour

	created := oneOff(t, held, onRoot("echo hi"))

	if status := finishedRun(t, held, created.Pipeline); status != "succeeded" {
		t.Fatalf("the one-off's run %s, want succeeded", status)
	}

	time.Sleep(3 * oneOffPoll)

	if held.server.Lookup(created.Pipeline) == nil {
		t.Error("a one-off was reaped inside its grace")
	}
}

// A restart reads a one-off back from its name alone: restored, its interrupted run finished, and then reaped like any other.
func TestARestartedDaemonStillReapsAOneOff(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	gate := filepath.Join(t.TempDir(), "gate")

	created := oneOff(t, held, onRoot("while [ ! -e "+strconv.Quote(gate)+" ]; do sleep 0.02; done"))

	deadline := time.Now().Add(30 * time.Second)
	for len(held.runner.Running(created.Pipeline)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the one-off never started")
		}

		time.Sleep(10 * time.Millisecond)
	}

	held.Close()

	local := scheduler.NewLocalRunner(nil, pipeline.Run{}, 1)

	server, err := web.New(nil, local)
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}

	restarted := newDaemon(t.Context(), server, local, held.state, held.exec, HistoryFlags{}, time.Hour)
	restarted.oneOffGrace = 0
	t.Cleanup(restarted.Close)

	err = restarted.load(t.Context())
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if server.Lookup(created.Pipeline) == nil {
		t.Fatal("a restart did not serve the interrupted one-off")
	}

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	waitUnserved(t, restarted, created.Pipeline)
}

// A failed queue whose undo cannot delete leaves a one-off served with nothing queued, as a crash between the record and the queue does for a restart; it is reaped all the same.
func TestAOneOffWithNothingQueuedIsStillReaped(t *testing.T) {
	isolateLogins(t)

	held := servingDaemon(t)
	held.oneOffGrace = 0

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
	names := slices.Collect(maps.Keys(held.served))
	held.mu.Unlock()

	if len(names) != 1 || !isOneOff(names[0]) {
		t.Fatalf("served %v, want the one-off the failed undo restored", names)
	}

	faulty.Store(false)

	select {
	case <-deleted:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s, served with nothing queued, was never reaped", names[0])
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

// The prefix is how a restart knows a one-off, so nothing else may take it on.
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
