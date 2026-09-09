package cli

// The half of the daemon that chooses a store driver and a workspace provider, which internal/web deliberately does not: it implements web.Manager, and the route calls it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/trigger"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
)

// daemon holds the pipelines this process serves, and everything started for each one.
type daemon struct {
	server   *web.Server
	runner   *web.LocalRunner
	state    string
	exec     ExecFlags
	history  HistoryFlags
	interval time.Duration
	// Every pipeline's loops hang off a child of the process's own lifetime, so destroying one stops its loops and no others.
	base context.Context //nolint:containedctx // the daemon outlives any one request, and a set started from a request must not die with it
	// The verbs are serialized because each rebuilds a pipeline's world: interleaved, a destroy closes a store a concurrent set just handed to a poller.
	mu     sync.Mutex
	served map[string]*servedPipeline
}

// servedPipeline is one pipeline plus what has to be shut down when it goes.
type servedPipeline struct {
	target *web.Pipeline
	cancel context.CancelFunc
	loops  *sync.WaitGroup
	closer func()
}

// newDaemon wires the manager into the server it registers pipelines with.
func newDaemon(
	ctx context.Context, server *web.Server, runner *web.LocalRunner,
	state string, exec ExecFlags, history HistoryFlags, interval time.Duration,
) *daemon {
	return &daemon{
		server:   server,
		runner:   runner,
		state:    state,
		exec:     exec,
		history:  history,
		interval: interval,
		base:     ctx,
		served:   map[string]*servedPipeline{},
	}
}

// load is what makes a restart pick up where the last process left off, with nobody setting anything again.
//
//nolint:contextcheck // opening a database and closing a handle are not context-taking operations in this driver
func (d *daemon) load(ctx context.Context) error {
	reader, err := sqlite.OpenReader(d.state)
	if errors.Is(err, store.ErrNoState) {
		return nil
	}

	// A database that is not there is the ordinary first start; one this build cannot write is not.
	if err != nil {
		if errors.Is(err, store.ErrSchemaVersion) {
			return fmt.Errorf("web: %w", err)
		}

		return nil
	}

	rows, err := reader.Pipelines(ctx)

	closeErr := reader.Close()

	if err != nil {
		return fmt.Errorf("web: could not read %s: %w", d.state, err)
	}

	if closeErr != nil {
		return fmt.Errorf("web: could not read %s: %w", d.state, closeErr)
	}

	for _, row := range rows {
		// Recorded by a `steps run` against this file rather than set into this daemon: history worth keeping, and no configuration to serve.
		if row.CurrentSHA == "" {
			continue
		}

		err = d.restore(ctx, row.Name, row.Path)
		if err != nil {
			return err
		}
	}

	return nil
}

// restore serves a pipeline from the database, which after a restart is the only copy of it there is.
func (d *daemon) restore(ctx context.Context, name, from string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.restoreHeld(ctx, name, from)
}

// restoreHeld is restore for a caller already holding the lock, so a rename does not drop it between forgetting a name and serving the new one.
//
//nolint:contextcheck // opening and closing a store take none, and the placement check deliberately reads the daemon's own context — see accept
func (d *daemon) restoreHeld(ctx context.Context, name, from string) error {
	st, err := sqlite.OpenStore(d.state, name)
	if err != nil {
		return fmt.Errorf("web: could not open state for %q: %w", name, err)
	}

	revision, found, err := st.CurrentRevision(ctx)
	if err != nil {
		_ = st.Close()

		return fmt.Errorf("web: could not read the configuration of %q: %w", name, err)
	}

	if !found {
		_ = st.Close()

		return fmt.Errorf("web: %q has no configuration set", name)
	}

	cfg, err := d.accept(name, revision.Source, revision.Includes)
	if err != nil {
		_ = st.Close()

		// Refused rather than skipped: serving the rest while silently dropping one is a daemon that looks healthy and is missing a pipeline.
		return fmt.Errorf("web: %q cannot run here: %w", name, err)
	}

	provider, err := d.provider(cfg, false)
	if err != nil {
		_ = st.Close()

		return fmt.Errorf("web: %q cannot run here: %w", name, err)
	}

	fmt.Printf("steps web: serving %s (config %s)\n", name, shortConfig(revision.SHA))

	d.start(ctx, name, cfg, st, provider, from)

	return nil
}

