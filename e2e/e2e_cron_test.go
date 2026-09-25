package e2e

// The built-in cron resource, end to end through a running daemon: a version
// is minted when a slot of the expression passes, and only then.

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
)

// cronNeverDue is a slot no test will run inside of: midnight on the 29th of
// February, one hour every four years.
const cronNeverDue = "0 0 29 2 *"

// cronPipeline has one resource that must fire on its first check and one
// that must not: `now` says fire_immediately, `someday` has no slot within
// the last hour, which is the cold-start rule the type inherits from
// govuk-pay/cron-resource.
const cronPipeline = `
defaults:
  preflight:
    disabled: true
resources:
- name: someday
  type: cron
  source:
    expression: "` + cronNeverDue + `"
- name: now
  type: cron
  source:
    expression: "` + cronNeverDue + `"
    fire_immediately: true
jobs:
- name: later
  plan:
  - get: someday
    trigger: true
  - task: work
    inputs: [someday]
    run: |
      cat someday/epoch >> PROCESSED-later
      echo >> PROCESSED-later
- name: first
  plan:
  - get: now
    trigger: true
  - task: work
    inputs: [now]
    run: |
      cat now/epoch >> PROCESSED
      echo >> PROCESSED
`

// TestWatchCronFiresOnceWhenToldToAndNotWhenNothingIsDue: fire_immediately
// mints exactly one version on the first check and the daemon then waits for
// the slot; a resource with no slot in the last hour mints nothing at all,
// however many polls go by.
func TestWatchCronFiresOnceWhenToldToAndNotWhenNothingIsDue(t *testing.T) {
	fixture := newWatchFixture(t, cronPipeline)
	fixture.resources = []string{"now"}

	started := time.Now()

	for range 3 {
		fixture.watch(t)
	}

	got := fixture.did(t)
	if len(got) != 1 {
		t.Fatalf("job first processed %v, want exactly one version", got)
	}

	epoch, err := strconv.ParseInt(got[0], 10, 64)
	if err != nil {
		t.Fatalf("epoch %q is not an integer: %v", got[0], err)
	}

	if at := time.Unix(epoch, 0); at.Before(started.Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Errorf("the version's epoch %s is not the moment the check ran (%s)", at, started)
	}

	if later := fieldsOf(t, fixture.processed+"-later"); len(later) != 0 {
		t.Errorf("job later processed %v, want nothing — no slot of %q fell in the last hour", later, cronNeverDue)
	}
}

// cronEverySecondPipeline is a six-field expression: a slot every other second.
const cronEverySecondPipeline = `
defaults:
  preflight:
    disabled: true
resources:
- name: tick
  type: cron
  source:
    expression: "*/2 * * * * *"
jobs:
- name: build
  plan:
  - get: tick
    trigger: true
  - task: work
    inputs: [tick]
    run: |
      cat tick/epoch >> PROCESSED
      echo >> PROCESSED
`

// TestWatchCronMintsAVersionEachTimeASlotPasses: the seconds field is honored,
// a version is minted at most once per poll, and each one is later than the
// last — the shape a schedule has to have for a job to run on it.
func TestWatchCronMintsAVersionEachTimeASlotPasses(t *testing.T) {
	fixture := newWatchFixture(t, cronEverySecondPipeline)
	fixture.resources = []string{"tick"}

	deadline := time.Now().Add(15 * time.Second)

	var got []string

	for time.Now().Before(deadline) {
		fixture.watch(t)

		got = fixture.did(t)
		if len(got) >= 3 {
			break
		}
	}

	if len(got) < 3 {
		t.Fatalf("job processed %v after 15s, want at least three versions of a slot every two seconds", got)
	}

	previous := int64(0)

	for _, raw := range got {
		epoch, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("epoch %q is not an integer: %v", raw, err)
		}

		if epoch <= previous {
			t.Errorf("versions %v are not strictly later one to the next", got)
		}

		previous = epoch
	}
}

// TestValidateRefusesABadCronExpression: the expression is compiled where the
// pipeline is checked, so a slot nobody can compute is refused before a
// daemon ever polls it — the same door a webhook's expressions go through.
func TestValidateRefusesABadCronExpression(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
resources:
- name: tick
  type: cron
  source:
    expression: "0 25 * * *"
jobs:
- name: build
  plan:
  - get: tick
    trigger: true
`)

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), `resource "tick"`) || !strings.Contains(err.Error(), `source.expression "0 25 * * *"`) {
		t.Errorf("validate: err = %v, want the resource and its expression named", err)
	}
}

// fieldsOf is a processed file's entries, none when the job never ran.
func fieldsOf(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir()-scoped file the job under test wrote
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Fields(string(data))
}
