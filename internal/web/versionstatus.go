package web

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// versionRow is one version on a resource page with one cell per job that
// gets the resource, in the order the jobs are configured.
type versionRow struct {
	Version string
	Cells   []versionCell
}

// versionCell is where one version stands in one job: the runs that took it,
// newest first, or — when there are none — the word for why not.
type versionCell struct {
	Job   string
	Runs  []store.RunRow
	State string
}

// Latest is the run a cell leads with; the zero row when there is none.
func (c versionCell) Latest() store.RunRow {
	if len(c.Runs) == 0 {
		return store.RunRow{}
	}

	return c.Runs[0]
}

// Title lists every run of the version in the job, for the cell's tooltip.
func (c versionCell) Title() string {
	lines := make([]string, len(c.Runs))
	for i, run := range c.Runs {
		lines[i] = statusMark(run.Status) + " " + shortID(run.ID) + " " + statusWord(run.Status)
	}

	return strings.Join(lines, "\n")
}

// consumer is a job that gets the resource, and how it picks a version.
type consumer struct {
	Job  string
	mode string
	pin  map[string]string
}

// consumersOf is every job that gets the resource. A job with any
// `version: every` get of it fans out over every version, so that wins over a
// second, plainer get of the same resource in the same job.
func consumersOf(cfg *config.Config, resource string) []consumer {
	var consumers []consumer

	for _, job := range cfg.Jobs {
		gets := job.ResourceGets(resource)
		if len(gets) == 0 {
			continue
		}

		one := consumer{Job: job.Name, mode: "latest"}

		for _, get := range gets {
			if get.VersionEvery() {
				one.mode, one.pin = "every", nil

				break
			}

			if pin, ok := get.Version.(map[string]any); ok {
				one.mode, one.pin = "pinned", map[string]string{}
				for k, v := range pin {
					one.pin[k] = fmt.Sprint(v)
				}
			}
		}

		consumers = append(consumers, one)
	}

	return consumers
}

// runlessState says why a job has no run of a version. Every answer comes
// from what the store already records — the job's consumed mark, its newest
// built version, its queue row — so "no run" is never left blank, where it
// could mean any of four different things.
//
// queued: the job has a pending or claimed queue row and this version is
// still ahead of it. waiting: still ahead, but nothing will start it yet.
// reaped: the job took it, but run_history: has since removed the run.
// superseded: a latest-mode job only ever builds the newest version, and this
// is not it. not pinned: the job only ever builds its pin.
func runlessState(c consumer, order, mark, newest int64, pinned, queued bool) string {
	ahead := "waiting"
	if queued {
		ahead = "queued"
	}

	switch c.mode {
	case "every":
		if order > mark {
			return ahead
		}

		return "reaped"
	case "pinned":
		if !pinned {
			return "not pinned"
		}

		return ahead
	default:
		if order == newest {
			return ahead
		}

		return "superseded"
	}
}

// versionRows builds the matrix: versions newest first, one cell per
// consumer.
func versionRows(ctx context.Context, st store.Store, resource string, versions []string, consumers []consumer) ([]versionRow, error) {
	rows := make([]versionRow, len(versions))
	for i, version := range versions {
		rows[i] = versionRow{Version: version, Cells: make([]versionCell, len(consumers))}
	}

	if len(consumers) == 0 {
		return rows, nil
	}

	runs, err := st.VersionRuns(ctx, resource)
	if err != nil {
		return nil, err //nolint:wrapcheck // VersionRuns names the resource
	}

	orders, err := st.VersionOrders(ctx, resource)
	if err != nil {
		return nil, err //nolint:wrapcheck // VersionOrders names the resource
	}

	queue, err := st.ListTriggerQueue(ctx, 25)
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller wraps with its own context
	}

	byJob := runsByJob(runs)

	var newest int64
	for _, order := range orders {
		newest = max(newest, order)
	}

	for col, c := range consumers {
		var mark int64

		if c.mode == "every" {
			mark, err = st.ConsumedMark(ctx, c.Job, resource)
			if err != nil {
				return nil, err //nolint:wrapcheck // ConsumedMark names the job
			}
		}

		state, _ := queuedState(queue, c.Job)
		fillColumn(rows, col, c, byJob[c.Job], orders, mark, newest, state != "waiting")
	}

	return rows, nil
}

// fillColumn draws one consumer's cell on every row.
func fillColumn(rows []versionRow, col int, c consumer, runs map[string][]store.RunRow, orders map[string]int64, mark, newest int64, queued bool) {
	for i := range rows {
		cell := versionCell{Job: c.Job, Runs: runs[rows[i].Version]}
		if len(cell.Runs) == 0 {
			cell.State = runlessState(c, orders[rows[i].Version], mark, newest, matchesPin(rows[i].Version, c.pin), queued)
		}

		rows[i].Cells[col] = cell
	}
}

// runsByJob indexes runs as [job][version], each list newest first because
// VersionRuns already is.
func runsByJob(runs []store.VersionRun) map[string]map[string][]store.RunRow {
	byJob := map[string]map[string][]store.RunRow{}

	for _, run := range runs {
		job := run.Run.JobName
		if byJob[job] == nil {
			byJob[job] = map[string][]store.RunRow{}
		}

		byJob[job][run.Version] = append(byJob[job][run.Version], run.Run)
	}

	return byJob
}

// matchesPin reports whether a stored version carries every field of a pin,
// compared as text the way a pin is written.
func matchesPin(versionJSON string, pin map[string]string) bool {
	if pin == nil {
		return false
	}

	var version map[string]any

	err := json.Unmarshal([]byte(versionJSON), &version)
	if err != nil {
		return false
	}

	for k, want := range pin {
		got, ok := version[k]
		if !ok || fmt.Sprint(got) != want {
			return false
		}
	}

	return true
}

// Class is the status class a run-less cell draws with: the shared .st
// vocabulary where a word already means the same thing, two of its own where
// none does.
func (c versionCell) Class() string {
	switch c.State {
	case "superseded", "not pinned":
		return "skipped"
	default:
		return c.State
	}
}
