package main

// The dispatcher is the admission controller for durably-queued webhook runs.
// handleHook only ever writes a `queued` run row; this single goroutine is what
// promotes those rows to `running`, bounded by a global concurrency cap and each
// hook's maxInFlight. Its first pass re-drains any rows that survived a serve
// restart (durability), and a buffered signal wakes it whenever a run is
// enqueued or a slot frees. All slot acquisition is non-blocking try-acquire, so
// no goroutine ever waits while holding a semaphore — the design is deadlock-free
// by construction.

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
)

type dispatcher struct {
	eng       *engine.Engine
	store     journal.Store
	byMachine map[string]*servedMachine // machine name -> spec (queued rows carry Machine, not path)
	global    chan struct{}             // cap = global --max-in-flight
	perHook   map[string]chan struct{}  // machine name -> cap = maxInFlight
	signal    chan struct{}             // buffered(1): enqueue or slot-freed nudge
	inflight  sync.Map                  // runID -> struct{}: launched, not yet off "queued"
}

func newDispatcher(eng *engine.Engine, store journal.Store, machines map[string]*servedMachine, maxInFlight int) *dispatcher {
	if maxInFlight <= 0 {
		maxInFlight = runtime.NumCPU()
	}
	d := &dispatcher{
		eng:       eng,
		store:     store,
		byMachine: make(map[string]*servedMachine, len(machines)),
		global:    make(chan struct{}, maxInFlight),
		perHook:   make(map[string]chan struct{}, len(machines)),
		signal:    make(chan struct{}, 1),
	}
	for name, sm := range machines {
		d.byMachine[name] = sm
		d.perHook[name] = make(chan struct{}, sm.maxInFlight())
	}
	return d
}

// poke wakes the dispatcher without blocking if a wake is already pending.
func (d *dispatcher) poke() {
	select {
	case d.signal <- struct{}{}:
	default:
	}
}

// run drains the queue, then sleeps until the next enqueue or freed slot. The
// initial drain is the crash-recovery scan: queued rows from a prior serve.
func (d *dispatcher) run(ctx context.Context) {
	for {
		d.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-d.signal:
		}
	}
}

// drain launches as many queued runs as the caps allow, oldest first. It stops
// the pass when the global cap is exhausted and skips a hook that is at its own
// cap (other hooks' rows may still fit).
func (d *dispatcher) drain(ctx context.Context) {
	queued, err := d.store.ListRunsByStatus(ctx, journal.StatusQueued)
	if err != nil {
		d.eng.Listener.Warn("dispatcher: listing queued runs", "err", err.Error())
		return
	}
	for _, run := range queued {
		if _, busy := d.inflight.Load(run.ID); busy {
			continue // already launched this pass or a prior one
		}
		hook := d.byMachine[run.Machine]
		if hook == nil {
			continue // machine not registered in this serve — leave it queued
		}
		select { // 1) global slot first
		case d.global <- struct{}{}:
		default:
			return // no global capacity: nothing more can start this pass
		}
		select { // 2) then the per-hook slot
		case d.perHook[run.Machine] <- struct{}{}:
		default:
			<-d.global // release the global slot before moving on (the one invariant)
			continue   // this hook is full; another hook's rows may still fit
		}
		d.inflight.Store(run.ID, struct{}{})
		go d.execute(ctx, run.ID, run.Machine, hook)
	}
}

// execute runs one dispatched run to its next stopping point (terminal or a
// human gate — a park returns and frees the slot; the gate is answered later
// via the UI/CLI, not the dispatcher). The run is bounded by the server
// lifetime ctx (shutdown cancels it) with a hard 30-minute ceiling. The defer
// releases both semaphores and nudges the dispatcher to fill the freed slot.
func (d *dispatcher) execute(ctx context.Context, runID, machineName string, hook *servedMachine) {
	defer func() {
		<-d.perHook[machineName]
		<-d.global
		d.inflight.Delete(runID)
		d.poke()
	}()
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, err := d.eng.StartQueued(runCtx, hook.m, runID)
	if err != nil {
		d.eng.Listener.Warn("queued run failed", "run", runID, "machine", hook.m.Name, "err", err.Error())
		// Demote out of the queue so a persistent start error can't hot-loop
		// the dispatcher. If this also fails the queue list will fail too, so
		// drain returns early rather than spinning.
		_ = d.store.UpdateRun(ctx, runID, journal.StatusFailed, "")
	}
}
