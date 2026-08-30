package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// approvalPipeline gates a publishing step behind a human decision.
func approvalPipeline(t *testing.T, dir, timeout string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - task: draft
    inputs: []
    run: echo drafted >> %s
  - approval:
      message: "Draft is ready — publish?"
      timeout: %s
  - task: publish
    inputs: []
    run: echo published >> %s
`, filepath.Join(dir, "draft.log"), timeout, filepath.Join(dir, "publish.log")))
}

// TestApprovalBlocksUntilApproved is the safeguard: the step after the
// approval must not run until a person says yes.
func TestApprovalBlocksUntilApproved(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "30s")

	done := make(chan error, 1)

	go func() { done <- run([]string{"run", path, "--job", "publish"}) }()

	// The draft step runs; the publish step does not, because the plan is
	// parked. Poll rather than sleep-and-hope so the test says what it means.
	waitForFile(t, filepath.Join(dir, "draft.log"))
	assertNoFile(t, filepath.Join(dir, "publish.log"))

	approveFirstPending(t, path, "approve")

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	assertLineCount(t, filepath.Join(dir, "publish.log"), 1)
}

// TestApprovalRejectionFailsTheJob verifies a person saying no is a FAILURE —
// a decision about the work — and that the gated step never runs.
func TestApprovalRejectionFailsTheJob(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "30s")

	done := make(chan error, 1)

	go func() { done <- run([]string{"run", path, "--job", "publish"}) }()

	waitForFile(t, filepath.Join(dir, "draft.log"))
	approveFirstPending(t, path, "reject")

	err := <-done
	if err == nil {
		t.Fatal("the job succeeded despite being rejected")
	}

	if !strings.Contains(err.Error(), "rejected") {
		t.Errorf("error does not say it was rejected: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "publish.log"))
}

// TestApprovalExpiresUnanswered covers the third outcome, which is
// deliberately not a rejection: "nobody looked at this" and "somebody said no"
// are different things to read in a log.
func TestApprovalExpiresUnanswered(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "1s")

	err := run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("the job succeeded with nobody answering the approval")
	}

	if !strings.Contains(err.Error(), "expired unanswered") {
		t.Errorf("error does not distinguish an expiry from a rejection: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "publish.log"))
}

// TestApprovalCannotBeDecidedTwice keeps the audit trail honest: a decision is
// a record, not a setting.
func TestApprovalCannotBeDecidedTwice(t *testing.T) {
	dir := t.TempDir()
	path := approvalPipeline(t, dir, "30s")

	done := make(chan error, 1)

	go func() { done <- run([]string{"run", path, "--job", "publish"}) }()

	waitForFile(t, filepath.Join(dir, "draft.log"))
	approveFirstPending(t, path, "approve")

	<-done

	err := run([]string{"approve", path, "1"})
	if err == nil {
		t.Fatal("an already-decided approval was decided again")
	}
}

// waitForFile blocks until a path exists, so a test asserting on a background
// run says "when this has happened" rather than "after this many seconds".
func waitForFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		if fileExists(path) {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", path)
}

// approveFirstPending waits for an approval to be recorded and decides it.
func approveFirstPending(t *testing.T, pipelinePath, verb string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	var last error

	for time.Now().Before(deadline) {
		last = run([]string{"approvals", verb, pipelinePath, "1"})
		if last == nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("timed out waiting to %s approval 1: %v", verb, last)
}

// fileExists reports whether a path is present.
func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}
