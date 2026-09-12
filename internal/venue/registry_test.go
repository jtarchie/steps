package venue

// Scopes sharing one Registry, against the fake clouds.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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

	resolved := mustResolve(t, leases)
	mustRelease(t, leases)

	if resolved.Instance != "worker-1" || resolved.Rung != RungStatic {
		t.Errorf("resolved = %+v, want worker-1 as a static worker", resolved)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.starts) != 0 || len(fake.stops) != 0 {
		t.Errorf("starts = %v, stops = %v on a machine steps did not start — want neither", fake.starts, fake.stops)
	}
}

// parkedFake is the parked rung's contract with no cloud behind it: a machine found running is used and never parked, and one found parked is started and parked again by its release, which a test can hold in flight.
type parkedFake struct {
	mu      sync.Mutex
	running bool
	starts  int
	stops   int
	// parking hears each park as it is issued, and landing holds it there until the test lets it land.
	parking chan struct{}
	landing chan struct{}
}

func (p *parkedFake) acquire(_ context.Context, worker Worker) (Worker, func(context.Context) error, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.running {
		return worker.asStatic(worker.Instance), nil, nil
	}

	p.running = true
	p.starts++

	return worker.asStatic(worker.Instance), p.park, nil
}

func (p *parkedFake) park(context.Context) error {
	if p.parking != nil {
		p.parking <- struct{}{}
	}

	if p.landing != nil {
		<-p.landing
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.running = false
	p.stops++

	return nil
}

// reclaim is the cloud stopping the machine on its own account, as a spot interruption does.
func (p *parkedFake) reclaim() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.running = false
}

func (p *parkedFake) state() (running bool, starts, stops int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.running, p.starts, p.stops
}

// holdLanding makes the fake's parks wait for the returned func, which the test's end also calls so no park outlives it.
func holdLanding(t *testing.T, fake *parkedFake) func() {
	t.Helper()

	fake.parking, fake.landing = make(chan struct{}, 1), make(chan struct{})
	land := sync.OnceFunc(func() { close(fake.landing) })
	t.Cleanup(land)

	return land
}

func usersOf(r *Registry) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	users := 0
	for _, held := range r.current {
		users += held.refs
	}

	return users
}

func within[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()

	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("never happened: %s", what)

		var zero T

		return zero
	}
}

// A resolver that gives up mid-acquisition answers only for itself: its cancellation, kept as the entry's answer, failed every scope sharing the machine for as long as any of them held it.
func TestRegistryKeepsNoAcquisitionItsResolverAbandoned(t *testing.T) {
	inFlight := make(chan struct{})

	var calls atomic.Int32

	registry := NewRegistryWith(func(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
		if calls.Add(1) == 1 {
			close(inFlight)
			<-ctx.Done()

			return Worker{}, nil, fmt.Errorf("starting %s: %w", worker.Instance, ctx.Err())
		}

		return worker.asStatic(worker.Instance), nil, nil
	})

	workers := boxWorker(t, "aws://stopped/i-0abc123def456789")
	webhook := registry.Leases(workers)
	job := registry.Leases(workers)

	ctx, cancel := context.WithCancel(context.Background())
	abandoned := make(chan error, 1)

	go func() {
		_, err := webhook.Resolve(ctx, "box")
		abandoned <- err
	}()

	within(t, "the webhook's acquisition started", inFlight)

	resolved := make(chan error, 1)

	go func() {
		_, err := job.Resolve(context.Background(), "box")
		resolved <- err
	}()

	eventually(t, "the job waits on the same machine", func() bool { return usersOf(registry) == 2 })
	cancel()

	err := within(t, "the webhook gave up", abandoned)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the webhook's resolve: %v, want its own cancellation", err)
	}

	err = within(t, "the job resolved", resolved)
	if err != nil {
		t.Fatalf("the job's resolve: %v — it was handed the webhook's cancellation", err)
	}

	if n := calls.Load(); n != 2 {
		t.Errorf("%d acquisitions, want the job acquiring for itself once the webhook gave up", n)
	}

	mustRelease(t, webhook)
	mustRelease(t, job)
}

