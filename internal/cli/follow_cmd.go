package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/runview"
	"github.com/jtarchie/steps/internal/store"
)

// RunsFollowCmd attaches the live view to a run some other process is executing — a daemon's, or a `steps run` in another terminal — by polling what it records, the way fly watch attaches to a build: the state file, or with --target the daemon's API, so a daemon on another host is followed the same way.
type RunsFollowCmd struct {
	ReadFlags     `embed:""`
	ProgressFlags `embed:""`
	RunID         string `arg:""                                   help:"the run to follow (default: the newest)" optional:""`
	Job           string `help:"follow the newest run of this job"`
	// No STEPS_TARGET: that names the daemon the control verbs talk to, and read from the environment it would quietly move every follow off the state file.
	Target string `help:"follow the run on this steps web daemon rather than in a state file, optionally http://user:password@host" name:"target"`
}

//nolint:gochecknoglobals // a test seam: tests shrink it, the way web's liveBatch is shrunk
var (
	// followPoll is how often the store is re-read; web's live stream polls at the same rate for the same reason — the bus only carries this process's runs.
	followPoll = 400 * time.Millisecond
	// followBatch pages a run's events, so one past any single read keeps streaming.
	followBatch = 500
	// followSettle is how long a finished run gets for a job_finished the sink may have dropped.
	followSettle = 2 * time.Second
	// followQuiet is silence long enough to say the run may be orphaned — a warning, never an exit, since a long agent turn looks the same.
	followQuiet = 10 * time.Minute
)

