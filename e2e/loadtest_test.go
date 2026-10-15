package e2e

// Advisory ceiling probe, run by `task loadtest`; skipped otherwise. It does not assert a number: it prints where start latency, failures and process footprint depart from linear as concurrent jobs grow.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

type loadtarget struct{ pipeline, job string }

func loadtestEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func loadtestLevels(t *testing.T) []int {
	t.Helper()

	fields := strings.Split(loadtestEnv("STEPS_LOADTEST_LEVELS", "10,25,50,100"), ",")
	levels := make([]int, 0, len(fields))

	for _, field := range fields {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 {
			t.Fatalf("STEPS_LOADTEST_LEVELS %q: %v", field, err)
		}

		levels = append(levels, n)
	}

	return levels
}

// TestLoadtestConcurrentJobs ramps N held no-op jobs against one daemon with every cap lifted, in two shapes: N pipelines of one job (the default --max-concurrent 1 each), and one pipeline of N jobs.
func TestLoadtestConcurrentJobs(t *testing.T) {
	if os.Getenv("STEPS_LOADTEST") == "" {
		t.Skip("advisory; run with `task loadtest`")
	}

	hold := loadtestEnv("STEPS_LOADTEST_HOLD", "2")
	levels := loadtestLevels(t)

	t.Logf("hold=%ss; start = enqueue->start; over = run time beyond the hold", hold)
	t.Logf("%-9s %5s %4s %7s %9s %9s %9s %8s %5s %5s %7s", "shape", "n", "fail", "peak", "start_p50", "start_p95", "over_p95", "wall", "gor", "fds", "heapMB")

	for _, shape := range strings.Split(loadtestEnv("STEPS_LOADTEST_SHAPES", "pipelines,jobs"), ",") {
		for _, n := range levels {
			loadtestLevel(t, shape, n, hold)
		}
	}
}

// loadtestServe starts a daemon holding the shape's pipelines and returns what to trigger.
func loadtestServe(t *testing.T, shape string, n int, hold string) (*webProcess, []loadtarget) {
	t.Helper()

	dir := t.TempDir()
	job := func(name string) string {
		return "- name: " + name + "\n  interruptible: true\n  plan:\n  - task: hold\n    inputs: []\n    run: sleep " + hold + "\n"
	}

	targets := make([]loadtarget, 0, n)

	// The default of one worker per pipeline is the point of the pipelines shape: n workers EACH would be n² pollers, which measures the harness.
	flags := []string{"--interval", "1h"}

	if shape == "jobs" {
		var plan strings.Builder

		plan.WriteString("jobs:\n")

		for i := range n {
			name := fmt.Sprintf("j%d", i)
			plan.WriteString(job(name))

			targets = append(targets, loadtarget{"one", name})
		}

		path := filepath.Join(dir, "one.yml")
		writePipelineFile(t, path, plan.String())

		return startWebFor(t, path, append(flags, "--max-concurrent", strconv.Itoa(n))...), targets
	}

	var served *webProcess

	for i := range n {
		name := fmt.Sprintf("p%d", i)
		path := filepath.Join(dir, name+".yml")
		writePipelineFile(t, path, "jobs:\n"+job(name))

		targets = append(targets, loadtarget{name, name})

		if served == nil {
			served = startWebFor(t, path, flags...)

			continue
		}

		served.set(t, name, path)
	}

	return served, targets
}

// footprint is the highest goroutine, descriptor and heap count seen while it samples.
type footprint struct {
	mu       sync.Mutex
	gor, fds int
	heap     uint64
}

func (f *footprint) sample() {
	var ms runtime.MemStats

	runtime.ReadMemStats(&ms)

	open, _ := os.ReadDir("/dev/fd")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.gor = max(f.gor, runtime.NumGoroutine())
	f.fds = max(f.fds, len(open))
	f.heap = max(f.heap, ms.HeapInuse)
}

// watch samples until the returned stop is called, which waits for the last sample.
func (f *footprint) watch() (stop func()) {
	quit, done := make(chan struct{}), make(chan struct{})

	go func() {
		defer close(done)

		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()

		for {
			f.sample()

			select {
			case <-quit:
				return
			case <-tick.C:
			}
		}
	}()

	return func() {
		close(quit)
		<-done
	}
}

func loadtestLevel(t *testing.T, shape string, n int, hold string) {
	t.Helper()

	served, targets := loadtestServe(t, shape, n, hold)
	defer served.stopIfRunning(t)

	var seen footprint

	stop := seen.watch()

	for _, tg := range targets {
		served.trigger(t, tg.pipeline, tg.job)
	}

	rows := loadtestDrain(t, served.state, targets, n)

	stop()

	holdFor, _ := time.ParseDuration(hold + "s")
	r := summarize(t, rows, holdFor)

	seen.mu.Lock()
	defer seen.mu.Unlock()

	t.Logf("%-9s %5d %4d %7d %9s %9s %9s %8s %5d %5d %7d", shape, n, r.failed, r.peak, pct(r.starts, .5), pct(r.starts, .95), pct(r.overs, .95), r.wall.Round(10*time.Millisecond), seen.gor, seen.fds, seen.heap>>20)

	ph := loadtestPhases(t, served.state, targets, rows, holdFor)
	t.Logf("          phases p95: claim->run %s | run->step %s | step overhead %s | step->finish %s | finish->queue %s | ENQUEUE->RUN p50 %s p95 %s", pct(ph[0], .95), pct(ph[1], .95), pct(ph[2], .95), pct(ph[3], .95), pct(ph[4], .95), pct(ph[5], .5), pct(ph[5], .95))
}

