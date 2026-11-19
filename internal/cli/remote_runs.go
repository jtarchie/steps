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
	pipeline string
}

func (r remoteRuns) path(rest string) string {
	return "/api/pipelines/" + url.PathEscape(r.pipeline) + rest
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
	var run store.RunRow

	found, err := r.getJSON(ctx, r.path("/runs/"+url.PathEscape(id)), &run)

	return run, found, err
}

func (r remoteRuns) RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error) {
	var rows []store.RunEventRow

	query := url.Values{"after": {strconv.FormatInt(afterSeq, 10)}, "limit": {strconv.Itoa(limit)}}

	found, err := r.getJSON(ctx, r.path("/runs/"+url.PathEscape(runID)+"/events?"+query.Encode()), &rows)
	if err == nil && !found {
		return nil, fmt.Errorf("%s no longer serves %s", r.client.target, r.pipeline)
	}

	return rows, err
}

func (r remoteRuns) newest(ctx context.Context, job string) (store.RunRow, bool, error) {
	var runs []store.RunRow

	query := url.Values{"job": {job}, "limit": {"1"}}

	found, err := r.getJSON(ctx, r.path("/runs?"+query.Encode()), &runs)
	if err != nil {
		return store.RunRow{}, false, err
	}

	if !found {
		return store.RunRow{}, false, fmt.Errorf("%s does not serve %s", r.client.target, r.pipeline)
	}

	if len(runs) == 0 {
		return store.RunRow{}, false, nil
	}

	return runs[0], true, nil
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

// started waits for the one-off's run to exist: a run is named when the daemon claims its queue row, not when it is queued, and a one-off pipeline holds only that one job's runs, so its first is ours. ponytail: waits until the daemon reaps a one-off that never started a run (its grace), with Ctrl-C to leave sooner; a queue-row read on the API is the upgrade.
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

func (c *daemonClient) oneOff(req web.OneOffRequest) (web.OneOffResult, error) {
	var result web.OneOffResult

	payload, err := json.Marshal(req)
	if err != nil {
		return result, fmt.Errorf("could not encode the configuration: %w", err)
	}

	status, body, err := c.do(http.MethodPost, "/api/one-offs", payload)
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
