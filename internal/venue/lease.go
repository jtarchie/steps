package venue

// A step never acquires a machine on its own account: a SCOPE does (a job, one poll, one webhook delivery), and every scope in a process shares one Registry that counts them, because two owners of one machine is how a job's end used to stop an instance another job was mid-step on.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"
)

// Acquirer brings a worker's machine into existence and says how to give it back; a nil release means there is nothing of steps' to give back.
type Acquirer func(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error)

// Registry is every machine this process acquired, by the worker mapping that named it, and how many scopes are using each.
type Registry struct {
	acquire Acquirer
	mu      sync.Mutex
	current map[string]*entry
	closed  bool
	// expiring counts idle timers that may still fire, so Close can wait for a give-back already in flight.
	expiring sync.WaitGroup
}

type entry struct {
	lease

	source Worker
	refs   int
	// retired is a machine somebody watched die: nothing new may take it, and its last user gives it back without an idle window.
	retired bool
	idle    *time.Timer
	// gen tells a timer that fired late that the window it was armed for was superseded.
	gen int
}

// NewRegistry acquires through the clouds.
func NewRegistry() *Registry { return NewRegistryWith(acquire) }

// NewRegistryWith exists for a borrowable venue that is not a cloud — the only way to prove sharing without an account.
func NewRegistryWith(acquirer Acquirer) *Registry {
	return &Registry{acquire: acquirer, current: map[string]*entry{}}
}

// Leases opens one scope over the registry.
func (r *Registry) Leases(workers map[string]Worker) *Leases {
	return &Leases{registry: r, held: map[string]*entry{}, source: maps.Clone(workers)}
}

func (r *Registry) hold(worker Worker) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	held, ok := r.current[worker.URL]
	if !ok {
		held = &entry{source: worker}
		r.current[worker.URL] = held
	}

	held.refs++
	r.stopIdle(held)

	return held
}

// stopIdle leaves a timer that already fired to find the entry held again and stand down on its own.
func (r *Registry) stopIdle(held *entry) {
	if held.idle != nil && held.idle.Stop() {
		r.expiring.Done()
	}

	held.idle = nil
}

// drop holds the idle window on the registry's own timer so no job, poll or request waits it out; asking the lease whether it holds a machine never waits on a cloud call, because an acquirer holds a count and so cannot be mid-acquisition at zero.
func (r *Registry) drop(ctx context.Context, held *entry, immediate bool) error {
	r.mu.Lock()

	held.refs--
	if held.refs > 0 {
		r.mu.Unlock()

		return nil
	}

	if immediate || r.closed || held.retired || held.source.Idle <= 0 || !held.holding() {
		r.forget(held)
		r.mu.Unlock()

		return held.give(ctx)
	}

	held.gen++
	gen := held.gen

	r.expiring.Add(1)
	//nolint:contextcheck // the window outlives every caller's context, and the release bounds its own calls
	held.idle = time.AfterFunc(held.source.Idle, func() { r.expire(held, gen) })
	r.mu.Unlock()

	fmt.Printf("worker %s: nothing is using it; keeping it for %s (?idle=)\n", held.source.URL, held.source.Idle)

	return nil
}

func (r *Registry) expire(held *entry, gen int) {
	defer r.expiring.Done()

	r.mu.Lock()

	if held.refs > 0 || held.gen != gen {
		r.mu.Unlock()

		return
	}

	held.idle = nil
	r.forget(held)
	r.mu.Unlock()

	// Nothing that could cancel this is still around, and the release bounds its own calls.
	err := held.give(context.Background())
	if err != nil {
		fmt.Printf("warning: worker %s could not be given back after its idle window: %v\n", held.source.URL, err)
	}
}

// forget stops handing this entry out, so the next user acquires fresh.
func (r *Registry) forget(held *entry) {
	if r.current[held.source.URL] == held {
		delete(r.current, held.source.URL)
	}
}

func (r *Registry) retire(held *entry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	held.retired = true
	r.forget(held)
}

func (r *Registry) isRetired(held *entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return held.retired
}

// Close leaves reporting to its caller, because an instance left running bills until somebody finds it.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()

	r.closed = true

	idle := []*entry{}

	for _, held := range r.current {
		if held.refs == 0 {
			r.stopIdle(held)
			idle = append(idle, held)
			r.forget(held)
		}
	}

	r.mu.Unlock()

	err := giveAll(ctx, idle)

	r.expiring.Wait()

	return err
}