// Run follows one run to its end and exits as that run did.
func (r *RunsFollowCmd) Run(streams Streams) error {
	out := streams.Out

	if r.RunID != "" && r.Job != "" {
		return errors.New("--job does not apply to a named run; a run belongs to one job already")
	}

	if r.Target != "" {
		return r.followRemote(streams)
	}

	// An error, not the list views' empty answer: follow's exit status is the run's, and `follow --job deploi && promote` must not promote.
	if stateIsEmpty(r.state()) {
		return fmt.Errorf("nothing to follow: %s in %s", noRunsYet(r.Pipeline), r.state())
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	ctx, cancel := streams.cancellable()
	defer cancel()

	run, err := r.target(ctx, st)
	if err != nil {
		return err
	}

	sayf(out, "following %s · %s · %s\n", run.ID, runview.Scrub(run.JobName), runview.Scrub(string(run.Status)))

	if run.Status != store.RunRunning {
		sayf(out, "run %s already %s, replaying\n", run.ID, runview.Scrub(string(run.Status)))
	}

	return watch(ctx, out, r.ProgressFlags, st, st, run, func(last store.RunRow) {
		sayf(out, "detached: %s is still running — steps runs follow -p %s %s%s\n", last.ID, r.Pipeline, last.ID, dbNote(r.DB, st))
	})
}

// followRemote is Run against a daemon's API.
func (r *RunsFollowCmd) followRemote(streams Streams) error {
	out := streams.Out

	if r.DB != "" {
		return errors.New("--db and --target name two places to follow from; give one")
	}

	name, err := namedPipeline(r.Pipeline, "runs follow --target")
	if err != nil {
		return err
	}

	client, err := newDaemonClient(r.Target)
	if err != nil {
		return err
	}

	remote := remoteRuns{client: client, pipeline: name}

	ctx, cancel := streams.cancellable()
	defer cancel()

	run, err := remote.target(ctx, r.RunID, r.Job)
	if err != nil {
		return err
	}

	sayf(out, "following %s · %s · %s on %s\n", run.ID, runview.Scrub(run.JobName), runview.Scrub(string(run.Status)), client.target)

	return watch(ctx, out, r.ProgressFlags, nil, remote, run, func(last store.RunRow) {
		sayf(out, "detached: %s is still running — steps runs follow --target %s -p %s %s\n", last.ID, client.target, name, last.ID)
	})
}

// watch follows run to its end and is the command's result: a detach says how to come back, anything else is how the run came out. usage may be nil.
func watch(
	ctx context.Context, out io.Writer, progress ProgressFlags, usage store.Usage, st followStore, run store.RunRow, detached func(store.RunRow),
) error {
	ctx, render, stop := progress.followRenderer(ctx, out, usage)
	defer stop()

	last, err := followRun(ctx, st, run, func(event events.Event) { render(runview.ScrubEvent(event)) })

	stop()

	if errors.Is(err, context.Canceled) {
		if last.Status == store.RunRunning {
			detached(last)
		}

		return fmt.Errorf("detached from run %s: %w", last.ID, err)
	}

	if err != nil {
		return err
	}

	return followedOutcome(last)
}

// followRenderer is the live view on a terminal and lines elsewhere; the stop it returns may be called twice.
func (p ProgressFlags) followRenderer(ctx context.Context, out io.Writer, st store.Usage) (context.Context, func(events.Event), func()) {
	if !p.wantsLive(out) {
		return ctx, followPlain(out), func() {}
	}

	ctx, live, stop := newLive(ctx, out, st)

	// A follower has no byte stream of its own: a chunk is the bytes a local run's Stream would have carried into the step's tail.
	return ctx, func(event events.Event) {
		if event.Type == events.TypeStepOutputChunk {
			_, _ = io.WriteString(live.Stream(event.StepID, event.Status == events.StreamStderr), event.Text)

			return
		}

		live.Event(event)
	}, sync.OnceFunc(stop)
}

// target is the run named, or the newest — of --job when given.
func (r *RunsFollowCmd) target(ctx context.Context, st interface {
	store.Runs
	store.Meta
},
) (store.RunRow, error) {
	if r.RunID != "" {
		return recordedRun(ctx, st, r.RunID)
	}

	runs, err := st.ListRuns(ctx, r.Job, 1)
	if err != nil {
		return store.RunRow{}, fmt.Errorf("could not read runs: %w", err)
	}

	if len(runs) == 0 {
		if r.Job != "" {
			return store.RunRow{}, fmt.Errorf("nothing to follow: no runs of job %q recorded for pipeline %q", r.Job, st.Pipeline())
		}

		return store.RunRow{}, fmt.Errorf("nothing to follow: no runs recorded for pipeline %q", st.Pipeline())
	}

	return runs[0], nil
}

// followedOutcome is the error `steps run` would have returned, so a script wrapping both reads one set of exit codes. runs.status never records "errored", so an errored run exits as failed.
func followedOutcome(run store.RunRow) error {
	switch run.Status {
	case store.RunSucceeded:
		return nil
	case store.RunFailed:
		return outcome.Fail(fmt.Errorf("run %s failed", run.ID)) //nolint:wrapcheck // Fail is the wrap: it marks the error the way RunJob does
	case store.RunAborted:
		return fmt.Errorf("run %s aborted: %w", run.ID, context.Canceled)
	case store.RunRunning:
		fallthrough
	default:
		return fmt.Errorf("run %s ended %q", run.ID, run.Status)
	}
}

// followStore is the two reads a follower makes, which a state file and a daemon's API both answer.
type followStore interface {
	FindRunRow(ctx context.Context, id string) (store.RunRow, bool, error)
	RunEvents(ctx context.Context, runID string, afterSeq int64, limit int) ([]store.RunEventRow, error)
}

// followRun renders a run's recorded events from the first until the run is over, and returns its last row.
//
// Over means three things at once, because FinishRun lands before job_finished is published and the sink writes after both: the row is final, the latest attempt's job_finished has been rendered (a resume reuses the id and starts again), and one poll after that found nothing — the resume hint and usage report trail it.
func followRun(ctx context.Context, st followStore, run store.RunRow, render func(events.Event)) (store.RunRow, error) {
	follower := &follower{st: st, render: render, active: time.Now()}

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return run, ctx.Err() //nolint:wrapcheck // a bare context.Canceled is the detach the caller checks for
		case <-timer.C:
		}

		// Before the drain, as web's stream does: a run that finishes mid-drain is still read as running, and the loop comes round for its last events.
		current, ok, err := st.FindRunRow(ctx, run.ID)
		if err != nil {
			return run, followErr(ctx, err)
		}

		if !ok {
			return run, fmt.Errorf("run %s is gone — reaped by run_history or its pipeline destroyed while following", run.ID)
		}

		run = current

		drained, err := follower.drain(ctx, run)
		if err != nil {
			return run, followErr(ctx, err)
		}

		if follower.over(run, drained) {
			return run, nil
		}

		timer.Reset(followPoll)
	}
}

