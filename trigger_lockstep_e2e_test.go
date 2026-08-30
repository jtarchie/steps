package main

// Multi-`version: every` through the CLI: input sets, observed from outside.
//
// Same discipline as trigger_e2e_test.go — every test drives the real CLI
// (`web --once`, or `run` where a manual trigger is the shape under test)
// against a real pipeline and asserts only on what an operator could see.
// These are separate because they cover behavior that did not exist when that
// file froze: more than one get fanning out, in lockstep.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lockstepFixture is watchFixture with two independent feeds, for a job whose
// plan has two `version: every` gets.
type lockstepFixture struct {
	pipeline  string
	feedA     string
	feedB     string
	processed string
}

// lockstepPipeline pairs two cursor-driven feeds: each build appends
// "<a>+<b>" so the processed file IS the sequence of input sets built.
const lockstepPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' {{ .source.file }}
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: a
  type: feed
  source: {file: FEED_A}
- name: b
  type: feed
  source: {file: FEED_B}
jobs:
- name: build
  plan:
  - get: a
    trigger: true
    version: every
  - get: b
    trigger: true
    version: every
  - task: work
    inputs: [a, b]
    run: echo "$(cat a/n.txt)+$(cat b/n.txt)" >> PROCESSED
`

func newLockstepFixture(t *testing.T, pipelineYAML string) *lockstepFixture {
	t.Helper()

	dir := t.TempDir()
	fixture := &lockstepFixture{
		pipeline:  filepath.Join(dir, "pipeline.yml"),
		feedA:     filepath.Join(dir, "feed-a.txt"),
		feedB:     filepath.Join(dir, "feed-b.txt"),
		processed: filepath.Join(dir, "processed.txt"),
	}

	body := strings.NewReplacer(
		"FEED_A", fixture.feedA,
		"FEED_B", fixture.feedB,
		"PROCESSED", fixture.processed,
	).Replace(pipelineYAML)

	err := os.WriteFile(fixture.pipeline, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Both feeds start with one version: an EMPTY first check records no
	// baseline at all (recordHistory returns before seeding), so the first
	// non-empty poll would become the cold start and swallow whatever it saw.
	// Every test therefore begins from a cold start at v1 — consumed by
	// seeding, never built — and makes its arrivals from v2.
	fixture.feed(t, fixture.feedA, 1)
	fixture.feed(t, fixture.feedB, 1)

	return fixture
}

// feed writes the given feed as the versions 1..n.
func (f *lockstepFixture) feed(t *testing.T, path string, n int) {
	t.Helper()

	var lines strings.Builder

	for i := 1; i <= n; i++ {
		fmt.Fprintf(&lines, "%d\n", i)
	}

	err := os.WriteFile(path, []byte(lines.String()), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *lockstepFixture) watch(t *testing.T) {
	t.Helper()

	mustRun(t, "web", f.pipeline, "--once")
}

// watchExpectingFailure runs a cycle whose job is meant to fail, and insists
// that it did. Discarding the error would let a test that no longer injects a
// failure keep passing as a no-failure scenario.
// coldStart runs the first-ever poll, which builds the newest set it finds
// and seeds everything below it as taken (see watchFixture.coldStart). Every
// test here is about what arrives AFTER a baseline exists, so that one build
// is discarded rather than written into each expectation.
func (f *lockstepFixture) coldStart(t *testing.T) {
	t.Helper()

	f.watch(t)

	err := os.WriteFile(f.processed, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *lockstepFixture) watchExpectingFailure(t *testing.T) {
	t.Helper()

	err := run([]string{"web", f.pipeline, "--once"})
	if err == nil {
		t.Fatal("the cycle was supposed to fail a build; it succeeded")
	}
}

// mustReplace is strings.Replace that fails the test when it matches nothing.
// Every fixture here is built by patching a shared pipeline, and a silent
// no-op patch degrades a test into a weaker one that still passes.
func mustReplace(t *testing.T, s, old, replacement string) string {
	t.Helper()

	out := strings.Replace(s, old, replacement, 1)
	if out == s {
		t.Fatalf("fixture patch matched nothing: %q", old)
	}

	return out
}

func (f *lockstepFixture) assertDid(t *testing.T, want ...string) {
	t.Helper()

	data, err := os.ReadFile(f.processed)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("job built the sets %v, want %v", got, want)
	}
}

// TestWatchLockstepStreamingInterleave is the primary scenario: updates
// arriving one resource at a time, the steady state of any real pair of
// inputs. Each cycle builds exactly one set — the newly arrived version
// paired with the sibling HELD at its last built version — and repeats
// nothing.
func TestWatchLockstepStreamingInterleave(t *testing.T) {
	fixture := newLockstepFixture(t, lockstepPipeline)
	fixture.coldStart(t) // baseline at (a1, b1): built once, then discarded

	fixture.feed(t, fixture.feedA, 2)
	fixture.feed(t, fixture.feedB, 2)
	fixture.watch(t)
	fixture.assertDid(t, "2+2")

	// b moves alone: one set, a held where it stands.
	fixture.feed(t, fixture.feedB, 3)
	fixture.watch(t)
	fixture.assertDid(t, "2+2", "2+3")

	// a catches up alone: one set, b held.
	fixture.feed(t, fixture.feedA, 3)
	fixture.watch(t)
	fixture.assertDid(t, "2+2", "2+3", "3+3")

	// Quiet poll: nothing re-runs.
	fixture.watch(t)
	fixture.assertDid(t, "2+2", "2+3", "3+3")
}

// TestWatchLockstepDiagonal: several inputs with backlogs at once — a burst,
// or cold recovery — advance in lockstep, one step per set, the shorter input
// holding at its newest once exhausted. Concourse's diagonal, not a cross
// product: 3x2 backlogs mean three builds, not six.
func TestWatchLockstepDiagonal(t *testing.T) {
	fixture := newLockstepFixture(t, lockstepPipeline)
	fixture.coldStart(t) // baseline: built once, then discarded

	fixture.feed(t, fixture.feedA, 4)
	fixture.feed(t, fixture.feedB, 3)
	fixture.watch(t)
	fixture.assertDid(t, "2+2", "3+3", "4+3")

	fixture.watch(t)
	fixture.assertDid(t, "2+2", "3+3", "4+3")
}

// TestWatchLockstepFailureAdvances: a set that fails is consumed — its marks
// advanced when the build STARTED — so later sets still run and nothing is
// retried. Same rule as the single-every case, per set instead of per
// version.
func TestWatchLockstepFailureAdvances(t *testing.T) {
	fixture := newLockstepFixture(t, mustReplace(t, lockstepPipeline,
		`run: echo "$(cat a/n.txt)+$(cat b/n.txt)" >> PROCESSED`,
		`run: |
      echo "$(cat a/n.txt)+$(cat b/n.txt)" >> PROCESSED
      test "$(cat a/n.txt)" != 3`))

	fixture.coldStart(t) // baseline: built once, then discarded

	fixture.feed(t, fixture.feedA, 4)
	fixture.feed(t, fixture.feedB, 4)
	fixture.watchExpectingFailure(t) // the middle set fails

	fixture.assertDid(t, "2+2", "3+3", "4+4")

	fixture.watch(t)
	fixture.assertDid(t, "2+2", "3+3", "4+4")
}

// TestWatchLockstepSkipStillConsumesTheSet: a set whose chain already
// succeeded — here because a pinned run built it first — skips instead of
// re-running, and the skip must still advance EVERY fanning cursor, or the
// set stays unconsumed and re-fans forever. Observed the only way an
// operator could: after the skip, the task changes; a consumed set stays
// idle, an unconsumed one would rebuild under the new task.
func TestWatchLockstepSkipStillConsumesTheSet(t *testing.T) {
	fixture := newLockstepFixture(t, lockstepPipeline)
	fixture.coldStart(t) // baseline: built once, then discarded

	fixture.feed(t, fixture.feedA, 2)
	fixture.feed(t, fixture.feedB, 2)
	fixture.watch(t)
	fixture.assertDid(t, "2+2")

	// A pinned run builds (a3, b3) out of band — and consumes nothing.
	fixture.feed(t, fixture.feedA, 3)
	fixture.feed(t, fixture.feedB, 3)
	mustRun(t, "web", fixture.pipeline, "--once", "--pin", "n=3")
	fixture.assertDid(t, "2+2", "3+3")

	// The natural flow reaches the same set — a MANUAL run, since the poll
	// already baselined a3/b3 during the pinned cycle and will not trigger
	// again. Its chain already succeeded, so it skips — and is consumed by
	// the skip.
	mustRun(t, "run", fixture.pipeline)
	fixture.assertDid(t, "2+2", "3+3")

	// The proof the skip consumed it: change the task — so the old chain no
	// longer masks a re-fan — and deliver a4 to trigger the job. A consumed
	// (a3, b3) leaves exactly one set, (a4, b3-held). Leftover unconsumed
	// versions would fan an extra (a3, b3) set and build "changed:3+3".
	pipeline, err := os.ReadFile(fixture.pipeline)
	if err != nil {
		t.Fatal(err)
	}

	changed := mustReplace(t, string(pipeline), `echo "$(cat`, `echo "changed:$(cat`)

	err = os.WriteFile(fixture.pipeline, []byte(changed), 0o600) //nolint:gosec // a t.TempDir()-scoped file this test wrote
	if err != nil {
		t.Fatal(err)
	}

	fixture.feed(t, fixture.feedA, 4)
	fixture.watch(t)
	fixture.assertDid(t, "2+2", "3+3", "changed:4+3")
}

// TestWatchLockstepNonEveryRidesAlong: a non-every get is not a fan-out point
// — it binds its single resolved version into EVERY set, however many sets
// its every siblings produce.
func TestWatchLockstepNonEveryRidesAlong(t *testing.T) {
	fixture := newLockstepFixture(t, mustReplace(t, lockstepPipeline, `
  - get: a
    trigger: true
    version: every`, `
  - get: a
    trigger: true`))

	fixture.coldStart(t) // baseline: built once, then discarded

	fixture.feed(t, fixture.feedA, 2)
	fixture.feed(t, fixture.feedB, 3)
	fixture.watch(t)

	fixture.assertDid(t, "2+2", "2+3")
}

// TestWatchLockstepColdStartBuildsOneSet: with the cold-start rule now
// building the newest version, a first poll facing backlogs in BOTH feeds
// must still build exactly ONE set — the newest of each — and not walk the
// diagonal over everything it just discovered. This is the shape the amended
// rule could most easily break, and the reason `version: every` needs its own
// cold-start test rather than sharing the single-resource one.
func TestWatchLockstepColdStartBuildsOneSet(t *testing.T) {
	fixture := newLockstepFixture(t, lockstepPipeline)

	fixture.feed(t, fixture.feedA, 4)
	fixture.feed(t, fixture.feedB, 3)

	fixture.watch(t)

	fixture.assertDid(t, "4+3")

	// And the backlog below stays taken: a quiet poll adds nothing.
	fixture.watch(t)
	fixture.assertDid(t, "4+3")
}
