package web

// Which workspace a pipeline's runs materialize in, while that answer changes underneath them.

import (
	"log/slog"
	"sync"

	"github.com/jtarchie/steps/internal/workspace"
)

// providers is a map a `steps pipeline set` writes while the drain reads it, plus the accounting that lets a replaced provider be closed rather than leaked.
//
// A run must keep the provider it started with: Close removes the tree it created, so retiring one under a build in flight deletes that build's own workspace. Each lease counts its runs and the old one is closed only once they finish.
type providers struct {
	mu     sync.Mutex
	leases map[string]*providerLease
}

// providerLease is one pipeline's current provider and the runs still holding it.
type providerLease struct {
	provider workspace.Provider
	wg       sync.WaitGroup
}

func newProviders() *providers {
	return &providers{leases: map[string]*providerLease{}}
}

// take hands out the provider a run will use, and the release its caller owes.
func (p *providers) take(slug string) (workspace.Provider, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	lease, held := p.leases[slug]
	if !held {
		return nil, nil, false
	}

	lease.wg.Add(1)

	return lease.provider, lease.wg.Done, true
}

// set installs a provider and retires the one it replaces.
func (p *providers) set(slug string, provider workspace.Provider) {
	p.mu.Lock()
	previous := p.leases[slug]
	p.leases[slug] = &providerLease{provider: provider}
	p.mu.Unlock()

	retire(slug, previous)
}

// remove retires a destroyed pipeline's provider.
func (p *providers) remove(slug string) {
	p.mu.Lock()
	previous := p.leases[slug]
	delete(p.leases, slug)
	p.mu.Unlock()

	retire(slug, previous)
}

// retire closes a provider once the runs holding it have finished, in the background because a set must not block on somebody's twenty-minute build.
func retire(slug string, lease *providerLease) {
	if lease == nil {
		return
	}

	go func() {
		lease.wg.Wait()

		err := lease.provider.Close()
		if err != nil {
			slog.Warn("web.provider_close", "pipeline", slug, "error", err)
		}
	}()
}

// Close retires every provider, waiting for the runs holding them — the daemon's own shutdown, where there is nothing left to be responsive for.
func (p *providers) Close() {
	p.mu.Lock()
	leases := p.leases
	p.leases = map[string]*providerLease{}
	p.mu.Unlock()

	for slug, lease := range leases {
		lease.wg.Wait()

		err := lease.provider.Close()
		if err != nil {
			slog.Warn("web.provider_close", "pipeline", slug, "error", err)
		}
	}
}