// A park that has not landed is still the registry's business: forgotten before its Stop, a scope resolving in between found the instance running, took it with no release of its own, and then had it parked underneath it.
func TestRegistryWaitsForAParkToLandBeforeTakingTheMachine(t *testing.T) {
	fake := &parkedFake{}
	land := holdLanding(t, fake)
	registry := NewRegistryWith(fake.acquire)
	workers := boxWorker(t, "aws://stopped/i-0abc123def456789")
	first := registry.Leases(workers)

	mustResolve(t, first)

	released := make(chan error, 1)

	go func() { released <- first.ReleaseAll(context.Background()) }()

	within(t, "the park was issued", fake.parking)

	second := registry.Leases(workers)
	resolved := make(chan error, 1)

	go func() {
		_, err := second.Resolve(context.Background(), "box")
		resolved <- err
	}()

	select {
	case err := <-resolved:
		t.Fatalf("resolved (%v) while the last park was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	land()

	err := within(t, "the park landed", released)
	if err != nil {
		t.Fatalf("first release: %v", err)
	}

	err = within(t, "the second user resolved", resolved)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}

	if running, starts, _ := fake.state(); !running || starts != 2 {
		t.Fatalf("running=%v after %d starts — want the second user on a machine it started itself, after the park", running, starts)
	}

	mustRelease(t, second)

	if running, _, stops := fake.state(); running || stops != 2 {
		t.Errorf("running=%v after %d stops — want the second user's machine parked by its own release", running, stops)
	}
}

// A parked machine outlives its evictions under one entry: retired and replaced, the dead entry and its replacement named one instance, and whichever ended first parked it under the other.
func TestRegistryParksARestartedMachineOnlyAfterItsLastUser(t *testing.T) {
	fake := &parkedFake{}
	registry := NewRegistryWith(fake.acquire)
	workers := boxWorker(t, "aws://stopped/i-0abc123def456789")
	evicted := registry.Leases(workers)
	dead := mustResolve(t, evicted)

	fake.reclaim()
	evicted.Abandon("box", dead.URL)
	mustResolve(t, evicted)

	joined := registry.Leases(workers)
	mustResolve(t, joined)
	mustRelease(t, evicted)

	if running, starts, stops := fake.state(); !running {
		t.Fatalf("after the evicted job ended: %d starts, %d stops and the machine parked — under a job still on it", starts, stops)
	}

	mustRelease(t, joined)

	if running, starts, stops := fake.state(); running || starts != 2 || stops != 1 {
		t.Errorf("running=%v, %d starts, %d stops — want the restarted machine parked once, by its last user", running, starts, stops)
	}
}

// Re-acquired while still running — the notice came, the stop has not — it is still the machine steps started, so it is still parked at the end.
func TestRegistryStillParksAMachineReacquiredRunning(t *testing.T) {
	fake := &parkedFake{}
	registry := NewRegistryWith(fake.acquire)
	job := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))
	dying := mustResolve(t, job)

	job.Abandon("box", dying.URL)
	mustResolve(t, job)
	mustRelease(t, job)

	if running, starts, stops := fake.state(); running || starts != 1 || stops != 1 {
		t.Errorf("running=%v, %d starts, %d stops — want the machine steps started parked at the end", running, starts, stops)
	}
}

// Every spelling of one parked instance is one machine with one count: as two entries they were two owners, and the first to finish parked it under the other.
func TestRegistryCountsEverySpellingOfAParkedMachine(t *testing.T) {
	fake := &parkedFake{}
	registry := NewRegistryWith(fake.acquire)
	plain := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))
	rooted := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789/var/tmp/steps?idle=1h"))

	mustResolve(t, plain)

	onRooted := mustResolve(t, rooted)
	if onRooted.Root != "/var/tmp/steps" {
		t.Errorf("the second spelling resolved to %q, want its own root on the shared machine", onRooted.URL)
	}

	mustRelease(t, plain)

	if running, _, _ := fake.state(); !running {
		t.Fatal("the first spelling's end parked the machine the second was still on")
	}

	mustRelease(t, rooted)

	// Kept for the longest window any spelling asked for, not the one the first user happened to write.
	if running, starts, stops := fake.state(); !running || starts != 1 || stops != 0 {
		t.Fatalf("running=%v, %d starts, %d stops — want one machine, kept warm for the ?idle= one spelling asked for", running, starts, stops)
	}

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, _, stops := fake.state(); stops != 1 {
		t.Errorf("%d stops after Close, want the warm machine parked", stops)
	}
}

