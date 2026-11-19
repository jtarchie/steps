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

// A question a model parks mid-step fires the job's on_pending, the model's own text reaching the command only through STEPS_PENDING_*; a question a --answer seed settles is never announced.
func TestOnPendingAnnouncesAQuestionAModelAsks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notice := filepath.Join(dir, "notice")

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		switch req.historyCallCount("ask_user") {
		case 0:
			return callsTool("ask_user", map[string]any{"question": "Seeded?"})
		case 1:
			return callsTool("ask_user", map[string]any{"question": "Which environment?", "options": []any{"staging", "prod"}})
		default:
			return says("done")
		}
	})

	path := writePipeline(t, dir, `
defaults:
  preflight:
    disabled: true
agents:
- name: deployer
  source: { endpoint: `+fake.URL+`/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - builtin: ask_user
    timeout: 2m
jobs:
- name: deploy
  on_pending:
    task: notify
    run: printf '%s|%s|%s|%s|%s\n' "((.:pending.kind))" "((.:pending.job))" "$STEPS_PENDING_MESSAGE" "$STEPS_PENDING_OPTIONS" "$STEPS_PENDING_COMMAND" >> `+notice+`
  plan:
  - agent: deployer
    messages: [Ask, then say done.]
`)

	done := make(chan error, 1)

	go func() {
		done <- stepsRun(t, []string{"run", path, "--job", "deploy", "--answer", "Seeded?=seeded-only"})
	}()

	waitForFile(t, notice)

	// The seed settled question 1, so the one parked is 2.
	id := "2"
	mustRun(t, append([]string{"questions", "answer", id, "staging"}, readArgs(path)...)...)

	err := <-done
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := strings.TrimSpace(readFileString(t, notice))
	want := `ask_user|deploy|Which environment?|["staging","prod"]|steps questions answer ` + id + ` <answer> ` + strings.Join(readArgs(path), " ")

	if got != want {
		t.Errorf("on_pending wrote %q, want %q: one notice, for the question a person had to answer", got, want)
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
