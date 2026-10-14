package web

import (
	"context"
	"maps"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/scheduler"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// countingStore counts every read a board render makes of the store, by
// method.
type countingStore struct {
	store.Store

	mu    sync.Mutex
	calls map[string]int
}

func (c *countingStore) count(method string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls[method]++
}

func (c *countingStore) reset() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()

	calls := c.calls
	c.calls = map[string]int{}

	return calls
}

func (c *countingStore) Paused(ctx context.Context) (bool, error) {
	c.count("Paused")

	return c.Store.Paused(ctx) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) HeldJobs(ctx context.Context) ([]store.HeldJob, error) {
	c.count("HeldJobs")

	return c.Store.HeldJobs(ctx) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) CheckErrors(ctx context.Context) ([]store.CheckError, error) {
	c.count("CheckErrors")

	return c.Store.CheckErrors(ctx) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) Approvals(ctx context.Context, pendingOnly bool, limit int) ([]store.Approval, error) {
	c.count("Approvals")

	return c.Store.Approvals(ctx, pendingOnly, limit) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) Questions(ctx context.Context, pendingOnly bool, limit int) ([]store.Question, error) {
	c.count("Questions")

	return c.Store.Questions(ctx, pendingOnly, limit) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) LatestRunByJob(ctx context.Context) (map[string]store.RunRow, error) {
	c.count("LatestRunByJob")

	return c.Store.LatestRunByJob(ctx) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) LatestFinishedRunByJob(ctx context.Context) (map[string]store.RunRow, error) {
	c.count("LatestFinishedRunByJob")

	return c.Store.LatestFinishedRunByJob(ctx) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) ListRuns(ctx context.Context, jobName string, limit int) ([]store.RunRow, error) {
	c.count("ListRuns")

	return c.Store.ListRuns(ctx, jobName, limit) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) ListTriggerQueue(ctx context.Context, limit int) ([]store.QueueRow, error) {
	c.count("ListTriggerQueue")

	return c.Store.ListTriggerQueue(ctx, limit) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) FirstRunSince(ctx context.Context, jobName string, since time.Time) (store.RunRow, bool, error) {
	c.count("FirstRunSince")

	return c.Store.FirstRunSince(ctx, jobName, since) //nolint:wrapcheck // a test passthrough
}

func (c *countingStore) SerialGroupHolder(ctx context.Context, jobName string) (string, error) {
	c.count("SerialGroupHolder")

	return c.Store.SerialGroupHolder(ctx, jobName) //nolint:wrapcheck // a test passthrough
}

// TestABoardPollReadsEachThingOnce pins what one poll of the board costs the
// store: the shell, the attention list, the mark and the page share one
// request's reads, and a busy job's last finished run is one query for the
// whole board rather than one per busy job. On this fixture that was 18 reads
// per poll before (Paused 3, HeldJobs/CheckErrors/LatestRunByJob/
// ListTriggerQueue 2 each, ListRuns 4) and is 9.
func TestABoardPollReadsEachThingOnce(t *testing.T) {
	t.Parallel()

	server, counted := busyBoard(t)

	code, body := get(t, server, "/p/demo")
	if code != http.StatusOK {
		t.Fatalf("GET /p/demo = %d", code)
	}

	if !strings.Contains(body, "build") {
		t.Fatal("the board did not render")
	}

	calls := counted.reset()

	want := map[string]int{
		"Paused":                 1,
		"HeldJobs":               1,
		"CheckErrors":            1,
		"Approvals":              1,
		"Questions":              1,
		"LatestRunByJob":         1,
		"LatestFinishedRunByJob": 1,
		"ListTriggerQueue":       1, // the board's 25 rows are the first of the switcher's 50
		"FirstRunSince":          1,
	}

	total := 0
	for _, n := range calls {
		total += n
	}

	t.Logf("one board poll: %d store reads %v", total, calls)

	if !maps.Equal(calls, want) {
		t.Errorf("one board poll read %v (%d reads, keys %v); want %v", calls, total, slices.Sorted(maps.Keys(calls)), want)
	}
}

// busyBoard is a pipeline with two jobs busy over a finished run each — the
// shape that cost a read per busy job — and a third waiting in the queue.
func busyBoard(t *testing.T) (*Server, *countingStore) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")

	writeFile(t, path, `
jobs:
  - name: build
    plan:
      - task: compile
        run: "true"
  - name: deploy
    plan:
      - task: ship
        run: "true"
  - name: lint
    plan:
      - task: vet
        run: "true"
`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	inner, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = inner.Close() })

	for _, job := range []string{"build", "lint"} {
		finishedRun(t, &Pipeline{Pipeline: &scheduler.Pipeline{Store: inner}}, job+"-1", job, "failed")

		time.Sleep(2 * time.Millisecond)

		err = inner.StartRun(t.Context(), store.NewRun{ID: job + "-2", JobName: job})
		if err != nil {
			t.Fatal(err)
		}
	}

	err = inner.EnqueueJob(t.Context(), "deploy", "test", "")
	if err != nil {
		t.Fatal(err)
	}

	counted := &countingStore{Store: inner, calls: map[string]int{}}

	server, err := New([]*Pipeline{NewPipeline("demo", path, cfg, counted, events.New(nil))}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, counted
}
