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

// parkWait is twice a park's own bound, cleanupTimeout, so a park that used all of it still lands before its next user gives up waiting.
var parkWait = 2 * cleanupTimeout //nolint:gochecknoglobals // a test seam for a wait measured in minutes, as acquireTimeout is

// closeWait is cleanupTimeout because every scope was cancelled before Close, and what a cancelled scope has left is its acquirer's own clean-up, bounded by that.
var closeWait = cleanupTimeout //nolint:gochecknoglobals // as parkWait

// Registry is every machine this process acquired, by the machine each worker mapping names, and how many scopes are using each.
type Registry struct {
	acquire Acquirer
	mu      sync.Mutex
	current map[string]*entry
	// live is every entry a scope still counts toward or that is still being given back, retired or not: what Close waits for.
	live map[*entry]struct{}
	// drained is closed once a closed registry has nothing live.
	drained chan struct{}
	closed  bool
	// expiring counts idle timers that may still fire, so Close can wait for a give-back already in flight.
	expiring sync.WaitGroup
}

type entry struct {
	lease

	source Worker
	key    string
	refs   int
	// window is the longest ?idle= any spelling of the machine asked for, since honoring a shorter one silently ignores what an operator wrote down.
	window time.Duration
	// retired is a machine somebody watched die: nothing new may take it, and its last user gives it back without an idle window.
	retired bool
	idle    *time.Timer
	// gen tells a timer that fired late that the window it was armed for was superseded.
	gen int
	// gone is made when the entry is taken out of service and closed once its machine is given back.
	gone chan struct{}
}

// parked is an instance that outlives its users: every mapping of it, and every life of it after an eviction, is this one entry.
func (e *entry) parked() bool { return e.source.Rung == RungStopped }

// NewRegistry acquires through the clouds.
func NewRegistry() *Registry { return NewRegistryWith(acquire) }

// NewRegistryWith exists for a borrowable venue that is not a cloud — the only way to prove sharing without an account.
func NewRegistryWith(acquirer Acquirer) *Registry {
	return &Registry{acquire: acquirer, current: map[string]*entry{}, live: map[*entry]struct{}{}}
}

// Leases opens one scope over the registry.
func (r *Registry) Leases(workers map[string]Worker) *Leases {
	return &Leases{registry: r, held: map[string]*entry{}, source: maps.Clone(workers)}
}

// hold counts one more user of the machine a worker names, or answers what to wait on while that machine's last park is still landing.
func (r *Registry) hold(worker Worker) (*entry, <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := worker.registryKey()

	held, ok := r.current[key]
	if ok && held.gone != nil {
		return nil, held.gone
	}

	if !ok {
		held = &entry{source: worker, key: key}
		r.current[key] = held
		r.live[held] = struct{}{}
	}

	held.refs++
	held.window = max(held.window, worker.Idle)
	r.stopIdle(held)

	return held, nil
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
	// Gone already: Close gave it back as the process stopped.
	if held.refs > 0 || held.gone != nil {
		r.mu.Unlock()

		return nil
	}

	if immediate || r.closed || held.retired || held.window <= 0 || !held.holding() {
		r.takeOut(held)
		r.mu.Unlock()

		return r.giveBack(ctx, held)
	}

	held.gen++
	gen := held.gen

	r.expiring.Add(1)
	//nolint:contextcheck // the window outlives every caller's context, and the release bounds its own calls
	held.idle = time.AfterFunc(held.window, func() { r.expire(held, gen) })
	r.mu.Unlock()

	fmt.Printf("worker %s: nothing is using it; keeping it for %s (?idle=)\n", held.source.URL, held.window)

	return nil
}

func (r *Registry) expire(held *entry, gen int) {
	defer r.expiring.Done()

	r.mu.Lock()

	if held.refs > 0 || held.gen != gen || held.gone != nil {
		r.mu.Unlock()

		return
	}

	held.idle = nil
	r.takeOut(held)
	r.mu.Unlock()

	// Nothing that could cancel this is still around, and the release bounds its own calls.
	err := r.giveBack(context.Background(), held)
	if err != nil {
		fmt.Printf("warning: worker %s could not be given back after its idle window: %v\n", held.source.URL, err)
	}
}

// takeOut stops handing an entry out; the caller holds r.mu. A parked machine stays findable until its park lands: forgotten first, a scope resolving in between found the instance still running, took it with no release, and had it parked underneath it.
func (r *Registry) takeOut(held *entry) {
	held.gone = make(chan struct{})

	if !held.parked() {
		r.forget(held)
	}
}

