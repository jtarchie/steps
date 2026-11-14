package e2e

// An untagged image: step runs exactly as one tagged onto a local: worker: through docker volumes, never a bind mount of this machine's directory (steps#246).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/testdocker"
)

// TestAnUntaggedImageStepRunsLikeOneTaggedLocal runs one task both ways and compares what each saw: the same workdir, the same input, and no STEPS_WORKER for the untagged one.
func TestAnUntaggedImageStepRunsLikeOneTaggedLocal(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	untagged, tagged := filepath.Join(dir, "untagged.txt"), filepath.Join(dir, "tagged.txt")

	job := func(name, tags, published string) string {
		return fmt.Sprintf(`- name: %[1]s
  plan:
  - task: seed
    outputs: [in]
    run: echo seeded > in/seed
  - task: look%[2]s
    image: %[3]s
    inputs: [in]
    outputs: [out]
    run: |
      pwd > out/seen
      cat in/seed >> out/seen
  - task: publish
    inputs: [out]
    run: cp out/seen %[4]s && echo "${STEPS_WORKER-unset}" > %[4]s.worker
`, name, tags, dockerE2EImage, published)
	}

	path := writePipeline(t, dir, "jobs:\n"+job("untagged", "", untagged)+job("tagged", "\n    tags: [box]", tagged))

	// Untagged last: runPlacements reads the newest run.
	mustRun(t, path, "--job", "tagged", "--worker", "box=local:")
	mustRun(t, path, "--job", "untagged")

	got, want := readFileString(t, untagged), readFileString(t, tagged)
	if got != want {
		t.Errorf("untagged saw %q, tagged local: saw %q: an untagged image: step must run the way a local: one does", got, want)
	}

	if !strings.Contains(got, "seeded") {
		t.Errorf("untagged saw %q, want its input", got)
	}

	placed := placementNamed(t, runPlacements(t, path), "look")
	if placed.Tag != "" || placed.Address == "" {
		t.Errorf("untagged placement = %+v, want a row with no tag naming this machine's daemon", placed)
	}
}

// TestAnUntaggedImageStepNeedsNoSharedTMPDIR puts the workspace where a docker VM cannot see it: a bind mount there is an empty directory, a volume does not care.
func TestAnUntaggedImageStepNeedsNoSharedTMPDIR(t *testing.T) {
	testdocker.Require(t)

	unshared, err := os.MkdirTemp("/tmp", "steps-unshared-")
	if err != nil {
		t.Skipf("no /tmp to stand in for a path the docker VM does not share: %v", err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(unshared) })
	t.Setenv("TMPDIR", unshared)

	dir := t.TempDir()

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: seed
    outputs: [in]
    run: echo seeded > in/seed
  - task: copy
    image: %s
    inputs: [in]
    outputs: [out]
    run: cat in/seed > out/copy
  - task: check
    inputs: [out]
    run: test "$(cat out/copy)" = seeded
`, dockerE2EImage))

	mustRun(t, path)
}

// TestAnUntaggedImageStepsCacheIsAVolume: the cache survives between runs, clear-task-cache reaches it, and this machine's workspace holds no copy of it.
func TestAnUntaggedImageStepsCacheIsAVolume(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	report := filepath.Join(dir, "runs.txt")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    image: %[3]s
    volatile: true
    caches:
    - path: .gocache
    outputs: [out]
    run: |
      n=$(cat .gocache/runs 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > .gocache/runs
      echo "$n" > out/runs
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[2]s
`, root, report, dockerE2EImage))

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")
	mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile")
	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != "1\n2\n1\n" {
		t.Errorf("runs reported %q, want the second run to see the cache and the run after the clear to start cold (\"1\\n2\\n1\\n\")", got)
	}

	var onHost []string

	_ = filepath.WalkDir(root, func(p string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "runs" && strings.Contains(p, "steps-task-caches") {
			onHost = append(onHost, p)
		}

		return nil
	})

	if len(onHost) > 0 {
		t.Errorf("the cache is on this machine at %v, want it in a docker volume", onHost)
	}
}

// TestAnUntaggedImageResourceRunsThroughVolumes: a get's in: is a container step with a tree too, so it takes the same path a task does.
func TestAnUntaggedImageResourceRunsThroughVolumes(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.txt")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: here
  image: %[1]s
  config:
    check: printf '[{"ref":"v1"}]'
    in: pwd > where.txt

resources:
- name: repo
  type: here
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: publish
    inputs: [repo]
    run: cp repo/where.txt %[2]s
`, dockerE2EImage, seen))

	mustRun(t, path)

	if got := readFileString(t, seen); got != "/steps/work\n" {
		t.Errorf("the get ran in %q, want the volume workdir /steps/work", got)
	}
}

// TestAnEditBetweenContainerStepsReachesTheNext edits an input on the host between two image: steps, same size and within the same second: the build's digest memo must forget the first read, or the second mounts the tree the first one saw.
func TestAnEditBetweenContainerStepsReachesTheNext(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.txt")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: seed
    outputs: [tree]
    run: printf aaaa > tree/file
  - task: first
    image: %[1]s
    inputs: [tree]
    run: test "$(cat tree/file)" = aaaa
  - task: edit
    inputs: [tree]
    outputs: [tree]
    run: printf bbbb > tree/file
  - task: second
    image: %[1]s
    inputs: [tree]
    outputs: [out]
    run: cat tree/file > out/seen
  - task: publish
    inputs: [out]
    run: cp out/seen %[2]s
`, dockerE2EImage, seen))

	mustRun(t, path)

	if got := readFileString(t, seen); got != "bbbb" {
		t.Errorf("the step after the edit read %q, want bbbb", got)
	}
}
