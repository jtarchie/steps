package e2e

// caches: a directory a step keeps between runs, which only makes it faster and never changes what it is.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/testsshd"
)

// taskCachePipeline counts its runs in a cache dir; --force and volatile: because a skipped step never runs, so would never touch the cache.
func taskCachePipeline(t *testing.T, dir, root, report string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    volatile: true
    caches:
    - path: .gocache
    outputs: [bin]
    run: |
      n=$(cat .gocache/runs 2>/dev/null || echo 0)
      n=$((n + 1))
      echo "$n" > .gocache/runs
      echo "$n" >> %[2]s
      echo built > bin/app
`, root, report))
}

// TestEndToEndTaskCacheSurvivesToTheNextRun: the cache survives, and a warm cache never moves the node hash.
func TestEndToEndTaskCacheSurvivesToTheNextRun(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := taskCachePipeline(t, dir, filepath.Join(dir, "ws"), report)

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")

	got := readFileString(t, report)
	if got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the first's cache (\"1\\n2\\n\")", got)
	}

	db := openStateDB(t, path)

	var hashes int

	err := db.QueryRowContext(t.Context(), `SELECT COUNT(DISTINCT hash) FROM nodes WHERE resource = 'compile'`).Scan(&hashes)
	if err != nil {
		t.Fatalf("query node hashes: %v", err)
	}

	if hashes != 1 {
		t.Errorf("compile recorded %d distinct node hashes, want 1 — the cache directory reached the step's identity", hashes)
	}
}

// TestEndToEndAgentCacheSurvivesToTheNextRun: an agent's run_shell is where self-build's compiles happen, so its caches: must persist the same way.
func TestEndToEndAgentCacheSurvivesToTheNextRun(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	count := `n=$(cat .cache/runs 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > .cache/runs; echo "$n" >> ` + report

	run := []turn{callsTool("run_shell", map[string]any{"command": count}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[2]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    volatile: true
    messages:
      - Build it.
`, fake.URL, filepath.Join(dir, "ws")))

	mustRun(t, "run", path, "--force")
	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the first's cache (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndPlacedTaskCacheStaysOnItsWorker: the local stand-in stays empty, so a warm second run can only have come from the worker's own volume.
func TestEndToEndPlacedTaskCacheStaysOnItsWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    tags: [box]
    image: `+dockerE2EImage+`
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
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box=local:")
	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}

// removeTaskCacheVolumes drops the cache volumes this process made, which by design nothing else ever removes.
func removeTaskCacheVolumes(t *testing.T) {
	t.Helper()

	docker, err := dockerapi.New("")
	if err != nil {
		t.Logf("cleanup: %v", err)

		return
	}

	defer func() { _ = docker.Close() }()

	ctx := context.WithoutCancel(t.Context())

	volumes, err := docker.ListVolumes(ctx, map[string]string{"steps.pid": strconv.Itoa(os.Getpid())})
	if err != nil {
		t.Logf("cleanup: %v", err)

		return
	}

	for _, volume := range volumes {
		if strings.HasPrefix(volume.Name, "steps-tc-") {
			_ = docker.RemoveVolume(ctx, volume.Name)
		}
	}
}

