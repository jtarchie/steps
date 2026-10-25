package e2e

// What a placed step cost its worker's disk (steps#225): a copy where an overlay belonged moves no bytes over the wire, so only the worker's disk knows.

import (
	"strconv"
	"testing"
)

// diskPayload is far past diskSlack, so one accidental copy of it cannot hide in the slack.
const diskPayload = 8 << 20

// diskSlack is what empty volumes, an overlay's work directory and du's block rounding may add to a step's own writes.
const diskSlack = 1 << 20

// TestEndToEndPlacedStepRecordsTheDiskItsVolumesUse: a consumer on the producer's worker costs only its own output; one on the other worker had the tree poured into its cache too.
func TestEndToEndPlacedStepRecordsTheDiskItsVolumesUse(t *testing.T) {
	requireDockerE2E(t)

	const written = 256 << 10

	_, _, workers := twoWorkers(t)
	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: blob
  image: `+dockerE2EImage+`
  config:
    check: printf '[{"ref":"v1"}]'
    in: head -c `+strconv.Itoa(diskPayload)+` /dev/urandom > blob.bin

resources:
- name: repo
  type: blob
  tags: [a]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: seed
    tags: [a]
    image: `+dockerE2EImage+`
    outputs: [src]
    run: head -c `+strconv.Itoa(diskPayload)+` /dev/urandom > src/blob.bin
  - task: here
    tags: [a]
    image: `+dockerE2EImage+`
    inputs: [src]
    outputs: [out]
    run: test -s src/blob.bin && head -c `+strconv.Itoa(written)+` /dev/urandom > out/note.bin
  - task: there
    tags: [b]
    image: `+dockerE2EImage+`
    inputs: [src]
    outputs: [out]
    run: test -s src/blob.bin && head -c `+strconv.Itoa(written)+` /dev/urandom > out/note.bin
`)

	mustRun(t, append([]string{path}, workers...)...)

	placements := runPlacements(t, path)

	within := func(name string, low, high int64) {
		t.Helper()

		p := placementNamed(t, placements, name)
		if p.VolumeBytes == nil {
			t.Fatalf("%s: no volume bytes recorded", name)
		}

		if got := *p.VolumeBytes; got < low || got > high {
			t.Errorf("%s: its volumes hold %d bytes on the worker; want %d..%d", name, got, low, high)
		}
	}

	within("repo", diskPayload, diskPayload+diskSlack)
	within("seed", diskPayload, diskPayload+diskSlack)
	within("here", written, written+diskSlack)
	within("there", diskPayload+written, diskPayload+written+diskSlack)
}

// TestEndToEndPlacedStepIsWeighedWhenItsLastOutputComesHome: an output this machine already had is fetched back rather than held, so the weighing rides on whichever output was held, not on the last one named.
func TestEndToEndPlacedStepIsWeighedWhenItsLastOutputComesHome(t *testing.T) {
	requireDockerE2E(t)

	const written = 256 << 10

	_, _, workers := twoWorkers(t)
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: notes
    outputs: [notes]
    run: echo first > notes/log.txt
  - task: both
    tags: [a]
    image: `+dockerE2EImage+`
    inputs: [notes]
    outputs: [out, notes]
    run: head -c `+strconv.Itoa(written)+` /dev/urandom > out/note.bin && echo second >> notes/log.txt
`)

	mustRun(t, append([]string{path}, workers...)...)

	both := placementNamed(t, runPlacements(t, path), "both")
	if both.VolumeBytes == nil || *both.VolumeBytes < written {
		t.Errorf("both: volume bytes = %v, want at least the %d bytes it wrote to the output it held", both.VolumeBytes, written)
	}
}
