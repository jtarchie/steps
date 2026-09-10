package cli

// The half of the daemon that chooses a store driver and a workspace provider, which internal/web deliberately does not: it implements web.Manager, and the route calls it.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// daemonWriteBound is what a give-back write gets on its own context, since the request's is likeliest to be cancelled exactly when one runs.
const daemonWriteBound = 30 * time.Second

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

	// A database that is not there, or created but not yet filled in, is the
	// ordinary first start. Anything else — a corrupt file, a mode this
	// process cannot read, a schema this build cannot satisfy — is refused,
	// because the alternative is a daemon that starts clean, prints "no
	// pipelines set" and quietly stops polling and draining every pipeline
	// still in the file.
	if err != nil {
		if errors.Is(err, store.ErrNoState) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("web: could not read %s: %w", d.state, err)
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

	provider, err := d.provider(cfg, st, false)
	if err != nil {
		_ = st.Close()

		return fmt.Errorf("web: %q cannot run here: %w", name, err)
	}

	fmt.Printf("steps web: serving %s (config %s)\n", name, shortConfig(revision.SHA))

	d.start(name, cfg, st, provider, from)

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

	// Resolved BEFORE the record, because the record is durable and this is the last thing that can refuse: a workspace this machine cannot supply used to leave current_revision_id naming a configuration the next restart refused, and a daemon that refuses to start has no endpoint left to fix it through.
	provider, err := d.workspaceFor(cfg, st, existing)
	if err != nil {
		d.closeIfNew(st, serving)

		return web.SetResult{}, err
	}

	err = d.record(ctx, st, cfg, req)
	if err != nil {
		closeProvider(provider)
		d.closeIfNew(st, serving)

		return web.SetResult{}, err
	}

	if serving {
		d.replace(existing, cfg, provider, req.From)
	} else {
		d.start(name, cfg, st, provider, req.From)
	}

	fmt.Printf("steps web: %s set to config %s\n", name, shortConfig(cfg.Revision.SHA))

	return result, nil
}

// workspaceFor is the provider this set needs, nil when an unmoved workspace: keeps the one the pipeline already has — so the ordinary set changes no build directory underneath a run.
func (d *daemon) workspaceFor(cfg *config.Config, st store.Store, existing *servedPipeline) (workspace.Provider, error) {
	if existing == nil {
		return d.provider(cfg, st, false)
	}

	if reflect.DeepEqual(existing.target.Config().Workspace, cfg.Workspace) {
		return nil, nil //nolint:nilnil // "no new provider needed" is the answer, not a missing one
	}

	return d.provider(cfg, st, true)
}

func closeProvider(provider workspace.Provider) {
	if provider != nil {
		_ = provider.Close()
	}
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

// replace IS the list of everything re-derived when a configuration changes: the workspace built from it, the admission rules that live in SQL rather than in the Config, and the superseded revision nothing can reach any more.
func (d *daemon) replace(existing *servedPipeline, cfg *config.Config, provider workspace.Provider, from string) {
	if provider != nil {
		d.runner.SetProvider(existing.target.Slug, provider)
	}

	existing.target.SetConfig(cfg)
	existing.target.SetPath(from)

	// The daemon's context, never the request's: these are what make the queue obey the configuration just applied, and a sender that hung up must not leave serial: unwritten while the drain starts anyway.
	web.SyncQueueLimits(d.base, existing.target)
	d.sweep(d.base, existing.target)
}

// replacing suppresses Validate()'s stale-build sweep for a durable workspace.root:, because that sweep removes EVERY build directory under the root with no ownership check — which at startup clears a crashed process's leftovers and mid-flight deletes the tree of whatever is running.
func (d *daemon) provider(cfg *config.Config, st store.Store, replacing bool) (workspace.Provider, error) {
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

	// The other half of --artifact-store, which the setup() path this replaced did here: without it the flag mirrors nothing and warns about nothing, which reads as configured and binds nothing.
	err = attachArtifactStore(provider, st, d.exec.ArtifactStore)
	if err != nil {
		_ = provider.Close()

		return nil, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	return provider, nil
}

// start launches the two loops a served pipeline is: the drain that runs what the queue holds, and the poll that fills it.
func (d *daemon) start(
	name string, cfg *config.Config, st store.Store,
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

	// Before either loop, because ResetStaleRunning reads every running row as abandoned — true for a pipeline nothing here is running yet, false a moment later. On the loops' own context, since a sender that hung up must not leave the queue admitting jobs the pipeline forbade.
	web.PrepareQueue(loopCtx, target)
	web.SyncQueueLimits(loopCtx, target)
	d.sweep(loopCtx, target)

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

	// Its own context: the likeliest reason a destroy is running is that the terminal that asked has already gone, and the request's context aborts the DELETE mid-transaction with the pipeline already torn down.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	err = served.target.Store.Delete(writeCtx)

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

	served, held := d.served[from]
	if !held || served == nil {
		return fmt.Errorf("%w: %s", web.ErrNoSuchPipeline, from)
	}

	// Where it was set from travels with the identity: a rename moves the name, not the file somebody uploaded.
	setFrom := served.target.Path()

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	// The UPDATE first, and every scoped row reaches the pipeline by id so the loops go on working under the old name: the served map is not the whole file, and a name a `steps run` row already holds used to fail the UNIQUE constraint AFTER the source pipeline had been torn down and its handle closed — lost until a restart.
	err := served.target.Store.Rename(writeCtx, to)
	if err != nil {
		return fmt.Errorf("%w: could not rename %q to %q: %w", web.ErrRefused, from, to, err)
	}

	_, err = d.detach(from)
	if err != nil {
		return err
	}

	served.closer()
	d.runner.RemoveProvider(from)

	err = d.restoreHeld(writeCtx, to, setFrom)
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

	// Cancelled in one pass first, so the daemon's stop time is the longest pipeline's wind-down rather than the sum of all of them.
	for name, served := range d.served {
		d.server.Remove(name)
		served.cancel()
	}

	for _, served := range d.served {
		served.loops.Wait()
		served.closer()
	}

	d.served = map[string]*servedPipeline{}

	// The providers are the runner's, and only their Close removes the temp root each one created — closing the store alone left a steps-* tree behind on every shutdown.
	d.runner.Close()
}
