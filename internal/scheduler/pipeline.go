package scheduler

import (
	"sync/atomic"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// Pipeline is one held pipeline as the drain sees it: its name, its store handle, the bus its runs publish on, and the configuration being served. Two held pipelines may share a state FILE (see --db), but never a store handle: each one is scoped to its own pipeline row, which is what keeps their histories and caches apart.
type Pipeline struct {
	// Slug is the name `steps pipeline set` was given: the route, the store's pipelines.name and the Config's name are ONE identity, not three kept in agreement.
	Slug string
	// cfg is swapped by a `steps pipeline set` while handlers, the drain and the poller read it, so a plain field would be a data race rather than a stale read.
	cfg   atomic.Pointer[config.Config]
	Store store.Store
	// Bus carries live run events for runs this process itself executes.
	// Runs started by a separate `steps run` land in the store but not on
	// this bus, which is why every live view falls back to replaying the
	// stored events rather than assuming the bus saw everything.
	Bus *events.Bus
}

// NewPipeline is a constructor rather than a struct literal because a literal would leave cfg nil for whatever reads it before the caller filled it in.
func NewPipeline(slug string, cfg *config.Config, st store.Store, bus *events.Bus) *Pipeline {
	held := &Pipeline{Slug: slug, Store: st, Bus: bus}
	held.cfg.Store(cfg)

	return held
}

// Config is the configuration being served right now.
//
// Every reader goes through here rather than holding the pointer across a
// swap: a job started before a reload must run the plan it was queued
// against, which it does by taking this once, while the NEXT job takes the
// new one.
func (p *Pipeline) Config() *config.Config { return p.cfg.Load() }

// SetConfig swaps in a configuration a `steps pipeline set` just applied.
func (p *Pipeline) SetConfig(cfg *config.Config) { p.cfg.Store(cfg) }
