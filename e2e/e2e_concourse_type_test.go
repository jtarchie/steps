package e2e

// steps#237: a stock Concourse resource image runs as Concourse spells it — type: registry-image, source.repository — with its own /opt/resource scripts handed Concourse's request on stdin, and its {version, metadata} answers unwrapped.

import (
	"strings"
	"testing"
)

const concourseTimeImage = "concourse/time-resource"

func TestAStockConcourseResourceImageChecksGetsAndPuts(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
resource_types:
- name: time
  type: registry-image
  source: {repository: `+concourseTimeImage+`}
  defaults: {interval: 1m}

resources:
- name: clock
  type: time
  source: {location: UTC}

jobs:
- name: build
  plan:
  - get: clock
  - task: look
    inputs: [clock]
    run: test -s clock/input && echo "fetched $(wc -c < clock/input) bytes"
    assert:
      stdout: fetched
  - put: clock
`)

	printed, err := runAndCapture(t, "run", "--no-preflight", path)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, printed)
	}

	// The check's version and the put's: every one is a time, none the {version, metadata} envelope the put answered with.
	versions := putVersions(t, path, "clock")
	if len(versions) < 2 {
		t.Fatalf("the history holds %v, want the checked version and the one the put produced", versions)
	}

	for _, version := range versions {
		if _, ok := version["time"]; !ok || version["version"] != nil || version["metadata"] != nil {
			t.Errorf("recorded %v, want a version, not the envelope", version)
		}
	}

	if strings.Contains(printed, `"metadata"`) {
		t.Errorf("an answer's JSON reached the log rather than being read:\n%s", printed)
	}
}
