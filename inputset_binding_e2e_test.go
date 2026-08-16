package main

// What an input set binds, what a build's success records, and what a get
// reports having run — the three seams multi-`every` introduced or moved,
// observed through the CLI.
//
// The first two were wrong when input sets first shipped: a set keyed by
// RESOURCE overrode a second get's own `version:`, and green records keyed per
// JOB kept only the last set's versions. Each test here fails loudly against
// that shape, which is the only reason to keep them.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bindingFixture is a pipeline over one cursor-driven feed, run through the
// CLI a job at a time.
type bindingFixture struct {
	pipeline  string
	feed      string
	processed string
}

func newBindingFixture(t *testing.T, pipelineYAML string) *bindingFixture {
	t.Helper()

	dir := t.TempDir()
	fixture := &bindingFixture{
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

// write puts literal contents in the feed, for a check that reads it whole.
func (f *bindingFixture) write(t *testing.T, contents string) {
	t.Helper()

	err := os.WriteFile(f.feed, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// items writes the feed as the versions 1..n.
func (f *bindingFixture) items(t *testing.T, n int) {
	t.Helper()

	var lines strings.Builder

	for i := 1; i <= n; i++ {
		fmt.Fprintf(&lines, "%d\n", i)
	}

	err := os.WriteFile(f.feed, []byte(lines.String()), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *bindingFixture) run(t *testing.T, job string) {
	t.Helper()

	mustRun(t, "run", f.pipeline, "--job", job)
}

func (f *bindingFixture) runExpectingFailure(t *testing.T, job string) {
	t.Helper()

	err := run([]string{"run", f.pipeline, "--job", job})
	if err == nil {
		t.Fatalf("job %q was supposed to fail; it succeeded", job)
	}
}

func (f *bindingFixture) assertDid(t *testing.T, want ...string) {
	t.Helper()

	data, err := os.ReadFile(f.processed)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("job processed %v, want %v", got, want)
	}
}

// cursorFeedType is the check/in pair every fixture here shares.
const cursorFeedType = `
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
`

// TestRunAliasedGetKeepsItsOwnVersion: two gets, one resource — the fanning
// one walks its versions while the ALIAS holds the version it pinned. This is
// the diff-against-a-baseline shape, and a set keyed by resource silently
// broke it: the alias fetched whatever the fan-out was on, so a comparison
// against the baseline always found no difference.
func TestRunAliasedGetKeepsItsOwnVersion(t *testing.T) {
	fixture := newBindingFixture(t, cursorFeedType+`
jobs:
- name: build
  plan:
  - get: items
    version: every
  - get: baseline
    resource: items
    version: {n: "1"}
  - task: work
    inputs: [items, baseline]
    run: echo "$(cat items/n.txt)-vs-$(cat baseline/n.txt)" >> PROCESSED
`)

	fixture.items(t, 3)
	fixture.run(t, "build")

	fixture.assertDid(t, "1-vs-1", "2-vs-1", "3-vs-1")
}

// passedAcrossSets is an upstream that fans out and a downstream gated on it.
const passedAcrossSets = cursorFeedType + `
jobs:
- name: upstream
  plan:
  - get: items
    version: every
  - task: ok
    inputs: [items]
    run: RUN
- name: downstream
  plan:
  - get: items
    version: every
    passed: [upstream]
  - task: work
    inputs: [items]
    run: cat items/n.txt >> PROCESSED
`

// TestRunPassedSeesEveryGreenSet: a downstream `passed:` gate must see EVERY
// version its upstream built green, not only the last one. Recording green
// per job instead of per build lost the earlier sets outright — and nothing
// ever went back for them, since an exhausted input holds at its NEWEST
// covered version, so a version superseded within one run was skipped
// forever.
func TestRunPassedSeesEveryGreenSet(t *testing.T) {
	fixture := newBindingFixture(t, strings.Replace(passedAcrossSets, "run: RUN", `run: "true"`, 1))

	fixture.items(t, 2)
	fixture.run(t, "upstream")
	fixture.run(t, "downstream")

	fixture.assertDid(t, "1", "2")
}

// TestRunGetRecordsExecutionBeforeItsHooks: a get that runs IN PLACE — the
// second and later gets of a plan — must appear in assert.execution in the
// same position and on the same terms as one that fans out: before its own
// hooks, and whether or not the fetch succeeded. Recording it after the fetch
// (and after the hooks) inverted the [step, its hooks...] order every other
// step kind follows, and made a failed in-place get invisible.
func TestRunGetRecordsExecutionBeforeItsHooks(t *testing.T) {
	fixture := newBindingFixture(t, `
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
- name: broken
  config:
    check: printf '[{"ref":"r1"}]'
    in: exit 1
resources:
- name: items
  type: feed
  source: {}
- name: sidecar
  type: fixed
  source: {}
- name: unfetchable
  type: broken
  source: {}
jobs:
- name: ordered
  plan:
  - get: items
  - get: sidecar
    on_success:
      task: notify
      run: "true"
  - task: work
    inputs: [items, sidecar]
    run: cat items/n.txt >> PROCESSED
  assert:
    execution: [items, sidecar, notify, work]
    outcome: succeeded
- name: failing
  plan:
  - get: items
  - get: unfetchable
  - task: never
    run: "true"
  assert:
    execution: [items, unfetchable]
    outcome: failed
`)

	fixture.write(t, `[{"n":"1"}]`)
	fixture.run(t, "ordered")
	fixture.run(t, "failing")

	fixture.assertDid(t, "1")
}

// TestRunPassedKeepsGreenSetsFromAFailedRun: a build is green or it is not,
// one set at a time. A later set failing must not retract the green record of
// an earlier set that succeeded — the versions are already consumed (taken at
// build start), so losing their green record strands them: never green, never
// retried, invisible to every downstream gate.
func TestRunPassedKeepsGreenSetsFromAFailedRun(t *testing.T) {
	fixture := newBindingFixture(t, strings.Replace(passedAcrossSets,
		"run: RUN", `run: test "$(cat items/n.txt)" != 2`, 1))

	fixture.items(t, 2)
	fixture.runExpectingFailure(t, "upstream")
	fixture.run(t, "downstream")

	fixture.assertDid(t, "1")
}
