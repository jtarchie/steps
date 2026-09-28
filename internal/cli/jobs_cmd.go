package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/store"
)

// JobsCmd inspects and clears the watch circuit breaker.
//
// It exists because a held job is otherwise invisible: the trigger loop stops
// triggering it and says so once, in output that has long since scrolled past
// by the time anyone wonders why the nightly summary stopped arriving.
type JobsCmd struct {
	List    JobsListCmd    `cmd:"" default:"withargs"                                          help:"list jobs the circuit breaker is holding"`
	Release JobsReleaseCmd `cmd:"" help:"release a held job, so new versions trigger it again"`
}

// JobsListCmd is the listing, and the group's default: bare `steps jobs
// <pipeline>` still answers "what has the breaker stopped?".
type JobsListCmd struct {
	ReadFlags `embed:""`
}

// Run prints every held job.
func (j *JobsListCmd) Run() error {
	if nothingRecorded(j.ReadFlags, "no jobs are held") {
		return nil
	}

	st, cleanup, err := openStore(j.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	paused, err := st.PausedJobs(context.Background())
	if err != nil {
		return fmt.Errorf("could not list held jobs: %w", err)
	}

	if len(paused) == 0 {
		fmt.Println("no jobs are held")

		return nil
	}

	writer := newTabWriter()

	_, _ = fmt.Fprintln(writer, "JOB\tCONSECUTIVE FAILURES\tHELD SINCE")

	for _, job := range paused {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\n", job.Name, job.Consecutive, job.PausedAt)
	}

	return flush(writer)
}

// JobsReleaseCmd clears the breaker for one job. Not "resume": that word is --resume <run>'s, continuing a failed run.
//
// A subcommand rather than `jobs --release <name>`: a flag that turns a
// listing into a write reads as configuration and behaves as a mutation, and
// the verb should say which one it is.
type JobsReleaseCmd struct {
	ReadFlags `embed:""`
	Job       string `arg:""   help:"held job to release"`
}

// Run releases the named job.
func (j *JobsReleaseCmd) Run() error {
	st, cleanup, err := openStore(j.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	return releaseJob(context.Background(), st, j.Job)
}

// releaseJob takes a job out of the breaker's hold, refusing a name that is
// not held.
//
// Checked against the BREAKER rather than against a configuration, and that is
// the change `-p` forced: the pipeline this is about may exist nowhere on this
// machine — it was uploaded to a daemon — so there is no file to check a name
// against. The paused list is the better check anyway: it catches the typo
// this exists to catch, and it also catches releasing something that was never
// held, which used to report success having done nothing.
func releaseJob(ctx context.Context, st store.Store, name string) error {
	paused, err := st.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("could not read held jobs: %w", err)
	}

	found := false

	held := make([]string, 0, len(paused))

	for _, job := range paused {
		held = append(held, job.Name)

		if job.Name == name {
			found = true
		}
	}

	if !found {
		if len(held) == 0 {
			return fmt.Errorf("cannot release %q: no jobs are held", name)
		}

		return fmt.Errorf("cannot release %q: the held jobs are %s", name, strings.Join(held, ", "))
	}

	err = st.ResetJobFailures(ctx, name)
	if err != nil {
		return fmt.Errorf("could not release job %q: %w", name, err)
	}

	fmt.Printf("released: %s\n", name)

	return nil
}
