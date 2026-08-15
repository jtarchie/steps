package main

// The watch contract, end to end through the CLI.
//
// Every test here drives `steps watch --once` against a real pipeline and a
// real store, and asserts only on what an operator could see: which versions
// a job processed, and what the store holds afterwards. Nothing reaches into
// an unexported function.
//
// That is the point. This file exists to be the fixed reference while the
// machinery underneath it changes — triggering, version selection, `passed:`,
// pinning and caching are the most load-bearing behavior in the project, and
// they have been reworked repeatedly in ways whose defects only showed up
// where two layers meet. A suite written against the internals moves when the
// internals move and proves nothing about a refactor. This one should not
// have to change at all; an assertion here that has to be edited is a
// behavior change, and needs an argument rather than an edit.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// watchFixture writes a pipeline plus the file its check reads, and returns
// the pipeline path.
type watchFixture struct {
	dir       string
	pipeline  string
	feed      string
	processed string
}

func newWatchFixture(t *testing.T, pipelineYAML string) *watchFixture {
	t.Helper()

	dir := t.TempDir()
	fixture := &watchFixture{
		dir:       dir,
		pipeline:  filepath.Join(dir, "pipeline.yml"),
		feed:      filepath.Join(dir, "feed.txt"),
		processed: filepath.Join(dir, "processed.txt"),
	}

	body := strings.NewReplacer("FEED", fixture.feed, "PROCESSED", fixture.processed).Replace(pipelineYAML)

	err := os.WriteFile(fixture.pipeline, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return fixture
}

// items writes the feed as the versions 1..n, so a cursor-driven check has a
// backlog to be careful about.
func (f *watchFixture) items(t *testing.T, n int) {
	t.Helper()

	var lines strings.Builder

	for i := 1; i <= n; i++ {
		fmt.Fprintf(&lines, "%d\n", i)
	}

	f.write(t, f.feed, lines.String())
}

func (f *watchFixture) write(t *testing.T, path, contents string) {
	t.Helper()

	err := os.WriteFile(path, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// watch runs one poll-and-drain cycle through the CLI.
func (f *watchFixture) watch(t *testing.T, args ...string) {
	t.Helper()

	mustRun(t, append([]string{"watch", f.pipeline, "--once"}, args...)...)
}

// watchExpectingFailure runs a cycle whose job is meant to fail.
func (f *watchFixture) watchExpectingFailure(t *testing.T) {
	t.Helper()

	_ = run([]string{"watch", f.pipeline, "--once"})
}

// did returns the versions the job processed, in order.
func (f *watchFixture) did(t *testing.T) []string {
	t.Helper()

	data, err := os.ReadFile(f.processed)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Fields(string(data))
}

func (f *watchFixture) assertDid(t *testing.T, want ...string) {
	t.Helper()

	got := f.did(t)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("job processed %v, want %v", got, want)
	}
}

// cursorFeed is a resource whose check reports only what is newer than the
// version it was handed — the shape docs/resources.md prescribes and the
// shipped Slack pipeline uses. Everything about backlogs and floods depends
// on a check like this one.
const cursorFeed = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' FEED
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
    run: cat items/n.txt >> PROCESSED
`

// TestWatchColdStartDoesNotAnswerTheBacklog is the behavior the whole cursor
// effort exists for. A watcher pointed at a resource that already has history
// must not treat all of it as new: it records where things stand and waits.
func TestWatchColdStartDoesNotAnswerTheBacklog(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 20)

	fixture.watch(t)

	fixture.assertDid(t)
}

// TestWatchProcessesOnlyWhatIsNew: one new item after a cold start means one
// unit of work, not the whole window. For the pipeline this was built for,
// getting it wrong is 21 replies to threads nobody is waiting on.
func TestWatchProcessesOnlyWhatIsNew(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 20)
	fixture.watch(t)

	fixture.items(t, 21)
	fixture.watch(t)

	fixture.assertDid(t, "21")
}

// TestWatchLosesNothingAcrossPolls: two arrivals between drains are two units
// of work. Nothing may be dropped on the way through the queue.
func TestWatchLosesNothingAcrossPolls(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 20)
	fixture.watch(t)

	fixture.items(t, 21)
	fixture.watch(t)
	fixture.items(t, 22)
	fixture.watch(t)

	fixture.assertDid(t, "21", "22")
}

// TestWatchIsIdleWhenNothingChanges: polling a quiet resource repeatedly does
// no work at all. The obvious property, and the one a cursor bug breaks in
// the loudest possible way.
func TestWatchIsIdleWhenNothingChanges(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 20)
	fixture.watch(t)

	fixture.items(t, 21)
	fixture.watch(t)

	for range 3 {
		fixture.watch(t)
	}

	fixture.assertDid(t, "21")
}

// TestWatchVersionEveryTakesEachVersionOnce: a fan-out over several new
// versions runs each once, and a later poll repeats none of them — including
// after a failure, since a version is taken when its build starts.
func TestWatchVersionEveryTakesEachVersionOnce(t *testing.T) {
	fixture := newWatchFixture(t, strings.Replace(cursorFeed,
		"run: cat items/n.txt >> PROCESSED",
		"run: |\n      cat items/n.txt >> PROCESSED\n      test \"$(cat items/n.txt)\" != 22", 1))

	fixture.items(t, 20)
	fixture.watch(t)

	// Three new versions at once, the middle one failing its task.
	fixture.items(t, 23)
	fixture.watchExpectingFailure(t)

	fixture.assertDid(t, "21", "22", "23")

	// Nothing is retried, including the failure.
	fixture.watch(t)
	fixture.assertDid(t, "21", "22", "23")
}

// TestWatchSkipsUnchangedWork: re-triggering a job for a version it already
// built does that work no second time. This is plan/run lockstep observed
// from outside — if the planner and the executor resolved versions
// differently their hashes would not match and nothing would ever skip.
func TestWatchSkipsUnchangedWork(t *testing.T) {
	fixture := newWatchFixture(t, strings.Replace(cursorFeed, `
  - get: items
    trigger: true
    version: every`, `
  - get: items
    trigger: true`, 1))

	fixture.items(t, 1)
	fixture.watch(t)

	fixture.items(t, 2)
	fixture.watch(t)
	fixture.assertDid(t, "2")

	// The same version arriving again (a re-trigger of unchanged content).
	fixture.watch(t)
	fixture.assertDid(t, "2")
}

// TestWatchPinReachesPastWhatThePollSaw: a pin names a version, and naming
// one is an instruction. It must resolve even though the poll only ever
// observed newer versions.
func TestWatchPinReachesPastWhatThePollSaw(t *testing.T) {
	fixture := newWatchFixture(t, strings.Replace(cursorFeed, `
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' FEED`, `
    check: cat FEED`, 1))

	fixture.write(t, fixture.feed, `[{"n":"1"},{"n":"2"}]`)
	fixture.watch(t)

	fixture.write(t, fixture.feed, `[{"n":"1"},{"n":"2"},{"n":"3"}]`)
	fixture.watch(t, "--pin", "n=1")

	if got := fixture.did(t); len(got) == 0 || got[len(got)-1] != "1" {
		t.Errorf("processed %v, want the pinned version 1 to have been built", got)
	}
}

// TestWatchNumericVersionFieldsKeepTheirDigits: a version field goes back out
// to whatever produced it, so an id wider than float64 or a fractional
// timestamp must arrive at `in:` exactly as the check reported it. Rendering
// 1699887654.001200 as 1.6998876540012e+09 sends an API a value it has never
// seen.
func TestWatchNumericVersionFieldsKeepTheirDigits(t *testing.T) {
	fixture := newWatchFixture(t, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat FEED
    in: echo '{{ .version.ts }}' > ts.txt
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
    run: cat items/ts.txt >> PROCESSED
`)

	fixture.write(t, fixture.feed, `[{"ts":1699887654.001200}]`)
	fixture.watch(t)

	fixture.write(t, fixture.feed, `[{"ts":1699887654.001200},{"ts":1699887999.000100}]`)
	fixture.watch(t)

	fixture.assertDid(t, "1699887999.000100")
}

// TestWatchResolvesAnUntriggeredGet: a job may read resources nobody polls.
// Those still resolve, by running their own check.
func TestWatchResolvesAnUntriggeredGet(t *testing.T) {
	fixture := newWatchFixture(t, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat FEED
    in: echo {{ .version.n | shellquote }} > n.txt
- name: fixed
  config:
    check: printf '[{"ref":"r1"}]'
    in: echo r1 > ref.txt
resources:
- name: items
  type: feed
  source: {}
- name: sidecar
  type: fixed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - get: sidecar
  - task: work
    inputs: [items, sidecar]
    run: echo "$(cat items/n.txt)-$(cat sidecar/ref.txt)" >> PROCESSED
`)

	fixture.write(t, fixture.feed, `[{"n":"1"}]`)
	fixture.watch(t)

	fixture.write(t, fixture.feed, `[{"n":"2"}]`)
	fixture.watch(t)

	fixture.assertDid(t, "2-r1")
}
