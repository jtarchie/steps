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
	mu     sync.Mutex
	held   map[string]*lease
	source map[string]Worker
}

// lease is one tag's machine: acquired at most once, released at most once.
type lease struct {
	// once guards acquisition, so parallel steps sharing a tag wait for one
	// machine rather than each starting their own.
	once sync.Once
	// worker is what to dial once acquired, and err is why that never
	// happened. A failure sticks for the job: acquisition is expensive and
	// the second answer is the first one again.
	worker Worker
	err    error
	// release returns the machine. nil when nothing was acquired.
	release func(context.Context) error
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

	held.once.Do(func() {
		held.worker, held.release, held.err = acquire(ctx, worker)
	})

	return held.worker, held.err
}

// ReleaseAll returns every machine this job acquired.
//
// Best effort and exhaustive: one failure must not strand the others, since
// what is being released costs money for as long as it runs. Errors are
// joined so a caller can report all of them.
func (l *Leases) ReleaseAll(ctx context.Context) error {
	l.mu.Lock()

	held := make([]*lease, 0, len(l.held))
	for _, one := range l.held {
		held = append(held, one)
	}

	l.held = map[string]*lease{}
	l.mu.Unlock()

	var failures []error

	for _, one := range held {
		if one.release == nil {
			continue
		}

		err := one.release(ctx)
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}
