package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/labstack/echo/v5"
)

// handleFollow is the waiting room between enqueueing a job and its run
// existing. A queued job has no run id until a worker claims it, so the page
// reports what the queue is doing and forwards itself the moment the run
// appears.
func (s *Server) handleFollow(c *echo.Context) error {
	pipeline := pipelineOf(c)
	name := c.Param("job")

	_, err := pipeline.Config().FindJob(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(http.StatusOK, "follow", map[string]any{
		"Nav":   s.nav(c),
		"Title": name,
		"Crumbs": []crumb{
			{Label: "jobs", URL: "/p/" + pipeline.Slug},
			{Label: name, URL: "/p/" + pipeline.Slug + "/jobs/" + name + "/detail"},
			{Label: "queued"},
		},
		"Job":   name,
		"Bar":   barView{Abort: "/p/" + pipeline.Slug + "/jobs/" + name + "/queued/abort"},
		"Since": c.QueryParam("since"),
	})
}

// handleLatestRun answers the follow page: has a run of this job started
// since the given millisecond stamp, and what is the queue doing meanwhile.
func (s *Server) handleLatestRun(c *echo.Context) error {
	pipeline := pipelineOf(c)
	ctx := c.Request().Context()
	name := c.Param("job")

	// A missing or unparseable stamp means no run has been credited to this
	// page yet, so nothing that already exists can be the answer. Falling back
	// to zero would make FirstRunSince match the job's entire history and
	// forward to its OLDEST run, presenting an ancient transcript as the one
	// just triggered.
	//
	// The fallback is now+1ms, not now, because UnixMilli TRUNCATES: a run
	// started earlier in the current millisecond has a started_at greater than a
	// millisecond-floored "now", so it satisfies `>= since` and gets credited to
	// a page that never triggered anything. Rounding up is the direction that
	// matches the intent — a sentinel later than everything that already exists.
	//
	// It only surfaced when runs.started_at became zero-padded (see
	// store.sortableNano): before that, comparing '…123456789Z' against '…123Z'
	// as text rejected the run by accident of the trimming, not by design.
	millis, err := strconv.ParseInt(c.QueryParam("since"), 10, 64)
	if err != nil || millis <= 0 {
		millis = time.Now().UTC().UnixMilli() + 1
	}

	since := time.UnixMilli(millis).UTC()

	run, ok, findErr := pipeline.Store.FirstRunSince(ctx, name, since)
	if findErr != nil {
		return fmt.Errorf("web: %w", findErr)
	}

	if ok {
		//nolint:wrapcheck // echo's JSON error is returned verbatim
		return c.JSON(http.StatusOK, map[string]any{
			"run": run.ID,
			"url": fmt.Sprintf("/p/%s/runs/%s", pipeline.Slug, run.ID),
		})
	}

	// No run yet. Say why, so a job held by a serial group or sitting behind
	// a busy drainer reads as queued rather than as nothing happening.
	queue, queueErr := pipeline.Store.ListTriggerQueue(ctx, 25)
	if queueErr != nil {
		return fmt.Errorf("web: %w", queueErr)
	}

	state, inFlight := queuedState(queue, name)

	// With no row of this job left in the queue there is nothing waiting to explain.
	var checks []followCheck

	if state != "waiting" {
		checks, err = followChecks(ctx, readsOf(c, pipeline), s.capacity, name, state == "running", inFlight)
		if err != nil {
			return err
		}
	}

	//nolint:wrapcheck // echo's JSON error is returned verbatim
	return c.JSON(http.StatusOK, map[string]any{"run": nil, "state": state, "checks": checks})
}

// queuedState is where a job's newest queue row stands, and how many of its builds are running.
func queuedState(queue []store.QueueRow, name string) (string, int) {
	state := "waiting"
	inFlight := 0

	for _, row := range queue {
		if row.JobName != name {
			continue
		}

		if row.Status == "running" {
			inFlight++
		}

		if state == "waiting" && (row.Status == "pending" || row.Status == "running") {
			state = row.Status
		}
	}

	return state, inFlight
}

// followCheck is one line of what a queued run waits on, as Concourse's "preparing build" lists them.
type followCheck struct {
	Text  string `json:"text"`
	Clear bool   `json:"clear"`
}

// followChecks asks only what the drain itself asks (admits, then ClaimNextJob's limits), so a line never blames something that is not holding the run back. The limits stop mattering once a worker has claimed it.
func followChecks(ctx context.Context, reads *pipelineReads, capacity Capacity, name string, claimed bool, inFlight int) ([]followCheck, error) {
	pipeline := reads.target

	paused, err := reads.PausedErr(ctx)
	if err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}

	checks := []followCheck{{Text: "the pipeline is not paused", Clear: true}}
	if paused {
		checks[0] = followCheck{Text: "the pipeline is paused"}
	}

	if claimed {
		return append(checks, followCheck{Text: "a worker has claimed it", Clear: true}), nil
	}

	job, err := pipeline.Config().FindJob(name)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("no job %q in this pipeline", name))
	}

	if len(job.SerialGroups) > 0 {
		holder, err := pipeline.Store.SerialGroupHolder(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("web: %w", err)
		}

		check := followCheck{Text: "its serial group is free", Clear: true}
		if holder != "" {
			check = followCheck{Text: holder + " holds its serial group"}
		}

		checks = append(checks, check)
	}

	if check, limited := maxInFlightCheck(job, inFlight); limited {
		checks = append(checks, check)
	}

	if check, limited := maxRunsCheck(capacity); limited {
		checks = append(checks, check)
	}

	return append(checks, followCheck{Text: "no worker has claimed it yet"}), nil
}

// maxInFlightCheck is the follow line for the job's max_in_flight, and false when it has none.
func maxInFlightCheck(job *config.Job, inFlight int) (followCheck, bool) {
	limit := job.EffectiveMaxInFlight()
	if limit == config.UnlimitedInFlight {
		return followCheck{}, false
	}

	if inFlight >= limit {
		return followCheck{Text: fmt.Sprintf("its max_in_flight (%d) is full", limit)}, true
	}

	return followCheck{Text: fmt.Sprintf("its max_in_flight (%d) has room", limit), Clear: true}, true
}

// maxRunsCheck is the follow line for --max-runs, and false when the daemon has no such limit.
func maxRunsCheck(capacity Capacity) (followCheck, bool) {
	if capacity == nil {
		return followCheck{}, false
	}

	limit, active := capacity.RunCapacity()
	if limit <= 0 {
		return followCheck{}, false
	}

	if active >= limit {
		return followCheck{Text: fmt.Sprintf("the server's --max-runs (%d) is full", limit)}, true
	}

	return followCheck{Text: fmt.Sprintf("the server's --max-runs (%d) has room", limit), Clear: true}, true
}

// waitingOn is the first check holding a job's pending row back, the line the follow page leads with. A job the configuration no longer names has nothing to check, which reads as empty rather than as a 404 for the whole board.
func waitingOn(ctx context.Context, reads *pipelineReads, capacity Capacity, queue []store.QueueRow, name string) (string, error) {
	_, err := reads.target.Config().FindJob(name)
	if err != nil {
		return "", nil //nolint:nilerr // a dropped job is a row with no reason, not a broken page
	}

	_, inFlight := queuedState(queue, name)

	checks, err := followChecks(ctx, reads, capacity, name, false, inFlight)
	if err != nil {
		return "", err
	}

	for _, check := range checks {
		if !check.Clear {
			return check.Text, nil
		}
	}

	return "", nil
}
