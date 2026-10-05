package e2e

// A worker keeps what it PRODUCES, not only what it receives (steps#138).
//
// A placed get fetches a tree on the worker; the next placed step on that same
// worker names the tree as an input. Before this the tree came home and went
// straight back — one tree, two crossings. Now the worker files the tree it
// packed under the digest the next offer will name, and answers that offer
// with "already here".
//
// Every pipeline here mixes placed and local steps on purpose: the tree still
// comes home for the local step that reads it, and that step is what proves
// the bytes the worker kept are the bytes the pipeline saw.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// payloadBytes is one mebibyte of /dev/urandom: incompressible, so what
// crosses the wire is what the tree weighs, and the byte counters can tell a
// full transfer from an empty output directory without a margin argument.
const payloadBytes = 1 << 20

// holderPipeline is a placed producer on a, a placed consumer on consumerTag,
// and a local step that publishes what the consumer saw. get chooses whether
// the producer is a get or a task.
func holderPipeline(t *testing.T, dir, consumerTag string, get bool) string {
	t.Helper()

	producer := `
  - task: seed
    tags: [a]
    image: ` + dockerE2EImage + `
    outputs: [src]
    run: |
      head -c ` + strconv.Itoa(payloadBytes) + ` /dev/urandom > src/blob.bin
      printf '%s' "${STEPS_WORKER:-here}" > src/where.txt
`
	resources := ""

	if get {
		resources = `
resource_types:
- name: blob
  image: ` + dockerE2EImage + `
  config:
    check: printf '[{"ref":"v1"}]'
    in: |
      head -c ` + strconv.Itoa(payloadBytes) + ` /dev/urandom > blob.bin
      printf '%s' "${STEPS_WORKER:-here}" > where.txt

resources:
- name: repo
  type: blob
  tags: [a]
  source: {}
`
		producer = `
  - get: src
    resource: repo
`
	}

	return writePipeline(t, dir, resources+`
jobs:
- name: build
  plan:`+producer+`
  - task: consume
    tags: [`+consumerTag+`]
    image: `+dockerE2EImage+`
    inputs: [src]
    outputs: [out]
    run: |
      wc -c < src/blob.bin | tr -d ' ' > out/size.txt
      cp src/where.txt out/where.txt
  - task: publish
    inputs: [out]
    run: cp out/size.txt `+filepath.Join(dir, "size.txt")+` && cp out/where.txt `+filepath.Join(dir, "where.txt")+`
`)
}

// twoWorkers maps a= and b= to two local: workers with separate roots, so each has its own cache namespace on the one daemon; with one root "the other worker is cold" could never be true.
func twoWorkers(t *testing.T) (rootA, rootB string, args []string) {
	t.Helper()

	roots := t.TempDir()
	rootA = filepath.Join(roots, "a")
	rootB = filepath.Join(roots, "b")

	return rootA, rootB, []string{"--worker", "a=local:" + rootA, "--worker", "b=local:" + rootB}
}

// placementNamed finds one step's placement row.
func placementNamed(t *testing.T, placements []store.Placement, name string) store.Placement {
	t.Helper()

	for _, p := range placements {
		if p.StepName == name {
			return p
		}
	}

	t.Fatalf("no placement for %q among %d rows", name, len(placements))

	return store.Placement{}
}

// cacheHoldsPayload reports whether the docker+ cache of the local: worker rooted at root holds a tree at least the payload's size: its alias volumes are named for this process's namespace and a hash of the root.
func cacheHoldsPayload(t *testing.T, root string) bool {
	t.Helper()

	prefix := cachePrefix(root)

	//nolint:gosec // a filter this test built
	out, err := exec.CommandContext(t.Context(), "docker", "volume", "ls", "--filter", "name="+prefix, "--format", `{{.Name}} {{.Label "steps.size"}}`).Output()
	if err != nil {
		t.Fatalf("listing the worker's cache: %v", err)
	}

	for line := range strings.Lines(string(out)) {
		name, size, _ := strings.Cut(strings.TrimSpace(line), " ")

		bytes, err := strconv.Atoi(size)
		if strings.HasPrefix(name, prefix) && err == nil && bytes >= payloadBytes {
			return true
		}
	}

	return false
}

