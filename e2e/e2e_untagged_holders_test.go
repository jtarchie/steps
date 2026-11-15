package e2e

// An untagged image: step keeps its outputs in this machine's docker until something here reads them, as a step tagged onto a local: worker does (steps#259, phase 1).

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// untaggedPayload is big enough that a transfer of it cannot hide under the metadata a placement always moves.
const untaggedPayload = 4 << 20

// movedNothing is the most a placement may count and still be said to have moved no payload: archive headers and a few small files.
const movedNothing = 64 << 10

// untaggedProducer writes a random payload and its sha256 into src, from an untagged image: step.
func untaggedProducer() string {
	return fmt.Sprintf(`  - task: make
    image: %s
    outputs: [src]
    run: |
      head -c %d /dev/urandom > src/blob.bin
      sha256sum src/blob.bin | cut -d' ' -f1 > src/sum
`, dockerE2EImage, untaggedPayload)
}

// TestUntaggedImageStepsPassAnArtifactInsideDocker: the producer's output never comes home and the consumer is not sent it, yet the consumer reads the producer's bytes.
func TestUntaggedImageStepsPassAnArtifactInsideDocker(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`  - task: consume
    image: %[1]s
    inputs: [src]
    outputs: [out]
    run: |
      sha256sum src/blob.bin | cut -d' ' -f1 > out/got
      cp src/sum out/want
  - task: publish
    inputs: [out]
    run: cp out/got %[2]s/got && cp out/want %[2]s/want
`, dockerE2EImage, dir))

	out, kept, err := runKept(t, path, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	assertNeverLanded(t, kept)

	got, want := readFileString(t, filepath.Join(dir, "got")), readFileString(t, filepath.Join(dir, "want"))
	if got != want || strings.TrimSpace(got) == "" {
		t.Errorf("the consumer hashed %q, the producer wrote %q: the consumer must read the producer's bytes", got, want)
	}

	placements := runPlacements(t, path)

	if made := placementNamed(t, placements, "make"); made.BytesReceived > movedNothing {
		t.Errorf("make received %d bytes home, want at most %d: its output stays in docker until something here reads it", made.BytesReceived, movedNothing)
	}

	if consumed := placementNamed(t, placements, "consume"); consumed.BytesSent > movedNothing {
		t.Errorf("consume sent %d bytes, want at most %d: the tree it reads is already in docker", consumed.BytesSent, movedNothing)
	}
}

// TestAHostStepReadsWhatAnUntaggedImageStepHeld: a step with no image runs here, so the held output is pulled home for it, whole.
func TestAHostStepReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`  - task: read
    inputs: [src]
    run: cp src/blob.bin src/sum %s/
`, dir))

	mustRun(t, path)

	requireSumMatches(t, dir)

	if made := placementNamed(t, runPlacements(t, path), "make"); made.BytesReceived > movedNothing {
		t.Errorf("make received %d bytes home, want at most %d: the host step pulls it on demand instead", made.BytesReceived, movedNothing)
	}
}

// resumeBuilds are the two builds a resumed step can live in: the job's, or with a get first, the get's.
var resumeBuilds = map[string]string{"job build": "", "get build": `
resource_types:
- name: here
  config:
    check: printf '[{"ref":"v1"}]'
    in: "true"

resources:
- name: tick
  type: here
  source: {}
`} //nolint:gochecknoglobals // a fixed table two tests share

// TestAResumeReadsWhatAnUntaggedImageStepHeld: the run that held the output is gone, so the resumed one learns where the bytes are from the kept tree; with a get first the steps live in the get's build rather than the job's.
func TestAResumeReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	for name, get := range resumeBuilds {
		t.Run(name, func(t *testing.T) {
			assertResumeReadsHeld(t, get)
		})
	}
}

