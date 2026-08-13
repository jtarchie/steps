package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// replayIDPattern pulls the forked run's id out of the line a replay prints.
var replayIDPattern = regexp.MustCompile(`replay: ([A-Za-z0-9]+) from`)

// TestReplayRerunsOnlyTheTargetStep is the whole feature: tuning the LAST step
// of a plan must not cost the plan.
//
// Agent steps are unconditionally unskippable, so editing a synthesizer's
// prompt re-runs every lens, compiler and reviewer ahead of it — at full price,
// every time. Replay restores the state the source run reached instead: the
// forked workspace carries what earlier steps produced, and their step record
// says they ran.
func TestReplayRerunsOnlyTheTargetStep(t *testing.T) {
	dir := t.TempDir()

	expensive := filepath.Join(dir, "expensive.log")
	tuned := filepath.Join(dir, "tuned.log")

	pipeline := func(message string) string {
		return fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: expensive
    outputs: [artifact]
    run: |
      echo ran >> %[1]s
      echo produced > artifact/file.txt
  - task: tuned
    inputs: [artifact]
    run: |
      echo %[3]s >> %[2]s
      test -s artifact/file.txt
`, expensive, tuned, message)
	}

	path := writePipeline(t, dir, pipeline("first"))

	// --keep-workspace, because a replay forks the tree the source run left.
	err := run([]string{"run", path, "--job", "publish", "--keep-workspace"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	assertLineCount(t, expensive, 1)
	assertLineCount(t, tuned, 1)

	source := runIDFromStore(t, path)

	// The edit under test: only the last step's text changes.
	writePipeline(t, dir, pipeline("second"))

	out := captureStdout(t, func() {
		err = run([]string{"run", path, "--job", "publish", "--replay", source, "--from", "tuned", "--keep-workspace"})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
	})

	if !replayIDPattern.MatchString(out) {
		t.Errorf("the replay did not report a forked run id:\n%s", out)
	}

	// The expensive step did NOT run again — that is the entire point.
	assertLineCount(t, expensive, 1)

	// ...and the tuned step did, against the artifact the first run produced.
	assertLineCount(t, tuned, 2)

	if !strings.Contains(readFile(t, tuned), "second") {
		t.Error("the replayed step ran the old text, so the edit under test was not what executed")
	}
}

// TestReplayForksRatherThanMutating pins the choice not to re-enter the source
// run: the run being compared against has to still be there afterwards, or
// measuring a prompt change destroys its own baseline.
func TestReplayForksRatherThanMutating(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: first
    run: echo ran >> %[1]s
  - task: second
    run: echo ran >> %[1]s
`, marker))

	err := run([]string{"run", path, "--job", "publish", "--keep-workspace"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	source := runIDFromStore(t, path)

	out := captureStdout(t, func() {
		err = run([]string{"run", path, "--job", "publish", "--replay", source, "--from", "second", "--keep-workspace"})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
	})

	match := replayIDPattern.FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no replay id reported:\n%s", out)
	}

	if match[1] == source {
		t.Error("the replay reused the source run's id, so the baseline it was meant to be compared against is gone")
	}

	// The source run's own record survives untouched.
	if !runExists(t, path, source) {
		t.Error("the source run disappeared; a replay must fork history, not overwrite it")
	}
}

// TestReplayRefusesAnUnknownStep keeps a typo from silently replaying the
// whole plan, which is exactly what the feature exists to avoid paying for.
func TestReplayRefusesAnUnknownStep(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: only
    run: echo ran >> %s
`, marker))

	err := run([]string{"run", path, "--job", "publish", "--keep-workspace"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	source := runIDFromStore(t, path)

	err = run([]string{"run", path, "--job", "publish", "--replay", source, "--from", "typo"})
	if err == nil {
		t.Fatal("a replay from a step that does not exist was accepted")
	}

	if !strings.Contains(err.Error(), "no step named") {
		t.Errorf("the error does not name the problem: %v", err)
	}
}

// TestReplayNeedsAFromStep refuses the spelling that would quietly re-run
// everything at full price.
func TestReplayNeedsAFromStep(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: only
    run: echo ran >> %s
`, marker))

	err := run([]string{"run", path, "--job", "publish", "--keep-workspace"})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	err = run([]string{"run", path, "--job", "publish", "--replay", runIDFromStore(t, path)})
	if err == nil {
		t.Fatal("--replay without --from was accepted")
	}

	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("the error does not say what is missing: %v", err)
	}
}

// runIDFromStore returns the most recent run's id for a pipeline. Read from
// the store rather than scraped from stdout: a successful run prints no id,
// and it is successful runs a replay forks.
func runIDFromStore(t *testing.T, pipelinePath string) string {
	t.Helper()

	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) == 0 {
		t.Fatal("no runs recorded")
	}

	return runs[0].ID
}

// runExists reports whether a run's record survives.
func runExists(t *testing.T, pipelinePath, runID string) bool {
	t.Helper()

	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	_, ok, err := st.FindRunRow(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	return ok
}