// TestEndToEndSSHWorkerTaskCacheStaysOnItsWorker: a bare ssh:// worker keeps its caches under its root, beside the builds it removes.
func TestEndToEndSSHWorkerTaskCacheStaysOnItsWorker(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	worker := testsshd.New(t).URL
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    tags: [box]
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
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box="+worker)
	mustRun(t, "run", path, "--force", "--worker", "box="+worker)

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndPlacedAgentCacheStaysOnItsWorker crosses the seam the task test does not: the agent builds its own runner spec, which must carry the keys to the worker.
func TestEndToEndPlacedAgentCacheStaysOnItsWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	count := `n=$(cat .cache/runs 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > .cache/runs; echo "$n" > out/runs`
	run := []turn{callsTool("run_shell", map[string]any{"command": count}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[3]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    tags: [box]
    volatile: true
    outputs: [out]
    messages:
      - Build it.
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[4]s
`, fake.URL, dockerE2EImage, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box=local:")
	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the second run to see the worker's cache (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndPlacedStepCacheNeverCrossesTheWire: a cache warmed here must not reach a worker, where it would be packed into the tree sent over and thrown away under the worker's own. The step cannot see the difference, so the proof is the bytes the placement sent.
func TestEndToEndPlacedStepCacheNeverCrossesTheWire(t *testing.T) {
	dir := t.TempDir()
	worker := testsshd.New(t).URL

	const markerBytes = 1 << 20

	pipeline := func(tags string) string {
		return fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    %[2]s
    volatile: true
    caches:
    - path: .gocache
    run: head -c %[3]d /dev/zero > .gocache/marker
`, filepath.Join(dir, "ws"), tags, markerBytes)
	}

	path := writePipeline(t, dir, pipeline(""))
	mustRun(t, "run", path, "--force")

	writePipelineFile(t, path, pipeline("tags: [box]"))
	mustRun(t, "run", path, "--force", "--worker", "box="+worker)

	var sent int64

	err := openStateDB(t, path).QueryRowContext(t.Context(), `SELECT bytes_sent FROM run_placements WHERE step_name = 'compile'`).Scan(&sent)
	if err != nil {
		t.Fatalf("query placement: %v", err)
	}

	if sent >= markerBytes {
		t.Errorf("the placement sent %d bytes, want fewer than the %d-byte local cache: it crossed the wire", sent, markerBytes)
	}
}

// TestEndToEndPlacedAgentCacheNeverCrossesTheWire is the agent's own seam for the same rule: it attaches its caches itself, so it must also know not to.
func TestEndToEndPlacedAgentCacheNeverCrossesTheWire(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()

	const markerBytes = 1 << 20

	run := []turn{callsTool("run_shell", map[string]any{"command": fmt.Sprintf("head -c %d /dev/zero > .cache/marker", markerBytes)}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	pipeline := func(tags string) string {
		return fmt.Sprintf(`
workspace:
  root: %[3]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    %[4]s
    volatile: true
    messages:
      - Build it.
`, fake.URL, dockerE2EImage, filepath.Join(dir, "ws"), tags)
	}

	path := writePipeline(t, dir, pipeline(""))
	mustRun(t, "run", path, "--force")

	writePipelineFile(t, path, pipeline("tags: [box]"))

	// Volumes an earlier test in this shard process left behind carry the same pid label and would count as bytes this placement sent.
	before := volumeNames(t)

	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	// A placed agent records no placement row (observed: notePlacement is only called for tasks and resources), so the bytes are read off the daemon: every tree a docker+ session sends is filled into a data volume labelled with its size.
	if filled := volumeBytesFilled(t, before); filled >= markerBytes {
		t.Errorf("the session filled %d bytes into the worker, want fewer than the %d-byte local cache: it crossed the wire", filled, markerBytes)

		// Seen only under full-gate load, twice, never alone: name what was counted so the next one says which tree it was.
		for _, volume := range processVolumes(t) {
			if !before[volume.Name] {
				t.Logf("counted %s: %v", volume.Name, volume.Labels)
			}
		}
	}
}

// volumeNames is the set of data volumes this process has on the local daemon.
func volumeNames(t *testing.T) map[string]bool {
	t.Helper()

	names := map[string]bool{}

	for _, volume := range processVolumes(t) {
		names[volume.Name] = true
	}

	return names
}

// volumeBytesFilled sums the trees this process sent to the local daemon in volumes that were not already there.
func volumeBytesFilled(t *testing.T, already map[string]bool) int64 {
	t.Helper()

	var total int64

	for _, volume := range processVolumes(t) {
		if already[volume.Name] {
			continue
		}

		size, _ := strconv.ParseInt(volume.Labels["steps.size"], 10, 64)
		total += size
	}

	return total
}

func processVolumes(t *testing.T) []dockerapi.Volume {
	t.Helper()

	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = docker.Close() }()

	volumes, err := docker.ListVolumes(t.Context(), map[string]string{"steps.pid": strconv.Itoa(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}

	return volumes
}

// TestVolumeBytesFilledIgnoresVolumesAlreadyThere: a volume an earlier test in this shard process left behind has the same pid label, and counting it made the placed-agent test above fail with someone else's megabyte.
func TestVolumeBytesFilledIgnoresVolumesAlreadyThere(t *testing.T) {
	requireDockerE2E(t)

	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = docker.Close() })

	name := "steps-test-leftover-" + strconv.Itoa(os.Getpid())
	labels := map[string]string{"steps.pid": strconv.Itoa(os.Getpid()), "steps.size": strconv.Itoa(2 << 20)}

	_, err = docker.CreateVolume(t.Context(), name, labels, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = docker.RemoveVolume(context.WithoutCancel(t.Context()), name) })

	if got := volumeBytesFilled(t, nil); got < 2<<20 {
		t.Fatalf("the leftover counted %d bytes with nothing excluded, want at least 2 MiB (the helper is not seeing the volume)", got)
	}

	if got := volumeBytesFilled(t, volumeNames(t)); got != 0 {
		t.Errorf("volumes already there counted %d bytes, want 0", got)
	}
}

