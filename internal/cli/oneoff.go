package cli

// The daemon's half of `steps run --target` (#283): a one-off is a pipeline the daemon names, serves without a poll, queues one job of, and forgets once nothing is left to run and a grace has passed.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// ponytail: oneOffPrefix marks a one-off, which a restart restores unpolled and reaps; a name, not a column — a pipeline somebody set under this prefix before it was reserved would be read as a one-off and reaped; a pipelines.one_off column (a schema step in both drivers) is the upgrade.
const oneOffPrefix = "one-off."

// oneOffPoll is how often a one-off's queue is asked whether it is done.
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

	cfg, err := d.accept(name, req.Source, convertIncludes[string](req.Includes))
	if err != nil {
		return web.OneOffResult{}, err
	}

	_, err = cfg.FindJob(req.Job)
	if err != nil {
		return web.OneOffResult{}, fmt.Errorf("%w: %w", web.ErrRefused, err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	st, err := d.stateFor(name, nil)
	if err != nil {
		return web.OneOffResult{}, err
	}

	provider, err := d.workspaceFor(st, nil)
	if err != nil {
		release(st)

		return web.OneOffResult{}, err
	}

	err = d.record(ctx, st, cfg, web.SetRequest{From: req.From})
	if err != nil {
		closeProvider(provider)
		release(st)

		return web.OneOffResult{}, err
	}

	d.start(name, cfg, st, provider, req.From)

	served, ok := d.served[name]
	if !ok {
		return web.OneOffResult{}, fmt.Errorf("could not serve the one-off %s", name)
	}

	// Its own context, as record's: a sender that hung up after the pipeline started must still leave it something to run and so to reap after. The actor travels with the values.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), daemonWriteBound)
	defer cancel()

	err = d.runner.Enqueue(writeCtx, served.target.Pipeline, req.Job, "one-off", false)
	if err != nil {
		return web.OneOffResult{}, errors.Join(fmt.Errorf("could not queue %s: %w", req.Job, err), d.destroyLocked(writeCtx, name))
	}

	slog.InfoContext(ctx, "web.one_off", "pipeline", name, "job", req.Job, "config", shortConfig(cfg.Revision.SHA))

	return web.OneOffResult{Pipeline: name, Job: req.Job}, nil
}

// reapWhenDone forgets a one-off once its queue has been settled for the grace. The destroy runs outside this loop because it waits for the loops to end.
func (d *daemon) reapWhenDone(ctx context.Context, st store.Queue, name string) {
	ticker := time.NewTicker(oneOffPoll)
	defer ticker.Stop()

	var settledAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		// Re-armed by anything queued meanwhile — a person pressing retry on the one-off's page — so the grace counts from the last run.
		if !queueSettled(ctx, st) {
			settledAt = time.Time{}

			continue
		}

		if settledAt.IsZero() {
			settledAt = time.Now()
		}

		if time.Since(settledAt) < d.oneOffGrace {
			continue
		}

		d.reaping.Add(1)

		go func() {
			defer d.reaping.Done()

			err := d.Destroy(context.WithoutCancel(ctx), name)
			if err != nil && !errors.Is(err, web.ErrNoSuchPipeline) {
				slog.ErrorContext(ctx, "web.one_off.unreaped", "pipeline", name, "error", err)
			}
		}()

		return
	}
}

// queueSettled is a queue holding nothing waiting or running: a restart's interrupted run reads pending again, so it is resumed before it is reaped. An empty one is settled too, so the grace counts from the start: the revision is durable before the job is queued, and a crash or a failed queue in between leaves a one-off with nothing to run that every restart would serve again.
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
