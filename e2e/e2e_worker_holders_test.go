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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// payloadBytes is one mebibyte of /dev/urandom: incompressible, so what
// crosses the wire is what the tree weighs, and the byte counters can tell a
// full transfer from an empty output directory without a margin argument.
const payloadBytes = 1 << 20

// holderPipeline is a placed producer, a placed consumer, and a local step
// that publishes what the consumer saw. producerTag and consumerTag choose the
// machines; get chooses whether the producer is a get or a task.
func holderPipeline(t *testing.T, dir, producerTag, consumerTag string, get bool) string {
	t.Helper()

	producer := `
  - task: seed
    tags: [` + producerTag + `]
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
  config:
    check: printf '[{"ref":"v1"}]'
    in: |
      head -c ` + strconv.Itoa(payloadBytes) + ` /dev/urandom > blob.bin
      printf '%s' "${STEPS_WORKER:-here}" > where.txt

resources:
- name: repo
  type: blob
  tags: [` + producerTag + `]
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

// twoWorkers maps a= and b= to two local: workers with separate roots, so
// each has its own artifact cache — with one shared root both shims would
// file under the same directory and "the other worker is cold" could never be
// true.
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

// cacheHoldsPayload reports whether a worker root's artifact cache holds a
// blob.bin of the payload's size — the tree the producer packed, filed.
func cacheHoldsPayload(t *testing.T, root string) bool {
	t.Helper()

	held := false

	_ = filepath.WalkDir(filepath.Join(root, "steps-shim", "artifacts"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != "blob.bin" {
			return nil //nolint:nilerr // an absent cache is "not held", not an error
		}

		info, statErr := os.Stat(path)
		if statErr == nil && info.Size() == payloadBytes {
			held = true
		}

		return nil
	})

	return held
}

func assertPublished(t *testing.T, dir, wantWhere string) {
	t.Helper()

	if got := readFileString(t, filepath.Join(dir, "size.txt")); got != strconv.Itoa(payloadBytes)+"\n" {
		t.Errorf("size.txt = %q, want the payload's size", got)
	}

	if got := readFileString(t, filepath.Join(dir, "where.txt")); got != wantWhere {
		t.Errorf("where.txt = %q, want %q — the tree the consumer read is not the one the producer wrote", got, wantWhere)
	}
}

// TestEndToEndPlacedGetFeedsThePlacedTaskWithoutResending: the get's tree
// stays on the worker, so the consumer's offer for it is answered "already
// here" and only the consumer's empty output directory crosses.
func TestEndToEndPlacedGetFeedsThePlacedTaskWithoutResending(t *testing.T) {
	dir := t.TempDir()
	rootA, _, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", "a", true)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir, "a")

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
	dir := t.TempDir()
	rootA, _, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", "a", false)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir, "a")

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
	dir := t.TempDir()
	rootA, rootB, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "a", "b", false)

	mustRun(t, append([]string{path}, workers...)...)

	assertPublished(t, dir, "a")

	placements := runPlacements(t, path)

	consume := placementNamed(t, placements, "consume")
	if consume.BytesSent < payloadBytes {
		t.Errorf("consume sent %d bytes to a worker that never had them; want the whole payload", consume.BytesSent)
	}

	// The other half of the ledger: the producer's output came home, and the
	// consumer's tiny one did too.
	seed := placementNamed(t, placements, "seed")
	if seed.BytesReceived < payloadBytes {
		t.Errorf("seed brought %d bytes home, want the whole payload it produced", seed.BytesReceived)
	}

	if consume.BytesReceived >= payloadBytes {
		t.Errorf("consume brought %d bytes home for an output holding two short lines", consume.BytesReceived)
	}

	if !cacheHoldsPayload(t, rootA) {
		t.Error("the producing worker's cache does not hold the output")
	}

	if !cacheHoldsPayload(t, rootB) {
		t.Error("the consuming worker's cache does not hold what it received")
	}
}