// Set is the only way a pipeline arrives or changes.
//
//nolint:contextcheck // as restoreHeld: the validation reads the daemon's context on purpose, and opening a store takes none
func (d *daemon) Set(ctx context.Context, name string, req web.SetRequest) (web.SetResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	cfg, err := d.accept(name, req.Source, req.Includes)
	if err != nil {
		return web.SetResult{}, err
	}

	existing, serving := d.served[name]

	st, err := d.stateFor(name, existing)
	if err != nil {
		return web.SetResult{}, err
	}

	result, err := d.check(ctx, st, cfg, req)
	if err != nil {
		d.closeIfNew(st, serving)

		return web.SetResult{}, err
	}

	err = d.record(ctx, st, cfg, req)
	if err != nil {
		d.closeIfNew(st, serving)

		return web.SetResult{}, err
	}

	if serving {
		err = d.replace(ctx, existing, cfg, req.From)
	} else {
		err = d.adopt(ctx, name, cfg, st, req.From)
	}

	if err != nil {
		return web.SetResult{}, err
	}

	fmt.Printf("steps web: %s set to config %s\n", name, shortConfig(cfg.Revision.SHA))

	return result, nil
}

// stateFor reuses the handle already serving this pipeline: a second one on the same file is a second pool contending for its write lock.
func (d *daemon) stateFor(name string, existing *servedPipeline) (store.Store, error) {
	if existing != nil {
		return existing.target.Store, nil
	}

	st, err := sqlite.OpenStore(d.state, name)
	if err != nil {
		return nil, fmt.Errorf("could not open state for %q: %w", name, err)
	}

	return st, nil
}

// closeIfNew releases only a handle this set opened, since an already-served pipeline is still using its own.
func (d *daemon) closeIfNew(st store.Store, serving bool) {
	if !serving {
		_ = st.Close()
	}
}

// accept is everything `steps validate` checks except the network, run HERE because every one of those answers is about this machine — and it is the bar `steps run` enforces, so a set that passes cannot produce a run that dies at preflight.
func (d *daemon) accept(name, source string, includes map[string]string) (*config.Config, error) {
	cfg, err := config.Parse([]byte(source), name, config.Bundle(includes))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	err = fileProblems(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	problems := cfg.CheckEnvironment()
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s cannot run here:\n%s", web.ErrRefused, name, renderProblems(problems))
	}

	// The DAEMON's context, not the request's: --worker was parsed onto the
	// process's, and a set carries none of it — the same reason the webhook
	// handler holds one. A polled resource whose tags: this daemon cannot
	// place can never be checked, and the poll loop's own reaction is to log
	// that every interval forever, so it is refused here where somebody reads.
	err = pipeline.ValidatePipelinePlacement(d.base, cfg, trigger.Resources(cfg))
	if err != nil {
		return nil, fmt.Errorf("%w: %s cannot run here: %w", web.ErrRefused, name, err)
	}

	d.history.Apply(cfg)

	return cfg, nil
}

// check refuses a set whose sender diffed against a configuration that has since moved, which is what makes the diff worth showing.
func (d *daemon) check(ctx context.Context, st store.Store, cfg *config.Config, req web.SetRequest) (web.SetResult, error) {
	current, found, err := st.CurrentRevision(ctx)
	if err != nil {
		return web.SetResult{}, fmt.Errorf("could not read the current configuration: %w", err)
	}

	// An empty expectation is a sender that did not look, which a script legitimately is.
	if req.ExpectSHA != "" && req.ExpectSHA != current.SHA {
		return web.SetResult{}, fmt.Errorf("%w: it is now %s", web.ErrRevisionMoved, shortConfig(current.SHA))
	}

	return web.SetResult{
		SHA:       cfg.Revision.SHA,
		Created:   !found,
		Unchanged: found && current.SHA == cfg.Revision.SHA,
	}, nil
}