// giveAll releases concurrently, because slow Stops in turn could outlive the budget the later ones needed.
func giveAll(ctx context.Context, entries []*entry) error {
	failures := make([]error, len(entries))

	var wg sync.WaitGroup

	for i, one := range entries {
		wg.Go(func() { failures[i] = one.give(ctx) })
	}

	wg.Wait()

	return errors.Join(failures...)
}

// Leases is one scope's claim on the registry: the machines it is using, by tag.
type Leases struct {
	registry *Registry
	// own is a registry nothing else shares, so nothing this scope holds may outlive it.
	own bool
	mu  sync.Mutex
	// held are machines this scope counts toward, by tag.
	held map[string]*entry
	// retired are machines this scope stopped using but still counts toward, so its end still gives them back — see Abandon.
	retired []*entry
	source  map[string]Worker
}

// lease is a mutex rather than a sync.Once because a release has to WAIT for an acquisition in flight: a Once orders completion only against callers of Do, so a release reading its fields would race, and a live instance's release closure could be dropped while it bills.
type lease struct {
	mu       sync.Mutex
	acquired bool
	// err sticks for the scopes that saw it, since the second answer is the first one again; an entry with no machine is forgotten when its last user leaves, so the next one tries again.
	worker  Worker
	err     error
	release func(ctx context.Context) error
}

func (l *lease) resolve(ctx context.Context, worker Worker, acquirer Acquirer) (Worker, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.acquired {
		return l.worker, l.err
	}

	l.acquired = true
	l.worker, l.release, l.err = acquirer(ctx, worker)

	return l.worker, l.err
}

// give waits for any acquisition in flight, so a machine that arrives late is still released rather than stranded.
func (l *lease) give(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.release == nil {
		return nil
	}

	release := l.release
	l.release = nil

	return release(ctx)
}

func (l *lease) holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.release != nil
}

// NewLeases is a scope with a registry of its own, for a one-shot command: with no later user in the process to keep a machine warm for, everything goes back when the scope ends.
func NewLeases(workers map[string]Worker) *Leases {
	leases := NewRegistry().Leases(workers)
	leases.own = true

	return leases
}

// Resolve acquires only when no scope in the process is using the machine yet; a worker that needs no acquisition costs nothing.
func (l *Leases) Resolve(ctx context.Context, tag string) (Worker, error) {
	worker, ok := l.source[tag]
	if !ok {
		return Worker{}, fmt.Errorf("%w: no worker is mapped to tag %q", ErrWorker, tag)
	}

	if !worker.needsAcquisition() {
		return worker, nil
	}

	l.mu.Lock()

	held := l.held[tag]

	// Another scope watched this machine die. This one still owes it a release, and needs a fresh one.
	if held != nil && l.registry.isRetired(held) {
		l.retired = append(l.retired, held)
		held = nil
	}

	if held == nil {
		held = l.registry.hold(worker)
		l.held[tag] = held
	}

	l.mu.Unlock()

	return held.resolve(ctx, worker, l.registry.acquire)
}

// Abandon is identity-checked so a stale notice cannot orphan the fresh machine a sibling re-acquired under the same tag, retires the machine so no other scope is handed it, and does not release it: a parallel sibling may still be inside the grace the notice promised, and a spot stop or hibernate leaves a live instance only the last user's release will ever end.
func (l *Leases) Abandon(tag, dialURL string) {
	l.mu.Lock()
	held, ok := l.held[tag]
	l.mu.Unlock()

	if !ok || !held.abandonIf(dialURL) {
		return
	}

	l.registry.retire(held)

	l.mu.Lock()

	if l.held[tag] == held {
		delete(l.held, tag)
		l.retired = append(l.retired, held)
	}

	l.mu.Unlock()
}

// abandonIf never matches an acquisition in flight: a machine still being acquired is not the machine anybody watched die.
func (l *lease) abandonIf(dialURL string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.acquired && l.err == nil && l.worker.URL == dialURL
}

// ReleaseAll is exhaustive and joins its errors, because one failure must not strand the others while they bill.
func (l *Leases) ReleaseAll(ctx context.Context) error {
	l.mu.Lock()

	held := make([]*entry, 0, len(l.held)+len(l.retired))
	for _, one := range l.held {
		held = append(held, one)
	}

	held = append(held, l.retired...)

	l.held = map[string]*entry{}
	l.retired = nil
	l.mu.Unlock()

	failures := make([]error, len(held))

	var wg sync.WaitGroup

	for i, one := range held {
		wg.Go(func() { failures[i] = l.registry.drop(ctx, one, l.own) })
	}

	wg.Wait()

	return errors.Join(failures...)
}
