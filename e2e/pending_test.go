package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// A parked approval runs the job's on_pending before the wait, handing it the park as ((.:pending.*)) and STEPS_PENDING_*; the run then waits as before and a person's decision carries it through.
func TestOnPendingAnnouncesAnApproval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notice := filepath.Join(dir, "notice")
	shipped := filepath.Join(dir, "shipped")

	path := writePipeline(t, dir, `
jobs:
- name: release
  on_pending:
    task: notify
    run: printf '%s|%s|%s|%s|%s|%s\n' "((.:pending.id))" "((.:pending.kind))" "((.:pending.job))" "$STEPS_PENDING_MESSAGE" "$STEPS_PENDING_COMMAND" "$STEPS_PENDING_ID" >> `+notice+`
  plan:
  - approval:
      message: "Ship it?"
      timeout: 30s
  - task: ship
    inputs: []
    run: echo shipped > `+shipped+`
`)

	done := make(chan error, 1)

	go func() { done <- stepsRun(t, []string{"run", path, "--job", "release"}) }()

	waitForFile(t, notice)
	assertNoFile(t, shipped)

	approveFirstPending(t, path, "approve")

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.TrimSpace(readFileString(t, notice))
	want := "1|approval|release|Ship it?|steps approvals approve 1 " + strings.Join(readArgs(path), " ") + "|1"

	if got != want {
		t.Errorf("on_pending wrote %q, want %q", got, want)
	}
}

// The nearest on_pending wins: a step's replaces the job's for that step, so one park is one notice. Its failure is logged and the wait's outcome is the wait's own: here nobody answers, so the step expires.
func TestOnPendingNearestWinsAndItsFailureLeavesTheWaitAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	jobNotice := filepath.Join(dir, "job")
	stepNotice := filepath.Join(dir, "step")

	path := writePipeline(t, dir, `
jobs:
- name: release
  on_pending:
    task: job-notice
    run: echo job >> `+jobNotice+`
  plan:
  - approval:
      message: "Ship it?"
      timeout: 1s
    on_pending:
      task: step-notice
      inputs: []
      run: echo "((.:pending.expires_at))" >> `+stepNotice+`; exit 1
`)

	err := stepsRun(t, []string{"run", path, "--job", "release"})
	if err == nil || !strings.Contains(err.Error(), "expired unanswered") {
		t.Fatalf("run = %v, want the approval to expire unanswered despite its on_pending failing", err)
	}

	assertNoFile(t, jobNotice)
	assertLineCount(t, stepNotice, 1)

	if got := strings.TrimSpace(readFileString(t, stepNotice)); !strings.Contains(got, "T") || !strings.HasSuffix(got, "Z") {
		t.Errorf("pending.expires_at = %q, want an RFC 3339 UTC time", got)
	}
}
