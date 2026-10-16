package venue

// Exclusive pools (#205), against a fake acquirer that numbers its machines.

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/jtarchie/steps/internal/events"
)

const poolTemplate = "aws://launch/lt-0def4567890abcde"

// ?exclusive and ?max= are launch-rung words on every cloud, and every spelling that cannot mean a pool is refused rather than read as one machine.
func TestParseWorkerPoolOptions(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]struct {
		exclusive bool
		max       int
	}{
		poolTemplate + "?exclusive":                            {true, 0},
		poolTemplate + "?exclusive=true&max=3":                 {true, 3},
		"gcp://launch/tmpl?zone=us-central1-a&max=1&exclusive": {true, 1},
		poolTemplate: {false, 0},
	} {
		worker := mustParse(t, raw)
		if worker.Exclusive != want.exclusive || worker.Max != want.max {
			t.Errorf("ParseWorker(%q) = exclusive %v max %d, want %+v", raw, worker.Exclusive, worker.Max, want)
		}

		// A launched machine is dialled by a URL re-parsed as a static one, which refuses both.
		if again := mustParse(t, worker.asStatic("i-0abc123def456789").URL); again.Exclusive || again.Max != 0 {
			t.Errorf("%q dialled as a static machine still carries the pool: %+v", raw, again)
		}
	}

	for raw, why := range map[string]string{
		"aws://i-0abc123def456789?exclusive":               "names one that already exists",
		"aws://stopped/i-0abc123def456789?exclusive&max=2": "names one that already exists",
		"gcp://stopped/worker-1?zone=z&exclusive":          "names one that already exists",
		"ssh://box?exclusive":                              "does not describe a ssh worker",
		"docker+ssh://box?max=2":                           "does not describe a docker+ssh worker",
		poolTemplate + "?max=3":                            "add ?exclusive",
		poolTemplate + "?exclusive&max=0":                  "from 1",
		poolTemplate + "?exclusive&max=three":              "from 1",
		poolTemplate + "?exclusive=false":                  "takes no value",
	} {
		_, err := ParseWorker(raw)
		if !errors.Is(err, ErrWorker) || !strings.Contains(err.Error(), why) {
			t.Errorf("ParseWorker(%q) = %v, want it refused: %s", raw, err, why)
		}
	}
}

// Exclusive and max are part of the machine's identity; the other options are not, as for a shared machine.
func TestRegistryKeysAPoolApartFromTheSharedMachine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, one, other string }{
		{"shared and exclusive", poolTemplate, poolTemplate + "?exclusive"},
		{"two caps", poolTemplate + "?exclusive&max=2", poolTemplate + "?exclusive&max=3"},
		{"capped and not", poolTemplate + "?exclusive", poolTemplate + "?exclusive&max=3"},
	} {
		if mustParse(t, tc.one).registryKey() == mustParse(t, tc.other).registryKey() {
			t.Errorf("%s: %q and %q are one machine", tc.name, tc.one, tc.other)
		}
	}

	same := mustParse(t, poolTemplate+"?exclusive&max=2&idle=5m").registryKey()
	if other := mustParse(t, poolTemplate+"?max=2&exclusive=true").registryKey(); other != same {
		t.Errorf("two spellings of one pool keyed %q and %q", same, other)
	}

	if mustParse(t, poolTemplate+"?exclusive").launchLabels()[labelWorker] == mustParse(t, poolTemplate).launchLabels()[labelWorker] {
		t.Error("a pool's machines are labelled as the shared machine of the same template")
	}
}

func poolLeases(t *testing.T, registry *Registry, raw string) *Leases {
	t.Helper()

	return registry.Leases(boxWorker(t, raw))
}

// resolveAsync resolves in the background, answering on the channel.
func resolveAsync(ctx context.Context, leases *Leases) <-chan resolved {
	got := make(chan resolved, 1)

	go func() {
		worker, err := leases.Resolve(ctx, "box")
		got <- resolved{worker, err}
	}()

	return got
}

type resolved struct {
	worker Worker
	err    error
}

func waitersOf(r *Registry) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	waiting := 0
	for _, p := range r.pools {
		waiting += len(p.waiters)
	}

	return waiting
}