// A machine still in use at shutdown — a webhook's check, cancelled along with everything else — goes back once its user lets go, and Close waits for that rather than leaving it to bill after the process has gone.
func TestRegistryCloseWaitsForAMachineStillInUse(t *testing.T) {
	fake := &parkedFake{}
	registry := NewRegistryWith(fake.acquire)
	webhook := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789?idle=1h"))

	mustResolve(t, webhook)

	closed := make(chan error, 1)

	go func() { closed <- registry.Close(context.Background()) }()

	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a scope still held a machine", err)
	case <-time.After(100 * time.Millisecond):
	}

	mustRelease(t, webhook)

	err := within(t, "Close returned", closed)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if running, _, stops := fake.state(); running || stops != 1 {
		t.Errorf("running=%v after %d stops — want the machine given back at its user's end, idle window or not", running, stops)
	}
}

// When Close stops waiting, what a wedged scope still holds goes back anyway: the process is exiting, and the scope with it.
func TestRegistryCloseGivesBackWhatIsStillHeldWhenItStopsWaiting(t *testing.T) {
	stillHeld := func(t *testing.T, closing func(*Registry) error) {
		t.Helper()

		fake := &parkedFake{}
		land := holdLanding(t, fake)
		registry := NewRegistryWith(fake.acquire)
		wedged := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

		mustResolve(t, wedged)

		started := time.Now()
		closed := make(chan error, 1)

		go func() { closed <- closing(registry) }()

		within(t, "Close gave the machine back", fake.parking)

		// The wedged scope lets go while that give-back is still in flight.
		released := make(chan error, 1)

		go func() { released <- wedged.ReleaseAll(context.Background()) }()

		eventually(t, "the wedged scope let go", func() bool { return usersOf(registry) == 0 })
		land()

		err := within(t, "Close returned", closed)
		if err != nil {
			t.Fatalf("Close: %v", err)
		}

		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Errorf("Close took %s", elapsed)
		}

		err = within(t, "the late release returned", released)
		if err != nil {
			t.Fatalf("late release: %v", err)
		}

		if running, _, stops := fake.state(); running || stops != 1 {
			t.Errorf("running=%v after %d stops — want the machine given back once, as the process exits", running, stops)
		}
	}

	t.Run("at its own bound", func(t *testing.T) {
		previous := closeWait
		closeWait = 50 * time.Millisecond

		t.Cleanup(func() { closeWait = previous })

		stillHeld(t, func(r *Registry) error { return r.Close(context.Background()) })
	})

	t.Run("at its caller's deadline", func(t *testing.T) {
		previous := closeWait
		closeWait = 10 * time.Second

		t.Cleanup(func() { closeWait = previous })

		stillHeld(t, func(r *Registry) error {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			return r.Close(ctx)
		})
	})
}

// A park that never lands is not waited on forever, and a user that gives up waiting takes nothing: either way the next user fails rather than taking a machine about to be parked underneath it.
func TestRegistryGivesUpOnAParkThatNeverLands(t *testing.T) {
	previous := parkWait
	parkWait = 50 * time.Millisecond

	t.Cleanup(func() { parkWait = previous })

	fake := &parkedFake{}
	land := holdLanding(t, fake)
	registry := NewRegistryWith(fake.acquire)
	workers := boxWorker(t, "aws://stopped/i-0abc123def456789")
	first := registry.Leases(workers)

	mustResolve(t, first)

	released := make(chan error, 1)

	go func() { released <- first.ReleaseAll(context.Background()) }()

	within(t, "the park was issued", fake.parking)

	_, err := registry.Leases(workers).Resolve(context.Background(), "box")
	if err == nil {
		t.Error("resolved onto a machine whose park never landed")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = registry.Leases(workers).Resolve(cancelled, "box")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a resolver that left while waiting: %v, want its cancellation", err)
	}

	land()

	err = within(t, "the park landed", released)
	if err != nil {
		t.Fatalf("first release: %v", err)
	}

	if _, starts, _ := fake.state(); starts != 1 {
		t.Errorf("%d starts, want nothing acquired while the park was in flight", starts)
	}
}

