package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/web"
)

// remoteRuns answers a follower's reads from a daemon's API rather than a state file, which is what reaches a daemon on another host.
type remoteRuns struct {
	client   *daemonClient
	pipeline pipelineName
}

// getJSON reads one answer into into, reporting false for a 404.
func (r remoteRuns) getJSON(ctx context.Context, path string, into any) (bool, error) {
	status, body, err := r.client.doContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return false, err
	}

	if status == http.StatusNotFound {
		return false, nil
	}

	if status != http.StatusOK {
		return false, daemonError(r.client.target, status, body)
	}

	err = json.Unmarshal(body, into)
	if err != nil {
		return false, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return true, nil
}

// FindRunRow reads a 404 as gone, the run and its pipeline alike: a reaped one-off is both.
func (r remoteRuns) FindRunRow(ctx context.Context, id string) (store.RunRow, bool, error) {
	var run web.RunJSON

	found, err := r.getJSON(ctx, pipelinePath(r.pipeline, "runs", id), &run)

	return run.Row(), found, err
}

func (r remoteRuns) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error) {
	var answer []web.RunEventJSON

	query := url.Values{"after": {strconv.FormatInt(afterSeq, 10)}, "limit": {strconv.Itoa(limit)}}

	found, err := r.getJSON(ctx, pipelinePath(r.pipeline, "runs", runID, "events")+"?"+query.Encode(), &answer)
	if err != nil {
		return nil, err
	}

	if !found {
		return nil, fmt.Errorf("%s no longer serves %s", r.client.target, r.pipeline)
	}

	rows := make([]store.RunEventRow, 0, len(answer))
	for _, row := range answer {
		rows = append(rows, row.Row())
	}

	return rows, nil
}

func (r remoteRuns) newest(ctx context.Context, job string) (store.RunRow, bool, error) {
	var runs []web.RunJSON

	query := url.Values{"job": {job}, "limit": {"1"}}

	found, err := r.getJSON(ctx, pipelinePath(r.pipeline, "runs")+"?"+query.Encode(), &runs)
	if err != nil {
		return store.RunRow{}, false, err
	}

	if !found {
		return store.RunRow{}, false, fmt.Errorf("%s does not serve %s", r.client.target, r.pipeline)
	}

	if len(runs) == 0 {
		return store.RunRow{}, false, nil
	}

	return runs[0].Row(), true, nil
}

// target is the run named, or the newest — of job when given.
func (r remoteRuns) target(ctx context.Context, runID, job string) (store.RunRow, error) {
	if runID != "" {
		run, found, err := r.FindRunRow(ctx, runID)
		if err != nil {
			return run, err
		}

		if !found {
			return run, fmt.Errorf("%s has no run %s in %s", r.client.target, runID, r.pipeline)
		}

		return run, nil
	}

	run, found, err := r.newest(ctx, job)
	if err != nil {
		return run, err
	}

	if !found {
		return run, fmt.Errorf("nothing to follow: no runs recorded for %s on %s", r.pipeline, r.client.target)
	}

	return run, nil
}

// queued waits for the queue row an enqueue answered to start its run: a run is named when the daemon claims its row, not when it is queued. A row that ends without one is the answer too, so an abort before the start exits as an abort rather than waiting forever (#283).
func (r remoteRuns) queued(ctx context.Context, id int64) (store.RunRow, error) {
	path := pipelinePath(r.pipeline, "queued", strconv.FormatInt(id, 10))

	for {
		var row web.QueuedJSON

		found, err := r.getJSON(ctx, path, &row)
		if err != nil {
			return store.RunRow{}, err
		}

		if !found {
			return store.RunRow{}, fmt.Errorf("%s no longer holds queued run %d of %s", r.client.target, id, r.pipeline)
		}

		if row.Run != "" {
			return r.target(ctx, row.Run, "")
		}

		err = endedUnstarted(row)
		if err != nil {
			return store.RunRow{}, err
		}

		select {
		case <-ctx.Done():
			return store.RunRow{}, fmt.Errorf("left before queued run %d of %s started on %s: %w", id, row.Job, r.client.target, ctx.Err())
		case <-time.After(followPoll):
		}
	}
}

// endedUnstarted is how a row with no run ended, nil while it may still start one. An abort wraps context.Canceled so it exits as the aborted run it would have been.
func endedUnstarted(row web.QueuedJSON) error {
	switch store.QueueStatus(row.Status) {
	case store.QueuePending, store.QueueRunning:
		return nil
	case store.QueueAborted:
		return fmt.Errorf("the queued run of %s was aborted before it started: %w", row.Job, context.Canceled)
	case store.QueueSucceeded, store.QueueFailed, store.QueueSkipped:
		fallthrough
	default:
		return fmt.Errorf("the queued run of %s ended %s without starting: %s", row.Job, row.Status, row.Error)
	}
}

func (r remoteRuns) oneOffStarted(ctx context.Context, created web.OneOffResult) (store.RunRow, error) {
	if created.Queued != 0 {
		return r.queued(ctx, created.Queued)
	}

	return r.started(ctx, created.Job)
}

// started is queued for a daemon too old to answer the row it queued: a one-off pipeline holds only that one job's runs, so its first is ours, and an abort before the start is waited on until Ctrl-C.
func (r remoteRuns) started(ctx context.Context, job string) (store.RunRow, error) {
	for {
		run, found, err := r.newest(ctx, job)
		if err != nil || found {
			return run, err
		}

		select {
		case <-ctx.Done():
			return run, fmt.Errorf("left before %s started on %s: %w", job, r.client.target, ctx.Err())
		case <-time.After(followPoll):
		}
	}
}

// sendOneOff is the request as JSON, or with folders as a multipart body.
func (c *daemonClient) sendOneOff(ctx context.Context, inputs InputFlags, req web.OneOffRequest) (int, []byte, error) {
	if len(req.Inputs) == 0 {
		payload, err := json.Marshal(req)
		if err != nil {
			return 0, nil, fmt.Errorf("could not encode the configuration: %w", err)
		}

		return c.doContext(ctx, http.MethodPost, "/api/one-offs", payload)
	}

	body, contentType, err := inputs.uploadBody(ctx, req)
	if err != nil {
		return 0, nil, err
	}

	defer discardBody(body)

	return c.oneOffUpload(ctx, body, contentType)
}

func (c *daemonClient) oneOffWith(ctx context.Context, inputs InputFlags, req web.OneOffRequest) (web.OneOffResult, error) {
	var result web.OneOffResult

	status, body, err := c.sendOneOff(ctx, inputs, req)
	if err != nil {
		return result, err
	}

	if status != http.StatusCreated {
		return result, daemonError(c.target, status, body)
	}

	err = json.Unmarshal(body, &result)
	if err != nil {
		return result, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	if result.Pipeline == "" {
		return result, errors.New("the daemon's answer named no pipeline")
	}

	return result, nil
}
