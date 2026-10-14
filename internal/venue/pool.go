package venue

// Exclusive mappings (#205): a launch rung written ?exclusive gives each job a machine of its own, from a pool capped by ?max=. Scheme-agnostic on purpose: every launch rung is one Acquirer away from the same pool.

import (
	"context"
	"fmt"
	"slices"

	"github.com/jtarchie/steps/internal/events"
)

// pool is one exclusive mapping's machines. Its count lives in this process's memory only, which is what spares it Concourse's leaked and double-counted rows (#6648, #7791, #6206); a restart forgets it, as it forgets a shared machine.
type pool struct {
	key string
	max int
	// members are the machines in service: each serves one job at a time, and any of them carries a check.
	members []*entry
	// reserved keeps a job's place when its machine is retired under it, so its replacement does not queue behind newer waiters.
	reserved map[*Leases]int
	// waiters are served first come, first served: Concourse wakes them at random, and FIFO costs nothing in one process and cannot starve anybody.
	waiters []*waiter
	// used counts let-gos, so the most recently used machine is handed out first and the extras a burst made expire on ?idle=.
	used int
}

type waiter struct {
	pool   *pool
	scope  *Leases
	worker Worker
	// got is buffered, so a handover under the registry's lock never blocks on a waiter that is leaving.
	got chan *entry
}

func (p *pool) room() bool {
	if p.max == 0 {
		return true
	}

	size := len(p.members)
	for _, places := range p.reserved {
		size += places
	}

	return size < p.max
}

// warmest is the member last let go of: for a job, one no job holds and whose acquisition did not fail; for a check, any.
func (p *pool) warmest(forJob bool) *entry {
	var best *entry

	for _, one := range p.members {
		if forJob && (one.jobs > 0 || one.failed()) {
			continue
		}

		if best == nil || one.used > best.used {
			best = one
		}
	}

	return best
}

// failed is an acquisition that answered with an error, which a new job must not be handed: the error sticks to the entry.
func (l *lease) failed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.acquired && l.err != nil
}

// pending is what a claim has to wait on before it can be counted: a park landing, a sibling step's claim on the same pool, or this scope's turn in the pool.
type pending struct {
	landing <-chan struct{}
	sibling <-chan struct{}
	queued  *waiter
}

func (p pending) await(ctx context.Context, worker Worker) error {
	if p.sibling == nil {
		return awaitLanded(ctx, p.landing, worker)
	}

	select {
	case <-p.sibling:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("worker %s: waiting for a machine: %w", worker.URL, ctx.Err())
	}
}

// claimPool is claim for an exclusive mapping; the caller holds l.mu. A job takes one machine per pool however many of its tags or parallel steps name it, so its steps share the files already there and never wait on each other.
func (l *Leases) claimPool(tag string, worker Worker) (*entry, pending) {
	key := worker.registryKey()

	if settling, ok := l.claiming[key]; ok {
		return nil, pending{sibling: settling}
	}

	for _, other := range l.held {
		if other.key == key && l.registry.share(other, worker, l) {
			l.held[tag] = other

			return other, pending{}
		}
	}

	held, queued, warm := l.registry.take(l, worker)
	if held == nil {
		l.claiming[key] = make(chan struct{})

		return nil, pending{queued: queued}
	}

	l.held[tag] = held

	if warm {
		l.warm[tag] = held
	}

	return held, pending{}
}

// queue waits this job's turn with no bound of its own: only the job ending, or its abort, stops it.
func (l *Leases) queue(ctx context.Context, tag string, worker Worker, queued *waiter) (*entry, error) {
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		key := worker.registryKey()
		close(l.claiming[key])
		delete(l.claiming, key)
	}()

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: all %d of its machines (?max=) are taken by other jobs; waiting for one", worker.URL, worker.Max))

	select {
	case held := <-queued.got:
		l.adopt(tag, held)

		return held, nil
	case <-ctx.Done():
		// Handed over in the same instant: counted toward this scope, so its end gives it back to the next waiter.
		if held := l.registry.leave(queued); held != nil {
			l.adopt(tag, held)
		}

		return nil, fmt.Errorf("worker %s: waiting for one of its %d machines: %w", worker.URL, worker.Max, ctx.Err())
	}
}

func (l *Leases) adopt(tag string, held *entry) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.held[tag] = held
}

