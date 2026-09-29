package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// artifactResumePipeline is a job that changes a get's artifact in one step
// and needs that change in the next, which fails until flag exists. Every
// fetch appends to fetches.log, so a resume that fetches again is counted.
// tail follows the failing step.
func artifactResumePipeline(dir, versions, gets, tail string) string {
	return fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: |
      echo {{ .version.n | shellquote }} >> %[2]s
      echo {{ .version.n | shellquote }} > n.txt
- name: recorder
  config:
    out: printf '{"published":"yes"}\n'

resources:
- name: ticks
  type: counter
  source: {}
- name: publication
  type: recorder
  source: {}

jobs:
- name: build
  on_failure:
    task: notify
    run: echo failed >> %[3]s
  plan:
%[6]s
  - task: change
    inputs: [repo]
    outputs: [repo]
    run: |
      echo change >> %[4]s
      echo "changed $(cat repo/n.txt)" > repo/changed.txt
  - task: fragile
    inputs: [repo]
    run: |
      cat repo/changed.txt >> %[5]s && { test -f %[7]s || test "$(cat repo/n.txt)" = one; }
%[8]s
`,
		versions,
		filepath.Join(dir, "fetches.log"),
		filepath.Join(dir, "notified.log"),
		filepath.Join(dir, "change.log"),
		filepath.Join(dir, "seen.log"),
		gets,
		filepath.Join(dir, "fixed"),
		tail,
	)
}

// failThenCapture runs the pipeline expecting failure, returning the run id
// and the workspace a resume of it continues in.
//
// The workspace comes from the run's row, not the "workspace kept at" line:
// that line names the job-level build, while the row is re-pointed at the
// tree the get fetched into, which is the one a resume reuses.
func failThenCapture(t *testing.T, path string) (string, string) {
	t.Helper()

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	runID := resumeID(t, out)

	var root string

	err := openStateDB(t, path).QueryRowContext(t.Context(), `SELECT workspace FROM runs WHERE id = ?`, runID).Scan(&root)
	if err != nil {
		t.Fatal(err)
	}

	return runID, root
}

// TestResumeContinuesFromTheArtifactTheSkippedStepsChanged is #190: a resume
// fetched the get again, the fetch replaced the artifact, and the steps that
// had changed it were skipped as already succeeded — so the retried step ran
// on a clean checkout, missing everything the run was resumed to keep.
func TestResumeContinuesFromTheArtifactTheSkippedStepsChanged(t *testing.T) {
	for name, gets := range map[string]struct {
		plan    string
		fetches int
	}{
		"plain get":      {"  - get: repo\n    resource: ticks", 1},
		"version: every": {"  - get: repo\n    resource: ticks\n    version: every", 1},
		"in-place get":   {"  - get: a\n    resource: ticks\n  - get: repo\n    resource: ticks", 2},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			versions := filepath.Join(dir, "versions.json")
			writePipelineFile(t, versions, `[{"n":"two"}]`)

			path := writePipeline(t, dir, artifactResumePipeline(dir, versions, gets.plan, ""))

			runID, _ := failThenCapture(t, path)
			assertLineCount(t, filepath.Join(dir, "fetches.log"), gets.fetches)

			writePipelineFile(t, filepath.Join(dir, "fixed"), "")

			out := captureStdout(t, func() {
				err := cli.Run([]string{"run", path, "--resume", runID})
				if err != nil {
					t.Fatalf("resume failed: %v", err)
				}
			})

			assertLineCount(t, filepath.Join(dir, "fetches.log"), gets.fetches)
			assertLineCount(t, filepath.Join(dir, "change.log"), 1)

			if got := readFileString(t, filepath.Join(dir, "seen.log")); strings.Count(got, "changed two") != 2 {
				t.Errorf("the retried step did not see the change the skipped step made: seen.log = %q", got)
			}

			if n := strings.Count(out, "skip: repo (already fetched"); n != 1 {
				t.Errorf("the kept get was reported %d times, want once:\n%s", n, out)
			}
		})
	}
}

// TestResumeKeepsTheLastBuildsArtifactWhenEarlierBuildsFinished: a resume
// has one tree, the last build's, and every build of it is handed that tree.
// A finished earlier build fetching again would put ITS version over the
// artifact the last build is continuing from.
//
// The put keeps build #0's chain out of the step cache, which would otherwise
// skip it whole and hide a fetch it should not make.
func TestResumeKeepsTheLastBuildsArtifactWhenEarlierBuildsFinished(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions,
		"  - get: repo\n    resource: ticks\n    version: every", "  - put: publication\n    inputs: [repo]"))

	runID, _ := failThenCapture(t, path)
	assertLineCount(t, filepath.Join(dir, "fetches.log"), 2)

	writePipelineFile(t, filepath.Join(dir, "fixed"), "")

	err := cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 2)
	assertLineCount(t, filepath.Join(dir, "change.log"), 2)

	lines := strings.Split(strings.TrimSpace(readFileString(t, filepath.Join(dir, "seen.log"))), "\n")
	if last := lines[len(lines)-1]; last != "changed two" {
		t.Errorf("build #1 continued from %q, want its own changed artifact; seen.log = %q", last, lines)
	}
}

// TestResumeRefusesTwoBuildsWithWorkLeft: two builds that both got partway
// cannot continue in the one tree a run keeps. The refusal comes before the
// run is put back in flight, so the job's on_failure does not page anybody
// over a command that was turned away.
func TestResumeRefusesTwoBuildsWithWorkLeft(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"},{"n":"three"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks\n    version: every", ""))

	runID, _ := failThenCapture(t, path)
	assertLineCount(t, filepath.Join(dir, "fetches.log"), 2)
	assertLineCount(t, filepath.Join(dir, "notified.log"), 1)

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil {
		t.Fatalf("the resume continued two unfinished builds in one tree:\n%s", out)
	}

	for _, want := range []string{"#0", "--pin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 2)
	assertLineCount(t, filepath.Join(dir, "change.log"), 2)
	assertLineCount(t, filepath.Join(dir, "notified.log"), 1)

	if strings.Contains(out, "resume with:") {
		t.Errorf("a refused resume offered itself again:\n%s", out)
	}
}

// TestResumeRefusesWhenTheKeptArtifactIsGone: a get the skipped steps changed
// cannot be fetched fresh without losing their work, so an artifact missing
// from the kept tree is named rather than quietly replaced.
func TestResumeRefusesWhenTheKeptArtifactIsGone(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks", ""))

	runID, root := failThenCapture(t, path)

	err := os.RemoveAll(filepath.Join(root, "artifacts", "repo"))
	if err != nil {
		t.Fatal(err)
	}

	writePipelineFile(t, filepath.Join(dir, "fixed"), "")

	out := captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil {
		t.Fatalf("the resume continued without the artifact the skipped steps changed:\n%s", out)
	}

	for _, want := range []string{`get "repo" was already fetched`, root} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 1)
}

// TestResumeRefusesWhenTheKeptWorkspaceIsGone: with the whole tree gone there
// is nothing to continue from, and that is said before anything runs.
func TestResumeRefusesWhenTheKeptWorkspaceIsGone(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	writePipelineFile(t, versions, `[{"n":"two"}]`)

	path := writePipeline(t, dir, artifactResumePipeline(dir, versions, "  - get: repo\n    resource: ticks", ""))

	runID, root := failThenCapture(t, path)

	err := os.RemoveAll(root)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil || !strings.Contains(err.Error(), "workspace "+root+" is gone") {
		t.Fatalf("want a refusal naming the missing workspace %s, got %v", root, err)
	}

	assertLineCount(t, filepath.Join(dir, "fetches.log"), 1)

	if strings.Contains(out, "resume with:") {
		t.Errorf("the refusal came after the run was put back in flight:\n%s", out)
	}
}