// cachePrefix names the alias volumes of the local: worker rooted at root: this process's namespace, then a hash of the root.
func cachePrefix(root string) string {
	sum := sha256.Sum256([]byte(root))

	return "steps-a-" + os.Getenv("STEPS_TEST_CACHE_NAMESPACE") + hex.EncodeToString(sum[:4]) + "-"
}

// sweepCache is a local step that empties that worker's cache, as an eviction pass or an operator could.
func sweepCache(root string) string {
	return "docker volume ls -q --filter name=" + cachePrefix(root) + " | xargs docker volume rm -f"
}

// assertPublished checks the local step saw the producer's tree: the payload
// whole, and written on a.
func assertPublished(t *testing.T, dir string) {
	t.Helper()

	if got := readFileString(t, filepath.Join(dir, "size.txt")); got != strconv.Itoa(payloadBytes)+"\n" {
		t.Errorf("size.txt = %q, want the payload's size", got)
	}

	if got := readFileString(t, filepath.Join(dir, "where.txt")); got != "a" {
		t.Errorf("where.txt = %q, want %q — the tree the consumer read is not the one the producer wrote", got, "a")
	}
}

// TestEndToEndPlacedGetFeedsThePlacedTaskWithoutResending: the get's tree
// stays on the worker, so the consumer's offer for it is answered "already
// here" and only the consumer's empty output directory crosses.
func TestEndToEndPlacedGetFeedsThePlacedTaskWithoutResending(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	rootA, _, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", true)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir)

	consume := placementNamed(t, runPlacements(t, path), "consume")
	if consume.BytesSent >= payloadBytes {
		t.Errorf("consume sent %d bytes to the worker that fetched them; want under the payload — the worker did not keep what it produced", consume.BytesSent)
	}

	if !cacheHoldsPayload(t, rootA) {
		t.Error("the worker's artifact cache does not hold the fetched tree")
	}
}

// TestEndToEndPlacedTaskOutputFeedsTheNextPlacedTask: it is every fetch, not
// only a get's — a task's declared outputs come home and are consumed by the
// next placed step exactly the same way.
func TestEndToEndPlacedTaskOutputFeedsTheNextPlacedTask(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	rootA, _, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", false)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir)

	consume := placementNamed(t, runPlacements(t, path), "consume")
	if consume.BytesSent >= payloadBytes {
		t.Errorf("consume sent %d bytes to the worker that produced them; want under the payload", consume.BytesSent)
	}

	if !cacheHoldsPayload(t, rootA) {
		t.Error("the worker's artifact cache does not hold the produced output")
	}
}

// TestEndToEndAnotherWorkerIsStillCold: the cache is the machine's. A consumer
// placed elsewhere gets the whole tree, which is what makes the two
// assertions above mean "kept on the worker" rather than "kept somewhere".
func TestEndToEndAnotherWorkerIsStillCold(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	rootA, rootB, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "b", false)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir)

	placements := runPlacements(t, path)

	consume := placementNamed(t, placements, "consume")
	if consume.BytesSent < payloadBytes {
		t.Errorf("consume sent %d bytes to a worker that never had them; want the whole payload", consume.BytesSent)
	}

	// The other half of the ledger: neither step brought its output home at
	// the time it finished. The consumer on b was fed by a pipe from a, and
	// the local publish by a pull from b — neither is a placement's own.
	for _, name := range []string{"seed", "consume"} {
		if got := placementNamed(t, placements, name).BytesReceived; got != 0 {
			t.Errorf("%s brought %d bytes home at the time it finished, want 0", name, got)
		}
	}

	if !cacheHoldsPayload(t, rootA) {
		t.Error("the producing worker's cache does not hold the output")
	}

	if !cacheHoldsPayload(t, rootB) {
		t.Error("the consuming worker's cache does not hold what it received")
	}
}