// record interns the configuration and makes it the one this pipeline serves.
func (d *daemon) record(ctx context.Context, st store.Store, cfg *config.Config, req web.SetRequest) error {
	err := RecordRevision(ctx, st, cfg)
	if err != nil {
		return err
	}

	err = st.SetCurrentRevision(ctx, cfg.Revision.SHA, req.From)
	if err != nil {
		return fmt.Errorf("could not set the configuration: %w", err)
	}

	err = st.SetSourcePath(ctx, req.From)
	if err != nil {
		return fmt.Errorf("could not record where the configuration was set from: %w", err)
	}

	return nil
}

// adopt starts serving a pipeline this daemon did not hold.
func (d *daemon) adopt(ctx context.Context, name string, cfg *config.Config, st store.Store, from string) error {
	provider, err := d.provider(cfg, false)
	if err != nil {
		_ = st.Close()

		return err
	}

	d.start(ctx, name, cfg, st, provider, from)

	return nil
}

// replace IS the list of everything re-derived when a configuration changes: the workspace built from it, the admission rules that live in SQL rather than in the Config, and the superseded revision nothing can reach any more.
func (d *daemon) replace(ctx context.Context, existing *servedPipeline, cfg *config.Config, from string) error {
	// An unmoved workspace: keeps its provider, so the ordinary set changes no build directory underneath a run.
	if !reflect.DeepEqual(existing.target.Config().Workspace, cfg.Workspace) {
		provider, err := d.provider(cfg, true)
		if err != nil {
			return err
		}

		d.runner.SetProvider(existing.target.Slug, provider)
	}

	existing.target.SetConfig(cfg)
	existing.target.SetPath(from)

	web.SyncQueueLimits(ctx, existing.target)
	d.sweep(ctx, existing.target)

	return nil
}

// replacing suppresses Validate()'s stale-build sweep for a durable workspace.root:, because that sweep removes EVERY build directory under the root with no ownership check — which at startup clears a crashed process's leftovers and mid-flight deletes the tree of whatever is running.
func (d *daemon) provider(cfg *config.Config, replacing bool) (workspace.Provider, error) {
	keep := d.exec.KeepWorkspace
	if replacing && cfg.Workspace != nil && cfg.Workspace.Root != "" {
		keep = true
	}

	provider, err := workspace.NewProvider(cfg.Workspace, keep)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace: %w", web.ErrRefused, err)
	}

	err = provider.Validate()
	if err != nil {
		_ = provider.Close()

		return nil, fmt.Errorf("%w: workspace: %w", web.ErrRefused, err)
	}

	return provider, nil
}