// placedCachePipeline is TestEndToEndPlacedTaskCacheStaysOnItsWorker's pipeline with the compile step's placement lines as given, so one file can be warmed placed or unplaced.
func placedCachePipeline(t *testing.T, dir, placement, report string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    %[2]s
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
    run: cat out/runs >> %[3]s
`, filepath.Join(dir, "ws"), placement, report))
}

// TestEndToEndClearTaskCacheEmptiesTheLocalCache: fly clear-task-cache's job, against a cache this machine keeps.
func TestEndToEndClearTaskCacheEmptiesTheLocalCache(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := taskCachePipeline(t, dir, filepath.Join(dir, "ws"), report)

	mustRun(t, "run", path, "--force")

	out := captureStdout(t, func() { mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile") })
	if !strings.Contains(out, "cleared .gocache") {
		t.Errorf("clear-task-cache printed %q, want it to say it cleared .gocache", out)
	}

	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != "1\n1\n" {
		t.Errorf("runs reported %q, want the run after the clear to start cold (\"1\\n1\\n\")", got)
	}
}

// TestEndToEndClearTaskCacheOnlyThePathNamed: -c is compared cleaned, which Concourse's own command does not do.
func TestEndToEndClearTaskCacheOnlyThePathNamed(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    volatile: true
    caches:
    - path: .a
    - path: .b
    run: |
      for c in .a .b; do
        n=$(cat $c/runs 2>/dev/null || echo 0)
        n=$((n + 1))
        echo "$n" > $c/runs
        printf '%%s=%%s ' "$c" "$n" >> %[2]s
      done
      echo >> %[2]s
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force")
	mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile", "-c", "./.a/")
	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != ".a=1 .b=1 \n.a=1 .b=2 \n" {
		t.Errorf("runs reported %q, want only .a cleared", got)
	}

	err := cli.Run([]string{"clear-task-cache", path, "-j", "build", "-s", "compile", "-c", ".c"})
	if err == nil || !strings.Contains(err.Error(), ".a, .b") {
		t.Errorf("clearing an undeclared path: %v, want a refusal naming the step's paths", err)
	}
}

// TestEndToEndClearTaskCacheRefusesWhatItCannotName: each refusal names what the pipeline does have.
func TestEndToEndClearTaskCacheRefusesWhatItCannotName(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: compile
    caches:
    - path: .gocache
    run: "true"
  - task: lint
    run: "true"
`)

	for _, tc := range []struct{ name, job, step, want string }{
		{"unknown job", "deploy", "compile", `no job named "deploy"`},
		{"unknown step", "build", "test", `no task or agent step named "test"`},
		{"no caches", "build", "lint", `"lint" declares no caches`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := cli.Run([]string{"clear-task-cache", path, "-j", tc.job, "-s", tc.step})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want an error containing %q", err, tc.want)
			}
		})
	}

	out := captureStdout(t, func() { mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile") })
	if !strings.Contains(out, "workspace.root") {
		t.Errorf("with no workspace.root it printed %q, want it to say no cache outlives a run", out)
	}
}

// TestEndToEndClearTaskCacheRefusesAnUnmappedWorker: the local cache must survive the refusal, or the command cleared half of what it was asked and said no.
func TestEndToEndClearTaskCacheRefusesAnUnmappedWorker(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")

	path := placedCachePipeline(t, dir, "", report)
	mustRun(t, "run", path, "--force")

	placedCachePipeline(t, dir, "tags: [box]", report)

	err := cli.Run([]string{"clear-task-cache", path, "-j", "build", "-s", "compile"})
	if err == nil || !strings.Contains(err.Error(), "--worker box=") {
		t.Fatalf("clearing a placed step with no --worker: %v, want a refusal naming the tag", err)
	}

	placedCachePipeline(t, dir, "", report)
	mustRun(t, "run", path, "--force")

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want the refused clear to leave the local cache warm (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndClearTaskCacheOnACloudWorker: a launched machine took its caches with it, so there is nothing to do; a parked one keeps them on a disk only a start reaches, which costs money and so is asked for.
func TestEndToEndClearTaskCacheOnACloudWorker(t *testing.T) {
	dir := t.TempDir()
	path := placedCachePipeline(t, dir, "tags: [box]", filepath.Join(dir, "runs.txt"))

	out := captureStdout(t, func() {
		mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile", "--worker", "box=aws://launch/lt-0123456789abcdef0?region=us-east-2")
	})
	if !strings.Contains(out, "worker box: nothing to clear") {
		t.Errorf("a launch-rung worker printed %q, want it to say there is nothing to clear", out)
	}

	err := cli.Run([]string{"clear-task-cache", path, "-j", "build", "-s", "compile", "--worker", "box=aws://stopped/i-0123456789abcdef0?region=us-east-2"})
	if err == nil || !strings.Contains(err.Error(), "--start") {
		t.Errorf("a stopped worker without --start: %v, want a refusal naming --start", err)
	}
}

// TestEndToEndClearTaskCacheOnADockerWorker: the worker's volume goes, and one a container still mounts is refused by the daemon, reported, and fails the command.
func TestEndToEndClearTaskCacheOnADockerWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	path := placedCachePipeline(t, dir, "tags: [box]\n    image: "+dockerE2EImage, report)

	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	release := holdTaskCacheVolumes(t)

	err := cli.Run([]string{"clear-task-cache", path, "-j", "build", "-s", "compile", "--worker", "box=local:"})
	if err == nil {
		t.Error("clearing a volume a container mounts succeeded, want the daemon's refusal reported as a failure")
	}

	release()
	mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile", "--worker", "box=local:")
	mustRun(t, "run", path, "--force", "--worker", "box=local:")

	if got := readFileString(t, report); got != "1\n1\n" {
		t.Errorf("runs reported %q, want the run after the clear to start cold (\"1\\n1\\n\")", got)
	}
}

// holdTaskCacheVolumes mounts every cache volume this process made into a never-started container, which is enough for the daemon to refuse removing them.
func holdTaskCacheVolumes(t *testing.T) func() {
	t.Helper()

	docker, err := dockerapi.New("")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = docker.Close() })

	volumes, err := docker.ListVolumes(t.Context(), map[string]string{"steps.pid": strconv.Itoa(os.Getpid())})
	if err != nil {
		t.Fatal(err)
	}

	var mounts []dockerapi.Bind

	for _, volume := range volumes {
		if strings.HasPrefix(volume.Name, "steps-tc-") {
			mounts = append(mounts, dockerapi.Bind{Source: volume.Name, Target: "/cache" + strconv.Itoa(len(mounts))})
		}
	}

	if len(mounts) == 0 {
		t.Fatal("the run left no cache volume to hold")
	}

	holder, err := docker.CreateHolder(t.Context(), "steps-test-hold-"+strconv.Itoa(os.Getpid()), dockerE2EImage, nil, mounts)
	if err != nil {
		t.Fatal(err)
	}

	var once sync.Once

	remove := func() {
		once.Do(func() { _ = docker.RemoveContainer(context.WithoutCancel(t.Context()), holder) })
	}

	t.Cleanup(remove)

	return remove
}