// giveBack is takeOut's other half.
func (r *Registry) giveBack(ctx context.Context, held *entry) error {
	err := held.give(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.forget(held)
	delete(r.live, held)
	close(held.gone)

	if r.drained != nil && len(r.live) == 0 {
		close(r.drained)
		r.drained = nil
	}

	return err
}

// forget stops handing this entry out, so the next user acquires fresh.
func (r *Registry) forget(held *entry) {
	if r.current[held.key] == held {
		delete(r.current, held.key)
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
	drained := make(chan struct{})
	r.drained = drained

	idle := []*entry{}

	for _, held := range r.current {
		if held.refs == 0 && held.gone == nil {
			r.stopIdle(held)
			r.takeOut(held)
			idle = append(idle, held)
		}
	}

	if len(r.live) == 0 {
		close(drained)
		r.drained = nil
	}

	r.mu.Unlock()

	err := r.giveAll(ctx, idle)

	// Scopes still using a machine — a webhook's check is one nothing else waits for — would otherwise leave it to bill after the process is gone.
	bound := time.NewTimer(closeWait)
	defer bound.Stop()

	select {
	case <-drained:
	case <-ctx.Done():
		err = errors.Join(err, r.giveBackHeld(ctx))
	case <-bound.C:
		err = errors.Join(err, r.giveBackHeld(ctx))
	}

	r.expiring.Wait()

	return err
}

// giveBackHeld gives back whatever is still in use when Close stops waiting, since the process is exiting and its users with it; one already being given back is waited for, through its lease.
func (r *Registry) giveBackHeld(ctx context.Context) error {
	r.mu.Lock()

	held, giving := []*entry{}, []*entry{}

	for one := range r.live {
		if one.gone != nil {
			giving = append(giving, one)

			continue
		}

		r.takeOut(one)
		held = append(held, one)
	}

	r.mu.Unlock()

	for _, one := range held {
		fmt.Printf("warning: worker %s is still in use as this process stops; giving it back anyway\n", one.source.URL)
	}

	err := r.giveAll(ctx, held)

	for _, one := range giving {
		err = errors.Join(err, one.give(ctx))
	}

	return err
}

// giveAll releases concurrently, because slow Stops in turn could outlive the budget the later ones needed.
func (r *Registry) giveAll(ctx context.Context, entries []*entry) error {
	failures := make([]error, len(entries))

	var wg sync.WaitGroup

	for i, one := range entries {
		wg.Go(func() { failures[i] = r.giveBack(ctx, one) })
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

	machine, release, err := acquirer(ctx, worker)
	// A parked machine re-acquired after an eviction still owes the park its first start promised, however this acquisition found it.
	if release != nil {
		l.release = release
	}

	// The resolver leaving is no answer about the machine: kept, it failed every scope sharing the entry until the last of them let go.
	if err != nil && ctx.Err() != nil {
		return Worker{}, err
	}

	l.acquired, l.worker, l.err = true, machine, err

	return machine, err
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

// holding is a machine worth keeping warm: acquired, not abandoned since, and steps' to give back.
func (l *lease) holding() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.acquired && l.err == nil && l.release != nil
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

	for {
		held, landing := l.claim(tag, worker)
		if landing == nil {
			machine, err := held.resolve(ctx, worker, l.registry.acquire)
			if err != nil {
				return Worker{}, err
			}

			return dialOf(worker, machine), nil
		}

		err := awaitLanded(ctx, landing, worker)
		if err != nil {
			return Worker{}, err
		}
	}
}

// claim is this scope's entry for a tag, counted toward the registry's if it has none yet — or, while that machine's last park is landing, what to wait on first, outside every lock.
func (l *Leases) claim(tag string, worker Worker) (*entry, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()

	held := l.held[tag]

	// Another scope watched this machine die. This one still owes it a release, and needs a fresh one.
	if held != nil && l.registry.isRetired(held) {
		l.retired = append(l.retired, held)
		delete(l.held, tag)

		held = nil
	}

	if held != nil {
		return held, nil
	}

	held, landing := l.registry.hold(worker)
	if held != nil {
		l.held[tag] = held
	}

	return held, landing
}

func awaitLanded(ctx context.Context, landing <-chan struct{}, worker Worker) error {
	bound := time.NewTimer(parkWait)
	defer bound.Stop()

	select {
	case <-landing:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s: waiting for its last park to land: %w", worker.URL, ctx.Err())
	case <-bound.C:
		return fmt.Errorf("worker %s: its last park had not landed after %s", worker.URL, parkWait)
	}
}

// dialOf is the machine an entry holds as this spelling reaches it: every mapping of one parked instance shares its entry, and each connects its own way — its root, its shim.
func dialOf(spelling, machine Worker) Worker {
	if spelling.Rung != RungStopped || machine.Instance != spelling.Instance {
		return machine
	}

	return spelling.asStatic(spelling.Instance)
}

// Abandon is identity-checked so a stale notice cannot orphan the fresh machine a sibling re-acquired under the same tag, retires the machine so no other scope is handed it, and does not release it: a parallel sibling may still be inside the grace the notice promised, and a spot stop or hibernate leaves a live instance only the last user's release will ever end.
func (l *Leases) Abandon(tag, dialURL string) {
	l.mu.Lock()
	held, ok := l.held[tag]
	l.mu.Unlock()

	if !ok || !held.abandonIf(dialURL, l.source[tag]) || held.parked() {
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

// abandonIf never matches an acquisition in flight: a machine still being acquired is not the machine anybody watched die. A parked one is left for its next user to restart rather than retired, because its replacement is the same instance: as two entries, whichever ended first parked it under the other.
func (l *lease) abandonIf(dialURL string, spelling Worker) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if !l.acquired || l.err != nil || dialOf(spelling, l.worker).URL != dialURL {
		return false
	}

	if spelling.Rung == RungStopped {
		l.acquired, l.worker = false, Worker{}
	}

	return true
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
