package e2e

// End-to-end placement of RESOURCES: a resource's check, in and out on a
// worker, through the real CLI, onto a real shim.
//
// Same transport as the task tests (local: runs the shim as a child process),
// and the same proof: STEPS_WORKER is set only inside a placed command, so a
// command that reports it ran through a venue rather than here.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

// probeType is a resource type whose every stage reports where it ran: the
// check in its version, the in as a file — naming both where the version it
// was handed came from and where it ran itself — and the out in its returned
// version.
const probeType = `
resource_types:
- name: probe
  config:
    check: printf '[{"ref":"v1","where":"%s"}]' "${STEPS_WORKER:-here}"
    in: printf '%s/%s' "{{ .version.where }}" "${STEPS_WORKER:-here}" > where.txt
    out: test -f src/where.txt && printf '{"ref":"pushed","where":"%s"}' "${STEPS_WORKER:-here}"
`

// resourceWorkerPipeline tags the RESOURCE, so its check, in and out are all
// placed, while the task between them stays here.
func resourceWorkerPipeline(t *testing.T, dir string) string {
	t.Helper()

	return writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - task: note
    inputs: [src]
    run: cp src/where.txt `+filepath.Join(dir, "fetched.txt")+`
  - put: repo
    inputs: [src]
`)
}

// checkedVersions returns the versions a check of the resource recorded.
func checkedVersions(t *testing.T, pipelinePath, resource string) []map[string]any {
	t.Helper()

	st, err := store.OpenStore(cli.StatePath(pipelinePath, ""), cli.PipelineName(pipelinePath))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	versions, err := st.ResourceVersions(context.Background(), resource)
	if err != nil {
		t.Fatalf("reading versions: %v", err)
	}

	return versions
}

// TestEndToEndResourceTagPlacesCheckInAndOut is the feature: a source only
// reachable from a worker's network is checked, fetched and pushed from that
// worker, and the fetched bytes still come home for the local step that
// reads them.
func TestEndToEndResourceTagPlacesCheckInAndOut(t *testing.T) {
	dir := t.TempDir()
	path := resourceWorkerPipeline(t, dir)

	mustRun(t, path, "--worker", "vpc=local:")

	// The check the get resolved its version with ran there, the in: ran
	// there, and its tree came back here.
	fetched := readFileString(t, filepath.Join(dir, "fetched.txt"))
	if fetched != "vpc/vpc" {
		t.Errorf("in: reported %q, want the worker's tag twice — the check or the fetch ran here, or the tree never came back", fetched)
	}

	// The check ran there: the version it reported carries the tag.
	versions := checkedVersions(t, path, "repo")
	if len(versions) == 0 || versions[0]["where"] != "vpc" {
		t.Errorf("checked versions = %v, want one reporting the worker", versions)
	}

	assertRecordedOnWorker(t, path)
}

// assertRecordedOnWorker: both placed steps say where they ran, the local
// one does not, and the machine's own facts are recorded for both.
func assertRecordedOnWorker(t *testing.T, path string) {
	t.Helper()

	worker := map[string]string{}

	for _, row := range stepEvents(t, path) {
		if row.Type == "step_finished" {
			worker[row.StepName] = row.Worker
		}
	}

	for _, name := range []string{"src", "repo"} {
		if !strings.HasPrefix(worker[name], "vpc (") {
			t.Errorf("step %q recorded worker %q, want the resource's tag", name, worker[name])
		}
	}

	if worker["note"] != "" {
		t.Errorf("the local task recorded worker %q, want none", worker["note"])
	}

	placed := 0

	for _, row := range runPlacements(t, path) {
		if row.Tag == "vpc" {
			placed++
		}
	}

	if placed != 2 {
		t.Errorf("run_placements has %d rows for the tag, want the get and the put", placed)
	}
}

// TestEndToEndStepTagOverridesResourceTag: the step's own tags: wins over the
// resource's, so one pipeline can fetch a resource from two places.
func TestEndToEndStepTagOverridesResourceTag(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
    tags: [other]
  - task: note
    inputs: [src]
    run: cp src/where.txt `+filepath.Join(dir, "fetched.txt")+`
`)

	mustRun(t, path, "--worker", "vpc=local:", "--worker", "other=local:")

	// The check is the resource's and runs where the resource says; the
	// fetch is the step's.
	fetched := readFileString(t, filepath.Join(dir, "fetched.txt"))
	if fetched != "vpc/other" {
		t.Errorf("in: reported %q, want the resource's tag for the check and the step's own for the fetch", fetched)
	}
}

