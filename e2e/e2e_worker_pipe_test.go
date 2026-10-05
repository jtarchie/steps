package e2e

// Worker to worker through the orchestrator, with no store (steps#143).
//
// Without --artifact-store a tree one worker holds still never lands here:
// the orchestrator pipes the holder's stream into the consumer's upload as it
// arrives. The same bytes cross this machine's link, and none of them its
// disk.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// runKept runs a pipeline with --keep-workspace and answers what it printed,
// the kept build tree (which a passing run names only in its log), and the run's error.
func runKept(t *testing.T, path string, args []string) (string, string, error) {
	t.Helper()

	var err error

	logs := captureStderr(t)
	out := captureStdout(t, func() {
		err = cli.Run(append([]string{path, "--keep-workspace"}, args...))
	})

	return out, keptWorkspaceDir(t, out+logs()), err
}

// assertNeverLanded fails if any blob.bin sits under the kept build tree:
// the orchestrator's artifact directory, or a step directory it built.
func assertNeverLanded(t *testing.T, kept string) {
	t.Helper()

	_ = filepath.WalkDir(kept, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Name() == "blob.bin" {
			t.Errorf("the tree landed on the orchestrator at %s", path)
		}

		return nil
	})
}

// TestEndToEndTwoWorkersPipeThroughTheOrchestratorWithNoStore: a produces, b
// consumes, and the megabyte crosses this machine's link without touching
// its disk — for a get's tree and a task's output alike.
func TestEndToEndTwoWorkersPipeThroughTheOrchestratorWithNoStore(t *testing.T) {
	for _, get := range []bool{true, false} {
		t.Run("get="+strconv.FormatBool(get), func(t *testing.T) {
			assertPiped(t, get)
		})
	}
}

func assertPiped(t *testing.T, get bool) {
	t.Helper()
	requireDockerE2E(t)

	dir := t.TempDir()
	_, rootB, workers := twoWorkers(t)
	path := holderPipeline(t, dir, "b", get)

	out, kept, err := runKept(t, path, workers)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	assertPublished(t, dir)
	assertNeverLanded(t, kept)

	// Said, because the pipe runs before the step's first command and is
	// otherwise a silent pause.
	if !strings.Contains(out, `piping "src" from worker local:`) {
		t.Errorf("the run did not say it piped src:\n%s", out)
	}

	placements := runPlacements(t, path)

	consume := placementNamed(t, placements, "consume")
	if consume.BytesSent < payloadBytes {
		t.Errorf("consume was sent %d bytes; want the payload, piped across this machine's link", consume.BytesSent)
	}

	for _, p := range placements {
		if p.StepName != "publish" && p.BytesReceived != 0 {
			t.Errorf("%s brought %d bytes home; want 0", p.StepName, p.BytesReceived)
		}
	}

	if !cacheHoldsPayload(t, rootB) {
		t.Error("the consuming worker's cache does not hold what was piped to it")
	}
}

// TestEndToEndAPlacedPutIsPipedWhatAnotherWorkerHolds: a put on b whose
// input a holds gets it piped, alongside one made here.
func TestEndToEndAPlacedPutIsPipedWhatAnotherWorkerHolds(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	// out: runs in a container, so it reports where it ran in the version it returns rather than in a host file.
	path := writePipeline(t, dir, `
resource_types:
- name: sink
  image: `+dockerE2EImage+`
  config:
    check: printf '[]'
    out: test -s big/blob.bin && test -s note/n.txt && printf '{"ref":"pushed","where":"%s"}' "${STEPS_WORKER:-here}"

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
    image: `+dockerE2EImage+`
    outputs: [big]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > big/blob.bin
  - task: annotate
    outputs: [note]
    run: echo hello > note/n.txt
  - put: target
    inputs: [big, note]
`)

	out, kept, err := runKept(t, path, workers)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	said := false

	for _, row := range latestRunEvents(t, path) {
		said = said || row.Text == `put: target (version: {"ref":"pushed","where":"b"})`
	}

	if !said {
		t.Error("the put did not answer a version from b, with both inputs present")
	}

	assertNeverLanded(t, kept)

	put := placementNamed(t, runPlacements(t, path), "target")
	if put.BytesSent < payloadBytes {
		t.Errorf("the put was sent %d bytes; want the big input piped", put.BytesSent)
	}
}

// TestEndToEndAHolderThatLostTheTreeFailsThePipedConsumerByName: the holder's
// cache is emptied between the producer and a consumer on another worker, and
// the build fails naming the input and the holder — with nothing half-placed
// on the consumer.
func TestEndToEndAHolderThatLostTheTreeFailsThePipedConsumerByName(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	rootA, rootB, workers := twoWorkers(t)
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    image: `+dockerE2EImage+`
    outputs: [src]
    run: head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > src/blob.bin
  - task: sweep
    run: `+sweepCache(rootA)+`
  - task: consume
    tags: [b]
    image: `+dockerE2EImage+`
    inputs: [src]
    # A pipe that never ends must fail here rather than wedge the shard.
    timeout: 60s
    run: wc -c < src/blob.bin
`)

	err := cli.Run(append([]string{path}, workers...))
	if err == nil {
		t.Fatal("the build passed with its input gone from the only machine that held it")
	}

	for _, want := range []string{`"src"`, "local:" + rootA, "no longer holds it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err.Error(), want)
		}
	}

	if cacheHoldsPayload(t, rootB) {
		t.Error("the consumer kept a tree its holder could not send")
	}
}

// escapingLinkPipeline seeds a tree on a holding a link to a file outside it,
// and has b read the link into leaked.
func escapingLinkPipeline(t *testing.T, dir string) (string, string) {
	t.Helper()

	leaked := filepath.Join(dir, "leaked.txt")

	return writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    image: `+dockerE2EImage+`
    outputs: [src]
    run: echo made > src/made.txt && ln -s /etc/hosts src/leak
  - task: consume
    tags: [b]
    image: `+dockerE2EImage+`
    inputs: [src]
    outputs: [out]
    run: cat src/leak > out/leaked.txt
  - task: publish
    inputs: [out]
    run: cp out/leaked.txt `+leaked+`
`), leaked
}

// TestEndToEndAWorkerCannotPlantAnEscapingLinkOnAnother: a tree crossing
// from one worker to another refuses a link that leaves it, as it did when
// every such tree landed here first — a worker does not get to name files on
// another.
func TestEndToEndAWorkerCannotPlantAnEscapingLinkOnAnother(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	_, _, workers := twoWorkers(t)
	path, leaked := escapingLinkPipeline(t, dir)

	assertLinkRefused(t, cli.Run(append([]string{path}, workers...)), leaked)
}

func assertLinkRefused(t *testing.T, err error, leaked string) {
	t.Helper()

	if err == nil {
		t.Fatal("the build passed with a link out of the tree planted on another worker")
	}

	for _, want := range []string{"points outside the tree", `"src"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %s", err.Error(), want)
		}
	}

	_, statErr := os.Stat(leaked)
	if statErr == nil {
		t.Error("the consumer ran and read through the link")
	}
}
