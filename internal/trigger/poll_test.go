package trigger

// Poll is Watch with the workers removed, so what it must be tested for is
// the removal: that it fills the queue and then leaves it alone, because the
// process that called it (steps web) is draining the same rows itself.

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

const pollFeed = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat VERSIONS
    in: "true"
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - task: work
    run: echo built
`

const untriggeredPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: "echo []"
    in: "true"
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
`

// TestPollEnqueuesAndLeavesTheRowAlone: the queued job must still be there,
// unclaimed, when polling stops. A Poll that drained would report a run and
// hand this test an empty queue — which is exactly what would happen to the
// UI's own runner if this function grew workers.
func TestPollEnqueuesAndLeavesTheRowAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	writeVersions(t, versions, `[{"n":"1"}]`)

	cfg := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", versions, 1))
	st := mustOpenStore(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// The cold start records where things stand and enqueues nothing, so the
	// arrival below is the only thing Poll can be reacting to.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}

	writeVersions(t, versions, `[{"n":"1"},{"n":"2"}]`)

	done := make(chan error, 1)

	go func() { done <- Poll(ctx, staticConfig(cfg), st, 20*time.Millisecond) }()

	id, jobName := waitForQueuedJob(ctx, t, st)

	cancel()

	err = <-done
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if jobName != "build" || id == 0 {
		t.Errorf("claimed (%d, %q), want a queued build", id, jobName)
	}

	// Nothing ran: the row this test just claimed was still waiting for a
	// drainer that Poll deliberately does not provide.
	runs, err := st.ListRuns(t.Context(), "build", 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	if len(runs) != 0 {
		t.Errorf("Poll recorded %d run(s); it must fill the queue, never drain it", len(runs))
	}
}

// TestPollReportsNothingToWatch: the caller decides whether that is fatal —
// watch refuses to start, web serves anyway — so it has to be recognizable
// rather than just an error string.
func TestPollReportsNothingToWatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, untriggeredPipeline)
	st := mustOpenStore(t, dir)

	err := Poll(t.Context(), staticConfig(cfg), st, time.Second)
	if !errors.Is(err, ErrNoTriggers) {
		t.Fatalf("Poll = %v, want ErrNoTriggers", err)
	}
}

// waitForQueuedJob claims the first row polling enqueues.
func waitForQueuedJob(ctx context.Context, t *testing.T, st *store.Store) (int64, string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		id, jobName, ok, err := st.ClaimNextJob(ctx)
		if err != nil {
			t.Fatalf("ClaimNextJob: %v", err)
		}

		if ok {
			return id, jobName
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("nothing was ever enqueued")

	return 0, ""
}

// swappedFeed is pollFeed with a second resource and a job that triggers on
// it — the shape of an edit that adds something to watch.
const swappedFeed = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat VERSIONS
    in: "true"
resources:
- name: items
  type: feed
  source: {}
- name: extras
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - task: work
    run: echo built
- name: publish
  plan:
  - get: extras
    trigger: true
  - task: ship
    run: echo shipped
`

// TestPollFollowsAConfigSwap is the seam between the daemon's reload and this
// loop: `steps web` swaps the configuration under a running poller, and a
// loop holding the one it started with would go on checking resources the
// operator deleted while never checking the ones they added.
//
// Asserted through what the poll ENQUEUES, not through what it read: a
// source consulted per cycle whose result is then ignored would pass any
// check of the reading alone.
func TestPollFollowsAConfigSwap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")

	// Empty at the cold start, so it records nothing and enqueues nothing —
	// which is what makes every row below attributable to a poll cycle. A
	// cold start over a non-empty feed takes the newest version and triggers
	// once, and that row would satisfy the wait before the loop had run.
	writeVersions(t, versions, `[]`)

	before := loadConfig(t, dir, strings.Replace(pollFeed, "VERSIONS", versions, 1))
	after := loadConfig(t, dir, strings.ReplaceAll(swappedFeed, "VERSIONS", versions))
	st := mustOpenStore(t, dir)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	_, err := pollOnce(ctx, before, st)
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}

	current := &atomic.Pointer[config.Config]{}
	current.Store(before)

	done := make(chan error, 1)

	go func() { done <- Poll(ctx, current.Load, st, 20*time.Millisecond) }()

	// build can only come from a poll cycle, and the swap below can only
	// happen after one — so publish, which exists in no configuration the
	// loop has read yet, is reachable only by reading the source again.
	writeVersions(t, versions, `[{"n":"1"}]`)
	waitForQueuedName(ctx, t, st, "build")

	current.Store(after)
	writeVersions(t, versions, `[{"n":"1"},{"n":"2"}]`)

	waitForQueuedName(ctx, t, st, "publish")

	cancel()

	err = <-done
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

// waitForQueuedName waits for the trigger queue to hold a row for one job,
// without claiming it — the caller is asserting what polling decided, not
// standing in for a drainer.
func waitForQueuedName(ctx context.Context, t *testing.T, st *store.Store, jobName string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		rows, err := st.ListTriggerQueue(ctx, 25)
		if err != nil {
			t.Fatalf("ListTriggerQueue: %v", err)
		}

		if slices.ContainsFunc(rows, func(row store.QueueRow) bool { return row.JobName == jobName }) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("the poller never enqueued %s", jobName)
}