// start launches the two loops a served pipeline is: the drain that runs what the queue holds, and the poll that fills it.
//
//nolint:contextcheck // the loops hang off the DAEMON's lifetime, not the request's — see the comment on loopCtx
func (d *daemon) start(
	ctx context.Context, name string, cfg *config.Config, st store.Store,
	provider workspace.Provider, from string,
) {
	bus := events.New(pipeline.StoreSink(st))
	target := web.NewPipeline(name, from, cfg, st, bus)

	// Rooted in the daemon's lifetime rather than the request's: a set is over in milliseconds and what it starts has to outlive it.
	loopCtx, cancel := context.WithCancel(d.base)

	target.Webhook = trigger.WebhookHandler(loopCtx, target.Config, st)

	err := d.server.Add(target)
	// Only reachable if two sets of one name interleaved, which the lock above prevents, so this is the assertion rather than a path.
	if err != nil {
		cancel()
		slog.Error("web.register", "pipeline", name, "error", err)

		return
	}

	d.runner.SetProvider(name, provider)

	// Before either loop, because ResetStaleRunning reads every running row as abandoned — true for a pipeline nothing here is running yet, false a moment later.
	web.PrepareQueue(ctx, target)
	web.SyncQueueLimits(ctx, target)
	d.sweep(ctx, target)

	loops := &sync.WaitGroup{}
	loops.Add(2)

	go func() {
		defer loops.Done()

		d.runner.DrainPipeline(loopCtx, target)
	}()

	go func() {
		defer loops.Done()

		// The METHOD, not its result: a later set swaps the configuration under this loop, and a value would pin it to whatever was set first.
		pollErr := trigger.Poll(loopCtx, target.Config, st, d.interval)
		if pollErr != nil {
			slog.Error("web.poll_stopped", "pipeline", name, "error", pollErr)
		}
	}()

	d.served[name] = &servedPipeline{
		target: target,
		cancel: cancel,
		loops:  loops,
		closer: func() {
			// The bus first: it drains queued events INTO the store, so
			// closing the store first throws away the tail of whatever run
			// was in flight.
			bus.Close()
			_ = st.Close()
		},
	}
}

// A set orphans a revision as readily as a reaped run does, and an operator iterating mints a multi-kilobyte row per upload.
func (d *daemon) sweep(ctx context.Context, target *web.Pipeline) {
	err := target.Store.Prune(ctx, store.Retention{}, "")
	if err != nil {
		slog.Warn("web.prune_failed", "pipeline", target.Slug, "error", err)
	}
}

// Destroy forgets a pipeline: its loops, its handle, and everything recorded under it.
func (d *daemon) Destroy(ctx context.Context, name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	served, err := d.detach(name)
	if err != nil {
		return err
	}

	err = served.target.Store.Delete(ctx)

	served.closer()
	d.runner.RemoveProvider(name)

	if err != nil {
		return fmt.Errorf("could not destroy %q: %w", name, err)
	}

	fmt.Printf("steps web: %s destroyed\n", name)

	return nil
}

// The handle is rebuilt rather than relabelled: the route, the pipelines row and the scope an agent pin is keyed by are ONE string, so moving only the row leaves two of the three answering to a name nothing else uses.
func (d *daemon) Rename(ctx context.Context, from, to string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, taken := d.served[to]; taken {
		return fmt.Errorf("%w: this daemon already serves a pipeline called %q", web.ErrRefused, to)
	}

	served, err := d.detach(from)
	if err != nil {
		return err
	}

	// Where it was set from travels with the identity: a rename moves the
	// name, not the file somebody uploaded.
	setFrom := served.target.Path()

	err = served.target.Store.Rename(ctx, to)

	served.closer()
	d.runner.RemoveProvider(from)

	if err != nil {
		return fmt.Errorf("could not rename %q: %w", from, err)
	}

	err = d.restoreHeld(ctx, to, setFrom)
	if err != nil {
		return err
	}

	fmt.Printf("steps web: %s renamed to %s\n", from, to)

	return nil
}

// detach waits for the loops before returning, so nothing is still reading a handle its caller is about to close.
func (d *daemon) detach(name string) (*servedPipeline, error) {
	served, held := d.served[name]
	if !held || served == nil {
		return nil, fmt.Errorf("%w: %s", web.ErrNoSuchPipeline, name)
	}

	d.server.Remove(name)
	delete(d.served, name)

	served.cancel()
	served.loops.Wait()

	return served, nil
}

// Close stops every pipeline's loops and releases what they held.
func (d *daemon) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()

	for name, served := range d.served {
		d.server.Remove(name)
		served.cancel()
		served.loops.Wait()
		served.closer()
	}

	d.served = map[string]*servedPipeline{}
}
