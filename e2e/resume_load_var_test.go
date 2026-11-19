package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndAResumeCapturesALoadVarAgain: a resume skipped the load_var: an earlier attempt finished without putting its value back, so the step that had failed read ((.:token)) as "no value in this run" — and the value, never re-registered, would have printed unmasked.
func TestEndToEndAResumeCapturesALoadVarAgain(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: write
    outputs: [meta]
    run: printf 's3cr3t-value' > meta/token.txt
  - load_var: token
    inputs: [meta]
    file: meta/token.txt
  - task: use
    run: |
      echo "[token ((.:token)) ok]"
      test -f %s
`, flag))

	first, err := runOut(t, path)
	if err == nil {
		t.Fatalf("expected the use step to fail before the fix:\n%s", first)
	}

	writePipelineFile(t, flag, "")

	var resumeErr error

	resumed := captureStdout(t, func() {
		resumeErr = stepsRun(t, []string{"run", path, "--resume", resumeID(t, first), "--progress", "plain"})
	})
	if resumeErr != nil {
		t.Fatalf("resume: %v\n%s", resumeErr, resumed)
	}

	if !strings.Contains(resumed, "[token *** ok]") || strings.Contains(resumed, "s3cr3t-value") {
		t.Errorf("the resumed step did not read the captured value hidden:\n%s", resumed)
	}

	if !strings.Contains(resumed, "skip: write (already succeeded)") {
		t.Errorf("the resume re-ran the step that wrote the file instead of keeping it:\n%s", resumed)
	}
}

// TestEndToEndAResumeRefusesAReaderWhoseSenderItWouldSkip: an agent's decision lives only in the attempt that made it, so a resume that skips the sender would hand its context: from: reader nothing — refused up front rather than run on a missing decision.
func TestEndToEndAResumeRefusesAReaderWhoseSenderItWouldSkip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	flag := filepath.Join(dir, "fixed")

	fake := newFakeLLM(t,
		callsTool("verdict", map[string]any{"choice": "bug"}),
		says("Classified."),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: triager
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - agent: triager
    inputs: []
    messages:
      - Classify the report.
    verdicts: [bug, feature]
  - task: record
    inputs: []
    context:
      from:
        triager: verdict
    run: |
      cat upstream/triager
      test -f %[2]s
`, fake.URL, flag))

	first, err := runOut(t, path)
	if err == nil {
		t.Fatalf("expected the record step to fail before the fix:\n%s", first)
	}

	writePipelineFile(t, flag, "")

	err = stepsRun(t, []string{"run", path, "--resume", resumeID(t, first)})
	if err == nil {
		t.Fatal("the resume ran a reader whose sender it would skip")
	}

	for _, want := range []string{`"record"`, `"triager"`, "start a new run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %s: %v", want, err)
		}
	}

	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want the first attempt's 2 and none from the refused resume", got)
	}
}
