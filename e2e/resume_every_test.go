package e2e

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestResumeReachesAVersionTheCursorAlreadyTook is the recovery path
// `version: every` had no way to reach.
//
// The cursor is taken as a build STARTS, whatever that build then does — which
// is Concourse's own rule and is deliberate (see runGetStep). The consequence
// is that a build failing halfway leaves its version consumed, so the ordinary
// "run it again" finds nothing to do. pipeline/get.go names the way out in as
// many words: "Re-running one is an explicit act there
// (concourse/concourse#413), which here is --resume or --pin."
//
// --resume selected nothing, and the shape of the miss is what makes
// it worth a test rather than a one-line change: the resumed run did not fail
// saying it could not reach the version, it selected NOTHING, ran zero steps,
// and exited GREEN — a resume of a failed run reporting success having
// continued nothing at all.
func TestResumeReachesAVersionTheCursorAlreadyTook(t *testing.T) {
	dir := t.TempDir()

	ran := filepath.Join(dir, "ran.log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
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
  - task: fragile
    run: |
      echo attempt >> %[1]s
      test -f %[2]s
`, ran, flag))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	runID := resumeID(t, out)
	assertLineCount(t, ran, 1)

	// Fix what broke, then continue the run — exactly what the failure message
	// told the operator to do.
	writePipelineFile(t, flag, "")

	out = captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--resume", runID})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	// The load-bearing assertion: the step the run died on actually ran again.
	// Before the fix this was still 1 — the get selected nothing, so the plan
	// behind it never executed.
	assertLineCount(t, ran, 2)

	if strings.Contains(out, "already taken") {
		t.Errorf("the resume was turned away by the cursor it is supposed to override:\n%s", out)
	}
}

// TestResumeDoesNotReopenAnotherRunsVersions is the boundary of the fix
// above, and the reason the re-opening is keyed by RUN rather than simply
// switched off.
//
// The blunt repair was to let a resume ignore the cursor wholesale.
// It passes the test above and is wrong: that re-opens every version any
// run ever took, so resuming one failed build would rebuild the history behind
// it -- re-running an agent, re-pushing a branch, re-opening a pull request
// for work that was finished and green. Concourse keeps these apart for the
// same reason (fly rerun-build re-runs ONE build against the inputs it was
// created with; it does not reschedule the job), and so does this: --resume
// re-opens the versions of the run named on the command line, and no others.
func TestResumeDoesNotReopenAnotherRunsVersions(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	published := filepath.Join(dir, "published.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, versions, `[{"n":"one"}]`)

	// The put: is what makes this test able to fail, and finding that out is
	// the point of it. A chain of cacheable steps re-selected by mistake is
	// SKIPPED by the merkle cache, so a resume that wrongly re-opened a green
	// version looked identical to one that did not. A put is never skippable —
	// neither is an agent — so the version gets published a second time, which
	// in the pipeline this came from means a branch re-pushed and a pull
	// request re-opened for work that was already merged.
	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt
- name: recorder
  config:
    out: |
      cat ticks/n.txt >> %[2]s
      printf '{"published":"yes"}\n'

resources:
- name: ticks
  type: counter
  source: {}
- name: publication
  type: recorder
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: fragile
    inputs: [ticks]
    run: test "$(cat ticks/n.txt)" != two || test -f %[3]s
  - put: publication
    inputs: [ticks]
`, versions, published, flag))

	// Run one takes "one" and publishes it. This is the green history a
	// resume must not disturb.
	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("the first version should have built: %v", err)
	}

	assertLineCount(t, published, 1)

	// Run two takes "two" and fails before publishing.
	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the second version to fail")
		}
	})

	runID := resumeID(t, out)
	assertLineCount(t, published, 1)

	// Fix what broke, then resume.
	writePipelineFile(t, flag, "")

	err = cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// Two, not three: "two" published at last, "one" left alone. Three is the
	// resume behaving like --force and re-publishing a version that was
	// already green.
	assertLineCount(t, published, 2)
}

// fanOutPipeline is a two-version fan-out whose second build fails until flag
// exists. The put: is what keeps the chain unskippable, so a green build can
// only be passed over on a resume by the run's own step record — never by the
// merkle cache — and a build wrongly passed over publishes nothing.
func fanOutPipeline(versions, ran, published, flag, defaults string) string {
	return fmt.Sprintf(`
%[5]s
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt
- name: recorder
  config:
    out: |
      cat ticks/n.txt >> %[3]s
      printf '{"published":"yes"}\n'

resources:
- name: ticks
  type: counter
  source: {}
- name: publication
  type: recorder
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: fragile
    inputs: [ticks]
    run: |
      cat ticks/n.txt >> %[2]s
      test "$(cat ticks/n.txt)" != two || test -f %[4]s
  - put: publication
    inputs: [ticks]
`, versions, ran, published, flag, defaults)
}

