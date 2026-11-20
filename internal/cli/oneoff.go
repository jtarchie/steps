package cli

// The daemon's half of `steps run --target` (#283): a one-off is a pipeline the daemon names, marks, serves without a poll, queues one job of, and forgets once --one-off-history newer one-offs have finished.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// oneOffPrefix is reserved so a name the daemon mints is never one somebody set; the pipelines.one_off column, not the name, is what a restart reads.
const oneOffPrefix = "one-off."

// oneOffPoll is how often a one-off's queue is asked whether it has settled.
const oneOffPoll = time.Second

func isOneOff(name string) bool { return strings.HasPrefix(name, oneOffPrefix) }

func errOneOffName(name string) error {
	return fmt.Errorf("%w: %q: names starting %q are the daemon's own, for steps run --target", web.ErrRefused, name, oneOffPrefix)
}

// OneOff validates as a set does, on this machine, and queues the job before answering, so no pipeline exists that nothing would ever reap.
//
//nolint:contextcheck // as Set: the validation reads the daemon's context on purpose, and opening a store takes none
func (d *daemon) OneOff(ctx context.Context, req web.OneOffRequest) (web.OneOffResult, error) {
	name := oneOffPrefix + strings.ToLower(rand.Text()[:12])

	cfg, err := d.acceptOneOff(name, req)
	if err != nil {
		return web.OneOffResult{}, err
	}

	// Before the lock, as validation is: a gigabyte from a slow link must not stall every other verb.
	err = d.receiveUploads(name, req)
	if err != nil {
		return web.OneOffResult{}, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	st, err := d.stateFor(name, nil)
	if err != nil {
		d.forgetUploads(ctx, name)

		return web.OneOffResult{}, err
	}

	provider, err := d.workspaceFor(st, nil)
	if err != nil {
		release(st)
		d.forgetUploads(ctx, name)

		return web.OneOffResult{}, err
	}

	err = d.markOneOff(ctx, st)
	if err == nil {
		err = d.record(ctx, st, cfg, web.SetRequest{From: req.From})
	}

	if err != nil {
		closeProvider(provider)
		release(st)
		d.forgetUploads(ctx, name)

		return web.OneOffResult{}, err
	}

	d.start(name, cfg, st, provider, req.From, true)

	served, ok := d.served[name]
	if !ok {
		return web.OneOffResult{}, fmt.Errorf("could not serve the one-off %s", name)
	}

	// Its own context, as record's: a sender that hung up after the pipeline started must still leave it something to run and so to reap after. The actor travels with the values.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	queued, err := d.enqueueOneOff(writeCtx, served.target.Pipeline, req)
	if err != nil {
		return web.OneOffResult{}, errors.Join(fmt.Errorf("could not queue %s: %w", req.Job, err), d.destroyLocked(writeCtx, name))
	}

	slog.InfoContext(ctx, "web.one_off", "pipeline", name, "job", req.Job, "config", shortConfig(cfg.Revision.SHA))

	return web.OneOffResult{Pipeline: name, Job: req.Job, Queued: queued}, nil
}

// acceptOneOff is accept plus what only a one-off asks: the job it names, and no pin with a force, which the daemon cannot honor together (web's queuePinnedTrigger).
func (d *daemon) acceptOneOff(name string, req web.OneOffRequest) (*config.Config, error) {
	if len(req.Pin) > 0 && req.Force {
		return nil, fmt.Errorf("%w: a pinned run cannot also skip the cache", web.ErrRefused)
	}

	cfg, err := d.accept(name, req.Source, convertIncludes[string](req.Includes))
	if err != nil {
		return nil, err
	}

	job, err := cfg.FindJob(req.Job)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	gets := job.Gets()
	for _, get := range req.Inputs {
		if _, ok := gets[get]; !ok {
			return nil, fmt.Errorf("%w: --input %q: job %q has no get named %q", web.ErrRefused, get, job.Name, get)
		}
	}

	return cfg, nil
}

func (d *daemon) enqueueOneOff(ctx context.Context, target *scheduler.Pipeline, req web.OneOffRequest) (int64, error) {
	if len(req.Pin) > 0 {
		return d.runner.EnqueuePinned(ctx, target, req.Job, "one-off, pinned", req.Pin) //nolint:wrapcheck // the caller wraps it naming the job
	}

	return d.runner.Enqueue(ctx, target, req.Job, "one-off", req.Force) //nolint:wrapcheck // as above
}

// trimWhenSettled asks for a trim each time the one-off's queue settles: the first is its own run finishing, a later one a retry from its page. The trim runs outside this loop because a destroy waits for the loops to end.
func (d *daemon) trimWhenSettled(ctx context.Context, st store.Queue) {
	ticker := time.NewTicker(oneOffPoll)
	defer ticker.Stop()

	settled := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := queueSettled(ctx, st)
		if now && !settled {
			d.reaping.Add(1)

			go func() {
				defer d.reaping.Done()

				d.trimOneOffs(context.WithoutCancel(ctx))
			}()
		}

		settled = now
	}
}

// finishedOneOff is a served one-off with nothing waiting or running, and when its newest run ended.
type finishedOneOff struct {
	name string
	at   time.Time
}

// trimOneOffs forgets the finished one-offs past the newest oneOffHistory, oldest first, as run_history trims a job's runs: a one-off still running is never counted nor reaped.
func (d *daemon) trimOneOffs(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.oneOffHistory <= 0 {
		return
	}

	var finished []finishedOneOff

	for name, served := range d.served {
		if !served.oneOff || !queueSettled(ctx, served.target.Store) {
			continue
		}

		finished = append(finished, finishedOneOff{name: name, at: newestFinish(ctx, served.target.Store)})
	}

	if len(finished) <= d.oneOffHistory {
		return
	}

	slices.SortFunc(finished, func(a, b finishedOneOff) int { return b.at.Compare(a.at) })

	for _, gone := range finished[d.oneOffHistory:] {
		err := d.destroyLocked(ctx, gone.name)
		if err != nil && !errors.Is(err, web.ErrNoSuchPipeline) {
			slog.ErrorContext(ctx, "web.one_off.unreaped", "pipeline", gone.name, "error", err)

			continue
		}

		slog.InfoContext(ctx, "web.one_off.reaped", "pipeline", gone.name, "kept", d.oneOffHistory)
	}
}

// newestFinish is when the one-off's newest run ended; one with no run, a crash between its record and its queue, is the oldest there is.
func newestFinish(ctx context.Context, st store.Runs) time.Time {
	runs, err := st.ListRuns(ctx, "", 1)
	if err != nil || len(runs) == 0 {
		return time.Time{}
	}

	return runs[0].FinishedAt
}

// queueSettled is a queue holding nothing waiting or running: a restart's interrupted run reads pending again, so it is resumed before it is counted. An empty one is settled too: the revision is durable before the job is queued, and a crash or a failed queue in between leaves a one-off with nothing to run that every restart would serve again.
func queueSettled(ctx context.Context, st store.Queue) bool {
	rows, err := st.ListTriggerQueue(ctx, 20)
	if err != nil {
		return false
	}

	for _, row := range rows {
		if row.Status == store.QueuePending || row.Status == store.QueueRunning {
			return false
		}
	}

	return true
}

// markOneOff comes before the record, whose last write makes the revision current: a restart serves exactly the rows with one, and must never serve a one-off polled.
func (d *daemon) markOneOff(ctx context.Context, st store.Control) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	return st.MarkOneOff(writeCtx) //nolint:wrapcheck // the driver names the pipeline
}