func blocked(t *testing.T, what string, got <-chan resolved) {
	t.Helper()

	select {
	case answer := <-got:
		t.Fatalf("%s did not wait: %+v", what, answer)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPoolGivesTwoConcurrentJobsTwoMachines(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	first := poolLeases(t, registry, poolTemplate+"?exclusive")
	second := poolLeases(t, registry, poolTemplate+"?exclusive")

	one := mustResolve(t, first)
	other := mustResolve(t, second)

	if one.Instance == other.Instance {
		t.Fatalf("two concurrent jobs share %s", one.Instance)
	}

	if acquired, _ := fake.counts(); acquired != 2 {
		t.Errorf("%d acquisitions, want one machine per job", acquired)
	}

	// A machine kept by a job is the job's whole: a second resolve hands back the same one.
	if again := mustResolve(t, first); again.Instance != one.Instance {
		t.Errorf("the job's second step got %s, want its own %s", again.Instance, one.Instance)
	}

	mustRelease(t, first)
	mustRelease(t, second)

	if _, released := fake.counts(); len(released) != 2 {
		t.Errorf("given back %v, want both machines, each as its job ended", released)
	}
}

func TestPoolCappedJobWaitsThenGetsTheFirstsMachine(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	first := poolLeases(t, registry, poolTemplate+"?exclusive&max=1&idle=5m")
	second := poolLeases(t, registry, poolTemplate+"?exclusive&max=1&idle=5m")

	held := mustResolve(t, first)

	var notes bytes.Buffer

	ctx := events.WithOutput(context.Background(), events.Output{Stdout: &notes})
	got := resolveAsync(ctx, second)

	blocked(t, "a job past max", got)

	mustRelease(t, first)

	answer := within(t, "the waiter handed the machine", got)
	if answer.err != nil || answer.worker.Instance != held.Instance {
		t.Fatalf("the waiter got %+v, want %s", answer, held.Instance)
	}

	if acquired, released := fake.counts(); acquired != 1 || len(released) != 0 {
		t.Errorf("%d acquisitions, %v given back: want the one machine handed over", acquired, released)
	}

	if !strings.Contains(notes.String(), "waiting for one") {
		t.Errorf("notes = %q, want the wait named in the transcript", notes.String())
	}

	if second.ReusedWarm("box") {
		t.Error("a machine handed straight from one job to the next is presumed dead")
	}

	mustRelease(t, second)

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPoolWaiterCanBeAborted(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	first := poolLeases(t, registry, poolTemplate+"?exclusive&max=1&idle=5m")
	aborted := poolLeases(t, registry, poolTemplate+"?exclusive&max=1&idle=5m")

	held := mustResolve(t, first)

	ctx, abort := context.WithCancel(context.Background())
	got := resolveAsync(ctx, aborted)

	eventually(t, "the second job queued", func() bool { return waitersOf(registry) == 1 })
	abort()

	answer := within(t, "the aborted waiter returned", got)
	if !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("an aborted wait = %+v, want context.Canceled", answer)
	}

	mustRelease(t, aborted)
	mustRelease(t, first)

	// Not handed to the job that left: the next job finds it kept warm.
	next := poolLeases(t, registry, poolTemplate+"?exclusive&max=1&idle=5m")
	if again := mustResolve(t, next); again.Instance != held.Instance || !next.ReusedWarm("box") {
		t.Errorf("the next job got %s (warm %v), want %s from its idle window", again.Instance, next.ReusedWarm("box"), held.Instance)
	}

	mustRelease(t, next)

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPoolServesWaitersInTheOrderTheyCame(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&max=1&idle=5m"
	holder := poolLeases(t, registry, raw)
	mustResolve(t, holder)

	scopes := make([]*Leases, 4)
	answers := make([]<-chan resolved, len(scopes))

	for i := range scopes {
		scopes[i] = poolLeases(t, registry, raw)
		answers[i] = resolveAsync(context.Background(), scopes[i])

		eventually(t, "the waiter queued", func() bool { return waitersOf(registry) == i+1 })
	}

	mustRelease(t, holder)

	for i := range scopes {
		within(t, "waiter served", answers[i])

		for _, later := range answers[i+1:] {
			blocked(t, "a later waiter", later)
		}

		mustRelease(t, scopes[i])
	}

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestPoolCheckRidesAlongOnABusyMachine(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&max=1"

	// An empty pool gets a machine for a check.
	alone := registry.CheckLeases(boxWorker(t, raw))
	mustResolve(t, alone)
	mustRelease(t, alone)

	job := poolLeases(t, registry, raw)
	busy := mustResolve(t, job)

	check := registry.CheckLeases(boxWorker(t, raw))

	rode := within(t, "the check on a busy machine", resolveAsync(context.Background(), check))
	if rode.err != nil || rode.worker.Instance != busy.Instance {
		t.Fatalf("the check got %+v, want to ride along on %s", rode, busy.Instance)
	}

	// The check leaving neither frees the job's machine for another job nor gives it back.
	mustRelease(t, check)

	other := poolLeases(t, registry, raw)
	blocked(t, "a second job while the first still holds the only machine", resolveAsync(context.Background(), other))

	mustRelease(t, job)
	mustRelease(t, other)

	if acquired, _ := fake.counts(); acquired != 2 {
		t.Errorf("%d acquisitions, want the lone check's machine and the job's", acquired)
	}
}

func TestPoolHandsOutTheMostRecentlyUsedMachine(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&idle=5m"

	jobs := map[string]*Leases{}

	for range 3 {
		job := poolLeases(t, registry, raw)
		jobs[mustResolve(t, job).Instance] = job
	}

	for _, instance := range []string{"i-1", "i-3", "i-2"} {
		mustRelease(t, jobs[instance])
	}

	next := poolLeases(t, registry, raw)
	if got := mustResolve(t, next); got.Instance != "i-2" {
		t.Errorf("the next job got %s, want i-2, the machine let go of last", got.Instance)
	}

	mustRelease(t, next)

	err := registry.Close(context.Background())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, released := fake.counts(); len(released) != 3 {
		t.Errorf("Close gave back %v, want all three", released)
	}
}

// The extras a burst made are kept on their own timers and go back on ?idle= rather than with the pool.
func TestPoolExpiresEachMachineOnItsOwnIdleWindow(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&idle=50ms"

	first := poolLeases(t, registry, raw)
	second := poolLeases(t, registry, raw)
	mustResolve(t, first)
	mustResolve(t, second)
	mustRelease(t, first)

	eventually(t, "the idle machine given back while the other is still held", func() bool {
		_, released := fake.counts()

		return len(released) == 1
	})

	mustRelease(t, second)

	eventually(t, "the second given back on its own window", func() bool {
		_, released := fake.counts()

		return len(released) == 2
	})
}

// The evicted job is re-placed through the same pool and keeps its place rather than queueing behind newer waiters.
func TestPoolReplacesAnEvictedJobsMachineAheadOfTheQueue(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&max=1"

	evicted := poolLeases(t, registry, raw)
	dead := mustResolve(t, evicted)

	waiting := poolLeases(t, registry, raw)
	got := resolveAsync(context.Background(), waiting)

	eventually(t, "the second job queued", func() bool { return waitersOf(registry) == 1 })

	evicted.Abandon("box", dead.URL)

	blocked(t, "a waiter while the evicted job's place is kept", got)

	replacement := within(t, "the evicted job re-placed", resolveAsync(context.Background(), evicted))
	if replacement.err != nil || replacement.worker.Instance == dead.Instance {
		t.Fatalf("the evicted job got %+v, want a fresh machine", replacement)
	}

	blocked(t, "a waiter while the evicted job holds its replacement", got)

	mustRelease(t, evicted)

	if answer := within(t, "the waiter served", got); answer.worker.Instance != replacement.worker.Instance {
		t.Errorf("the waiter got %s, want the replacement %s", answer.worker.Instance, replacement.worker.Instance)
	}

	if _, released := fake.counts(); released[dead.Instance] != 1 {
		t.Errorf("given back %v, want the dead machine once, with its job", released)
	}

	mustRelease(t, waiting)
}

// A job's evicted machine whose job ends without re-placing gives its place to the next waiter.
func TestPoolGivesUpAKeptPlaceWhenTheJobEnds(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&max=1"

	evicted := poolLeases(t, registry, raw)
	dead := mustResolve(t, evicted)

	waiting := poolLeases(t, registry, raw)
	got := resolveAsync(context.Background(), waiting)

	eventually(t, "the second job queued", func() bool { return waitersOf(registry) == 1 })
	evicted.Abandon("box", dead.URL)
	mustRelease(t, evicted)

	if answer := within(t, "the waiter served", got); answer.err != nil || answer.worker.Instance == dead.Instance {
		t.Fatalf("the waiter got %+v, want a fresh machine", answer)
	}

	mustRelease(t, waiting)
}

// Two tags, or two parallel steps, of one job on one capped pool take one machine: as two holds the job would wait on itself.
func TestPoolCountsOneMachinePerJobHoweverManyStepsAsk(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	worker := mustParse(t, poolTemplate+"?exclusive&max=1")
	job := registry.Leases(map[string]Worker{"a": worker, "b": mustParse(t, poolTemplate+"?max=1&exclusive")})

	var wg sync.WaitGroup

	instances := make([]string, 6)

	for i := range instances {
		wg.Go(func() {
			instances[i] = mustResolveTag(context.Background(), t, job, []string{"a", "b"}[i%2]).Instance
		})
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	within(t, "every step of the job resolved", done)

	for _, instance := range instances {
		if instance != instances[0] {
			t.Fatalf("one job's steps got %v, want one machine", instances)
		}
	}

	mustReleaseWith(context.Background(), t, job)

	if acquired, released := fake.counts(); acquired != 1 || released["i-1"] != 1 {
		t.Errorf("%d acquisitions, %v given back: want one machine, given back once", acquired, released)
	}

	next := poolLeases(t, registry, poolTemplate+"?exclusive&max=1")
	within(t, "the next job after the first let go", resolveAsync(context.Background(), next))
	mustRelease(t, next)
}

// A job's parallel steps waiting on a full pool queue once, and are handed one machine together.
func TestPoolQueuesAJobOnceForAllItsWaitingSteps(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	raw := poolTemplate + "?exclusive&max=1"
	holder := poolLeases(t, registry, raw)
	held := mustResolve(t, holder)

	job := registry.Leases(map[string]Worker{"a": mustParse(t, raw), "b": mustParse(t, raw)})

	answers := make([]<-chan resolved, 0, 3)

	for _, tag := range []string{"a", "b", "a"} {
		got := make(chan resolved, 1)

		go func() {
			worker, err := job.Resolve(context.Background(), tag)
			got <- resolved{worker, err}
		}()

		answers = append(answers, got)
	}

	eventually(t, "the job queued", func() bool { return waitersOf(registry) == 1 })

	for _, got := range answers {
		blocked(t, "a step of a job past max", got)
	}

	if waiting := waitersOf(registry); waiting != 1 {
		t.Fatalf("%d waiters for one job's steps, want 1", waiting)
	}

	mustRelease(t, holder)

	for _, got := range answers {
		if answer := within(t, "a step served", got); answer.err != nil || answer.worker.Instance != held.Instance {
			t.Errorf("a step got %+v, want the handed-over %s", answer, held.Instance)
		}
	}

	mustReleaseWith(context.Background(), t, job)

	if acquired, released := fake.counts(); acquired != 1 || released[held.Instance] != 1 {
		t.Errorf("%d acquisitions, %v given back: want one machine, given back once", acquired, released)
	}
}

// A failed acquisition is not handed to the next waiter: its error sticks to the entry.
func TestPoolDoesNotHandAFailedMachineToAWaiter(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
	)

	registry := NewRegistryWith(func(_ context.Context, worker Worker) (Worker, func(context.Context) error, error) {
		mu.Lock()
		defer mu.Unlock()

		calls++
		if calls == 1 {
			return Worker{}, nil, errors.New("no capacity")
		}

		return worker.asStatic("i-good"), func(context.Context) error { return nil }, nil
	})
	raw := poolTemplate + "?exclusive&max=1&idle=5m"

	failed := poolLeases(t, registry, raw)

	_, err := failed.Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("the first acquisition did not fail")
	}

	waiting := poolLeases(t, registry, raw)
	got := resolveAsync(context.Background(), waiting)

	eventually(t, "the second job queued", func() bool { return waitersOf(registry) == 1 })
	mustRelease(t, failed)

	if answer := within(t, "the waiter served", got); answer.err != nil || answer.worker.Instance != "i-good" {
		t.Fatalf("the waiter got %+v, want a fresh acquisition", answer)
	}

	mustRelease(t, waiting)
}

// A check whose acquisition failed and is still holding the entry must not hand its sticky error to a job arriving meanwhile.
func TestPoolDoesNotHandAJobACheckFailedAcquisition(t *testing.T) {
	t.Parallel()

	var (
		mu    sync.Mutex
		calls int
	)

	registry := NewRegistryWith(func(_ context.Context, worker Worker) (Worker, func(context.Context) error, error) {
		mu.Lock()
		defer mu.Unlock()

		calls++
		if calls == 1 {
			return Worker{}, nil, errors.New("no capacity")
		}

		return worker.asStatic("i-good"), func(context.Context) error { return nil }, nil
	})
	raw := poolTemplate + "?exclusive&max=2"

	check := registry.CheckLeases(boxWorker(t, raw))

	_, err := check.Resolve(context.Background(), "box")
	if err == nil {
		t.Fatal("the check's acquisition did not fail")
	}

	job := poolLeases(t, registry, raw)
	if got := mustResolve(t, job); got.Instance != "i-good" {
		t.Errorf("the job got %s, want a fresh acquisition", got.Instance)
	}

	mustRelease(t, check)
	mustRelease(t, job)
}

// Shutdown gives back what a pool kept warm at once, rather than waiting it out and calling it in use.
func TestPoolCloseGivesBackWhatItKeptWarm(t *testing.T) {
	t.Parallel()

	fake := &countingAcquirer{}
	registry := NewRegistryWith(fake.acquire)
	job := poolLeases(t, registry, poolTemplate+"?exclusive&idle=1h")

	mustResolve(t, job)
	mustRelease(t, job)

	closed := make(chan error, 1)

	go func() { closed <- registry.Close(context.Background()) }()

	err := within(t, "Close returned", closed)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, released := fake.counts(); released["i-1"] != 1 {
		t.Errorf("after Close: given back %v, want the warm machine", released)
	}
}

// Each machine of a pool is stamped as this daemon's in its own create request, as a shared launch is (#120).
func TestPoolStampsEveryMachineItLaunches(t *testing.T) {
	fake := &fakeEC2{}
	seamEC2(t, fake)

	registry := NewRegistry(testOwner)
	first := poolLeases(t, registry, poolTemplate+"?exclusive")
	second := poolLeases(t, registry, poolTemplate+"?exclusive")

	mustResolve(t, first)
	mustResolve(t, second)
	mustRelease(t, first)
	mustRelease(t, second)

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.fleets) != 2 {
		t.Fatalf("%d launches, want one per job", len(fake.fleets))
	}

	for i, fleet := range fake.fleets {
		value := ""

		for _, tag := range fleet.TagSpecifications[0].Tags {
			if aws.ToString(tag.Key) == labelOwner {
				value = aws.ToString(tag.Value)
			}
		}

		if rung, ours := ownedBy(testOwner, value, time.Now()); !ours || rung != RungLaunch {
			t.Errorf("launch %d: steps-owner = %q, want this daemon's launch stamp", i, value)
		}
	}
}

// One exclusive mapping is many machines, and a restart gives back every one of them.
func TestSweepGivesBackEveryMachineOfAPool(t *testing.T) {
	now := time.Now()
	fake := &fakeEC2{}
	seamEC2(t, fake)

	api := sweepingEC2{fakeEC2: fake, t: t, running: []ownedInstance{
		{name: "i-0pool1", stamp: stamp(testOwner, RungLaunch, now), started: now, template: "lt-0def4567890abcde"},
		{name: "i-0pool2", stamp: stamp(testOwner, RungLaunch, now), started: now, template: "lt-0def4567890abcde"},
		{name: "i-0pool3", stamp: stamp(testOwner, RungLaunch, now), started: now, template: "lt-0def4567890abcde"},
	}}
	ec2For = func(context.Context, Worker) (ec2API, error) { return api, nil }

	err := NewRegistry(testOwner).Sweep(context.Background(), map[string]Worker{"a": mustParse(t, poolTemplate+"?exclusive&max=3")})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := []string{"terminate i-0pool1", "terminate i-0pool2", "terminate i-0pool3"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}

func TestGCPSweepGivesBackEveryMachineOfAPool(t *testing.T) {
	now := time.Now()
	fake := &fakeGCE{running: []ownedInstance{
		{name: "steps-0a1b2c", stamp: stamp(testOwner, RungLaunch, now), started: now},
		{name: "steps-3d4e5f", stamp: stamp(testOwner, RungLaunch, now), started: now},
	}}
	seamGCP(t, fake, nil)

	err := NewRegistry(testOwner).Sweep(context.Background(), map[string]Worker{
		"a": mustParse(t, "gcp://launch/template-1?project=test-project&zone=us-central1-a&exclusive&max=2"),
	})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want := []string{"delete steps-0a1b2c", "delete steps-3d4e5f"}
	if got := fake.callLog(); !slices.Equal(got, want) {
		t.Errorf("calls = %v, want %v", got, want)
	}
}
