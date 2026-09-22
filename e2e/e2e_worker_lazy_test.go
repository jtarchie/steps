package e2e

// A placed step's outputs STAY on the worker (steps#138, rung 2).
//
// The orchestrator is one holder among the workers: bytes come home only
// when something running here reads them — a local step, an in-process
// reader like load_var: or assert: files:, or a cache that lives on this
// disk. Nothing else moves a tree off the machine that produced it.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndPlacedOutputsStayOnTheWorkerWithNoLocalReader: a placed
// producer and a placed consumer on one machine, and nothing here reads
// either output — so nothing comes home.
func TestEndToEndPlacedOutputsStayOnTheWorkerWithNoLocalReader(t *testing.T) {
	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [src]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > src/blob.bin
  - task: consume
    tags: [a]
    inputs: [src]
    outputs: [out]
    run: wc -c < src/blob.bin | tr -d ' ' > out/size.txt && cp out/size.txt `+filepath.Join(dir, "size.txt")+`
`)

	mustRun(t, append([]string{path}, workers...)...)

	if got := readFileString(t, filepath.Join(dir, "size.txt")); got != strconv.Itoa(payloadBytes)+"\n" {
		t.Errorf("size.txt = %q, want the payload's size", got)
	}

	placements := runPlacements(t, path)

	for _, name := range []string{"seed", "consume"} {
		if got := placementNamed(t, placements, name).BytesReceived; got != 0 {
			t.Errorf("%s brought %d bytes home; nothing here reads its output, so want 0", name, got)
		}
	}
}

// TestEndToEndALocalStepPullsAPlacedOutputWhenItReadsIt: the same job with
// a local step at the end. The producer's row still says nothing came home
// at the time it finished; the local step is what pulled the tree.
func TestEndToEndALocalStepPullsAPlacedOutputWhenItReadsIt(t *testing.T) {
	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", false)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir)

	placements := runPlacements(t, path)

	for _, name := range []string{"seed", "consume"} {
		if got := placementNamed(t, placements, name).BytesReceived; got != 0 {
			t.Errorf("%s brought %d bytes home at the time it finished; the local step is what should have pulled them", name, got)
		}
	}
}

// TestEndToEndAssertFilesStillSeesAPlacedTasksOutputs: an assert: files: is
// an in-process reader of the step's own outputs, so that step's tree comes
// home as it always did.
func TestEndToEndAssertFilesStillSeesAPlacedTasksOutputs(t *testing.T) {
	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [src]
    run: echo made > src/made.txt
    assert:
      files: [src/made.txt]
`)

	mustRun(t, append([]string{path}, workers...)...)

	if got := placementNamed(t, runPlacements(t, path), "seed").BytesReceived; got == 0 {
		t.Error("seed brought nothing home, yet its assert: files: passed — what did it check?")
	}
}

// TestEndToEndLoadVarReadsAPlacedOutput: load_var: reads a file in this
// process, from a tree a worker holds.
func TestEndToEndLoadVarReadsAPlacedOutput(t *testing.T) {
	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [meta]
    run: printf 'v1.2.3\n' > meta/version.txt
  - load_var: tag
    inputs: [meta]
    file: meta/version.txt
  - task: announce
    run: echo "releasing ((tag)) ok" > `+filepath.Join(dir, "announced.txt")+`
`)

	mustRun(t, append([]string{path}, workers...)...)

	if got := readFileString(t, filepath.Join(dir, "announced.txt")); got != "releasing v1.2.3 ok\n" {
		t.Errorf("announced = %q, want the value load_var read from the worker's tree", got)
	}
}

// TestEndToEndAHolderThatLostTheTreeFailsTheBuildByName: a claim is a
// claim. The worker's cache is emptied between the producer and a local
// reader, and the build fails naming the artifact and the machine rather
// than running the reader against nothing.
func TestEndToEndAHolderThatLostTheTreeFailsTheBuildByName(t *testing.T) {
	dir := t.TempDir()
	rootA, _, workers := twoWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [src]
    run: echo made > src/made.txt
  - task: sweep
    run: rm -rf `+filepath.Join(rootA, "steps-shim", "artifacts")+`
  - task: read
    inputs: [src]
    run: cat src/made.txt
`)

	err := cli.Run(append([]string{path}, workers...))
	if err == nil {
		t.Fatal("the build passed with its input gone from the only machine that held it")
	}

	for _, want := range []string{`"src"`, "local:" + rootA} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err.Error(), want)
		}
	}
}

// TestEndToEndPlacedGetWithTheResourceCacheDialsNothingOnTheSecondBuild is
// the seam #138 found untested: the resource cache is placement-agnostic,
// so a hit never reaches the worker. A changed consumer keeps the chain skip
// from answering for the whole job, so the get itself has to be the thing
// that was cached.
func TestEndToEndPlacedGetWithTheResourceCacheDialsNothingOnTheSecondBuild(t *testing.T) {
	dir := t.TempDir()
	_, _, workers := twoWorkers(t)

	pipeline := func(publishAs string) string {
		return writePipeline(t, dir, `
workspace:
  root: `+filepath.Join(dir, "ws-root")+`
  cache:
    resources: true

resource_types:
- name: counted
  config:
    check: printf '[{"ref":"v1"}]'
    in: |
      echo ran >> `+filepath.Join(dir, "fetches")+`
      printf '%s' "${STEPS_WORKER:-here}" > where.txt

resources:
- name: repo
  type: counted
  tags: [a]
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - task: publish
    inputs: [src]
    run: cp src/where.txt `+filepath.Join(dir, publishAs)+`
`)
	}

	path := pipeline("first.txt")
	mustRun(t, append([]string{path}, workers...)...)

	edited := pipeline("second.txt")

	err := os.Rename(edited, path)
	if err != nil {
		t.Fatalf("editing the pipeline: %v", err)
	}

	mustRun(t, append([]string{path}, workers...)...)

	if got := readFileString(t, filepath.Join(dir, "second.txt")); got != "a" {
		t.Errorf("the second build read %q, want the tree the first build fetched on a", got)
	}

	if fetches := strings.Count(readFileString(t, filepath.Join(dir, "fetches")), "ran"); fetches != 1 {
		t.Errorf("in: ran %d times, want 1 — the second build fetched again instead of reusing the cached version", fetches)
	}

	for _, p := range runPlacements(t, path) {
		if p.StepName == "src" || p.StepName == "repo" || strings.Contains(p.StepName, "in") {
			t.Errorf("the second build placed %q on %s; a resource cache hit dials nothing", p.StepName, p.Address)
		}
	}
}
