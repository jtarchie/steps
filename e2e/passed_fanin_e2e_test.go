package e2e

// passed: as Concourse schedules it: a fan-in builds versions one upstream build passed together, and only a trigger: true get starts a job.

import (
	"testing"
)

// fanInPipeline is docs/infra.md's fan-in; an id shared across both resources lets one --pin choose a pair, since a --pin applies to every get.
const fanInPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: repo-feed
  config:
    check: cat FEED
    in: echo {{ .version.r | shellquote }} > ref
- name: config-feed
  config:
    check: cat FEED.config
    in: echo {{ .version.c | shellquote }} > ref
resources:
- name: repo
  type: repo-feed
  source: {}
- name: config
  type: config-feed
  source: {}
jobs:
- name: upstream
  plan:
  - get: repo
  - get: config
- name: deploy
  plan:
  - get: repo
    trigger: true
    passed: [upstream]
  - get: config
    passed: [upstream]
  - task: ship
    inputs: [repo, config]
    run: echo "$(cat repo/ref)+$(cat config/ref)" >> PROCESSED
`

// fanInHistory is docs/infra.md's scenario: upstream passes r2+c1 then r1+c2, so r2 and c2 are each the newest green version and never passed together.
func fanInHistory(t *testing.T) *watchFixture {
	t.Helper()

	fixture := newWatchFixture(t, fanInPipeline)
	fixture.resources = []string{"repo", "config"}

	fixture.write(t, fixture.feed, `[{"id":"b","r":"r1"},{"id":"a","r":"r2"}]`)
	fixture.write(t, fixture.feed+".config", `[{"id":"a","c":"c1"},{"id":"b","c":"c2"}]`)

	for _, id := range []string{"a", "b"} {
		err := stepsRun(t, []string{
			"run", fixture.pipeline, "--db", fixture.db, "--name", fixture.name + "=" + fixture.pipeline,
			"--job", "upstream", "--pin", "id=" + id,
		})
		if err != nil {
			t.Fatalf("steps run upstream --pin id=%s: %v", id, err)
		}
	}

	return fixture
}

// TestRunPassedFanInBuildsTheSetThatPassedTogether: taking each resource's newest green version on its own built r2+c2, a pair nothing ever tested.
func TestRunPassedFanInBuildsTheSetThatPassedTogether(t *testing.T) {
	t.Parallel()

	fixture := fanInHistory(t)

	err := stepsRun(t, []string{
		"run", fixture.pipeline, "--db", fixture.db, "--name", fixture.name + "=" + fixture.pipeline, "--job", "deploy",
	})
	if err != nil {
		t.Fatalf("steps run deploy: %v", err)
	}

	fixture.assertDid(t, "r1+c2")
}

// TestWebReleasesTheFanInSetThatPassedTogether: judging only r2+c2 held deploy forever; the release must build the pair it judged, or the poll releases it again every tick.
func TestWebReleasesTheFanInSetThatPassedTogether(t *testing.T) {
	t.Parallel()

	fixture := fanInHistory(t)

	fixture.watch(t)

	fixture.assertDid(t, "r1+c2")
}

const gatedOnlyPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat FEED
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: upstream
  plan:
  - get: items
    trigger: true
- name: ship
  plan:
  - get: items
    trigger: true
    passed: [upstream]
  - task: work
    inputs: [items]
    run: echo "ship-$(cat items/n.txt)" >> PROCESSED
- name: audit
  plan:
  - get: items
    passed: [upstream]
  - task: work
    inputs: [items]
    run: echo "audit-$(cat items/n.txt)" >> PROCESSED
`

// TestWebNeverStartsAJobWhoseGetsHaveNoTrigger: Concourse schedules only on a trigger: true input; ship is the control, judged by the same poll as audit.
func TestWebNeverStartsAJobWhoseGetsHaveNoTrigger(t *testing.T) {
	t.Parallel()

	fixture := newWatchFixture(t, gatedOnlyPipeline)
	fixture.write(t, fixture.feed, `[{"n":"1"}]`)

	fixture.serve(t)
	waitForNthRun(t, fixture.db, fixture.name, "ship", 1)
	fixture.watch(t)

	fixture.assertDid(t, "ship-1")

	if runs := listRuns(t, fixture.db, fixture.name, "audit"); len(runs) != 0 {
		t.Errorf("audit ran %d time(s); a job with no trigger: true get is only ever started by a person", len(runs))
	}
}

const failingShipPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: cat FEED
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: upstream
  plan:
  - get: items
    trigger: true
  - task: work
    inputs: [items]
    run: echo "up-$(cat items/n.txt)" >> PROCESSED
- name: ship
  plan:
  - get: items
    trigger: true
    passed: [upstream]
  - task: work
    inputs: [items]
    run: echo "ship-$(cat items/n.txt)" >> PROCESSED; exit 1
`

// TestWebDoesNotReleaseWhatAFailedBuildUsed: Concourse builds a trigger input's first occurrence (versions_db.go's IsFirstOccurrence), whatever became of the build that used it, so a failing ship waits for a new version or a person instead of running every poll until a breaker holds it.
func TestWebDoesNotReleaseWhatAFailedBuildUsed(t *testing.T) {
	t.Parallel()

	fixture := newWatchFixture(t, failingShipPipeline)
	fixture.write(t, fixture.feed, `[{"n":"1"}]`)

	fixture.serve(t)
	waitForNthRun(t, fixture.db, fixture.name, "ship", 1)
	fixture.watch(t)
	fixture.assertDid(t, "up-1", "ship-1")

	fixture.write(t, fixture.feed, `[{"n":"1"},{"n":"2"}]`)

	// Started by the check that saw v2 would mean released for its gated set, still v1, which a racing upstream may or may not have moved on by the time it resolves.
	if second := waitForNthRun(t, fixture.db, fixture.name, "ship", 2); second.CreatedBy != "upstream:" {
		t.Errorf("ship's second run was started by %q, want upstream: v2 passing, not the check that saw it", second.CreatedBy)
	}

	fixture.watch(t)
	fixture.assertDid(t, "up-1", "ship-1", "up-2", "ship-2")

	fixture.served.trigger(t, fixture.name, "ship")
	waitForNthRun(t, fixture.db, fixture.name, "ship", 3)
	fixture.watch(t)
	fixture.assertDid(t, "up-1", "ship-1", "up-2", "ship-2", "ship-2")
}