// Close that stops waiting while a scope's own give-back is in flight waits on it through the lease: taken out again, the entry was given back twice and its gone channel closed twice.
func TestRegistryCloseWaitsOnAGiveBackAlreadyInFlight(t *testing.T) {
	previous := closeWait
	closeWait = 50 * time.Millisecond

	t.Cleanup(func() { closeWait = previous })

	fake := &parkedFake{}
	land := holdLanding(t, fake)
	registry := NewRegistryWith(fake.acquire)
	job := registry.Leases(boxWorker(t, "aws://stopped/i-0abc123def456789"))

	mustResolve(t, job)

	released := make(chan error, 1)

	go func() { released <- job.ReleaseAll(context.Background()) }()

	within(t, "the job's park was issued", fake.parking)

	closed := make(chan error, 1)

	go func() { closed <- registry.Close(context.Background()) }()

	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a park was still landing", err)
	case <-time.After(200 * time.Millisecond):
	}

	land()

	err := within(t, "Close returned", closed)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = within(t, "the job's release returned", released)
	if err != nil {
		t.Fatalf("release: %v", err)
	}

	if running, _, stops := fake.state(); running || stops != 1 {
		t.Errorf("running=%v after %d stops — want the machine parked once, by the give-back already in flight", running, stops)
	}
}

// parkedIn waits for a goroutine asleep on a mutex inside frame, so two lockers queued on one mutex are woken in the order the test parked them.
func parkedIn(t *testing.T, frame string) {
	t.Helper()

	eventually(t, "a goroutine asleep on a mutex in "+frame, func() bool {
		buf := make([]byte, 1<<20)

		for _, stack := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(stack, "[sync.Mutex.Lock") && strings.Contains(stack, frame) {
				return true
			}
		}

		return false
	})
}

// A sibling re-resolving between an abandon's retire and its bookkeeping has already moved the dead machine aside; redone, the abandon counted the dead machine twice and lost the replacement, giving one back under another scope and leaking the other.
func TestRegistryAbandonRacingASiblingsResolveCountsEachMachineOnce(t *testing.T) {
	var (
		mu       sync.Mutex
		acquired int
		released = map[string]int{}
	)

	registry := NewRegistryWith(func(_ context.Context, worker Worker) (Worker, func(context.Context) error, error) {
		mu.Lock()
		defer mu.Unlock()

		acquired++
		machine := worker.asStatic(fmt.Sprintf("i-%d", acquired))

		return machine, func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()

			released[machine.Instance]++

			return nil
		}, nil
	})

	workers := boxWorker(t, "aws://launch/lt-0def4567890abcde")
	job := registry.Leases(workers)
	other := registry.Leases(workers)

	dead := mustResolve(t, job)
	mustResolve(t, other)

	registry.mu.Lock()

	abandoned := make(chan struct{})

	go func() {
		defer close(abandoned)
		job.Abandon("box", dead.URL)
	}()

	parkedIn(t, "venue.(*Registry).retire(")

	resolved := make(chan error, 1)

	go func() {
		_, err := job.Resolve(context.Background(), "box")
		resolved <- err
	}()

	parkedIn(t, "venue.(*Registry).isRetired(")
	registry.mu.Unlock()

	within(t, "the abandon returned", abandoned)

	err := within(t, "the sibling resolved", resolved)
	if err != nil {
		t.Fatalf("sibling's resolve: %v", err)
	}

	mu.Lock()
	n := acquired
	mu.Unlock()

	if n != 2 {
		t.Fatalf("%d acquisitions: the sibling resolved before the abandon retired the machine, which is not the order this test arranges", n)
	}

	mustRelease(t, job)

	mu.Lock()
	early, fresh := released[dead.Instance], released["i-2"]
	mu.Unlock()

	if early != 0 || fresh != 1 {
		t.Fatalf("after the job ended: the dead machine given back %d times while another scope still held it, the replacement %d times — want 0 and 1", early, fresh)
	}

	mustRelease(t, other)

	mu.Lock()
	defer mu.Unlock()

	if released[dead.Instance] != 1 {
		t.Errorf("the dead machine given back %d times after its last user, want 1", released[dead.Instance])
	}
}
