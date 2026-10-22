package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// probeCache remembers what has already been verified in this process, so a
// long-lived `steps web` checks occasionally rather than on every poll. A
// process-wide cache rather than a per-run one for exactly that reason.
//
//nolint:gochecknoglobals // process-lifetime memo, deliberately shared across runs
var probeCache = &resultCache{entries: map[string]cacheEntry{}}

type cacheEntry struct {
	at  time.Time
	err error
}

type resultCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

// lookup returns a cached result when one is still within ttl. The entry
// carries WHEN the answer was established, which decisions that destroy state
// need — see probeModelCached.
func (c *resultCache) lookup(key string, ttl time.Duration, now time.Time) (cacheEntry, bool) {
	if ttl <= 0 {
		return cacheEntry{}, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || now.Sub(entry.at) > ttl {
		return cacheEntry{}, false
	}

	return entry, true
}

func (c *resultCache) store(key string, err error, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{at: now, err: err}
}

// ResetProbeCache clears everything preflight has verified AND every source
// pin it (or a mid-run cascade) selected. Tests use it to stay independent of
// each other; a test about the pin's own lifetime wants only the first half,
// since wiping the pin is what such a test is trying to observe.
//
// Every caller must be a serial test: this is process-wide state for all
// agents, so a `t.Parallel()` test that reached it would clear another's.
func ResetProbeCache() {
	probeCache.mu.Lock()
	probeCache.entries = map[string]cacheEntry{}
	probeCache.mu.Unlock()

	selectedSources.mu.Lock()
	defer selectedSources.mu.Unlock()

	selectedSources.by = map[pinScope]pinRecord{}
}

// probeModelCached probes a model, or answers from the cache, and reports
// WHEN the answer it returns was established.
//
// The timestamp is not bookkeeping: a decision that DESTROYS state has to
// insist on a fact from now. Collapsing it let a cached positive taken before
// an outage release a pin the mid-run cascade earned during one — the step
// then ran at the still-broken primary, cascaded, re-pinned, and did it again
// on the next poll, logging "the primary model answered preflight again"
// about a question nobody had asked in five minutes.
//
// A time rather than a did-I-send-it bool, because "just now" is a property
// of the ANSWER and not of the caller. Several agents legitimately share one
// target, so within a single preflight pass the first of them sends the
// request and the rest read it back out of the cache microseconds later — all
// of them holding an answer from this pass. Reporting that as stale stranded
// every agent but the first on its fallback permanently, since the keep also
// renews pinLifetime.
func probeModelCached(ctx context.Context, ri config.ResolvedInvocation, settings *config.Preflight) (time.Time, error) {
	// A CLI target is keyed on its own axis: "" is a perfectly ordinary
	// BaseURL for a CLI source, so a shared key space would let a CLI agent
	// and an endpoint-less HTTP one collide.
	//
	// APIKeyEnv is part of the key — an env var NAME, never its value —
	// because a probe exercises the credential too. A `fallback:` entry that
	// differs from its primary only in api_key_env is the documented key-
	// rotation shape (see nextViableFallback), and without this the fallback
	// read the PRIMARY's cached failure: a source that was serving runs
	// looked dead to reconsiderPin and to failOver alike.
	key := "model|" + ri.BaseURL + "|" + ri.ModelName + "|" + ri.APIKeyEnv
	if ri.CLI != "" {
		key = cliProbeKey(ri)
	}

	now := time.Now()

	entry, found := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.DebugContext(ctx, "preflight.cached", "target", key)

		return entry.at, entry.err
	}

	err := probeModel(ctx, ri, settings.ProbeTimeout())

	// A probe the CALLER abandoned learned nothing about the target. The
	// probe's own deadline firing IS a fact about the target and is cached;
	// ctx being done means Ctrl-C, a job deadline, or a shutdown, and
	// describeProbeError renders both identically, so without this an
	// aborted build left a healthy model looking dead for the whole cache
	// window.
	if ctx.Err() != nil {
		return now, err
	}

	probeCache.store(key, err, now)

	return now, err
}