// TestEndToEndClearTaskCacheOnAnSSHWorker: a bare worker's cache is a directory under its root.
func TestEndToEndClearTaskCacheOnAnSSHWorker(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	worker := testsshd.New(t).URL
	path := placedCachePipeline(t, dir, "tags: [box]", report)

	mustRun(t, "run", path, "--force", "--worker", "box="+worker)
	mustRun(t, "clear-task-cache", path, "-j", "build", "-s", "compile", "--worker", "box="+worker)
	mustRun(t, "run", path, "--force", "--worker", "box="+worker)

	if got := readFileString(t, report); got != "1\n1\n" {
		t.Errorf("runs reported %q, want the run after the clear to start cold (\"1\\n1\\n\")", got)
	}
}

// reapPipeline counts each cache's runs; v2 drops compile's .b and renames lint to vet, as an edited pipeline would.
func reapPipeline(t *testing.T, dir, placement, report string, v2 bool) string {
	t.Helper()

	compileCaches, compileRun, lint := "[{path: .a}, {path: .b}]", `echo "a=$(count .a) b=$(count .b)"`, "lint"
	if v2 {
		compileCaches, compileRun, lint = "[{path: .a}]", `echo "a=$(count .a)"`, "vet"
	}

	const count = `count() { n=$(($(cat "$1/n" 2>/dev/null || echo 0) + 1)); echo "$n" > "$1/n"; echo "$n"; }`

	return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: compile
    %[3]s
    volatile: true
    outputs: [cout]
    caches: %[4]s
    run: |
      %[7]s
      %[5]s > cout/r
  - task: %[6]s
    %[3]s
    volatile: true
    outputs: [lout]
    caches: [{path: .c}]
    run: |
      %[7]s
      echo "c=$(count .c)" > lout/r
  - task: publish
    volatile: true
    inputs: [cout, lout]
    run: cat cout/r lout/r >> %[2]s
`, filepath.Join(dir, "ws"), report, placement, compileCaches, compileRun, lint, count))
}

// assertReaped runs v1, another pipeline beside it, v2, then both again: a cache v2 no longer names starts cold, the one it kept stays warm, and another pipeline's is never touched.
func assertReaped(t *testing.T, placement string, run func(path string)) {
	t.Helper()

	dir, otherDir := t.TempDir(), t.TempDir()
	report, otherReport := filepath.Join(dir, "runs.txt"), filepath.Join(otherDir, "runs.txt")
	other := reapPipeline(t, otherDir, placement, otherReport, false)

	run(reapPipeline(t, dir, placement, report, false))
	run(other)
	run(reapPipeline(t, dir, placement, report, true))
	run(reapPipeline(t, dir, placement, report, false))
	run(other)

	if got, want := readFileString(t, report), "a=1 b=1\nc=1\na=2\nc=1\na=3 b=1\nc=1\n"; got != want {
		t.Errorf("runs reported %q, want %q: the caches v2 dropped (compile's .b, lint's .c) reaped, compile's .a kept", got, want)
	}

	if got, want := readFileString(t, otherReport), "a=1 b=1\nc=1\na=2 b=2\nc=2\n"; got != want {
		t.Errorf("the other pipeline reported %q, want %q: its caches are not this pipeline's to reap", got, want)
	}
}

func TestEndToEndPlacedTaskCacheOfARemovedStepIsReaped(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	assertReaped(t, "tags: [box]\n    image: "+dockerE2EImage, func(path string) {
		mustRun(t, "run", path, "--force", "--worker", "box=local:")
	})
}

func TestEndToEndSSHWorkerTaskCacheOfARemovedStepIsReaped(t *testing.T) {
	worker := testsshd.New(t).URL

	assertReaped(t, "tags: [box]", func(path string) {
		mustRun(t, "run", path, "--force", "--worker", "box="+worker)
	})
}

// TestEndToEndTaskCacheReapStaysInItsWorker: two local: roots on one daemon are two workers, and v2 placed on one leaves the other's caches alone.
func TestEndToEndTaskCacheReapStaysInItsWorker(t *testing.T) {
	requireDockerE2E(t)
	t.Cleanup(func() { removeTaskCacheVolumes(t) })

	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	placement := "tags: [box]\n    image: " + dockerE2EImage
	here, there := "box=local:"+filepath.Join(dir, "here"), "box=local:"+filepath.Join(dir, "there")

	mustRun(t, "run", reapPipeline(t, dir, placement, report, false), "--force", "--worker", here)
	mustRun(t, "run", reapPipeline(t, dir, placement, report, true), "--force", "--worker", there)
	mustRun(t, "run", reapPipeline(t, dir, placement, report, false), "--force", "--worker", here)

	if got, want := readFileString(t, report), "a=1 b=1\nc=1\na=1\nc=1\na=2 b=2\nc=2\n"; got != want {
		t.Errorf("runs reported %q, want %q: v2 on the other worker reaps only its own caches", got, want)
	}
}

// TestEndToEndSSHWorkerCacheThatCannotBeRestoredIsEmpty: f/x is a file in the input, so the cache under it cannot be put back; that is an empty cache, as on this machine, never a failed step, and the other cache is still kept.
func TestEndToEndSSHWorkerCacheThatCannotBeRestoredIsEmpty(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "runs.txt")
	worker := testsshd.New(t).URL
	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[1]s

jobs:
- name: build
  plan:
  - task: source
    volatile: true
    outputs: [f]
    run: echo file > f/x
  - task: compile
    tags: [box]
    volatile: true
    inputs: [f]
    caches: [{path: a}, {path: f/x/b}]
    outputs: [out]
    run: |
      n=$(($(cat a/n 2>/dev/null || echo 0) + 1))
      echo "$n" > a/n
      echo "$n" > out/runs
  - task: publish
    volatile: true
    inputs: [out]
    run: cat out/runs >> %[2]s
`, filepath.Join(dir, "ws"), report))

	mustRun(t, "run", path, "--force", "--worker", "box="+worker)
	mustRun(t, "run", path, "--force", "--worker", "box="+worker)

	if got := readFileString(t, report); got != "1\n2\n" {
		t.Errorf("runs reported %q, want both to run and the restorable cache kept (\"1\\n2\\n\")", got)
	}
}

