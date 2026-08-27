package venue

// A lease is a worker that has to be ACQUIRED before it can be dialed, and
// released when the job is done with it.
//
// Every venue before this one named a machine that already existed: local:
// forks a process, ssh:// and aws://i-* dial something already running. A
// parked or per-job instance does not exist yet at the moment a step asks for
// it, and cloud acquisition costs 20-90 seconds — so the unit of acquisition
// is the JOB, not the step. One lease per tag per job: the first placed step
// pays for the machine, every later step reuses it, and the job's end returns
// it.
//
// Lazy for the same reason the session is: a job whose placed steps are all
// cache hits must not start an instance to discover that.

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Leases holds one job's acquired workers, keyed by tag.
type Leases struct {
	mu   sync.Mutex
	held map[string]*lease
	// retired are machines a step stopped using but that still have to be
	// given back — see Abandon. Kept out of held so the next Resolve acquires
	// a fresh one, and kept at all so the job's end still releases them.
	retired []*lease
	source  map[string]Worker
}

// lease is one tag's machine: acquired at most once, released at most once.
//
// A mutex rather than a sync.Once, and the difference is the whole reason:
// Invalidate has to be able to WAIT for an acquisition in flight. A once
// orders its function's completion only against a caller of Do, so a
// non-calling reader of the fields it wrote gets no happens-before — which is
// both a data race and, worse, a live instance whose release closure is
// dropped on the floor while it keeps billing.
type lease struct {
	mu       sync.Mutex
	acquired bool
	// worker is what to dial once acquired, and err is why that never
	// happened. A failure sticks for the job: acquisition is expensive and
	// the second answer is the first one again.
	worker Worker
	err    error
	// release returns the machine. Its bool asks for the idle window to be
	// skipped: parking a machine after a delay is right at the end of a job
	// and wrong when the machine is being reclaimed. nil when nothing was
	// acquired.
	release func(ctx context.Context, immediate bool) error
}

// resolve acquires this lease's machine once, and waits for whoever is
// already acquiring it.
func (l *lease) resolve(ctx context.Context, worker Worker) (Worker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.acquired {
		return l.worker, l.err
	}

	l.acquired = true
	l.worker, l.release, l.err = acquire(ctx, worker)

	return l.worker, l.err
}

// give hands the machine back, waiting for any acquisition in flight so a
// machine that arrives late is still released rather than stranded.
func (l *lease) give(ctx context.Context, immediate bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.release == nil {
		return nil
	}

	release := l.release
	l.release = nil

	return release(ctx, immediate)
}

// NewLeases prepares a job's leases over the invocation's tag mapping. It
// acquires nothing; a tag naming an already-running machine never will.
func NewLeases(workers map[string]Worker) *Leases {
	source := make(map[string]Worker, len(workers))
	for tag, worker := range workers {
		source[tag] = worker
	}

	return &Leases{held: map[string]*lease{}, source: source}
}

// Resolve answers which machine a tag's steps run on, acquiring it if this is
// the first step to ask.
//
// A worker that needs no acquisition is returned unchanged and costs nothing,
// which is every scheme but the two acquisition rungs.
func (l *Leases) Resolve(ctx context.Context, tag string) (Worker, error) {
	worker, ok := l.source[tag]
	if !ok {
		return Worker{}, fmt.Errorf("%w: no worker is mapped to tag %q", ErrWorker, tag)
	}

	if !worker.needsAcquisition() {
		return worker, nil
	}

	l.mu.Lock()

	held, ok := l.held[tag]
	if !ok {
		held = new(lease)
		l.held[tag] = held
	}

	l.mu.Unlock()

	return held.resolve(ctx, worker)
}

// Abandon stops using the machine held for a tag, so the next Resolve
// acquires a fresh one — and only when it is the machine the caller watched
// die.
//
// Not released HERE, because a parallel sibling step may still be running on
// it inside the two-minute grace the notice promises: releasing on the spot
// would cut that grace to zero and turn the sibling's healthy work into a
// plain connection death its session cannot classify.
//
// But still released at the JOB'S end, which is after every sibling by
// construction. Dropping the release outright assumed a terminal notice means
// the machine is a corpse, and a spot instance-action is one of terminate,
// stop or hibernate: only the first destroys anything. Under
// InstanceInterruptionBehavior=stop the machine is merely STOPPED, so nothing
// ever terminated it and it billed — with its EBS root volume — for as long
// as the account lived, with nothing left holding a reference to look for it.
// Terminating one AWS already terminated is a no-op, so the redundant call on
// a real reclamation costs nothing.
//
// Identity-checked, because Invalidate-by-tag was a footgun: between one
// step's eviction and its forget, a sibling can have re-acquired a FRESH
// machine under the same tag, and a tag-keyed forget would orphan — or
// worse, release — the machine the sibling just paid for. A lease that is
// still acquiring, or that resolved to a different machine, is left alone.
func (l *Leases) Abandon(tag, dialURL string) {
	l.mu.Lock()
	held, ok := l.held[tag]
	l.mu.Unlock()

	if !ok || !held.abandonIf(dialURL) {
		return
	}

	l.mu.Lock()

	if l.held[tag] == held {
		delete(l.held, tag)
		l.retired = append(l.retired, held)
	}

	l.mu.Unlock()
}

// abandonIf gives up this lease's machine when it is the one named, reporting
// whether it did. Waits for an acquisition in flight, and never matches one:
// a machine still being acquired is not the machine anybody watched die.
//
// The release closure is deliberately kept — the caller moves this lease
// aside rather than forgetting it, so the job's end still gives the machine
// back. See Abandon.
func (l *lease) abandonIf(dialURL string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.acquired && l.err == nil && l.worker.URL == dialURL
}

// ReleaseAll returns every machine this job acquired.
//
// Best effort and exhaustive: one failure must not strand the others, since
// what is being released costs money for as long as it runs. Errors are
// joined so a caller can report all of them.
func (l *Leases) ReleaseAll(ctx context.Context) error {
	l.mu.Lock()

	held := make([]*lease, 0, len(l.held)+len(l.retired))
	for _, one := range l.held {
		held = append(held, one)
	}

	// Retired machines are released with the rest: they are exactly the ones
	// nothing else holds a reference to.
	held = append(held, l.retired...)

	l.held = map[string]*lease{}
	l.retired = nil
	l.mu.Unlock()

	// Concurrently, because releases can legitimately take a while — a
	// parked worker's ?idle= hold, a slow Stop — and a job's end waiting out
	// each one in turn could outlive the whole release budget on the later
	// ones, stranding running machines behind the earlier ones' patience.
	failures := make([]error, len(held))

	var wg sync.WaitGroup

	for i, one := range held {
		wg.Add(1)

		go func() {
			defer wg.Done()

			failures[i] = one.give(ctx, false)
		}()
	}

	wg.Wait()

	return errors.Join(failures...)
}