// follower is what followRun remembers between polls.
type follower struct {
	st     followStore
	render func(events.Event)

	after    int64
	lastAt   time.Time
	finished bool
	warned   bool
	active   time.Time
}

// drain renders every event recorded since the last one rendered, a page at a time.
func (f *follower) drain(ctx context.Context, run store.RunRow) (int, error) {
	drained := 0

	for more := true; more; {
		rows, err := f.st.RunEvents(ctx, run.ID, f.after, followBatch)
		if err != nil {
			return drained, err //nolint:wrapcheck // followErr wraps it, once it knows whether this was a detach
		}

		for _, row := range rows {
			f.render(runview.EventOf(row, run.JobName))

			f.after, f.lastAt = row.Seq, row.At

			switch row.Type {
			case events.TypeJobFinished:
				f.finished = true
			case events.TypeJobStarted, events.TypeStepStarted:
				f.finished = false
			}
		}

		drained += len(rows)
		more = len(rows) == followBatch
	}

	if drained > 0 {
		f.active, f.warned = time.Now(), false
	}

	return drained, nil
}

// over decides whether the run is done, rendering what the recorded events did not say: a dropped job_finished, or a silence that may be an orphan.
func (f *follower) over(run store.RunRow, drained int) bool {
	final := run.Status != store.RunRunning
	quiet := time.Since(f.active)

	switch {
	case final && f.finished && drained == 0:
		return true
	case final && !f.finished && quiet >= followSettle:
		// The sink drops on a full buffer, and without job_finished the live view never prints its summary or takes its region down.
		f.render(events.Event{
			Type: events.TypeJobFinished, RunID: run.ID, Job: run.JobName, StepIndex: -1,
			Status: string(run.Status), DurationMS: took(run, f.lastAt).Milliseconds(), At: f.lastAt,
		})

		return true
	case !final && !f.warned && quiet >= followQuiet:
		f.render(events.Event{
			Type: events.TypeStepNote, RunID: run.ID, Job: run.JobName, StepIndex: -1, Status: events.NoteWarn,
			Text: fmt.Sprintf("no events for %s — if the process running %s stopped, it will never finish; Ctrl-C detaches", followQuiet, run.ID),
		})

		f.warned = true
	}

	return false
}

// followErr keeps a store call cut short by Ctrl-C a detach rather than a store failure.
func followErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // a bare context.Canceled is the detach the caller checks for
	}

	return fmt.Errorf("could not read run: %w", err)
}

func took(run store.RunRow, lastAt time.Time) time.Duration {
	end := run.FinishedAt
	if end.IsZero() {
		end = lastAt
	}

	if end.IsZero() || run.StartedAt.IsZero() {
		return 0
	}

	return end.Sub(run.StartedAt)
}

// followPlain is runview.Plain plus what a local plain run never prints because the bytes streamed live: a step's output, and how the job ended.
func followPlain(w io.Writer) func(events.Event) {
	plain := runview.Plain(w)
	streamed := map[int64]bool{}

	return func(event events.Event) {
		plain(event)

		switch event.Type {
		case events.TypeStepOutputChunk:
			streamed[event.StepID] = true

			_, _ = io.WriteString(w, event.Text)
		case events.TypeStepOutput:
			// The record repeats what the chunks already printed, less its elided middle; a follower who was there for the chunks has seen more than it holds.
			if streamed[event.StepID] {
				return
			}

			_, _ = io.WriteString(w, event.Text+"\n")
		case events.TypeJobFinished:
			_, _ = fmt.Fprintf(w, "%s %s in %s\n", event.Job, event.Status,
				(time.Duration(event.DurationMS) * time.Millisecond).Round(100*time.Millisecond))
		}
	}
}
