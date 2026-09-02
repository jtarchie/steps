package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/trigger"
	"github.com/jtarchie/steps/internal/web"
)

// configWatcher keeps a served pipeline in step with the file it was loaded
// from.
//
// `steps web` read its YAML once, at startup, so applying an edit meant
// restarting the daemon — which drops whatever was running and is the reason
// anyone editing a pipeline first had to check whether a build was in flight.
//
// What it watches is the file and its vars, and nothing else: substitution
// happens before the parse, so a --vars-file is part of the configuration.
// A run_file: or system_file: is not, and neither is it covered by the
// revision hash — the hash is over the substituted pipeline file, taken
// before config.Load resolves the includes. An edit to one changes what a
// step executes without changing the pipeline, so it is picked up by the next
// run that reads it, as it was before this existed.
type configWatcher struct {
	target *web.Pipeline
	vars   VarFlags
	// history is the command line's retention limits, re-applied to every
	// configuration this loads. Startup applied them to the Config it parsed
	// and nothing else did: a swap replaces that object, so without this
	// `steps web --run-history 5` reverted to the built-in default at the
	// first tick, whether or not anyone had edited the file.
	history HistoryFlags
	// held is the last complaint logged, so a file left broken says so once
	// rather than once per tick — at a second apiece that is 86k identical
	// lines a day, which buries the save that finally fixed it.
	held string
}

func newConfigWatcher(target *web.Pipeline, vars VarFlags, history HistoryFlags) *configWatcher {
	return &configWatcher{target: target, vars: vars, history: history}
}

// Watch applies edits until the daemon stops.
//
// Its own interval rather than the trigger loop's: --interval is how often to
// ask a remote what it has, which is a network cost measured in seconds to
// minutes, while this is one local read. Sharing the trigger interval would
// have made a 5m poll mean a five-minute wait to see a saved file take
// effect.
func (w *configWatcher) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			swapped, err := w.check(ctx)
			if err != nil {
				// Once per distinct complaint, not once per tick: the page
				// carries the standing one (see Pipeline.Hold), so the log's
				// job is to timestamp the moment it started and the moment it
				// changed.
				if message := err.Error(); message != w.held {
					w.held = message

					slog.Error("web.reload_refused", "pipeline", w.target.Slug, "error", err)
				}

				continue
			}

			w.held = ""

			if swapped {
				fmt.Printf("steps web: %s reloaded %s\n", w.target.Slug, w.target.Path)
			}
		}
	}
}

// check loads the file and swaps it in if it is a configuration this daemon
// is not already serving. It reports whether it swapped.
//
// The hash first, and the parse only behind it. Not a stat: an mtime or a
// size would be a SECOND notion of "changed" that has to agree with the
// revision hash forever, and this is the revision hash — the same bytes, the
// same sha256, computed by the same function the loader calls. What it buys
// is that the steady state costs a read rather than a full parse, every
// validator, an exec.LookPath per stdio MCP server and a re-read of every
// run_file: include, once a second, forever.
func (w *configWatcher) check(ctx context.Context) (bool, error) {
	name := w.target.Slug

	revision, err := w.vars.Revision(w.target.Path)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	if revision.SHA == w.target.Config().Revision.SHA {
		// Unchanged, so the served Config is left exactly as it is — swapping
		// in an identical parse would throw away every in-place decision made
		// on the startup one (see history). The complaint still clears: a
		// broken save that has since been reverted is the same file arriving
		// at the same configuration by a different route.
		w.target.Clear()

		return false, nil
	}

	cfg, err := w.vars.Load(w.target.Path, name)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	// The gate: everything `steps validate` checks except the network. It is
	// the bar `steps run` already enforces before any step executes, so a
	// swap that passes here cannot produce a run that dies at preflight —
	// and no save waits on a model endpoint or an MCP server to be answered.
	//nolint:contextcheck // expression compilation is CPU-bound and takes no context; steps validate calls this same check without one
	err = fileProblems(cfg)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	problems := cfg.CheckEnvironment()
	if len(problems) > 0 {
		err = fmt.Errorf("%s cannot run here:\n%s", w.target.Path, renderProblems(problems))
		w.target.Hold(err)

		return false, err
	}

	// The command line's limits stand in for what the pipeline did not set,
	// exactly as they did at startup — applied here rather than left to the
	// caller because this is the only place a newly parsed Config exists.
	w.history.Apply(cfg)

	// Recorded BEFORE the swap: the store handle is what StartRun reads, so a
	// run admitted between the two would otherwise pin the configuration it
	// is no longer running.
	err = recordRevision(ctx, w.target.Store, cfg)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	w.noteNewTriggers(cfg)
	w.target.SetConfig(cfg)

	// The queue admits from SQL, not from the Config: serial groups and
	// max_in_flight are mirrored into tables ClaimNextJob reads, so a swap
	// that changed either is not in effect until they are rewritten. Without
	// this the pages showed a `serial: true` the queue went on ignoring.
	web.SyncQueueLimits(ctx, w.target)

	return true, nil
}

// noteNewTriggers says so when an edit adds the first trigger: true get to a
// pipeline that had none.
//
// A stated limitation rather than a silent one: the daemon decides at startup
// whether a pipeline has anything to poll, and starts no poller when it does
// not — so a pipeline that gains its first trigger is served with the new
// configuration and polled by nothing until a restart. The alternative is
// supervising the poll loop across swaps, which is a bigger change than this
// slice, and being quiet about it would leave an operator watching a resource
// that is never checked.
//
// ponytail: the poller is not supervised across swaps. Upgrade path is for
// startWatchingConfig to own the poll goroutine's lifetime — start one on the
// 0 -> N transition, cancel it on N -> 0 — at which point this message and
// the limitation it announces both go.
func (w *configWatcher) noteNewTriggers(cfg *config.Config) {
	had := len(trigger.Resources(w.target.Config()))
	has := len(trigger.Resources(cfg))

	if had == 0 && has > 0 {
		fmt.Printf("steps web: %s now has trigger: true resources; restart to begin polling them\n", w.target.Slug)
	}
}
