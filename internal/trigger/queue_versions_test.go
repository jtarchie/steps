package trigger

// The versions a poll resolves are the versions the job it enqueues runs on.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// cursorFeedPipeline is a cursor-driven resource: check emits only the items
// newer than the version it was handed, which is the shape docs/resources.md
// prescribes and the shipped Slack pipeline uses. The task appends every
// version it processes, so the test can count them.
func cursorFeedPipeline(feed, processed string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%%s{\"n\":\"%%s\"}", (k++?",":""), $1} END{printf "]"}' %s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, feed, processed)
}

// TestPollHandsItsVersionsToTheJobItEnqueues is the acceptance test for
// passing versions through the queue.
//
// A watcher started against an existing backlog must not answer the backlog.
// The poll gets this right on its own — a cold start seeds the baseline and
// enqueues nothing, and the next poll reports only what is genuinely new.
// The job used to throw that away and re-run check for itself, with no
// cursor, so it saw the whole window: 20 stale items plus the new one, each
// fanned out over as its own build. For the pipeline this feature was built
// for, that is 21 replies to threads nobody is waiting on and 21 paid model
// calls.
func TestPollHandsItsVersionsToTheJobItEnqueues(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	processed := filepath.Join(dir, "processed.txt")

	cfg := loadConfig(t, dir, cursorFeedPipeline(feed, processed))
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// A backlog nobody is waiting on.
	writeLines(t, feed, 20)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (cold start): %v", err)
	}

	if len(enqueued) != 0 {
		t.Fatalf("cold start enqueued %v, want nothing", enqueued)
	}

	// Exactly one new item arrives.
	writeLines(t, feed, 21)

	enqueued, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if len(enqueued) != 1 {
		t.Fatalf("enqueued = %v, want [build]", enqueued)
	}

	drainQueue(ctx, t, cfg, st)

	got := processedItems(t, processed)
	if len(got) != 1 || got[0] != "21" {
		t.Errorf("the job processed %v, want exactly [21] — the poll found one new item", got)
	}
}

// TestPollVersionsSurviveAReEnqueue covers the collision the queue dedupes
// on. A second poll while the first job is still pending used to be dropped
// outright, which was harmless only because the job re-derived everything.
// Now the row carries work, so dropping it would lose that work for good.
func TestPollVersionsSurviveAReEnqueue(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	processed := filepath.Join(dir, "processed.txt")

	cfg := loadConfig(t, dir, cursorFeedPipeline(feed, processed))
	st := mustOpenStore(t, dir)
	ctx := context.Background()

	writeLines(t, feed, 20)

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (cold start): %v", err)
	}

	// Two polls land before anything drains the queue.
	writeLines(t, feed, 21)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (first): %v", err)
	}

	writeLines(t, feed, 22)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (second): %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	got := processedItems(t, processed)
	if len(got) != 2 || got[0] != "21" || got[1] != "22" {
		t.Errorf("the job processed %v, want [21 22] — neither poll's findings may be dropped", got)
	}
}

// drainQueue runs claimed jobs the way runWorker does, until the queue is
// empty. Deliberately the real claim path rather than a direct RunJob: what
// is under test is what survives the round trip through the queue row.
func drainQueue(ctx context.Context, t *testing.T, cfg *config.Config, st *store.Store) {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	for range 10 {
		ran, err := drainOne(ctx, cfg, provider, st, nil, false)
		if err != nil {
			t.Fatalf("drainOne: %v", err)
		}

		if !ran {
			return
		}
	}

	t.Fatal("queue did not drain")
}

func writeLines(t *testing.T, path string, to int) {
	t.Helper()

	var lines strings.Builder

	for i := 1; i <= to; i++ {
		fmt.Fprintf(&lines, "%d\n", i)
	}

	err := os.WriteFile(path, []byte(lines.String()), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func processedItems(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Fields(string(data))
}

// TestSuppliedVersionsStillSkipOnRerun proves plan/run lockstep survived.
//
// Supplied versions are injected through the resource Cache, which is the one
// seam both the planner (merkle.PlanChains) and the executor read through. If
// they were injected anywhere else the two would resolve different lists,
// their hashes would not match, and nothing would ever be skipped — a
// failure that shows up as work silently repeating rather than as an error.
//
// No `version: every` here on purpose: the take-once cursor would suppress a
// repeat for its own reasons and the skip would prove nothing.
func TestSuppliedVersionsStillSkipOnRerun(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	ran := filepath.Join(dir, "ran.txt")

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat %s
    in: echo {{ .version.n | shellquote }} > n.txt
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
    inputs: [items]
    run: cat items/n.txt >> %s
`, feed, ran))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	writeVersions(t, feed, `[{"n":"1"}]`)

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	writeVersions(t, feed, `[{"n":"2"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	if got := processedItems(t, ran); len(got) != 1 {
		t.Fatalf("first run processed %v, want one version", got)
	}

	// Queue the same job again. The task is unchanged content over an
	// unchanged version, so the planner must find it already recorded and the
	// run must skip it.
	err = st.EnqueueJob(ctx, "build", "items")
	if err != nil {
		t.Fatal(err)
	}

	drainQueue(ctx, t, cfg, st)

	if got := processedItems(t, ran); len(got) != 1 {
		t.Errorf("after a second identical trigger the task ran %d times, want 1 — plan and run disagreed on the version", len(got))
	}
}

// TestColdStartSurvivesAPartialPoll: the baseline and the cold-start seeding
// land together, per resource, not at the end of the poll.
//
// pollOnce's end-of-poll baselining exists so a crash between check and
// enqueue cannot skip work — but a cold start enqueues nothing, so that
// ordering protects nothing and waiting was actively harmful: with the
// baseline deferred, ANY later resource's check failure aborted the poll
// first, the resource read as cold again next poll, and the re-seed marked
// versions that had arrived in between as already taken. Never enqueued,
// silently dropped, forever.
func TestColdStartSurvivesAPartialPoll(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	other := filepath.Join(dir, "other.json")
	processed := filepath.Join(dir, "processed.txt")

	// Two resources; "broken" is checked after "items" (job order) and its
	// versions file does not exist yet, so every poll aborts after items.
	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%%s{\"n\":\"%%s\"}", (k++?",":""), $1} END{printf "]"}' %s
    in: echo {{ .version.n | shellquote }} > n.txt
- name: fixed
  config:
    check: cat %s
    in: echo x > x.txt
resources:
- name: items
  type: feed
  source: {}
- name: broken
  type: fixed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
- name: sidecar
  plan:
  - get: broken
    trigger: true
`, feed, other, processed))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Cold start over a backlog; the poll ABORTS on broken's check.
	writeLines(t, feed, 20)

	_, err := pollOnce(ctx, cfg, st)
	if err == nil {
		t.Fatal("pollOnce: want the broken resource's check to fail the poll")
	}

	// Item 21 arrives while broken is still down. This is the window the
	// deferred baseline used to swallow: items read as cold again, and the
	// re-seed marked 21 as already taken.
	writeLines(t, feed, 21)

	_, err = pollOnce(ctx, cfg, st)
	if err == nil {
		t.Fatal("pollOnce: want the broken resource's check to still fail")
	}

	// broken recovers; the next poll must enqueue build for item 21.
	writeVersions(t, other, `[{"ref":"r1"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (recovered): %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	if got := processedItems(t, processed); len(got) != 1 || got[0] != "21" {
		t.Errorf("the job processed %v, want [21] — arrivals during a partial poll must not be seeded away", got)
	}
}