func assertResumeReadsHeld(t *testing.T, resources string) {
	t.Helper()
	requireDockerE2E(t)

	dir := t.TempDir()
	fixed := filepath.Join(dir, "fixed")

	first := ""
	if resources != "" {
		first = "  - get: tick\n"
	}

	path := writePipeline(t, dir, resources+`
jobs:
- name: build
  plan:
`+first+untaggedProducer()+fmt.Sprintf(`  - task: fragile
    run: test -f %s
  - task: read
    inputs: [src]
    run: cp src/blob.bin src/sum %s/
`, fixed, dir))

	out := captureStdout(t, func() {
		if cli.Run([]string{"run", path}) == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	writePipelineFile(t, fixed, "")

	out = captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--resume", resumeID(t, out)})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	if !strings.Contains(out, "skip: make") {
		t.Fatalf("the resume ran make again, so this test proves nothing about a held output:\n%s", out)
	}

	requireSumMatches(t, dir)
}

// TestAnUntaggedImageGetAndPutKeepTheTreeInDocker: a resource type with an image: holds its in: tree, and a put of that type is not sent a tree docker already has.
func TestAnUntaggedImageGetAndPutKeepTheTreeInDocker(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: blob
  image: %[1]s
  config:
    check: printf '[{"ref":"v1"}]'
    in: |
      head -c %[2]d /dev/urandom > blob.bin
      sha256sum blob.bin | cut -d' ' -f1 > sum
    out: test -s src/sum && test "$(sha256sum src/blob.bin | cut -d' ' -f1)" = "$(cat src/sum)" && printf '{"ref":"pushed"}'

resources:
- name: repo
  type: blob
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - put: repo
    inputs: [src]
`, dockerE2EImage, untaggedPayload))

	out, kept, err := runKept(t, path, nil)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	assertNeverLanded(t, kept)

	said := false

	for _, row := range latestRunEvents(t, path) {
		said = said || row.Text == `put: repo (version: {"ref":"pushed"})`
	}

	if !said {
		t.Error("the put did not answer a version, so it did not read the get's bytes intact")
	}

	placements := runPlacements(t, path)

	if got := placementNamed(t, placements, "src"); got.BytesReceived > movedNothing {
		t.Errorf("the get received %d bytes home, want at most %d: its tree stays in docker", got.BytesReceived, movedNothing)
	}

	if put := placementNamed(t, placements, "repo"); put.BytesSent > movedNothing {
		t.Errorf("the put sent %d bytes, want at most %d: the tree it reads is already in docker", put.BytesSent, movedNothing)
	}
}

// requireSumMatches checks the blob a host step copied out against the sum its producer wrote.
func requireSumMatches(t *testing.T, dir string) {
	t.Helper()

	blob, err := os.ReadFile(filepath.Join(dir, "blob.bin")) //nolint:gosec // a t.TempDir() file the pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	sum := sha256.Sum256(blob)

	if got, want := hex.EncodeToString(sum[:]), strings.TrimSpace(readFileString(t, filepath.Join(dir, "sum"))); got != want || len(blob) != untaggedPayload {
		t.Errorf("the host step read %d bytes hashing %s, want %d bytes hashing %s", len(blob), got, untaggedPayload, want)
	}
}

// TestAReplayReadsWhatAnUntaggedImageStepHeld: the replayed step is the first reader, in a tree forked from a run that never brought the output home.
func TestAReplayReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	pipeline := func(read string) string {
		return `
jobs:
- name: build
  plan:
` + untaggedProducer() + `  - task: read
` + read
	}

	path := writePipeline(t, dir, pipeline("    run: \"true\"\n"))

	err := cli.Run([]string{"run", path, "--keep-workspace"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	writePipeline(t, dir, pipeline(fmt.Sprintf("    inputs: [src]\n    run: cp src/blob.bin src/sum %s/\n", dir)))

	captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--replay", runIDFromStore(t, path), "--from", "read"})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
	})

	requireSumMatches(t, dir)
}

// TestAHostPutReadsWhatAnUntaggedImageStepHeld: a put whose type has no image: runs here, so its input is pulled home for it.
func TestAHostPutReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: sink
  config:
    check: printf '[]'
    out: cp src/blob.bin src/sum %s/ && printf '{"ref":"pushed"}'

resources:
- name: target
  type: sink
  source: {}

jobs:
- name: build
  plan:
`, dir)+untaggedProducer()+`  - put: target
    inputs: [src]
`)

	mustRun(t, path)

	requireSumMatches(t, dir)
}

