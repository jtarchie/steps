package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestRerunRebuildsABuildAgainstTheVersionsItWasCreatedWith is fly rerun-build (#146): a new run, the whole plan from the top, against exactly the versions the run's builds began with — not whatever the check reports now — recording which build it re-ran. Concourse has no cache, so every step of a rerun executes; here the rerun skips the cache to mean the same thing.
func TestRerunRebuildsABuildAgainstTheVersionsItWasCreatedWith(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"one"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: note
    inputs: [ticks]
    run: cat ticks/n.txt >> %[2]s
`, versions, ran))

	mustRun(t, "run", path, "--job", "build")

	st := openStoreFor(t, path)

	first := latestRunID(t, path)

	// A newer version arrives. An ordinary run would take it; the rerun must not.
	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	mustRun(t, "run", path, "--rerun", first)

	if got := strings.Fields(readFileString(t, ran)); strings.Join(got, " ") != "one one" {
		t.Fatalf("builds read %v, want [one one]: the rerun either took the newer version or was skipped by the cache", got)
	}

	second := latestRunID(t, path)

	run, ok, err := st.FindRunRow(t.Context(), second)
	if err != nil || !ok {
		t.Fatalf("FindRunRow(%s) = %v, %v", second, ok, err)
	}

	if second == first || run.RerunOf != first || run.RerunOfBuild != -1 {
		t.Errorf("rerun %s records rerun of %q build %d, want all of %q (-1)", second, run.RerunOf, run.RerunOfBuild, first)
	}

	// Rerunning a rerun reruns the original, as Concourse's RerunBuild does.
	mustRun(t, "run", path, "--rerun", second)

	third := latestRunID(t, path)

	again, _, err := st.FindRunRow(t.Context(), third)
	if err != nil || again.RerunOf != first {
		t.Errorf("a rerun of a rerun records rerun of %q (%v), want the original %q", again.RerunOf, err, first)
	}
}

// TestARerunTakesNothing: a rerun consumes no version, as Concourse's never runs the input algorithm. It matters for a rerun of a PINNED build: the cursor is a high-water mark, so taking the pinned version would leap it over every unbuilt version below, and the next ordinary run would silently skip them.
func TestARerunTakesNothing(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"1"},{"n":"2"},{"n":"3"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: note
    inputs: [ticks]
    run: cat ticks/n.txt >> %[2]s
`, versions, ran))

	mustRun(t, "run", path, "--job", "build", "--pin", "n=3")
	mustRun(t, "run", path, "--rerun", latestRunID(t, path))
	mustRun(t, "run", path, "--job", "build")

	// 3 is not built again: its chain is green, so the cache skips it.
	if got := strings.Join(strings.Fields(readFileString(t, ran)), " "); got != "3 3 1 2" {
		t.Errorf("builds read %q, want %q: the rerun of a pinned build moved the cursor past 1 and 2", got, "3 3 1 2")
	}
}

// TestARerunIsRefusedWhenItsVersionIsGone: Concourse aborts the rerun with "chosen version of input X not available" rather than choose another; so does this, before anything runs.
func TestARerunIsRefusedWhenItsVersionIsGone(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"1"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  version_history: 1

resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: note
    run: "true"
`, versions))

	mustRun(t, "run", path, "--job", "build")
	first := latestRunID(t, path)

	// The check no longer returns 1, and a history of one keeps only 2.
	writePipelineFile(t, versions, `[{"n":"2"}]`)
	mustRun(t, "run", path, "--job", "build")

	err := cli.Run([]string{"run", path, "--rerun", first})
	if err == nil || !strings.Contains(err.Error(), "chosen version of input ticks not available") {
		t.Fatalf("rerun of a build whose version was pruned = %v, want it refused naming the input", err)
	}
}

// TestARerunOfAJobWithNoGetRunsItAgain: there is nothing to bind, so a rerun is the plan again from the top.
func TestARerunOfAJobWithNoGetRunsItAgain(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: note
    run: echo ran >> %[1]s
`, ran))

	mustRun(t, "run", path, "--job", "build")
	mustRun(t, "run", path, "--rerun", latestRunID(t, path))

	assertLineCount(t, ran, 2)
}

// TestARerunOfAnOldBuildDoesNotJumpTheQueueDownstream: Concourse ranks a rerun's outputs at its original's position, so it never overtakes a newer build for passed:. Here passed: selects by version order, not by which build finished last, so a green rerun of an old build hands downstream nothing older than it already had.
func TestARerunOfAnOldBuildDoesNotJumpTheQueueDownstream(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.log")

	writePipelineFile(t, versions, `[{"n":"1"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: test
    run: "true"
- name: deploy
  plan:
  - get: ticks
    passed: [build]
  - task: ship
    inputs: [ticks]
    run: cat ticks/n.txt >> %[2]s
`, versions, deployed))

	mustRun(t, "run", path, "--job", "build")

	runs, err := openStoreFor(t, path).ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v", err)
	}

	writePipelineFile(t, versions, `[{"n":"1"},{"n":"2"}]`)
	mustRun(t, "run", path, "--job", "build")
	mustRun(t, "run", path, "--rerun", runs[0].ID)
	mustRun(t, "run", path, "--job", "deploy")

	if got := strings.TrimSpace(readFileString(t, deployed)); got != "2" {
		t.Errorf("deploy shipped %q, want 2: a rerun of the build of 1 jumped the queue", got)
	}
}

// TestRerunOfAFanOutRunRebuildsEveryBuild: a version: every run holds a build per version, and Retry is "this run again, with the same inputs" — every build, each against its own version, nothing that arrived since. <run>#<n> narrows it to one build, which is Concourse's rerun of one build.
func TestRerunOfAFanOutRunRebuildsEveryBuild(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")

	writePipelineFile(t, versions, `[{"n":"1"},{"n":"2"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: note
    inputs: [ticks]
    run: cat ticks/n.txt >> %[2]s
`, versions, ran))

	mustRun(t, "run", path, "--job", "build")
	first := latestRunID(t, path)

	writePipelineFile(t, versions, `[{"n":"1"},{"n":"2"},{"n":"3"}]`)

	mustRun(t, "run", path, "--rerun", first)
	mustRun(t, "run", path, "--rerun", first+"#1")

	if got := strings.Join(strings.Fields(readFileString(t, ran)), " "); got != "1 2 1 2 2" {
		t.Errorf("builds read %q, want %q: the whole run again, then its second build alone, and never 3", got, "1 2 1 2 2")
	}
}
