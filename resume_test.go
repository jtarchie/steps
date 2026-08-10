package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// resumeIDPattern pulls the run id out of the line a failed run prints.
var resumeIDPattern = regexp.MustCompile(`--resume ([A-Za-z0-9]+)`)

// TestResumeSkipsWhatAlreadySucceeded is the incident this exists for: a
// 50-minute job whose LAST step failed on a one-line shell bug, where the only
// options were re-running everything (and getting a different answer from the
// agent) or running the failed command by hand outside the tool.
func TestResumeSkipsWhatAlreadySucceeded(t *testing.T) {
	dir := t.TempDir()

	expensive := filepath.Join(dir, "expensive.log")
	fragile := filepath.Join(dir, "fragile.log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: expensive
    run: echo ran >> %[1]s
  - task: fragile
    run: |
      echo attempt >> %[2]s
      test -f %[3]s
`, expensive, fragile, flag))

	out := captureStdout(t, func() {
		err := run([]string{"run", path, "--job", "publish"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	runID := resumeID(t, out)

	assertLineCount(t, expensive, 1)
	assertLineCount(t, fragile, 1)

	// Fix the thing that broke, then resume.
	writePipelineFile(t, flag, "")

	out = captureStdout(t, func() {
		err := run([]string{"run", path, "--resume", runID})
		if err != nil {
			t.Fatalf("resume failed: %v", err)
		}
	})

	// The expensive step did NOT run again — the whole point.
	assertLineCount(t, expensive, 1)
	assertLineCount(t, fragile, 2)

	if !strings.Contains(out, "already succeeded") {
		t.Errorf("the resume did not say what it skipped:\n%s", out)
	}
}

// TestResumeKeepsTheWorkspace verifies the artifacts of the finished steps
// survive into the resumed run. Without that, "resume" would mean running the
// remaining steps against empty inputs and calling it a recovery.
func TestResumeKeepsTheWorkspace(t *testing.T) {
	dir := t.TempDir()

	seen := filepath.Join(dir, "seen.log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: produce
    run: echo from-the-first-run > artifact.txt
  - task: consume
    run: test -f %[2]s && cat artifact.txt >> %[1]s
`, seen, flag))

	out := captureStdout(t, func() {
		err := run([]string{"run", path, "--job", "publish"})
		if err == nil {
			t.Fatal("expected the consume step to fail")
		}
	})

	runID := resumeID(t, out)

	writePipelineFile(t, flag, "")

	err := run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if got := readFileString(t, seen); !strings.Contains(got, "from-the-first-run") {
		t.Errorf("the resumed step saw %q, want the first run's artifact", got)
	}
}

// TestResumeRejectsAnUnknownRun keeps a typo from silently starting a fresh
// run that looks like a recovery.
func TestResumeRejectsAnUnknownRun(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: publish
  plan:
  - task: work
    inputs: []
    run: "true"
`)

	err := run([]string{"run", path, "--resume", "NOSUCHRUN"})
	if err == nil {
		t.Fatal("resuming a run that was never recorded succeeded")
	}

	if !strings.Contains(err.Error(), "no run") {
		t.Errorf("error does not say the run is unknown: %v", err)
	}
}

// TestResumeUnknownRunUnderIsolation: a run id nothing recorded is refused
// under an isolating strategy exactly as it is under the shared one.
//
// This replaces a test that pinned the OPPOSITE contract — that --resume was
// refused outright under any workspace: strategy. That refusal was written
// when an isolating build tore its tree down per step; what it actually cost
// was recovery for the one shape that needs it most, since max_in_flight:
// REQUIRES isolation. Continuing an isolated run is now supported (see
// resume_isolation_test.go); an unknown id is still an error.
func TestResumeUnknownRunUnderIsolation(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: publish
  plan:
  - task: work
    inputs: []
    run: "true"
`)

	err := run([]string{"run", path, "--resume", "NOSUCHRUN"})
	if err == nil {
		t.Fatal("resume accepted a run id nothing recorded")
	}
}

// resumeID extracts the run id a failed run printed.
func resumeID(t *testing.T, out string) string {
	t.Helper()

	match := resumeIDPattern.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no resume id in output:\n%s", out)
	}

	return match[1]
}

// TestResumeDoesNotSkipAFailedBranch pins the worst shape a resume can take:
// reporting success while the work that failed was never re-attempted.
//
// The completed-step marker used to be recorded from inside the branch
// runners, under the ENCLOSING BLOCK's plan index — so one succeeding branch
// marked the whole in_parallel: as done, and the resume skipped the block
// including the branch that had failed. The job then exited 0.
func TestResumeDoesNotSkipAFailedBranch(t *testing.T) {
	dir := t.TempDir()

	good := filepath.Join(dir, "good.log")
	bad := filepath.Join(dir, "bad.log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: good
        inputs: []
        run: echo ran >> %[1]s
      - task: bad
        inputs: []
        run: echo attempt >> %[2]s && test -f %[3]s
`, good, bad, flag))

	out := captureStdout(t, func() {
		err := run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the failing branch to fail the block")
		}
	})

	runID := resumeID(t, out)

	writePipelineFile(t, flag, "")

	err := run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// The failing branch ran again. Without that, the resumed job would have
	// exited 0 having done nothing.
	assertLineCount(t, bad, 2)
}
