package e2e

// Worker to worker through the store (steps#138, rung 3).
//
// With --artifact-store, a tree one worker holds reaches another without
// touching this machine: the orchestrator asks the holder to push it under
// its digest, and hands the consumer a URL. The orchestrator moves no bytes;
// it mints URLs and knows who holds what.

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// wireBytes is what the store holds under wire/ for step trees, one-shot
// fetch objects excluded: the bytes a worker pushed for another to pull.
func (f *fakeS3) wireBytes() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	total := 0

	for key, body := range f.objects {
		if strings.Contains(key, "/wire/") && !strings.Contains(key, "/wire/out-") {
			total += len(body)
		}
	}

	return total
}

func storeWorkers(t *testing.T) (*fakeS3, []string) {
	t.Helper()

	fake, url := newFakeS3(t)
	_, _, workers := twoWorkers(t)

	return fake, append(workers, "--artifact-store", "s3://cas/team?endpoint="+url+"&region=us-east-1")
}

// TestEndToEndTwoWorkersShareThroughTheStoreNotTheOrchestrator: a gets, b
// consumes, and the megabyte crosses a→store→b with this machine sending
// none of it.
func TestEndToEndTwoWorkersShareThroughTheStoreNotTheOrchestrator(t *testing.T) {
	dir := t.TempDir()
	fake, args := storeWorkers(t)
	path := holderPipeline(t, dir, "b", true)

	mustRun(t, append([]string{path}, args...)...)

	assertPublished(t, dir)

	placements := runPlacements(t, path)

	consume := placementNamed(t, placements, "consume")
	if consume.BytesSent >= payloadBytes/2 {
		t.Errorf("this machine sent %d bytes to b for a tree a holds; want the store to have carried it", consume.BytesSent)
	}

	if got := fake.wireBytes(); got < payloadBytes || got > 2*payloadBytes {
		t.Errorf("the store holds %d bytes of trees, want the payload once — pushed by a, pulled by b", got)
	}

	for _, name := range []string{"src", "consume"} {
		for _, p := range placements {
			if p.StepName == name && p.BytesReceived != 0 {
				t.Errorf("%s brought %d bytes home; want 0 — nothing here read its tree at the time", name, p.BytesReceived)
			}
		}
	}
}

// TestEndToEndALocalStepInTheMiddleUploadsOnlyWhatItMade: the big tree is
// pulled here for the local step, and what goes on to b is the local step's
// own small output — the big one never enters the store.
func TestEndToEndALocalStepInTheMiddleUploadsOnlyWhatItMade(t *testing.T) {
	dir := t.TempDir()
	fake, args := storeWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [big]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > big/blob.bin
  - task: shrink
    inputs: [big]
    outputs: [small]
    run: wc -c < big/blob.bin | tr -d ' ' > small/size.txt
  - task: consume
    tags: [b]
    inputs: [small]
    outputs: [out]
    run: cp small/size.txt out/size.txt
  - task: publish
    inputs: [out]
    run: cp out/size.txt `+filepath.Join(dir, "size.txt")+`
`)

	mustRun(t, append([]string{path}, args...)...)

	if got := readFileString(t, filepath.Join(dir, "size.txt")); got != strconv.Itoa(payloadBytes)+"\n" {
		t.Errorf("size.txt = %q, want the payload's size", got)
	}

	consume := placementNamed(t, runPlacements(t, path), "consume")
	if consume.BytesSent >= payloadBytes/2 {
		t.Errorf("this machine sent %d bytes to b; its input was a few bytes", consume.BytesSent)
	}

	if got := fake.wireBytes(); got >= payloadBytes/2 {
		t.Errorf("the store holds %d bytes of trees; the big one was only ever read here", got)
	}
}

// TestEndToEndAPlacedPutReadsFromTwoHolders: a put on b with one input a
// holds and one made here. Both arrive; the big one through the store.
func TestEndToEndAPlacedPutReadsFromTwoHolders(t *testing.T) {
	dir := t.TempDir()
	fake, args := storeWorkers(t)
	path := writePipeline(t, dir, `
resource_types:
- name: sink
  config:
    check: printf '[]'
    out: |
      test -s big/blob.bin && test -s note/n.txt && printf '%s' "${STEPS_WORKER:-here}" > `+filepath.Join(dir, "pushed.txt")+`
      printf '{"ref":"pushed"}'

resources:
- name: target
  type: sink
  tags: [b]
  source: {}

jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    outputs: [big]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > big/blob.bin
  - task: annotate
    outputs: [note]
    run: echo hello > note/n.txt
  - put: target
    inputs: [big, note]
`)

	mustRun(t, append([]string{path}, args...)...)

	if got := readFileString(t, filepath.Join(dir, "pushed.txt")); got != "b" {
		t.Errorf("the put ran on %q, want b, with both inputs present", got)
	}

	put := placementNamed(t, runPlacements(t, path), "target")
	if put.BytesSent >= payloadBytes/2 {
		t.Errorf("this machine sent %d bytes to b for the put; want the big input to have come through the store", put.BytesSent)
	}

	if got := fake.wireBytes(); got < payloadBytes {
		t.Errorf("the store holds %d bytes of trees, want the big input a pushed", got)
	}
}
