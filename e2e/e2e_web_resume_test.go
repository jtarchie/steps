package e2e

// #208 rung 0: a daemon restart resumes the build it interrupted rather than starting it over, as Concourse's build tracker re-runs an orphaned plan and its finished steps short-circuit.

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A durable workspace.root: is the case the restart's stale-build sweep reaches, and volatile: keeps the step cache from passing for a resume.
func TestARestartResumesTheBuildItInterrupted(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	started := filepath.Join(dir, "started")
	block := filepath.Join(dir, "block")
	seen := filepath.Join(dir, "seen")

	err := os.WriteFile(block, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	fake := newRepeatingFakeLLM(t, says("reviewed"))

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true
agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
workspace:
  root: `+root+`
jobs:
- name: build
  interruptible: true
  plan:
  - task: make
    volatile: true
    inputs: []
    outputs: [out]
    run: |
      echo ran >> `+first+`
      echo made > out/file
  - agent: reviewer
    volatile: true
    inputs: [out]
    messages: [Review out/file.]
  - task: use
    volatile: true
    inputs: [out]
    run: |
      echo ran >> `+second+`
      cat out/file > `+seen+`
      touch `+started+`
      while [ -f `+block+` ]; do sleep 0.05; done
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "build")
	waitForFile(t, started)

	interrupted := newestRun(t, served.state, name, "build").ID

	served.stop(t)

	err = os.Remove(block)
	if err != nil {
		t.Fatal(err)
	}

	restarted := startWeb(t, "--db", served.state, "--interval", "1h")
	restarted.state = served.state
	defer restarted.stopIfRunning(t)

	if got := waitForQueueStatuses(t, served.state, name, "build:succeeded"); got != "build:succeeded" {
		t.Fatalf("queue = %s, want the interrupted row resumed to success", got)
	}

	restarted.stop(t)

	runs := listRuns(t, served.state, name, "build")
	if len(runs) != 1 || runs[0].ID != interrupted || runs[0].Status != "succeeded" {
		t.Fatalf("runs = %+v, want the one interrupted run %s carried on to success", runs, interrupted)
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("the finished agent step asked its model %d times, want once: an agent is resumed past, never re-asked", got)
	}

	assertResumedFiles(t, first, second, seen)

	assertStepIDsUnique(t, served.state, name, interrupted)
}

func assertResumedFiles(t *testing.T, first, second, seen string) {
	t.Helper()

	if got := readTrimmed(t, first); got != "ran" {
		t.Errorf("the finished step ran %q, want once: the resume did not skip it", got)
	}

	if got := readTrimmed(t, second); got != "ran\nran" {
		t.Errorf("the interrupted step ran %q, want twice: once cut off, once from scratch", got)
	}

	if got := readTrimmed(t, seen); got != "made" {
		t.Errorf("the resumed step read %q from its input, want what the skipped step made: the kept workspace was not the one resumed", got)
	}
}

// A resumed run appends to the events of the attempt it continues, and the run page folds rows by step id: an id minted twice merges a step of one attempt into another of the next.
func assertStepIDsUnique(t *testing.T, state, name, runID string) {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	rows, err := st.RunEvents(t.Context(), runID, 0, 0)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	owner := map[int64]string{}

	for _, row := range rows {
		if row.Type != events.TypeStepStarted {
			continue
		}

		if previous, taken := owner[row.StepID]; taken {
			t.Errorf("step id %d started twice (%s, then %s)", row.StepID, previous, row.StepName)
		}

		owner[row.StepID] = row.StepName
	}

	if len(owner) < 4 {
		t.Errorf("only %d steps started across both attempts, want make, the agent, use and use again", len(owner))
	}
}

// A restart while a run waits on an approval resumes the run onto the approval it already made. It used to ask again, leaving one more pending approval per restart that nothing waited on, so deciding the old one did nothing and the run stayed parked.
func TestARestartKeepsTheApprovalARunWaitsOn(t *testing.T) {
	restartKeepsTheApproval(t, "")
}

// The same, behind a step the resume skips: skipping it left the next step's parent hash unset, so the approval hashed differently and asked again (steps#230).
func TestARestartKeepsTheApprovalBehindAFinishedStep(t *testing.T) {
	restartKeepsTheApproval(t, `
  - task: draft
    run: echo drafted`)
}

func restartKeepsTheApproval(t *testing.T, before string) {
	t.Helper()

	dir := t.TempDir()
	done := filepath.Join(dir, "done")

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true
workspace:
  root: `+filepath.Join(dir, "ws")+`
jobs:
- name: deploy
  interruptible: true
  plan:`+before+`
  - approval:
      message: ship it?
      timeout: 1h
  - task: ship
    volatile: true
    run: echo shipped > `+done+`
`)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "deploy")

	original := waitForPendingApproval(t, served.state, name)

	served.stop(t)

	restarted := startWeb(t, "--db", served.state, "--interval", "1h")
	restarted.state = served.state
	defer restarted.stopIfRunning(t)

	// Decided over the CLI, as a person would, against the approval they were shown before the restart.
	err := cli.Run([]string{"approvals", "approve", strconv.FormatInt(original, 10), "-p", name, "--db", served.state})
	if err != nil {
		t.Fatalf("approving approval %d: %v", original, err)
	}

	waitForFile(t, done)

	if got := waitForQueueStatuses(t, served.state, name, "deploy:succeeded"); got != "deploy:succeeded" {
		t.Fatalf("queue = %s, want the resumed run carried through the approval it was already waiting on", got)
	}

	restarted.stop(t)

	if all := listApprovals(t, served.state, name); len(all) != 1 || all[0].ID != original || all[0].Status != "approved" {
		t.Errorf("approvals = %+v, want only the original, approved: the restart asked again", all)
	}
}

func listApprovals(t *testing.T, state, name string) []store.Approval {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	all, err := st.Approvals(t.Context(), false, 0)
	if err != nil {
		t.Fatalf("Approvals: %v", err)
	}

	return all
}
