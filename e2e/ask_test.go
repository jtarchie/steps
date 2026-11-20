package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// An ask: with options captures the answer as ((.:name)) and writes it to <name>/answer; one with none writes only the file. A --answer seed answers the first, and the second, which nobody answers, stands on its default.
func TestAskStepHandsItsAnswerToLaterSteps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - ask: bump
    question: "Which bump?"
    options: [major, minor, patch]
    timeout: 1m
  - ask: note
    question: "Anything for the notes?"
    default: nothing to add
    timeout: 1s
  - task: tag
    inputs: [bump, note]
    run: echo "[((.:bump))|$(cat bump/answer)|$(cat note/answer)]"
`)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "build", "--progress", "plain", "--answer", "which bump=minor"})
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if !strings.Contains(out, "[minor|minor|nothing to add]") {
		t.Errorf("the tag step did not read the answers:\n%s", out)
	}
}

// A closed list refuses an answer it does not offer, and a question nobody answers with no default aborts the step: nobody said no.
func TestAskStepWithoutADefaultAbortsWhenNobodyAnswers(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tagged := filepath.Join(dir, "tagged")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - ask: bump
    question: "Which bump?"
    options: [major, minor, patch]
    timeout: 1s
  - task: tag
    inputs: []
    run: touch `+tagged+`
`)

	err := stepsRun(t, []string{"run", path, "--job", "build", "--answer", "which bump=huge"})
	if err == nil || !strings.Contains(err.Error(), "not one of the offered options") {
		t.Fatalf("run = %v, want the seeded answer refused as not offered", err)
	}

	err = stepsRun(t, []string{"run", path, "--job", "build"})
	if err == nil || !strings.Contains(err.Error(), "expired unanswered") {
		t.Fatalf("run = %v, want the question to expire unanswered", err)
	}

	assertNoFile(t, tagged)
}

// A parked ask: fires on_pending with kind ask, and a person's answer from steps questions answer carries the run on.
func TestAskStepParksForAPersonAndAnnouncesIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	notice := filepath.Join(dir, "notice")

	path := writePipeline(t, dir, `
jobs:
- name: build
  on_pending:
    task: notify
    run: printf '%s|%s|%s|%s\n' "((.:pending.kind))" "$STEPS_PENDING_MESSAGE" "$STEPS_PENDING_OPTIONS" "((.:pending.command))" >> `+notice+`
  plan:
  - ask: bump
    question: "Which bump?"
    options: [major, minor, patch]
    timeout: 1m
  - task: tag
    inputs: []
    run: echo "[((.:bump))]"
`)

	var (
		err error
		out string
	)

	done := make(chan struct{})

	go func() {
		defer close(done)

		out = captureStdout(t, func() { err = stepsRun(t, []string{"run", path, "--job", "build", "--progress", "plain"}) })
	}()

	waitForFile(t, notice)
	mustRun(t, append([]string{"questions", "answer", "1", "major"}, readArgs(path)...)...)
	<-done

	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if !strings.Contains(out, "[major]") {
		t.Errorf("the tag step did not read the person's answer:\n%s", out)
	}

	got := strings.TrimSpace(readFileString(t, notice))
	want := `ask|Which bump?|["major","minor","patch"]|steps questions answer 1 <answer> ` + strings.Join(readArgs(path), " ")

	if got != want {
		t.Errorf("on_pending wrote %q, want %q", got, want)
	}
}

// --resume runs an ask: again, and it finds the answer this run already holds instead of asking: here the resume has no seed and nobody to answer, so asking would expire and abort.
func TestAskStepAResumeKeepsTheAnswerItAlreadyHas(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - ask: bump
    question: "Which bump?"
    options: [major, minor, patch]
    timeout: 1s
  - task: tag
    inputs: [bump]
    run: |
      echo "[((.:bump))|$(cat bump/answer)]"
      test -f %s
`, flag))

	var err error

	first := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "build", "--progress", "plain", "--answer", "which bump=patch"})
	})
	if err == nil {
		t.Fatalf("expected the tag step to fail before the fix:\n%s", first)
	}

	writePipelineFile(t, flag, "")

	resumed := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--resume", resumeID(t, first), "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, resumed)
	}

	if !strings.Contains(resumed, "[patch|patch]") {
		t.Errorf("the resumed step did not read the answer the first attempt got:\n%s", resumed)
	}
}

// An ask: that expired unanswered is asked afresh when the run is resumed, as an expired approval is.
func TestAskStepAResumeAsksAgainAfterAnExpiry(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: setup
    inputs: []
    run: echo setup
  - ask: bump
    question: "Which bump?"
    options: [major, minor, patch]
    timeout: 1s
  - task: tag
    inputs: []
    run: echo "[((.:bump))]"
`)

	first, err := runOut(t, path)
	if err == nil || !strings.Contains(err.Error(), "expired unanswered") {
		t.Fatalf("run = %v, want the question to expire unanswered:\n%s", err, first)
	}

	resumed := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--resume", resumeID(t, first), "--progress", "plain", "--answer", "which bump=minor"})
	})
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, resumed)
	}

	if !strings.Contains(resumed, "[minor]") {
		t.Errorf("the resume did not ask again:\n%s", resumed)
	}
}
