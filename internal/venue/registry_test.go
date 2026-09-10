package venue

// Scopes sharing one Registry, against the fake clouds.

import (
	"context"
	"testing"
	"time"
)

func boxWorker(t *testing.T, raw string) map[string]Worker {
	t.Helper()

	worker, err := ParseWorker(raw)
	if err != nil {
		t.Fatalf("ParseWorker(%q): %v", raw, err)
	}

	return map[string]Worker{"box": worker}
}

func mustResolve(t *testing.T, leases *Leases) Worker {
	t.Helper()

	worker, err := leases.Resolve(context.Background(), "box")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	return worker
}

func mustRelease(t *testing.T, leases *Leases) {
	t.Helper()

	err := leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}
}

func (f *fakeEC2) counts() (started, stopped, terminated int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.started), len(f.stopped), len(f.terminated)
}

func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("never happened: %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// The collision this registry exists for: one job's end used to stop the instance another job was mid-step on.
func TestRegistryStopsAMachineOnlyAfterItsLastUser(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	workers := boxWorker(t, "aws://stopped/i-0abc123def456789")
	registry := NewRegistry()
	first := registry.Leases(workers)
	second := registry.Leases(workers)

	mustResolve(t, first)
	mustResolve(t, second)
	mustRelease(t, second)

	if started, stopped, _ := fake.counts(); started != 1 || stopped != 0 {
		t.Fatalf("after one of two users left: %d starts, %d stops — want one start and the machine still up", started, stopped)
	}

	mustRelease(t, first)

	if _, stopped, _ := fake.counts(); stopped != 1 {
		t.Fatalf("after the last user left: %d stops, want 1", stopped)
	}
}

// The window is held on the registry's timer: a poll that waited it out would drop its next tick.
func TestRegistryKeepsAnIdleMachineForTheNextUser(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	workers := boxWorker(t, "aws://stopped/i-0abc123def456789?idle=300ms")
	registry := NewRegistry()
	poll := registry.Leases(workers)

	mustResolve(t, poll)

	started := time.Now()

	mustRelease(t, poll)

	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Errorf("releasing took %s: the scope waited out ?idle= itself", elapsed)
	}

	next := registry.Leases(workers)
	mustResolve(t, next)
	mustRelease(t, next)

	if starts, stops, _ := fake.counts(); starts != 1 || stops != 0 {
		t.Fatalf("inside the window: %d starts, %d stops — want the next user to find the machine warm", starts, stops)
	}

	eventually(t, "the machine parked once its window ran out", func() bool {
		_, stopped, _ := fake.counts()

		return stopped == 1
	})
}

// Shutdown is when a machine kept warm for a user that will never come has to go back, rather than bill until somebody notices.
func TestRegistryCloseGivesBackWhatItKeptWarm(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	registry := NewRegistry()
	leases := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789?idle=1h"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	closed := make(chan error, 1)

	go func() { closed <- registry.Close(context.Background()) }()

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned: it waited out the idle window rather than giving the machine back")
	}

	if _, stopped, _ := fake.counts(); stopped != 1 {
		t.Fatalf("after Close: %d stops, want the warm machine parked", stopped)
	}
}

// Sticky past its own users — kept "warm" like a machine — one exhausted spot pool would refuse every later job for the life of the daemon.
func TestRegistryForgetsAFailedAcquisition(t *testing.T) {
	fake := &fakeEC2{fleetEmpty: true}
	seamEC2(t, fake)

	workers := boxWorker(t, "aws://launch/lt-0def4567890abcde?idle=1h")
	registry := NewRegistry()

	t.Cleanup(func() { _ = registry.Close(context.Background()) })

	first := registry.Leases(workers)

	_, err := first.Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("an empty pool produced a machine")
	}

	mustRelease(t, first)

	fake.mu.Lock()
	fake.fleetEmpty = false
	fake.mu.Unlock()

	second := registry.Leases(workers)
	mustResolve(t, second)
	mustRelease(t, second)
}

// Whoever watches the machine die retires it for every scope, and it still goes back when the last scope that used it ends.
func TestRegistryRetiresAnEvictedMachineForEveryUser(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	workers := boxWorker(t, "aws://launch/lt-0def4567890abcde")
	registry := NewRegistry()
	watcher := registry.Leases(workers)
	sibling := registry.Leases(workers)

	dead := mustResolve(t, watcher)
	mustResolve(t, sibling)

	watcher.Abandon("box", dead.URL)
	mustResolve(t, sibling)

	fake.mu.Lock()
	fleets := len(fake.fleets)
	fake.mu.Unlock()

	if fleets != 2 {
		t.Fatalf("fleets = %d, want the sibling's next step on a fresh machine", fleets)
	}

	mustRelease(t, watcher)

	if _, _, terminated := fake.counts(); terminated != 0 {
		t.Fatalf("%d terminates while the sibling still counted toward the dead machine", terminated)
	}

	mustRelease(t, sibling)

	if _, _, terminated := fake.counts(); terminated != 2 {
		t.Fatalf("%d terminates, want the dead machine and its replacement both given back", terminated)
	}
}

// A parked instance found running may be anybody's work in progress, so steps stops only what it started.
func TestParkedRungLeavesAMachineItDidNotStartRunning(t *testing.T) {
	fake := &fakeEC2{alreadyRunning: true}
	seamEC2(t, fake)

	leases := NewLeases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

	resolved := mustResolve(t, leases)
	mustRelease(t, leases)

	if resolved.Instance != "i-0abc123def456789" {
		t.Errorf("resolved = %+v, want the running instance", resolved)
	}

	if started, stopped, _ := fake.counts(); started != 0 || stopped != 0 {
		t.Errorf("%d starts, %d stops on a machine steps did not start — want neither", started, stopped)
	}
}

func TestGCPParkedRungLeavesAMachineItDidNotStartRunning(t *testing.T) {
	fake := &fakeGCE{statuses: []string{"RUNNING"}}
	seamGCP(t, fake, nil)

	leases := NewLeases(boxWorker(t, "gcp://stopped/worker-1?project=test-project&zone=us-central1-a"))

	mustResolve(t, leases)
	mustRelease(t, leases)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.starts) != 0 || len(fake.stops) != 0 {
		t.Errorf("starts = %v, stops = %v on a machine steps did not start — want neither", fake.starts, fake.stops)
	}
}
