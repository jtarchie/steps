package e2e

// A version a job PUT counts as having passed it, as in Concourse: the
// downstream get resolves against what the upstream published, even though the
// resource's check never reports it.

import (
	"strings"
	"testing"
)

const putThenConsumePipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: image
  config:
    check: echo '[{"digest":"from-check"}]'
    in: echo {{ .version.digest | shellquote }} > digest.txt
    out: cat FEED
resources:
- name: toolchain
  type: image
  source: {}
jobs:
- name: build-toolchain
  plan:
  - put: toolchain
  - task: verify
    run: grep -qv sha-2 FEED
- name: implement
  plan:
  - get: toolchain
    passed: [build-toolchain]
  - task: work
    inputs: [toolchain]
    run: cat toolchain/digest.txt >> PROCESSED
`

func TestRunPassedCountsAVersionTheUpstreamPut(t *testing.T) {
	t.Parallel()

	fixture := newBindingFixture(t, putThenConsumePipeline)

	fixture.write(t, `{"digest":"sha-1"}`)
	fixture.run(t, "build-toolchain")
	fixture.run(t, "implement")

	fixture.assertDid(t, "sha-1")
}

func TestRunPassedIgnoresAVersionFromABuildThatFailedAfterPutting(t *testing.T) {
	t.Parallel()

	fixture := newBindingFixture(t, putThenConsumePipeline)

	fixture.write(t, `{"digest":"sha-1"}`)
	fixture.run(t, "build-toolchain")

	fixture.write(t, `{"digest":"sha-2"}`)
	fixture.runExpectingFailure(t, "build-toolchain")
	fixture.run(t, "implement")

	fixture.assertDid(t, "sha-1")
}

func TestRunPassedNamesTheGateWhenNothingHasPassed(t *testing.T) {
	t.Parallel()

	fixture := newBindingFixture(t, putThenConsumePipeline)

	err := stepsRun(t, []string{"run", fixture.pipeline, "--job", "implement"})
	if err == nil {
		t.Fatal("implement ran with nothing passed upstream")
	}

	want := "has passed [build-toolchain]"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the gate (%q)", err, want)
	}
}

// A hook put aliased with resource: publishes to the resource it names, as a
// put in the plan does (#273).
const hookPutThenConsumePipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: image
  config:
    check: echo '[{"digest":"from-check"}]'
    in: echo {{ .version.digest | shellquote }} > digest.txt
    out: cat FEED
resources:
- name: toolchain
  type: image
  source: {}
jobs:
- name: build-toolchain
  plan:
  - task: verify
    run: "true"
  on_success:
    put: publish
    resource: toolchain
- name: implement
  plan:
  - get: toolchain
    passed: [build-toolchain]
  - task: work
    inputs: [toolchain]
    run: cat toolchain/digest.txt >> PROCESSED
`

func TestRunPassedCountsAVersionAnAliasedHookPut(t *testing.T) {
	t.Parallel()

	fixture := newBindingFixture(t, hookPutThenConsumePipeline)

	fixture.write(t, `{"digest":"sha-1"}`)
	fixture.run(t, "build-toolchain")
	fixture.run(t, "implement")

	fixture.assertDid(t, "sha-1")
}