// TestEndToEndAgentContextSeesItsCache: the cache is in place before the agent reads its context_paths:, as a Concourse cache is mounted before the task starts, so a context file under the cache is the cache's, not the input copy it shadows. The first run has no context_paths: because an empty cache shadows the file it would read.
func TestEndToEndAgentContextSeesItsCache(t *testing.T) {
	dir := t.TempDir()
	run := []turn{callsTool("run_shell", map[string]any{"command": "echo from-cache > src/cache/notes"}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	pipeline := func(context string) string {
		return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[2]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell, read_file]
  caches:
  - path: src/cache

jobs:
- name: build
  plan:
  - task: source
    volatile: true
    outputs: [src]
    run: mkdir -p src/cache && echo from-input > src/cache/notes
  - agent: builder
    volatile: true
    inputs: [src]
    %[3]s
    messages:
      - Build it.
`, fake.URL, filepath.Join(dir, "ws"), context))
	}

	mustRun(t, "run", pipeline(""), "--force")
	mustRun(t, "run", pipeline("context_paths: [src/cache/notes]"), "--force")

	if second := fake.request(3); !second.toolResultContains("from-cache") {
		t.Errorf("the second run's context read %q, want the cache's notes (from-cache)", second.toolResults())
	}
}

// TestEndToEndAgentCacheSurvivesAFailedPrepare: once attached, the cache is filed back on every way out, so an agent refused before its first turn (a context path its cache does not hold) keeps it.
func TestEndToEndAgentCacheSurvivesAFailedPrepare(t *testing.T) {
	dir := t.TempDir()
	report := filepath.Join(dir, "seen.txt")
	run := []turn{callsTool("run_shell", map[string]any{"command": "cat .cache/notes >> " + report + " 2>/dev/null; echo kept > .cache/notes"}), says("done")}
	fake := newFakeLLM(t, append(run, run...)...)

	pipeline := func(context string) string {
		return writePipeline(t, dir, fmt.Sprintf(`
workspace:
  root: %[2]s

defaults:
  preflight:
    disabled: true

agents:
- name: builder
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell, read_file]
  caches:
  - path: .cache

jobs:
- name: build
  plan:
  - agent: builder
    volatile: true
    %[3]s
    messages:
      - Build it.
`, fake.URL, filepath.Join(dir, "ws"), context))
	}

	mustRun(t, "run", pipeline(""), "--force")

	err := cli.Run([]string{"run", pipeline("context_paths: [.cache/missing]"), "--force"})
	if err == nil {
		t.Fatal("a context path the cache does not hold was not refused")
	}

	mustRun(t, "run", pipeline(""), "--force")

	if got := readFileString(t, report); got != "kept\n" {
		t.Errorf("the run after the refused one saw %q, want the cache the first run kept (\"kept\\n\")", got)
	}
}
