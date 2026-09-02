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
// happens before the parse, so a --vars-file is part of the configuration,
// while a run_file: or system_file: is step content that reaches a run
// through the plan rather than through the parse. Those are picked up by the
// next swap or restart, as they were before this existed.
type configWatcher struct {
	target *web.Pipeline
	vars   VarFlags
}

func newConfigWatcher(target *web.Pipeline, vars VarFlags) *configWatcher {
	return &configWatcher{target: target, vars: vars}
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
				// Logged once per failed check rather than once per edit: a
				// file left broken keeps saying so, which is what an operator
				// tailing the log wants, and the page says it too (see
				// Pipeline.Hold).
				slog.Error("web.reload_refused", "pipeline", w.target.Slug, "error", err)

				continue
			}

			if swapped {
				fmt.Printf("steps web: %s reloaded %s\n", w.target.Slug, w.target.Path)
			}
		}
	}
}

// check loads the file and swaps it in if it is a configuration this daemon
// is not already serving. It reports whether it swapped.
//
// The whole load every time, rather than a cheap stat first: it is one read
// of a small local file, and the alternative is a second notion of "changed"
// — an mtime, a size — that has to agree with the revision hash forever. The
// hash the loader already computes is the one answer, and comparing it is
// what makes a re-saved but unedited file a no-op.
func (w *configWatcher) check(ctx context.Context) (bool, error) {
	name := w.target.Slug

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

	if cfg.Revision.SHA == w.target.Config().Revision.SHA {
		// Unchanged. Still clears a complaint left by an earlier broken save
		// that has since been reverted, which is the same file arriving at
		// the same configuration by a different route.
		w.target.SetConfig(cfg)

		return false, nil
	}

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
func (w *configWatcher) noteNewTriggers(cfg *config.Config) {
	had := len(trigger.Resources(w.target.Config()))
	has := len(trigger.Resources(cfg))

	if had == 0 && has > 0 {
		fmt.Printf("steps web: %s now has trigger: true resources; restart to begin polling them\n", w.target.Slug)
	}
}
