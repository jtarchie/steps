package e2e

import (
	"fmt"
	"path/filepath"
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
// (concourse/concourse#413), which here is --force or --resume."
//
// --force was true. --resume was not, and the shape of the miss is what makes
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
// The blunt repair was to let a resume ignore the cursor the way --force does.
// It passes the test above and is wrong: --force re-opens every version any
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