// TestEndToEndPlacedGetCachesLikeALocalOne: the invariant that keeps tags:
// out of the hash holds for a get too — a second run skips the placed fetch
// and everything under it.
func TestEndToEndPlacedGetCachesLikeALocalOne(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - task: note
    inputs: [src]
    run: cp src/where.txt `+filepath.Join(dir, "fetched.txt")+`
`)

	mustRun(t, path, "--worker", "vpc=local:")

	first := readFileString(t, filepath.Join(dir, "fetched.txt"))

	err := os.Remove(filepath.Join(dir, "fetched.txt"))
	if err != nil {
		t.Fatalf("removing the fetched file: %v", err)
	}

	// Same version, same inputs: the get is a cache hit and the chain under
	// it is skipped, so nothing is refetched and nothing rewritten.
	mustRun(t, path, "--worker", "vpc=local:")

	assertNoFile(t, filepath.Join(dir, "fetched.txt"))

	if first != "vpc/vpc" {
		t.Fatalf("the first run fetched %q, want it fetched on the worker", first)
	}
}

// TestEndToEndUnmappedResourceTagRefusesBeforeRunning: a resource that says
// it needs a machine does not quietly get checked from this one.
func TestEndToEndUnmappedResourceTagRefusesBeforeRunning(t *testing.T) {
	dir := t.TempDir()
	path := resourceWorkerPipeline(t, dir)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("a job whose resource carries an unmapped tag ran anyway")
	}

	if !strings.Contains(err.Error(), "vpc") {
		t.Errorf("error = %v, want it to name the unmapped tag", err)
	}

	assertNoFile(t, filepath.Join(dir, "fetched.txt"))
}

// TestEndToEndPollerChecksOnTheResourceWorker: the poller has no job and so
// no job lease, and it is the caller that most needs placement — a source
// only reachable from a worker has to be polled from there. One poll, through
// the CLI, with a lease of its own.
func TestEndToEndPollerChecksOnTheResourceWorker(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
    trigger: true
`)

	mustRun(t, "web", path, "--once", "--worker", "vpc=local:")

	versions := checkedVersions(t, path, "repo")
	if len(versions) == 0 || versions[0]["where"] != "vpc" {
		t.Errorf("the poll recorded %v, want a version the worker reported", versions)
	}
}

// TestEndToEndPollerRefusesAnUnmappedResourceTag: before anything is polled,
// on the same terms as a job.
func TestEndToEndPollerRefusesAnUnmappedResourceTag(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
    trigger: true
`)

	err := cli.Run([]string{"web", path, "--once"})
	if err == nil {
		t.Fatal("a poll of a resource with an unmapped tag ran anyway")
	}

	if !strings.Contains(err.Error(), "vpc") {
		t.Errorf("error = %v, want it to name the unmapped tag", err)
	}

	if versions := checkedVersions(t, path, "repo"); len(versions) != 0 {
		t.Errorf("the refused poll still recorded %v", versions)
	}
}

// TestEndToEndPinnedGetChecksOnTheResourceWorker: a pin is answered by the
// get's OWN check rather than by recorded history, so this is the one run
// where the check inside version resolution has to be placed too.
func TestEndToEndPinnedGetChecksOnTheResourceWorker(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - get: src
    resource: repo
  - task: note
    inputs: [src]
    run: cp src/where.txt `+filepath.Join(dir, "fetched.txt")+`
`)

	mustRun(t, path, "--worker", "vpc=local:", "--pin", "where=vpc")

	fetched := readFileString(t, filepath.Join(dir, "fetched.txt"))
	if fetched != "vpc/vpc" {
		t.Errorf("in: reported %q, want the pinned version found by a check on the worker", fetched)
	}
}

// TestEndToEndPutEvictionSpendsNoAttempts: the eviction promise a task has,
// held for a resource stage — a put reclaimed mid-command ends its attempts:
// loop rather than grinding the author's budget against a dead host.
func TestEndToEndPutEvictionSpendsNoAttempts(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "execs")
	t.Setenv(drainingWorkerEnv, count)

	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [gpu]
  source: {}

jobs:
- name: build
  plan:
  - put: repo
    attempts: 5
`)

	err := cli.Run([]string{path, "--worker", "gpu=local:"})
	if err == nil {
		t.Fatal("a put on a permanently reclaimed worker reported success")
	}

	if !strings.Contains(err.Error(), "reclaimed") {
		t.Errorf("error = %v, want it to say the worker was reclaimed", err)
	}

	execs := readFileString(t, count)
	if len(execs) != 1 {
		t.Fatalf("the worker saw %d commands, want 1 — the eviction was billed to the author's attempts:", len(execs))
	}
}

// TestEndToEndPutHookIsRecordedOnTheResourceWorker: a put hook inherits its
// resource's tag like a plan put, and is recorded under the hook's own scope
// — a hook has no node — so the machine it billed is not lost.
func TestEndToEndPutHookIsRecordedOnTheResourceWorker(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, probeType+`
resources:
- name: repo
  type: probe
  tags: [vpc]
  source: {}

jobs:
- name: build
  plan:
  - task: make
    outputs: [src]
    run: echo made > src/where.txt
    on_success:
      put: repo
      inputs: [src]
`)

	mustRun(t, path, "--worker", "vpc=local:")

	for _, row := range runPlacements(t, path) {
		if row.Tag == "vpc" && strings.Contains(row.Slot, "hook") {
			return
		}
	}

	t.Fatal("no run_placements row for the put hook — the machine it ran on was not recorded")
}