// take counts a scope toward a pool machine, or queues it; warm is a machine only an idle timer was holding.
func (r *Registry) take(scope *Leases, worker Worker) (*entry, *waiter, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := worker.registryKey()

	p := r.pools[key]
	if p == nil {
		p = &pool{key: key, max: worker.Max, reserved: map[*Leases]int{}}
		r.pools[key] = p
	}

	if scope.rides {
		held := p.warmest(false)
		warm := held != nil && held.refs == 0

		if held == nil {
			held = r.member(p, worker)
		}

		r.count(held, worker, nil)

		return held, nil, warm
	}

	reserved := p.takeReserved(scope)
	if reserved || len(p.waiters) == 0 {
		if held := p.warmest(true); held != nil {
			warm := held.refs == 0
			r.count(held, worker, scope)

			return held, nil, warm
		}

		if reserved || p.room() {
			held := r.member(p, worker)
			r.count(held, worker, scope)

			return held, nil, false
		}
	}

	queued := &waiter{pool: p, scope: scope, worker: worker, got: make(chan *entry, 1)}
	p.waiters = append(p.waiters, queued)

	return nil, queued, false
}

// takeReserved spends a place kept for scope, reporting whether there was one.
func (p *pool) takeReserved(scope *Leases) bool {
	places, kept := p.reserved[scope]
	if !kept {
		return false
	}

	if places <= 1 {
		delete(p.reserved, scope)
	} else {
		p.reserved[scope] = places - 1
	}

	return true
}

// member is a new machine of the pool, not yet acquired; the caller holds r.mu.
func (r *Registry) member(p *pool, worker Worker) *entry {
	held := &entry{source: worker, key: p.key, windowBy: worker.URL, pool: p}
	p.members = append(p.members, held)
	r.live[held] = struct{}{}

	return held
}

// count is one more hold on a machine, a job's when job is not nil; the caller holds r.mu.
func (r *Registry) count(held *entry, worker Worker, job *Leases) {
	held.refs++

	if job != nil {
		held.jobs++
		held.holder = job
	}

	if worker.Idle > held.window {
		held.window, held.windowBy = worker.Idle, worker.URL
	}

	r.stopIdle(held)
}

// share counts a scope's second tag on the pool machine its first already holds, unless that machine has since been retired.
func (r *Registry) share(held *entry, worker Worker, scope *Leases) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if held.retired || held.gone != nil {
		return false
	}

	job := scope
	if scope.rides {
		job = nil
	}

	r.count(held, worker, job)

	return true
}

// leave takes a waiter out of its queue, or answers the machine it was handed in the meantime.
func (r *Registry) leave(queued *waiter) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	at := slices.Index(queued.pool.waiters, queued)
	if at >= 0 {
		queued.pool.waiters = slices.Delete(queued.pool.waiters, at, at+1)

		return nil
	}

	return <-queued.got
}

// letGo ends a job's hold on a pool machine and hands it to the next waiter; the caller holds r.mu and has already dropped the ref.
func (r *Registry) letGo(held *entry, immediate bool) {
	held.jobs--
	if held.jobs > 0 {
		return
	}

	held.holder = nil
	held.pool.used++
	held.used = held.pool.used

	if !slices.Contains(held.pool.members, held) || immediate {
		return
	}

	r.serve(held.pool)
}

// leavePool takes a machine out of its pool, which makes room for a waiter; the caller holds r.mu.
func (r *Registry) leavePool(held *entry) {
	p := held.pool

	at := slices.Index(p.members, held)
	if at < 0 {
		return
	}

	p.members = slices.Delete(p.members, at, at+1)
	r.serve(p)
}

// serve hands free machines, then room for new ones, to waiters in the order they came; the caller holds r.mu. A closed registry serves nobody: every scope was cancelled before Close.
func (r *Registry) serve(p *pool) {
	for len(p.waiters) > 0 && !r.closed {
		next := p.waiters[0]

		held := p.warmest(true)
		if held == nil {
			if !p.room() {
				return
			}

			held = r.member(p, next.worker)
		}

		p.waiters = p.waiters[1:]
		r.count(held, next.worker, next.scope)
		next.got <- held
	}
}

// reserveFor keeps the place of the job holding a pool machine that is being retired; the caller holds r.mu.
func (r *Registry) reserveFor(held *entry) {
	if held.pool == nil || held.holder == nil || held.retired || !slices.Contains(held.pool.members, held) {
		return
	}

	held.pool.reserved[held.holder]++
}

// unreserve gives up the places a scope kept and never took back.
func (r *Registry) unreserve(scope *Leases) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, p := range r.pools {
		if _, kept := p.reserved[scope]; kept {
			delete(p.reserved, scope)
			r.serve(p)
		}
	}
}

// inService is every entry still being handed out, shared or pooled; the caller holds r.mu.
func (r *Registry) inService() []*entry {
	all := make([]*entry, 0, len(r.current))

	for _, held := range r.current {
		all = append(all, held)
	}

	for _, p := range r.pools {
		all = append(all, p.members...)
	}

	return all
}
