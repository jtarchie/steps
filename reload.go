package main

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
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

	// Re-checked against the includes the SERVED configuration resolved: an
	// edit that changes which files are included changes the YAML too, so it
	// is caught by the hash before this list can be out of date.
	revision, err := w.vars.Revision(w.target.Path, w.target.Config().Revision.Includes)
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

	err = w.checkWorkspace(cfg)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	err = w.adopt(ctx, cfg)
	if err != nil {
		w.target.Hold(err)

		return false, err
	}

	return true, nil
}

// adopt makes a configuration the one this pipeline is served and run under,
// and IS the list of everything that has to be re-derived when it changes.
//
// One list, called from both ends: `steps web` adopts the configuration it
// parsed at startup through this same function, so anything added here is
// applied on the first load and on every reload without either place
// remembering the other. That is the point of it existing at all. Every entry
// below was once a fact copied out of the Config at startup and left behind
// by a swap, and each one was found the same way — not by a failing test, but
// by someone reading the diff and asking what else came from the file:
//
//   - the command line's retention limits, applied in place to the Config
//     startup parsed, so a swap reverted --run-history to the built-in default
//   - the revision, recorded before the swap so a run admitted between the two
//     names the configuration it is actually running
//   - the queue's admission rules, which live in SQL rather than in the Config,
//     so a serial group added by an edit rendered on the board while the queue
//     went on admitting both jobs
//   - the superseded configuration's row, unreachable the moment nothing ran
//     under it
//
// A new thing derived from configuration goes here, or it becomes the next
// entry in that list.
func (w *configWatcher) adopt(ctx context.Context, cfg *config.Config) error {
	w.history.Apply(cfg)

	err := recordRevision(ctx, w.target.Store, cfg)
	if err != nil {
		return err
	}

	w.target.SetConfig(cfg)

	web.SyncQueueLimits(ctx, w.target)

	// Best-effort: a configuration that has already been adopted is not
	// un-adopted by a sweep that did not run.
	err = w.target.Store.PruneRevisions(ctx)
	if err != nil {
		slog.Warn("web.reload_prune_failed", "pipeline", w.target.Slug, "error", err)
	}

	return nil
}

// checkWorkspace validates an edited `workspace:` block, and says that a
// restart is what adopts it.
//
// The provider is built once, at startup, and handed to every run for the
// life of the process — so an edited workspace: renders on every page and
// changes nothing about where runs are materialized. Worse than stale:
// Validate() is what refuses an unusable one, and it had run only at startup,
// so an edit that would have been rejected then was accepted and silently
// ignored. This restores the refusal, which is the half that matters: a
// pipeline that cannot build its workspaces should not be served as though it
// can.
//
// ponytail: the edited workspace is validated but not adopted. Upgrading it
// means rebuilding the provider and swapping it under the drain, which reads
// the provider map per run — so the map has to become something safe to write
// while a build reads it, and a run holding the old provider has to keep it
// until it finishes. That is a larger change than the gap is worth today, and
// leaving it unsaid was the actual defect.
func (w *configWatcher) checkWorkspace(cfg *config.Config) error {
	if reflect.DeepEqual(w.target.Config().Workspace, cfg.Workspace) {
		return nil
	}

	// keep=false: this provider exists to answer whether the configuration is
	// usable, and is closed before it materializes anything.
	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}

	defer func() { _ = provider.Close() }()

	err = provider.Validate()
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}

	fmt.Printf("steps web: %s changed workspace:; restart to run under it\n", w.target.Slug)

	return nil
}