// TestResumeRerunsTheBuildThatFailedNotItsSibling is #144: run_steps was keyed
// by (run, index), and every build of a version: every fan-out walks its
// remainder from index 0 — so build #1's steps collided with build #0's, and a
// resume asked build #1 whether index 0 was done and heard build #0's yes.
// It skipped the failed task AND the put, and exited green having retried
// nothing.
func TestResumeRerunsTheBuildThatFailedNotItsSibling(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")
	published := filepath.Join(dir, "published.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	path := writePipeline(t, dir, fanOutPipeline(versions, ran, published, flag, ""))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected build #1 to fail")
		}
	})

	runID := resumeID(t, out)

	// Two, or the fan-out never happened and everything below is vacuous.
	assertLineCount(t, ran, 2)
	assertLineCount(t, published, 1)

	writePipelineFile(t, flag, "")

	out = captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--resume", runID})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	assertLineCount(t, published, 2)
	assertLineCount(t, ran, 3)

	lines := strings.Fields(readFileString(t, ran))
	if lines[len(lines)-1] != "two" || strings.Count(readFileString(t, ran), "one") != 1 {
		t.Errorf("the resume re-ran the wrong build: ran.log = %q", lines)
	}

	if !strings.Contains(out, "skip: fragile (already succeeded) [build #0]") {
		t.Errorf("the resume did not say which build's step it skipped:\n%s", out)
	}

	assertRunStepsPerBuild(t, path, runID)
}

// assertRunStepsPerBuild holds `runs steps <run>` to naming every build's
// steps, and to refusing a run the pipeline never recorded rather than
// printing an empty table.
func assertRunStepsPerBuild(t *testing.T, path, runID string) {
	t.Helper()

	listing := captureStdout(t, func() {
		err := cli.Run(append([]string{"runs", "steps", runID}, readArgs(path)...))
		if err != nil {
			t.Fatalf("runs steps %s: %v", runID, err)
		}
	})

	for _, want := range []string{
		`#0\s+fragile`, `#0\s+publication`, `#1\s+fragile`, `#1\s+publication`,
	} {
		if !regexp.MustCompile(want).MatchString(listing) {
			t.Errorf("runs steps did not list %q:\n%s", want, listing)
		}
	}

	limited := captureStdout(t, func() {
		err := cli.Run(append([]string{"runs", "steps", runID, "--limit", "1"}, readArgs(path)...))
		if err != nil {
			t.Fatalf("runs steps %s --limit 1: %v", runID, err)
		}
	})

	if !regexp.MustCompile(`#0\s+fragile`).MatchString(limited) || strings.Contains(limited, "publication") {
		t.Errorf("runs steps --limit 1 did not stop after the first row:\n%s", limited)
	}

	err := cli.Run(append([]string{"runs", "steps", "NOSUCHRUN"}, readArgs(path)...))
	if err == nil || !strings.Contains(err.Error(), "no run") {
		t.Errorf("runs steps with an unknown run id: want a \"no run\" error, got %v", err)
	}
}

// TestResumeDoesNotMistakeAStepBeforeTheGetForOneAfterIt is the same collision
// without any fan-out: the outer walk records prep at index 0, and the
// remainder after the get numbers fragile 0 too. One version, one build, and
// still a resume that skipped the step it was meant to retry.
func TestResumeDoesNotMistakeAStepBeforeTheGetForOneAfterIt(t *testing.T) {
	dir := t.TempDir()

	prep := filepath.Join(dir, "prep.log")
	fragile := filepath.Join(dir, "fragile.log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - task: prep
    run: echo ran >> %[1]s
  - get: ticks
  - task: fragile
    run: |
      echo attempt >> %[2]s
      test -f %[3]s
`, prep, fragile, flag))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	runID := resumeID(t, out)

	writePipelineFile(t, flag, "")

	err := cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	assertLineCount(t, prep, 1)
	assertLineCount(t, fragile, 2)
}

// TestResumeRefusesABuildWhoseVersionIsGone: a resume rebuilds each build
// against the versions it was created with, so a version version_history:
// has since pruned leaves that build with nothing to rebuild. Concourse
// aborts the rerun ("chosen version of input X not available") rather than
// choose another; so does this, naming the build and the version, and it
// runs nothing — not even the builds whose versions survive.
func TestResumeRefusesABuildWhoseVersionIsGone(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")
	published := filepath.Join(dir, "published.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)

	path := writePipeline(t, dir, fanOutPipeline(versions, ran, published, flag, "defaults:\n  version_history: 2"))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected build #1 to fail")
		}
	})

	runID := resumeID(t, out)

	assertLineCount(t, ran, 2)
	assertLineCount(t, published, 1)

	// The refresh at the start of the resume records "three" and, capped at
	// two, prunes "one" — build #0's version.
	writePipelineFile(t, versions, `[{"n":"three"}]`)
	writePipelineFile(t, flag, "")

	var err error

	out = captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil {
		t.Fatalf("the resume ran a build whose version is gone:\n%s", out)
	}

	for _, want := range []string{"#0", `"one"`, "no longer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	assertLineCount(t, ran, 2)
	assertLineCount(t, published, 1)
}
