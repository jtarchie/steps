package e2e

// The readers of an untagged image: step's held output that phase 1 of steps#259 reached by reading code alone: a hook, load_var:, a when: guard, and the --artifact-store mirror.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAHookReadsWhatAnUntaggedImageStepHeld: a host hook on the producer is the first reader here, so it pulls the output whole.
func TestAHookReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`    on_success:
      task: read
      inputs: [src]
      run: cp src/blob.bin src/sum %s/
`, dir))

	mustRun(t, path)

	requireSumMatches(t, dir)
}

// TestLoadVarReadsWhatAnUntaggedImageStepHeld: load_var: reads its file in this process, from a tree only docker has.
func TestLoadVarReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    image: `+dockerE2EImage+`
    outputs: [meta]
    run: printf 'v1.2.3\n' > meta/version.txt
  - load_var: tag
    inputs: [meta]
    file: meta/version.txt
  - task: announce
    run: echo "releasing ((.:tag)) ok" > `+filepath.Join(dir, "announced.txt")+`
`)

	mustRun(t, path)

	if got := readFileString(t, filepath.Join(dir, "announced.txt")); got != "releasing v1.2.3 ok\n" {
		t.Errorf("announced = %q, want the value load_var read from the tree docker held", got)
	}
}

// TestAGuardReadsWhatAnUntaggedImageStepHeld: a guard skips quietly on a false, so a guard handed an empty or partial input would skip rather than fail; each guard here is true only over the producer's whole payload.
func TestAGuardReadsWhatAnUntaggedImageStepHeld(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`  - task: in-docker
    image: %[1]s
    inputs: [src]
    outputs: [mark]
    when: test "$(sha256sum src/blob.bin | cut -d' ' -f1)" = "$(cat src/sum)"
    run: echo ok > mark/ok
  - task: here
    inputs: [src, mark]
    when: '[ $(wc -c < src/blob.bin) -eq %[2]d ] && test -s mark/ok'
    run: touch %[3]s/here-ran
`, dockerE2EImage, untaggedPayload, dir))

	mustRun(t, path)

	_, err := os.Stat(filepath.Join(dir, "here-ran"))
	if err != nil {
		t.Errorf("a guard read the held output short and skipped its step: %v", err)
	}
}

// TestTheArtifactStoreMirrorsWhatAnUntaggedImageStepHeld: the mirror needs the bytes, so an untagged image step's outputs come home to be published, and a machine whose step cache lost them restores them from the store instead of running the step.
func TestTheArtifactStoreMirrorsWhatAnUntaggedImageStepHeld(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	root := filepath.Join(dir, "ws-root")
	sums := filepath.Join(dir, "sums")
	fake, url := newFakeS3(t)
	storeFlag := "s3://cas/team?endpoint=" + url + "&region=us-east-1"

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
`+untaggedProducer()+fmt.Sprintf(`  - task: publish
    volatile: true
    inputs: [src]
    run: cat src/sum >> %s && [ $(wc -c < src/blob.bin) -eq %d ]
`, sums, untaggedPayload))

	mustRun(t, "run", path, "--force", "--workspace-root", root, "--artifact-store", storeFlag)

	if fake.puts == 0 {
		t.Fatal("the run published nothing, so the held output never reached the mirror")
	}

	err := evictLocalCache(root)
	if err != nil {
		t.Fatal(err)
	}

	rewritePipeline(t, path, "run: cat src/sum >> ", "run: true && cat src/sum >> ")

	mustRun(t, "run", path, "--workspace-root", root, "--artifact-store", storeFlag)

	if got := storeNodeResult(t, path, "make"); !strings.Contains(got, `"reused":true`) {
		t.Errorf("make on the second run: %s, want it restored from the artifact store", got)
	}

	if fake.gets == 0 {
		t.Error("nothing was fetched from the artifact store, so where did make's output come from?")
	}

	if lines := strings.Fields(readFileString(t, sums)); len(lines) != 2 || lines[0] != lines[1] {
		t.Errorf("the two runs read %q, want one payload twice", lines)
	}
}