// TestTheStepCacheFilesWhatAnUntaggedImageStepHeld: the cache under root: is a reader here, so both steps are filed and the second run restores both.
func TestTheStepCacheFilesWhatAnUntaggedImageStepHeld(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	sums := filepath.Join(dir, "sums")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s/ws

jobs:
- name: build
  plan:
`, dir)+untaggedProducer()+fmt.Sprintf(`  - task: consume
    image: %[1]s
    inputs: [src]
    outputs: [out]
    run: sha256sum src/blob.bin | cut -d' ' -f1 > out/got
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/got >> %[2]s
`, dockerE2EImage, sums))

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")

	lines := strings.Fields(readFileString(t, sums))
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Errorf("the two runs saw %q, want one payload twice: make must be restored from the cache, not run again", lines)
	}

	for _, step := range []string{"make", "consume"} {
		if got := storeNodeResult(t, path, step); !strings.Contains(got, `"reused":true`) {
			t.Errorf("%s on the second run: %s, want it restored from the step cache", step, got)
		}
	}
}

// runPins lists the pin volumes run left in docker: what keeps its held outputs from another session's eviction.
func runPins(t *testing.T, run string) []string {
	t.Helper()

	//nolint:gosec // a filter built from a run id this test read back
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "-q", "--filter", "label=steps.cache=pin", "--filter", "label=steps.run="+run).Output()
	if err != nil {
		t.Fatalf("listing pins: %v", err)
	}

	return strings.Fields(string(out))
}

// TestAFinishedRunReleasesWhatItPinned: a run that succeeds gives its held outputs back to eviction, so pins never outlast the run that needed them.
func TestAFinishedRunReleasesWhatItPinned(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`  - task: read
    inputs: [src]
    run: cp src/blob.bin src/sum %s/
`, dir))

	mustRun(t, path)

	if pins := runPins(t, lastRunID(t, path)); len(pins) != 0 {
		t.Errorf("the finished run left pins %v: its outputs stay over the cache bound until the pin lifetime runs out", pins)
	}
}

// TestAFailedRunKeepsItsPinsForTheResume: the failed run's held output stays pinned for --resume, and the resume, another process, releases it once it succeeds.
func TestAFailedRunKeepsItsPinsForTheResume(t *testing.T) {
	for name, get := range resumeBuilds {
		t.Run(name, func(t *testing.T) {
			assertResumeReleasesPins(t, get)
		})
	}
}

func assertResumeReleasesPins(t *testing.T, resources string) {
	t.Helper()
	requireDockerE2E(t)

	dir := t.TempDir()
	fixed := filepath.Join(dir, "fixed")

	first := ""
	if resources != "" {
		first = "  - get: tick\n"
	}

	path := writePipeline(t, dir, resources+`
jobs:
- name: build
  plan:
`+first+untaggedProducer()+fmt.Sprintf(`  - task: fragile
    run: test -f %s
  - task: read
    inputs: [src]
    run: cp src/blob.bin src/sum %s/
`, fixed, dir))

	out := captureStdout(t, func() {
		if cli.Run([]string{"run", path}) == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	run := resumeID(t, out)

	if len(runPins(t, run)) == 0 {
		t.Fatal("the failed run pinned nothing, so its held output is the cache's to evict before the resume reads it")
	}

	writePipelineFile(t, fixed, "")

	_ = captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--resume", run})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	if pins := runPins(t, run); len(pins) != 0 {
		t.Errorf("the resumed run succeeded and left pins %v", pins)
	}
}