// loadtestPhases splits each job's time between its recorded boundaries, so the "over" column can be attributed: claim to run row (setup), run row to the task starting, the task beyond its hold (process spawn and reaping), the task ending to the run finishing, the run finishing to the queue row closing, and enqueue to run row end to end. Each slice is sorted.
func loadtestPhases(t *testing.T, state string, targets []loadtarget, rows []store.QueueRow, holdFor time.Duration) [6][]time.Duration {
	t.Helper()

	var out [6][]time.Duration

	queued := map[string]store.QueueRow{}
	for _, row := range rows {
		queued[row.JobName] = row
	}

	for _, tg := range targets {
		q, ok := queued[tg.job]
		if !ok || q.Status != "succeeded" {
			continue
		}

		run, began, ended, found := loadtestRunTimes(t, state, tg)
		if !found {
			continue
		}

		out[0] = append(out[0], run.StartedAt.Sub(q.StartedAt))
		out[1] = append(out[1], began.Sub(run.StartedAt))
		out[2] = append(out[2], ended.Sub(began)-holdFor)
		out[3] = append(out[3], run.FinishedAt.Sub(ended))
		out[4] = append(out[4], q.FinishedAt.Sub(run.FinishedAt))
		out[5] = append(out[5], run.StartedAt.Sub(q.EnqueuedAt))
	}

	for i := range out {
		slices.Sort(out[i])
	}

	return out
}

// loadtestRunTimes is a job's run row and when its hold step started and finished, from the events the run recorded; found is false when any is missing (the event sink may drop under load).
func loadtestRunTimes(t *testing.T, state string, tg loadtarget) (store.RunRow, time.Time, time.Time, bool) {
	t.Helper()

	st, err := sqlite.OpenStore(state, tg.pipeline)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), tg.job, 1)
	if err != nil || len(runs) != 1 {
		return store.RunRow{}, time.Time{}, time.Time{}, false
	}

	events, err := st.RunEvents(t.Context(), runs[0].ID, 0, 1000)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	began, ended := holdSpan(events)

	return runs[0], began, ended, !began.IsZero() && !ended.IsZero()
}

func holdSpan(events []store.RunEventRow) (began, ended time.Time) {
	for _, e := range events {
		switch {
		case e.StepName != "hold":
		case e.Type == "step_started" && began.IsZero():
			began = e.At
		case e.Type == "step_finished":
			ended = e.At
		}
	}

	return began, ended
}

type levelResult struct {
	failed int
	peak   int
	starts []time.Duration
	overs  []time.Duration
	wall   time.Duration
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}

	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))].Round(time.Millisecond)
}

// summarize reads latencies and the true overlap off the queue rows' own timestamps.
func summarize(t *testing.T, rows []store.QueueRow, holdFor time.Duration) levelResult {
	t.Helper()

	type edge struct {
		at    time.Time
		delta int
	}

	var (
		r     levelResult
		edges []edge
	)

	first, last := rows[0].EnqueuedAt, rows[0].FinishedAt

	for _, row := range rows {
		if row.Status != "succeeded" {
			r.failed++

			if r.failed == 1 {
				t.Logf("first failure (%s): %s", row.JobName, row.Error)
			}

			continue
		}

		r.starts = append(r.starts, row.StartedAt.Sub(row.EnqueuedAt))
		r.overs = append(r.overs, row.FinishedAt.Sub(row.StartedAt)-holdFor)
		edges = append(edges, edge{row.StartedAt, 1}, edge{row.FinishedAt, -1})
		first = minTime(first, row.EnqueuedAt)
		last = maxTime(last, row.FinishedAt)
	}

	// Ends sort before starts at the same instant, so a back-to-back handoff is not counted as overlap.
	slices.SortFunc(edges, func(a, b edge) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}

		return a.delta - b.delta
	})

	live := 0

	for _, e := range edges {
		live += e.delta
		r.peak = max(r.peak, live)
	}

	slices.Sort(r.starts)
	slices.Sort(r.overs)

	r.wall = last.Sub(first)

	return r
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}

	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// loadtestDrain waits until every triggered job has a finished queue row. A job that never finishes fails the test: that is a hang, not a data point.
func loadtestDrain(t *testing.T, state string, targets []loadtarget, want int) []store.QueueRow {
	t.Helper()

	pipelines := map[string]bool{}
	for _, tg := range targets {
		pipelines[tg.pipeline] = true
	}

	deadline := time.Now().Add(10 * time.Minute)

	for time.Now().Before(deadline) {
		rows, settled := loadtestRows(t, state, pipelines)
		if settled && len(rows) >= want {
			return rows
		}

		time.Sleep(250 * time.Millisecond)
	}

	t.Fatalf("%d jobs did not all finish within 10m", want)

	return nil
}

// loadtestRows reads every pipeline's queue; settled is false while any row is pending or running.
func loadtestRows(t *testing.T, state string, pipelines map[string]bool) ([]store.QueueRow, bool) {
	t.Helper()

	var rows []store.QueueRow

	settled := true

	for name := range pipelines {
		st, err := sqlite.OpenStore(state, name)
		if err != nil {
			t.Fatalf("open state store: %v", err)
		}

		got, err := st.ListTriggerQueue(t.Context(), 100000)
		_ = st.Close()

		if err != nil {
			t.Fatalf("ListTriggerQueue: %v", err)
		}

		for _, row := range got {
			if row.Status == "pending" || row.Status == "running" {
				settled = false
			}

			rows = append(rows, row)
		}
	}

	return rows, settled
}
